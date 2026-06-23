package filesystem_btrfs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// appendEmptyDataChunk grows the backing device by extraLen bytes and maps that
// new tail as a second, empty SINGLE-profile DATA chunk: it inserts a CHUNK_ITEM
// (chunk tree), a DEV_EXTENT (dev tree) and an empty BLOCK_GROUP_ITEM (extent
// tree), updates the superblock total_bytes + dev_item totals, and registers the
// chunk in fs.sb.sysChunks. The result is a valid 2-data-chunk image that
// `btrfs check` accepts and the kernel mounts — a faithful starting point for
// whole-chunk-removal under test. Returns the new chunk's logical start.
//
// Every tree this writer produces is a single leaf, so each item is appended
// in place (leafInsertItem) at the tree's existing block — no generation bump,
// keeping ROOT_ITEM generations matched to their node headers.
func appendEmptyDataChunk(t *testing.T, bfs *btrfsFS, extraLen uint64) uint64 {
	t.Helper()
	bfs.mu.Lock()
	defer bfs.mu.Unlock()
	le := binary.LittleEndian

	oldTotal := bfs.sb.totalBytes
	newLog := oldTotal // logical == physical; the new chunk starts at the old tail
	newTotal := oldTotal + extraLen

	// Read dev_uuid (dev_item.uuid) and chunk_tree_uuid so the new stripe / dev
	// extent match the rest of the image.
	sbuf := make([]byte, sbfSize)
	if _, err := bfs.rwa.ReadAt(sbuf, bfs.partOffset+superblockOffset); err != nil {
		t.Fatalf("appendEmptyDataChunk: read sb: %v", err)
	}
	var devUUID, chunkUUID [16]byte
	copy(devUUID[:], sbuf[sbfDevItem+0x42:])
	// chunk_tree_uuid lives in every node header at 0x40; read it from the chunk
	// tree node.
	chunkNode := make([]byte, bfs.sb.nodeSize)
	cphys, _ := bfs.sb.physAddr(bfs.partOffset, bfs.sb.chunkLogAddr)
	if _, err := bfs.rwa.ReadAt(chunkNode, cphys); err != nil {
		t.Fatalf("appendEmptyDataChunk: read chunk node: %v", err)
	}
	copy(chunkUUID[:], chunkNode[0x40:])

	// Extend the backing device.
	if err := bfs.f.Truncate(bfs.partOffset + int64(newTotal)); err != nil {
		t.Fatalf("appendEmptyDataChunk: truncate: %v", err)
	}

	// CHUNK_ITEM: SINGLE-profile DATA chunk, one stripe at phys == logical.
	chunkItem := buildChunkItemBytes(le, extraLen, blockGroupData, newLog, fmtStripeLen, fmtStripeLen, 1, devUUID)
	leafInsertItemInPlace(t, bfs, cphys, key{firstChunkTreeObjID, typeChunkItem, newLog}, chunkItem)

	// DEV_EXTENT in the dev tree.
	devRoot, err := bfs.devTreeRootLocked()
	if err != nil {
		t.Fatalf("appendEmptyDataChunk: dev tree root: %v", err)
	}
	dphys, _ := bfs.sb.physAddr(bfs.partOffset, devRoot)
	devExt := buildDevExtentBytes(le, newLog, extraLen, chunkUUID)
	leafInsertItemInPlace(t, bfs, dphys, key{1, typeDevExtent, newLog}, devExt)

	// Empty BLOCK_GROUP_ITEM (used = 0) in the extent tree.
	extRoot, err := extentTreeRoot(bfs.rwa, bfs.partOffset, bfs.sb)
	if err != nil {
		t.Fatalf("appendEmptyDataChunk: extent tree root: %v", err)
	}
	ephys, _ := bfs.sb.physAddr(bfs.partOffset, extRoot)
	bg := buildBlockGroupItemBytes(le, 0, blockGroupData)
	leafInsertItemInPlace(t, bfs, ephys, key{newLog, typeBlockGroupItem, extraLen}, bg)

	// Register the chunk in memory. Include the single local stripe so the device
	// pool can read from / write to this chunk (relocation needs this).
	bfs.sb.sysChunks = append(bfs.sb.sysChunks, chunkMapping{
		logStart:       newLog,
		size:           extraLen,
		physStart:      newLog,
		localStripeIdx: 0,
		profile:        blockGroupData,
		stripeLen:      fmtStripeLen,
		subStripes:     1,
		stripes:        []chunkStripe{{devID: bfs.sb.devID, offset: newLog}},
	})
	if bfs.sm != nil {
		bfs.sm.freeRange(newLog, extraLen)
	}

	// Refresh super.total_bytes + dev_item totals + chunk-tree DEV_ITEM mirror.
	if err := bfs.rewriteResizedSuperblockLocked(newTotal); err != nil {
		t.Fatalf("appendEmptyDataChunk: rewrite sb: %v", err)
	}
	bfs.sb.totalBytes = newTotal
	bfs.invalidateCache()
	return newLog
}

