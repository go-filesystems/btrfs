package filesystem_btrfs

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// shrunkMetaRelocFixture is a 24→18 MiB image our writer produced: a ~200 KiB
// /keep.bin and an inline /small.txt were written, the ROOT_TREE and CSUM/UUID
// tree roots were placed high in the [18,24) MiB tail, then Shrink(18 MiB)
// COW-relocated those metadata blocks below the new size. `btrfs check` reports
// it CLEAN and the kernel loop-mounts it with both files byte-identical
// (validated on cb-tpm-ubuntu, kernel 6.17 / btrfs-progs 6.6.3). Committed
// zstd-compressed so emulated-arch CI (which cross-compiles the test binary) can
// still read it without a kernel.
//
//go:embed testdata/resize/shrunk-meta-reloc.img.zst
var shrunkMetaRelocFixture []byte

// shrunkChunkRemovalFixture is a 24→16 MiB image: /keep.bin + /small.txt were
// written, an empty 8 MiB trailing DATA chunk was appended (16→24 MiB), then
// Shrink(16 MiB) removed that whole empty chunk (CHUNK_ITEM / DEV_EXTENT /
// BLOCK_GROUP_ITEM deleted). `btrfs check`-clean and kernel-mountable with both
// files intact (same VM).
//
//go:embed testdata/resize/shrunk-chunk-removal.img.zst
var shrunkChunkRemovalFixture []byte

var (
	metaFixtureKeep  = bytes.Repeat([]byte("FIXTURE-KEEP-"), 256*16)
	metaFixtureSmall = []byte("small fixture body\n")
)