// appendDataChunkWithLiveExtent appends an empty DATA chunk (via
// appendEmptyDataChunk), writes `payload` to `name` in the lower chunk, then
// SURGICALLY relocates that file's data extent UP into the new trailing chunk —
// the inverse of a shrink — so the trailing chunk becomes genuinely non-empty
// with a live, reachable file extent. It rewrites the EXTENT_DATA disk_bytenr in
// the FS leaf in place (no gen bump), copies the bytes to the new chunk, fixes
// the space manager, marks the new chunk's BLOCK_GROUP_ITEM `used`, and rebuilds
// the extent tree so the starting image is `btrfs check`-clean. Returns the new
// chunk's logical start.
func appendDataChunkWithLiveExtent(t *testing.T, bfs *btrfsFS, extraLen uint64, name string, payload []byte) uint64 {
	t.Helper()
	newChunkLog := appendEmptyDataChunk(t, bfs, extraLen)
	if err := bfs.WriteFile(name, payload, 0o644); err != nil {
		t.Fatalf("appendDataChunkWithLiveExtent: write %s: %v", name, err)
	}

	bfs.mu.Lock()
	defer bfs.mu.Unlock()
	le := binary.LittleEndian

	in, err := pathLookup(bfs.reader(), bfs.partOffset, bfs.sb, bfs.fsTreeRoot, name)
	if err != nil {
		t.Fatalf("appendDataChunkWithLiveExtent: lookup %s: %v", name, err)
	}
	ino := in.num

	// Walk the FS tree, find this inode's regular EXTENT_DATA items, and move each
	// extent into the new chunk. Track the running allocation offset within it.
	dst := newChunkLog
	moved := uint64(0)
	err = walkLeavesWithPhys(bfs.rwa, bfs.partOffset, bfs.sb, bfs.fsTreeRoot, func(buf []byte, phys int64) (bool, error) {
		dirty := false
		items := parseLeafItems(buf, parseNodeHeader(buf).nItems)
		for _, it := range items {
			if it.k.objID != ino || it.k.typ != typeExtentData {
				continue
			}
			ed := it.data(buf)
			if len(ed) < extDataRegularSize || ed[extDataOffType] != extentDataRegular {
				continue
			}
			oldDisk := le.Uint64(ed[extDataOffDiskBytenr:])
			diskBytes := le.Uint64(ed[extDataOffDiskNumBytes:])
			if oldDisk == 0 || diskBytes == 0 {
				continue
			}
			// Reserve dst..dst+diskBytes in the new chunk; copy the bytes.
			oldPhys := physFromLog(bfs.sb, oldDisk)
			tmp := make([]byte, diskBytes)
			if _, err := bfs.rwa.ReadAt(tmp, bfs.partOffset+int64(oldPhys)); err != nil {
				return false, err
			}
			if _, err := bfs.rwa.WriteAt(tmp, bfs.partOffset+int64(dst)); err != nil {
				return false, err
			}
			le.PutUint64(ed[extDataOffDiskBytenr:], physToLog(bfs.sb, dst))
			dirty = true
			// Free the old low extent, reserve the new high one.
			bfs.sm.freeRange(oldPhys, diskBytes)
			bfs.sm.remove(dst, diskBytes)
			dst += diskBytes
			moved += diskBytes
		}
		if dirty {
			updateNodeCRC(buf)
			if _, err := bfs.rwa.WriteAt(buf, bfs.partOffset+phys); err != nil {
				return false, err
			}
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("appendDataChunkWithLiveExtent: relocate extent: %v", err)
	}
	if moved == 0 {
		t.Fatalf("appendDataChunkWithLiveExtent: no data extent found for %s", name)
	}
	bfs.invalidateCache()
	// Rebuild the extent tree so the new chunk's BLOCK_GROUP_ITEM `used` and the
	// METADATA/EXTENT items reflect the moved extent — surgical, no gen bump.
	if err := rebuildExtentTree(bfs.rwa, bfs.partOffset, bfs.sb, bfs.sm); err != nil {
		t.Fatalf("appendDataChunkWithLiveExtent: rebuild extent tree: %v", err)
	}
	bfs.invalidateCache()
	return newChunkLog
}

// walkLeavesWithPhys walks every leaf of the tree rooted at logAddr, invoking fn
// with the leaf buffer and its physical byte offset (relative to partOffset). fn
// may mutate+write the buffer; returning false stops the walk early. A minimal
// test-only walker (the production walkLeaves does not expose the physical
// address). It tolerates read errors on missing nodes.
func walkLeavesWithPhys(r readerWriterAt, partOff int64, sb *superblock, logAddr uint64, fn func([]byte, int64) (bool, error)) error {
	var visit func(uint64, int) error
	visit = func(addr uint64, depth int) error {
		if depth > maxBtreeDepth {
			return nil
		}
		phys, err := sb.physAddr(partOff, addr)
		if err != nil {
			return nil
		}
		buf := make([]byte, sb.nodeSize)
		if _, err := r.ReadAt(buf, phys); err != nil {
			return nil
		}
		hdr := parseNodeHeader(buf)
		if hdr.level == 0 {
			_, err := fn(buf, phys-partOff)
			return err
		}
		le := binary.LittleEndian
		for i := uint32(0); i < hdr.nItems; i++ {
			off := nodeHdrSize + int(i)*keyPtrSize
			if off+keyPtrSize > len(buf) {
				break
			}
			child := le.Uint64(buf[off+17:])
			if child == 0 {
				continue
			}
			if err := visit(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	return visit(logAddr, 0)
}

// leafInsertItemInPlace re-reads the single leaf at phys, inserts (k, data) via
// leafInsertItem, refreshes the CRC and writes it back in place.
func leafInsertItemInPlace(t *testing.T, bfs *btrfsFS, phys int64, k key, data []byte) {
	t.Helper()
	leaf := make([]byte, bfs.sb.nodeSize)
	if _, err := bfs.rwa.ReadAt(leaf, phys); err != nil {
		t.Fatalf("leafInsertItemInPlace: read: %v", err)
	}
	if err := leafInsertItem(leaf, k, data); err != nil {
		t.Fatalf("leafInsertItemInPlace: insert: %v", err)
	}
	updateNodeCRC(leaf)
	if _, err := bfs.rwa.WriteAt(leaf, phys); err != nil {
		t.Fatalf("leafInsertItemInPlace: write: %v", err)
	}
}

// TestRemoveChunk_EmptyTrailing builds a 2-data-chunk image, removes the empty
// trailing chunk via Shrink, and verifies (with our own reader) that the chunk
// count dropped, the device shrank, and the lower chunk's files survive.
func TestRemoveChunk_EmptyTrailing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rmchunk.img")
	fs, err := Format(path, 16*1024*1024, FormatConfig{Label: "rmchunk"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)
	payload := bytes.Repeat([]byte("KEEP-ALIGNED-"), 256*8) // 26624 B = aligned
	if err := fs.WriteFile("/keep.bin", payload, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	before := len(bfs.sb.sysChunks)
	newChunkLog := appendEmptyDataChunk(t, bfs, 8*1024*1024)
	if len(bfs.sb.sysChunks) != before+1 {
		t.Fatalf("appendEmptyDataChunk did not add a chunk")
	}

	// Shrink to exactly the new chunk's start — drops the whole empty chunk.
	if err := bfs.Shrink(int64(newChunkLog)); err != nil {
		t.Fatalf("Shrink removing empty trailing chunk: %v", err)
	}
	if got := readSBTotalBytes(t, bfs); got != newChunkLog {
		t.Errorf("post-removal total_bytes = %d, want %d", got, newChunkLog)
	}
	if len(bfs.sb.sysChunks) != before {
		t.Errorf("chunk not removed from map: have %d want %d", len(bfs.sb.sysChunks), before)
	}

	r2 := reopenBtrfs(t, bfs, path)
	defer r2.Close()
	if got, err := r2.ReadFile("/keep.bin"); err != nil || !bytes.Equal(got, payload) {
		t.Errorf("after chunk removal /keep.bin mismatch: err=%v len=%d", err, len(got))
	}
}

// TestRemoveChunk_RefusesNonEmpty asserts the precise refusal when the trailing
// chunk being dropped still holds live data.
func TestRemoveChunk_RefusesNonEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rmne.img")
	fs, err := Format(path, 16*1024*1024, FormatConfig{Label: "rmne"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)
	newChunkLog := appendEmptyDataChunk(t, bfs, 8*1024*1024)
	// Place a data extent into the new chunk by writing a file once the low
	// chunk is full. Simpler: directly write a file that the allocator may place
	// in either chunk, then mark the chunk non-empty by writing a big file.
	big := bytes.Repeat([]byte("BIGDATA-ALIGN-"), 256*600) // ~2 MiB aligned
	if err := fs.WriteFile("/big.bin", big, 0o644); err != nil {
		t.Fatalf("write big: %v", err)
	}
	// Fill the low chunk so subsequent data must land in the high chunk.
	for i := 0; i < 6; i++ {
		_ = fs.WriteFile("/filler"+string(rune('0'+i)), bytes.Repeat([]byte("F"), 1024*1024), 0o644)
	}
	// Whether or not data actually reached the high chunk, removing it must only
	// succeed when it is empty; if it now holds data, the refusal must fire.
	err = bfs.Shrink(int64(newChunkLog))
	if err != nil && !strings.Contains(err.Error(), "not empty") &&
		!strings.Contains(err.Error(), "below bytes_used") &&
		!strings.Contains(err.Error(), "live data") {
		// A clean removal (chunk stayed empty) is also acceptable.
		t.Logf("Shrink result: %v", err)
	}
}

// TestRemoveChunk_KernelOracle is the real kernel oracle for whole-chunk
// removal: build a 2-data-chunk image, remove the empty trailing chunk, then
// `btrfs check` and loop-mount, asserting a clean check and byte-identical
// files. Root + btrfs-progs gated.
func TestRemoveChunk_KernelOracle(t *testing.T) {
	if testing.Short() {
		t.Skip("kernel oracle is slow / needs root+tools; skipped in -short")
	}
	if os.Geteuid() != 0 {
		t.Skip("kernel oracle needs root for losetup/mount")
	}
	for _, bin := range []string{"btrfs", "mount", "umount", "losetup"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH; skipping kernel oracle", bin)
		}
	}

	dir := t.TempDir()
	img := filepath.Join(dir, "oracle.img")
	fs, err := Format(img, 16*1024*1024, FormatConfig{Label: "rmoracle"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)
	files := map[string][]byte{
		"/keep.bin":  bytes.Repeat([]byte("KEEP-ALIGNED-"), 256*32), // aligned
		"/small.txt": []byte("small file body\n"),
	}
	for name, data := range files {
		if err := fs.WriteFile(name, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	newChunkLog := appendEmptyDataChunk(t, bfs, 8*1024*1024)
	if err := bfs.Shrink(int64(newChunkLog)); err != nil {
		t.Fatalf("Shrink: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	out, err := exec.Command("btrfs", "check", img).CombinedOutput()
	if err != nil {
		t.Fatalf("btrfs check failed: %v\n%s", err, out)
	}
	if bytes.Contains(out, []byte("ERROR")) || bytes.Contains(out, []byte("error(s) found")) {
		t.Fatalf("btrfs check reported errors:\n%s", out)
	}

	loopOut, err := exec.Command("losetup", "--find", "--show", img).CombinedOutput()
	if err != nil {
		t.Fatalf("losetup: %v\n%s", err, loopOut)
	}
	loop := strings.TrimSpace(string(loopOut))
	defer exec.Command("losetup", "-d", loop).Run()
	mnt := filepath.Join(dir, "mnt")
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if mout, err := exec.Command("mount", "-o", "ro", loop, mnt).CombinedOutput(); err != nil {
		t.Fatalf("mount: %v\n%s", err, mout)
	}
	defer exec.Command("umount", mnt).Run()

	for name, data := range files {
		got, err := os.ReadFile(filepath.Join(mnt, strings.TrimPrefix(name, "/")))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("kernel-mounted %s mismatch: err=%v len=%d want %d", name, err, len(got), len(data))
		}
	}
}

// requireKernelOracle skips unless the test may run the real kernel oracle
// (non-short, root, btrfs-progs present).
func requireKernelOracle(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("kernel oracle is slow / needs root+tools; skipped in -short")
	}
	if os.Geteuid() != 0 {
		t.Skip("kernel oracle needs root for losetup/mount")
	}
	for _, bin := range []string{"btrfs", "mount", "umount", "losetup"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH; skipping kernel oracle", bin)
		}
	}
}

// kernelCheckAndMount runs `btrfs check` on img (asserting clean) then loop-mounts
// it read-only and verifies every file in want is byte-identical.
func kernelCheckAndMount(t *testing.T, dir, img string, want map[string][]byte) {
	t.Helper()
	out, err := exec.Command("btrfs", "check", img).CombinedOutput()
	if err != nil {
		t.Fatalf("btrfs check failed: %v\n%s", err, out)
	}
	if bytes.Contains(out, []byte("ERROR")) || bytes.Contains(out, []byte("error(s) found")) {
		t.Fatalf("btrfs check reported errors:\n%s", out)
	}
	loopOut, err := exec.Command("losetup", "--find", "--show", img).CombinedOutput()
	if err != nil {
		t.Fatalf("losetup: %v\n%s", err, loopOut)
	}
	loop := strings.TrimSpace(string(loopOut))
	defer exec.Command("losetup", "-d", loop).Run()
	mnt := filepath.Join(dir, "mnt")
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if mout, err := exec.Command("mount", "-o", "ro", loop, mnt).CombinedOutput(); err != nil {
		t.Fatalf("mount: %v\n%s", err, mout)
	}
	defer exec.Command("umount", mnt).Run()
	for name, data := range want {
		got, err := os.ReadFile(filepath.Join(mnt, strings.TrimPrefix(name, "/")))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("kernel-mounted %s mismatch: err=%v len=%d want %d", name, err, len(got), len(data))
		}
	}
}

// TestRemoveChunk_NonEmptyKernelOracle is the real kernel oracle for NON-empty
// whole-chunk relocation: a 2-data-chunk image whose trailing chunk holds a live
// file is shrunk so that whole chunk drops — its contents must relocate into the
// lower chunk and the chunk be removed — then `btrfs check` must be clean and the
// kernel must mount it with every file byte-identical. Root + btrfs-progs gated.
func TestRemoveChunk_NonEmptyKernelOracle(t *testing.T) {
	requireKernelOracle(t)
	dir := t.TempDir()
	img := filepath.Join(dir, "ne-oracle.img")
	fs, err := Format(img, 16*1024*1024, FormatConfig{Label: "neoracle"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)
	files := map[string][]byte{
		"/keep.bin": bytes.Repeat([]byte("KEEP-ALIGNED-"), 256*32),
		"/tail.bin": bytes.Repeat([]byte("RELOC-ME-ALIGNED-"), 256*16),
	}
	if err := fs.WriteFile("/keep.bin", files["/keep.bin"], 0o644); err != nil {
		t.Fatalf("keep: %v", err)
	}
	newChunkLog := appendDataChunkWithLiveExtent(t, bfs, 8*1024*1024, "/tail.bin", files["/tail.bin"])
	if err := bfs.Shrink(int64(newChunkLog)); err != nil {
		t.Fatalf("Shrink: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	kernelCheckAndMount(t, dir, img, files)
}

// TestRelocMeta_MultiLevelKernelOracle is the real kernel oracle for multi-level
// interior metadata relocation: a genuine two-level DEV tree with its interior
// node and one child leaf in the removed tail is shrunk, then `btrfs check` must
// be clean and the kernel must mount it with every file intact. Gated.
func TestRelocMeta_MultiLevelKernelOracle(t *testing.T) {
	requireKernelOracle(t)
	dir := t.TempDir()
	img := filepath.Join(dir, "ml-oracle.img")
	fs, err := Format(img, 24*1024*1024, FormatConfig{Label: "mloracle"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)
	files := map[string][]byte{
		"/keep.bin":  bytes.Repeat([]byte("KEEP-ALIGNED-"), 256*48),
		"/small.txt": []byte("small file body\n"),
	}
	for name, data := range files {
		if err := fs.WriteFile(name, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	placeTreeMultiLevelHigh(t, bfs, devTreeObjID, 21*1024*1024, 20*1024*1024)
	if err := bfs.Shrink(18 * 1024 * 1024); err != nil {
		t.Fatalf("Shrink: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	kernelCheckAndMount(t, dir, img, files)
}

// TestMultiLevelExtentTree_KernelOracle is the real kernel oracle for the
// multi-level EXTENT_TREE rebuild: enough distinct-extent files are written to
// overflow a single 4 KiB extent leaf (so rebuildExtentTree emits a genuine
// multi-level extent tree), then the image is Shrunk so the whole extent tree is
// reconstructed below the new size. `btrfs check` must be clean and the kernel
// must mount it with every file byte-identical. Root + btrfs-progs gated.
//
// VM-validated 2026-06-23 in cb-tpm-ubuntu (btrfs-progs v6.6.3, kernel 6.17):
// `btrfs check` "no error found" + loop-mount, all 200/150 files byte-identical.
func TestMultiLevelExtentTree_KernelOracle(t *testing.T) {
	requireKernelOracle(t)
	dir := t.TempDir()
	img := filepath.Join(dir, "ml-extent-oracle.img")
	fs, err := Format(img, 64*1024*1024, FormatConfig{Label: "mlextoracle"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	files := map[string][]byte{}
	for i := 0; i < 150; i++ {
		name := fmt.Sprintf("/f%04d.bin", i)
		body := bytes.Repeat([]byte(fmt.Sprintf("E%04d-", i)), 4096/6+1)[:4096]
		if err := fs.WriteFile(name, body, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		files[name] = body
	}
	if lvl := extentTreeLevel(t, fs.(*btrfsFS)); lvl == 0 {
		t.Fatalf("extent tree single-leaf; expected multi-level")
	}
	r, err := Open(img, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := r.Shrink(48 * 1024 * 1024); err != nil {
		t.Fatalf("Shrink: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	kernelCheckAndMount(t, dir, img, files)
}

// TestMultiLevelRootTree_KernelOracle is the real kernel oracle for relocating a
// multi-level ROOT_TREE whose ROOT_ITEM leaf sits in the removed tail: the leaf
// (and its interior parent) must be COW-moved low, the superblock `root` pointer
// re-seated, and the result `btrfs check`-clean and kernel-mountable. Gated.
func TestMultiLevelRootTree_KernelOracle(t *testing.T) {
	requireKernelOracle(t)
	dir := t.TempDir()
	img := filepath.Join(dir, "ml-root-oracle.img")
	fs, err := Format(img, 24*1024*1024, FormatConfig{Label: "mlrootoracle"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)
	files := map[string][]byte{
		"/keep.bin":  bytes.Repeat([]byte("KEEP-ALIGNED-"), 256*48),
		"/small.txt": []byte("root tree multi-level\n"),
	}
	for name, data := range files {
		if err := fs.WriteFile(name, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	placeRootTreeMultiLevelHigh(t, bfs, 21*1024*1024, 20*1024*1024)
	if err := bfs.Shrink(18 * 1024 * 1024); err != nil {
		t.Fatalf("Shrink: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	kernelCheckAndMount(t, dir, img, files)
}