// decompressZst inflates an embedded zstd image into a temp file, returning its
// path.
func decompressZst(t *testing.T, blob []byte) string {
	t.Helper()
	zr, err := zstd.NewReader(bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("zstd reader: %v", err)
	}
	defer zr.Close()
	raw, err := zr.DecodeAll(blob, nil)
	if err != nil {
		t.Fatalf("zstd decode: %v", err)
	}
	path := filepath.Join(t.TempDir(), "fixture.img")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// buildMetaRelocFixtureImage writes a deterministic post-metadata-reloc image to
// path. Shared by the fixture generator (FIXTURE_OUT) and exercised live by
// TestRelocMeta_RootTreeInTail's siblings.
func buildMetaRelocFixtureImage(t *testing.T, path string) {
	fs, err := Format(path, 24*1024*1024, FormatConfig{Label: "metafix"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)
	if err := fs.WriteFile("/keep.bin", metaFixtureKeep, 0o644); err != nil {
		t.Fatalf("keep: %v", err)
	}
	if err := fs.WriteFile("/small.txt", metaFixtureSmall, 0o644); err != nil {
		t.Fatalf("small: %v", err)
	}
	placeTreeRootHigh(t, bfs, rootTreeObjID, 20*1024*1024)
	placeTreeRootHigh(t, bfs, csumTreeObjID, 21*1024*1024)
	placeTreeRootHigh(t, bfs, uuidTreeObjID, 22*1024*1024)
	if err := bfs.Shrink(18 * 1024 * 1024); err != nil {
		t.Fatalf("Shrink: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// buildChunkRemovalFixtureImage writes a deterministic post-chunk-removal image.
func buildChunkRemovalFixtureImage(t *testing.T, path string) {
	fs, err := Format(path, 16*1024*1024, FormatConfig{Label: "chunkfix"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)
	if err := fs.WriteFile("/keep.bin", metaFixtureKeep, 0o644); err != nil {
		t.Fatalf("keep: %v", err)
	}
	if err := fs.WriteFile("/small.txt", metaFixtureSmall, 0o644); err != nil {
		t.Fatalf("small: %v", err)
	}
	newChunkLog := appendEmptyDataChunk(t, bfs, 8*1024*1024)
	if err := bfs.Shrink(int64(newChunkLog)); err != nil {
		t.Fatalf("Shrink: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestGenerateFixtures (gated by FIXTURE_OUT) writes the two post-shrink fixture
// images to ${FIXTURE_OUT}/shrunk-meta-reloc.img and
// ${FIXTURE_OUT}/shrunk-chunk-removal.img. Run under root in the validation VM
// so the emitted images are exactly the ones `btrfs check` blesses; the raw
// images are then zstd-compressed and committed under testdata/resize/.
func TestGenerateFixtures(t *testing.T) {
	dir := os.Getenv("FIXTURE_OUT")
	if dir == "" {
		t.Skip("FIXTURE_OUT not set")
	}
	buildMetaRelocFixtureImage(t, filepath.Join(dir, "shrunk-meta-reloc.img"))
	buildChunkRemovalFixtureImage(t, filepath.Join(dir, "shrunk-chunk-removal.img"))
}

// TestRelocMeta_FixtureReadback opens the committed kernel-blessed
// post-metadata-reloc image with our own reader (no kernel — runs on every CI
// arch incl. big-endian s390x) and asserts the geometry shrank, no metadata sits
// in the removed tail, and both files read back byte-for-byte.
func TestRelocMeta_FixtureReadback(t *testing.T) {
	path := decompressZst(t, shrunkMetaRelocFixture)
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	bfs := fs.(*btrfsFS)
	if got := bfs.sb.totalBytes; got != 18*1024*1024 {
		t.Errorf("fixture total_bytes = %d, want %d", got, 18*1024*1024)
	}
	assertNoMetaAboveLocked(t, bfs, 18*1024*1024)
	if got, err := fs.ReadFile("/keep.bin"); err != nil || !bytes.Equal(got, metaFixtureKeep) {
		t.Errorf("/keep.bin mismatch: err=%v len=%d", err, len(got))
	}
	if got, err := fs.ReadFile("/small.txt"); err != nil || !bytes.Equal(got, metaFixtureSmall) {
		t.Errorf("/small.txt mismatch: err=%v got=%q", err, got)
	}
}

// TestRemoveChunk_FixtureReadback opens the committed kernel-blessed
// post-chunk-removal image and asserts the trailing chunk is gone, the device
// shrank, and both files survive.
func TestRemoveChunk_FixtureReadback(t *testing.T) {
	path := decompressZst(t, shrunkChunkRemovalFixture)
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	bfs := fs.(*btrfsFS)
	if got := bfs.sb.totalBytes; got != 16*1024*1024 {
		t.Errorf("fixture total_bytes = %d, want %d", got, 16*1024*1024)
	}
	for _, m := range bfs.sb.sysChunks {
		if m.localStripeIdx < 0 {
			continue
		}
		if end := m.physStart + m.size; end > 16*1024*1024 {
			t.Errorf("chunk at phys 0x%X size %d extends past shrunk device size", m.physStart, m.size)
		}
	}
	if got, err := fs.ReadFile("/keep.bin"); err != nil || !bytes.Equal(got, metaFixtureKeep) {
		t.Errorf("/keep.bin mismatch: err=%v len=%d", err, len(got))
	}
	if got, err := fs.ReadFile("/small.txt"); err != nil || !bytes.Equal(got, metaFixtureSmall) {
		t.Errorf("/small.txt mismatch: err=%v got=%q", err, got)
	}
}

// placeTreeRootHigh relocates the single-leaf root node of the tree identified
// by objID (a ROOT_ITEM objectid, or rootTreeObjID for the ROOT_TREE itself) to
// a freshly allocated block at the HIGH end of the data chunk — physically
// inside [target, oldSize). It mirrors a fragmented real filesystem whose
// metadata happens to sit in the region a later shrink must evacuate. The
// resulting image is transaction-consistent (header bytenr/generation rewritten,
// ROOT_ITEM/superblock repointed, extent tree rebuilt) and `btrfs check`-clean,
// so it is a valid STARTING image for the relocation shrink under test.
//
// It returns the new (high) logical address of the relocated block.
func placeTreeRootHigh(t *testing.T, bfs *btrfsFS, objID, highPhys uint64) uint64 {
	t.Helper()
	bfs.mu.Lock()
	defer bfs.mu.Unlock()

	// Resolve the current root node of the target tree.
	var oldLog uint64
	if objID == rootTreeObjID {
		oldLog = bfs.sb.rootLogAddr
	} else {
		buf, it, err := searchTree(bfs.rwa, bfs.partOffset, bfs.sb, bfs.sb.rootLogAddr, objID, typeRootItem, 0)
		if err != nil {
			t.Fatalf("placeTreeRootHigh: locate ROOT_ITEM %d: %v", objID, err)
		}
		oldLog = binary.LittleEndian.Uint64(it.data(buf)[rootItemOffBytenr:])
	}

	// Read the leaf, copy it to highPhys with a rewritten header.
	oldPhys := physFromLog(bfs.sb, oldLog)
	leaf := make([]byte, bfs.sb.nodeSize)
	if _, err := bfs.rwa.ReadAt(leaf, bfs.partOffset+int64(oldPhys)); err != nil {
		t.Fatalf("placeTreeRootHigh: read leaf: %v", err)
	}
	// Move the block at its CURRENT header generation — this is a surgical
	// physical relocation, not a new transaction, so generations across the
	// image stay coherent (each tree's ROOT_ITEM.generation keeps matching its
	// node-header generation). Only the bytenr changes.
	newLog := physToLog(bfs.sb, highPhys)
	le := binary.LittleEndian
	le.PutUint64(leaf[0x30:], newLog)
	updateNodeCRC(leaf)
	if _, err := bfs.rwa.WriteAt(leaf, bfs.partOffset+int64(highPhys)); err != nil {
		t.Fatalf("placeTreeRootHigh: write high leaf: %v", err)
	}
	// Reserve the high block; release the old one so the allocator can reuse it.
	bfs.sm.remove(highPhys, uint64(bfs.sb.nodeSize))
	bfs.sm.freeRange(oldPhys, uint64(bfs.sb.nodeSize))

	// Repoint the reference to the new bytenr, preserving the block's
	// generation (no super.generation bump).
	if objID == rootTreeObjID {
		bfs.sb.rootLogAddr = newLog
		sbuf := make([]byte, sbfSize)
		if _, err := bfs.rwa.ReadAt(sbuf, bfs.partOffset+superblockOffset); err != nil {
			t.Fatalf("placeTreeRootHigh: read sb: %v", err)
		}
		le.PutUint64(sbuf[sbfRootLogAddr:], newLog)
		updateSuperblockCRC(sbuf)
		if _, err := bfs.rwa.WriteAt(sbuf, bfs.partOffset+superblockOffset); err != nil {
			t.Fatalf("placeTreeRootHigh: write sb: %v", err)
		}
	} else {
		if err := bfs.repointRootItemBytenrLocked(objID, newLog); err != nil {
			t.Fatalf("placeTreeRootHigh: repoint ROOT_ITEM %d: %v", objID, err)
		}
	}
	// Rebuild the extent tree in place so the relocated block is accounted at its
	// new address (and the old address dropped) without bumping any generation:
	// rebuildExtentTree writes the extent leaf at the extent root's current
	// location and edits the extent ROOT_ITEM in place, keeping the prior
	// generation coherent.
	if err := rebuildExtentTree(bfs.rwa, bfs.partOffset, bfs.sb, bfs.sm); err != nil {
		t.Fatalf("placeTreeRootHigh: rebuild extent tree: %v", err)
	}
	bfs.invalidateCache()
	return newLog
}

// repointRootItemBytenrLocked rewrites only the bytenr of the ROOT_ITEM
// (objID, ROOT_ITEM, 0) in the root-tree leaf, preserving its generation — a
// test-only surgical edit used by placeTreeRootHigh. Caller holds bfs.mu.
func (bfs *btrfsFS) repointRootItemBytenrLocked(objID, newBytenr uint64) error {
	phys, err := bfs.sb.physAddr(bfs.partOffset, bfs.sb.rootLogAddr)
	if err != nil {
		return err
	}
	leaf := make([]byte, bfs.sb.nodeSize)
	if _, err := bfs.rwa.ReadAt(leaf, phys); err != nil {
		return err
	}
	idx := findItemIdx(leaf, objID, typeRootItem, 0)
	if idx < 0 {
		return fmt.Errorf("ROOT_ITEM %d not found", objID)
	}
	items := parseLeafItems(leaf, parseNodeHeader(leaf).nItems)
	d := items[idx].data(leaf)
	binary.LittleEndian.PutUint64(d[rootItemOffBytenr:], newBytenr)
	updateNodeCRC(leaf)
	_, err = bfs.rwa.WriteAt(leaf, phys)
	return err
}

// reopenBtrfs closes fs and reopens the same path, returning the fresh handle.
func reopenBtrfs(t *testing.T, fs *btrfsFS, path string) *btrfsFS {
	t.Helper()
	if err := fs.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	r, err := Open(path, -1)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	return r.(*btrfsFS)
}

// TestRelocMeta_RootTreeInTail places the ROOT_TREE leaf high in the tail, then
// shrinks so the tail captures it. relocateTailMetadata must COW it down, the
// device must shrink, and every file must read back byte-identical (no kernel —
// runs on every CI arch including big-endian s390x).
func TestRelocMeta_RootTreeInTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rootmeta.img")
	fs, err := Format(path, 24*1024*1024, FormatConfig{Label: "rootmeta"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)
	files := map[string][]byte{
		"/a.txt": bytes.Repeat([]byte("AAAA"), 1000),
		"/b.txt": bytes.Repeat([]byte("BBBB"), 2000),
		"/c.txt": []byte("hello world\n"),
	}
	for name, data := range files {
		if err := fs.WriteFile(name, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	// Put the root-tree leaf at 20 MiB — well inside the [18 MiB, 24 MiB) tail.
	placeTreeRootHigh(t, bfs, rootTreeObjID, 20*1024*1024)

	if err := bfs.Shrink(18 * 1024 * 1024); err != nil {
		t.Fatalf("Shrink with root-tree metadata in tail: %v", err)
	}
	if got := readSBTotalBytes(t, bfs); got != 18*1024*1024 {
		t.Errorf("post-shrink total_bytes = %d, want %d", got, 18*1024*1024)
	}
	assertNoMetaAboveLocked(t, bfs, 18*1024*1024)

	r2 := reopenBtrfs(t, bfs, path)
	defer r2.Close()
	for name, data := range files {
		if got, err := r2.ReadFile(name); err != nil || !bytes.Equal(got, data) {
			t.Errorf("after shrink %s mismatch: err=%v len=%d want %d", name, err, len(got), len(data))
		}
	}
}

// TestRelocMeta_CsumTreeInTail places the (non-FS) CSUM_TREE root high, then
// shrinks to capture it — exercising the ROOT_ITEM-repoint path for a tree other
// than the root/fs trees.
func TestRelocMeta_CsumTreeInTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "csummeta.img")
	fs, err := Format(path, 24*1024*1024, FormatConfig{Label: "csummeta"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)
	payload := bytes.Repeat([]byte("payload-"), 4096)
	if err := fs.WriteFile("/data.bin", payload, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	placeTreeRootHigh(t, bfs, csumTreeObjID, 21*1024*1024)
	placeTreeRootHigh(t, bfs, uuidTreeObjID, 20*1024*1024)

	if err := bfs.Shrink(18 * 1024 * 1024); err != nil {
		t.Fatalf("Shrink with csum/uuid metadata in tail: %v", err)
	}
	assertNoMetaAboveLocked(t, bfs, 18*1024*1024)

	r2 := reopenBtrfs(t, bfs, path)
	defer r2.Close()
	if got, err := r2.ReadFile("/data.bin"); err != nil || !bytes.Equal(got, payload) {
		t.Errorf("after shrink /data.bin mismatch: err=%v len=%d", err, len(got))
	}
}

// TestRelocMeta_DataAndMetadataInTail exercises the combined path: the removed
// tail holds BOTH a live data extent AND several non-FS tree-root blocks, so the
// shrink must relocate data (COW) and metadata (block move) together. Verified
// with our own reader on every arch.
func TestRelocMeta_DataAndMetadataInTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "both.img")
	fs, err := Format(path, 24*1024*1024, FormatConfig{Label: "both"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)
	// A big low file, overwritten tiny, frees the low region; a marker placed
	// high will be data-relocated. Then push several tree roots high too.
	if err := fs.WriteFile("/big.bin", bytes.Repeat([]byte{'B'}, 12*1024*1024), 0o644); err != nil {
		t.Fatalf("big: %v", err)
	}
	marker := bytes.Repeat([]byte("MARK-ALIGNED-"), 256*16)
	if err := fs.WriteFile("/marker.bin", marker, 0o644); err != nil {
		t.Fatalf("marker: %v", err)
	}
	if err := fs.WriteFile("/big.bin", []byte("tiny\n"), 0o644); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	// Place csum + uuid + dev tree roots high in the [18,24) MiB tail.
	placeTreeRootHigh(t, bfs, csumTreeObjID, 21*1024*1024)
	placeTreeRootHigh(t, bfs, uuidTreeObjID, 20*1024*1024+512*1024)
	placeTreeRootHigh(t, bfs, devTreeObjID, 20*1024*1024)

	if err := bfs.Shrink(18 * 1024 * 1024); err != nil {
		t.Fatalf("Shrink combined data+metadata reloc: %v", err)
	}
	assertNoMetaAboveLocked(t, bfs, 18*1024*1024)
	r2 := reopenBtrfs(t, bfs, path)
	defer r2.Close()
	if got, err := r2.ReadFile("/marker.bin"); err != nil || !bytes.Equal(got, marker) {
		t.Errorf("relocated marker mismatch: err=%v len=%d", err, len(got))
	}
}

// assertNoMetaAboveLocked fails if any live metadata block sits at or above
// limit (the shrunk device size).
func assertNoMetaAboveLocked(t *testing.T, bfs *btrfsFS, limit uint64) {
	t.Helper()
	bfs.mu.Lock()
	defer bfs.mu.Unlock()
	if hit, found := bfs.liveMetaInRange(limit, bfs.sb.totalBytes+uint64(bfs.sb.nodeSize)); found {
		t.Errorf("live metadata block 0x%X remains at/above shrunk size %d", hit, limit)
	}
}

// TestRelocMeta_KernelOracle is the real kernel oracle for metadata-block
// relocation: build images whose removed tail holds (a) the ROOT_TREE leaf and
// (b) the CSUM/UUID tree roots, run the relocation shrink, then `btrfs check`
// and loop-mount, asserting a clean check and byte-identical files. Skip-gated
// unless root + btrfs-progs are present (CI native Linux runners only).
func TestRelocMeta_KernelOracle(t *testing.T) {
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

	cases := []struct {
		name    string
		objIDs  []uint64
		highPhy []uint64
	}{
		{"roottree", []uint64{rootTreeObjID}, []uint64{20 * 1024 * 1024}},
		{"csum_uuid", []uint64{csumTreeObjID, uuidTreeObjID}, []uint64{21 * 1024 * 1024, 20 * 1024 * 1024}},
		{"devtree", []uint64{devTreeObjID}, []uint64{20 * 1024 * 1024}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			img := filepath.Join(dir, "oracle.img")
			fs, err := Format(img, 24*1024*1024, FormatConfig{Label: "metaoracle"})
			if err != nil {
				t.Fatalf("Format: %v", err)
			}
			bfs := fs.(*btrfsFS)
			// Sector-aligned payloads only: the writer stores a regular extent's
			// num_bytes/ram_bytes verbatim, so an unaligned length would make
			// `btrfs check` flag "nbytes wrong" independently of relocation. The
			// existing data-extent oracle (TestReloc_KernelOracle) uses the same
			// aligned-size discipline.
			files := map[string][]byte{
				"/keep.bin":  bytes.Repeat([]byte("KEEP-ALIGNED-"), 256*48), // 159744 B = 39*4096
				"/small.txt": []byte("small file body\n"),                   // inline (nbytes = len)
			}
			for name, data := range files {
				if err := fs.WriteFile(name, data, 0o644); err != nil {
					t.Fatalf("write %s: %v", name, err)
				}
			}
			for i, objID := range tc.objIDs {
				placeTreeRootHigh(t, bfs, objID, tc.highPhy[i])
			}
			if err := bfs.Shrink(18 * 1024 * 1024); err != nil {
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
			_ = fmt.Sprint // keep fmt imported across edits
		})
	}
}
