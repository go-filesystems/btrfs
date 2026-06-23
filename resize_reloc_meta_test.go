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

// shrunkMultiLevelRelocFixture is a 24→18 MiB image whose removed tail held a
// genuine TWO-level DEV tree (interior node + a child leaf); Shrink path-COW-
// relocated the subtree below the new size. `btrfs check`-clean and kernel-
// mountable on cb-tpm-ubuntu (kernel 6.17 / btrfs-progs 6.6.3).
//
//go:embed testdata/resize/shrunk-multilevel-reloc.img.zst
var shrunkMultiLevelRelocFixture []byte

// shrunkNonEmptyChunkRelocFixture is a 16 MiB image with a second NON-empty DATA
// chunk holding /tail.bin; Shrink dropped that whole chunk, relocating its live
// content into the lower chunk. `btrfs check`-clean and kernel-mountable (same VM).
//
//go:embed testdata/resize/shrunk-nonempty-chunk-reloc.img.zst
var shrunkNonEmptyChunkRelocFixture []byte

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

// buildMultiLevelRelocFixtureImage writes a deterministic post-multi-level-
// metadata-reloc image: a 24→18 MiB shrink whose removed tail held a genuine
// two-level DEV tree (interior node + one child leaf), path-COW-relocated down.
func buildMultiLevelRelocFixtureImage(t *testing.T, path string) {
	fs, err := Format(path, 24*1024*1024, FormatConfig{Label: "mlfix"})
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
	placeTreeMultiLevelHigh(t, bfs, devTreeObjID, 21*1024*1024, 20*1024*1024)
	if err := bfs.Shrink(18 * 1024 * 1024); err != nil {
		t.Fatalf("Shrink: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// buildNonEmptyChunkRelocFixtureImage writes a deterministic post-non-empty-
// chunk-relocation image: a 2-data-chunk 16→? image whose non-empty trailing
// chunk was dropped, relocating its live /tail.bin into the lower chunk.
func buildNonEmptyChunkRelocFixtureImage(t *testing.T, path string) {
	fs, err := Format(path, 16*1024*1024, FormatConfig{Label: "nechunkfix"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)
	if err := fs.WriteFile("/keep.bin", metaFixtureKeep, 0o644); err != nil {
		t.Fatalf("keep: %v", err)
	}
	newChunkLog := appendDataChunkWithLiveExtent(t, bfs, 8*1024*1024, "/tail.bin", nonEmptyChunkTailPayload)
	if err := bfs.Shrink(int64(newChunkLog)); err != nil {
		t.Fatalf("Shrink: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// nonEmptyChunkTailPayload is a sector-aligned (regular, non-inline) body for the
// non-empty-chunk-relocation fixture's relocated file.
var nonEmptyChunkTailPayload = bytes.Repeat([]byte("TAIL-RELOC-ALIGNED-"), 256*16)

// TestGenerateFixtures (gated by FIXTURE_OUT) writes the post-shrink fixture
// images to ${FIXTURE_OUT}/. Run under root in the validation VM so the emitted
// images are exactly the ones `btrfs check` blesses; the raw images are then
// zstd-compressed and committed under testdata/resize/.
func TestGenerateFixtures(t *testing.T) {
	dir := os.Getenv("FIXTURE_OUT")
	if dir == "" {
		t.Skip("FIXTURE_OUT not set")
	}
	buildMetaRelocFixtureImage(t, filepath.Join(dir, "shrunk-meta-reloc.img"))
	buildChunkRemovalFixtureImage(t, filepath.Join(dir, "shrunk-chunk-removal.img"))
	buildMultiLevelRelocFixtureImage(t, filepath.Join(dir, "shrunk-multilevel-reloc.img"))
	buildNonEmptyChunkRelocFixtureImage(t, filepath.Join(dir, "shrunk-nonempty-chunk-reloc.img"))
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

// TestRelocMeta_MultiLevelFixtureReadback opens the committed kernel-blessed
// post-multi-level-reloc image with our own reader (every CI arch incl. s390x)
// and asserts the geometry shrank, no metadata sits in the tail, files survive.
func TestRelocMeta_MultiLevelFixtureReadback(t *testing.T) {
	path := decompressZst(t, shrunkMultiLevelRelocFixture)
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

// TestRemoveChunk_NonEmptyFixtureReadback opens the committed kernel-blessed
// post-non-empty-chunk-relocation image and asserts the trailing chunk is gone,
// the device shrank, and both the lower-chunk file and the relocated file survive.
func TestRemoveChunk_NonEmptyFixtureReadback(t *testing.T) {
	path := decompressZst(t, shrunkNonEmptyChunkRelocFixture)
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	bfs := fs.(*btrfsFS)
	for _, m := range bfs.sb.sysChunks {
		if m.localStripeIdx < 0 {
			continue
		}
		if end := m.physStart + m.size; end > bfs.sb.totalBytes {
			t.Errorf("chunk at phys 0x%X size %d extends past shrunk device %d", m.physStart, m.size, bfs.sb.totalBytes)
		}
	}
	if got, err := fs.ReadFile("/keep.bin"); err != nil || !bytes.Equal(got, metaFixtureKeep) {
		t.Errorf("/keep.bin mismatch: err=%v len=%d", err, len(got))
	}
	if got, err := fs.ReadFile("/tail.bin"); err != nil || !bytes.Equal(got, nonEmptyChunkTailPayload) {
		t.Errorf("relocated /tail.bin mismatch: err=%v len=%d", err, len(got))
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

// placeTreeMultiLevelHigh converts the single-leaf non-FS tree identified by
// objID into a real TWO-level tree and places its interior root node (plus one
// child leaf) physically high — inside [nodeHighPhys, ...) of the data chunk,
// i.e. in the region a later shrink must evacuate. It splits the tree's current
// single leaf into two child leaves (left keeps the first half of the items,
// right the rest), builds a level-1 interior node with one key-ptr per child
// (key = the child's first key, blockptr = the child's bytenr, generation = the
// child header generation), repoints the ROOT_ITEM at the interior node, and
// rebuilds the extent tree so every new block is accounted. Like
// placeTreeRootHigh it is a surgical physical layout change at the CURRENT
// generation (no super.generation bump), so the resulting image is
// transaction-consistent and `btrfs check`-clean — a valid STARTING image for
// the multi-level relocation shrink under test.
//
// leafHighPhys receives the RIGHT child leaf (also in the tail); the LEFT child
// leaf is allocated low (below the tail) so the test exercises moving an interior
// node AND one of its leaves while leaving the sibling in place. The tree must
// have at least two items in its single leaf (dev tree: DEV_ITEM + DEV_EXTENT;
// csum/uuid after seeding). Returns the interior node's logical address.
func placeTreeMultiLevelHigh(t *testing.T, bfs *btrfsFS, objID, nodeHighPhys, leafHighPhys uint64) uint64 {
	t.Helper()
	bfs.mu.Lock()
	defer bfs.mu.Unlock()
	le := binary.LittleEndian

	// Resolve the tree's current single-leaf root.
	buf, it, err := searchTree(bfs.rwa, bfs.partOffset, bfs.sb, bfs.sb.rootLogAddr, objID, typeRootItem, 0)
	if err != nil {
		t.Fatalf("placeTreeMultiLevelHigh: locate ROOT_ITEM %d: %v", objID, err)
	}
	oldLog := le.Uint64(it.data(buf)[rootItemOffBytenr:])
	oldPhys := physFromLog(bfs.sb, oldLog)
	leaf := make([]byte, bfs.sb.nodeSize)
	if _, err := bfs.rwa.ReadAt(leaf, bfs.partOffset+int64(oldPhys)); err != nil {
		t.Fatalf("placeTreeMultiLevelHigh: read leaf: %v", err)
	}
	hdr := parseNodeHeader(leaf)
	if hdr.level != 0 {
		t.Fatalf("placeTreeMultiLevelHigh: tree %d is not single-leaf (level %d)", objID, hdr.level)
	}
	n := int(hdr.nItems)
	if n < 2 {
		t.Fatalf("placeTreeMultiLevelHigh: tree %d has %d items, need >=2 to split", objID, n)
	}
	splitAt := n / 2

	// Build the LEFT child = copy of the leaf with the upper items deleted; the
	// RIGHT child = copy with the lower items deleted. leafDeleteItem keeps the
	// kernel's contiguous-data invariant.
	left := make([]byte, bfs.sb.nodeSize)
	copy(left, leaf)
	for i := n - 1; i >= splitAt; i-- {
		leafDeleteItem(left, i)
	}
	right := make([]byte, bfs.sb.nodeSize)
	copy(right, leaf)
	for i := splitAt - 1; i >= 0; i-- {
		leafDeleteItem(right, i)
	}

	// First key of each child (for the interior key-ptrs).
	leftKey := readKey(left[nodeHdrSize:])
	rightKey := readKey(right[nodeHdrSize:])

	gen := le.Uint64(leaf[0x50:]) // preserve current generation (surgical move)

	// Allocate the LEFT leaf low, the RIGHT leaf at leafHighPhys (in the tail).
	leftPhys, err := bfs.sm.allocNodeBlock()
	if err != nil {
		t.Fatalf("placeTreeMultiLevelHigh: alloc left leaf: %v", err)
	}
	leftLog := physToLog(bfs.sb, leftPhys)
	le.PutUint64(left[0x30:], leftLog)
	le.PutUint64(left[0x50:], gen)
	updateNodeCRC(left)
	if _, err := bfs.rwa.WriteAt(left, bfs.partOffset+int64(leftPhys)); err != nil {
		t.Fatalf("placeTreeMultiLevelHigh: write left leaf: %v", err)
	}

	rightLog := physToLog(bfs.sb, leafHighPhys)
	le.PutUint64(right[0x30:], rightLog)
	le.PutUint64(right[0x50:], gen)
	updateNodeCRC(right)
	if _, err := bfs.rwa.WriteAt(right, bfs.partOffset+int64(leafHighPhys)); err != nil {
		t.Fatalf("placeTreeMultiLevelHigh: write right leaf: %v", err)
	}
	bfs.sm.remove(leafHighPhys, uint64(bfs.sb.nodeSize))

	// Build the level-1 interior node: header cloned from the leaf (fsid,
	// chunk_tree_uuid, owner), level=1, nritems=2, two key-ptrs.
	node := make([]byte, bfs.sb.nodeSize)
	copy(node[:nodeHdrSize], leaf[:nodeHdrSize])
	node[0x64] = 1 // level
	le.PutUint32(node[0x60:], 2)
	le.PutUint64(node[0x50:], gen)
	writeKeyPtr := func(idx int, k key, blk, g uint64) {
		off := nodeHdrSize + idx*keyPtrSize
		le.PutUint64(node[off:], k.objID)
		node[off+8] = k.typ
		le.PutUint64(node[off+9:], k.offset)
		le.PutUint64(node[off+17:], blk)
		le.PutUint64(node[off+25:], g)
	}
	writeKeyPtr(0, leftKey, leftLog, gen)
	writeKeyPtr(1, rightKey, rightLog, gen)
	nodeLog := physToLog(bfs.sb, nodeHighPhys)
	le.PutUint64(node[0x30:], nodeLog)
	updateNodeCRC(node)
	if _, err := bfs.rwa.WriteAt(node, bfs.partOffset+int64(nodeHighPhys)); err != nil {
		t.Fatalf("placeTreeMultiLevelHigh: write interior node: %v", err)
	}
	bfs.sm.remove(nodeHighPhys, uint64(bfs.sb.nodeSize))

	// Release the old single leaf's block; repoint the ROOT_ITEM at the interior
	// node AND bump its recorded level to 1 (the kernel cross-checks
	// ROOT_ITEM.level against the root node header level), then rebuild the extent
	// tree in place.
	//
	// Skip the rebuild when the tree being made multi-level IS the extent tree:
	// rebuildExtentTree rewrites the extent leaf in place at the extent root's
	// bytenr as a LEVEL-0 leaf, which would flatten the interior node we just
	// built. (The extent-tree-in-tail case is a refusal test, where exact
	// post-build accounting is immaterial.)
	bfs.sm.freeRange(oldPhys, uint64(bfs.sb.nodeSize))
	if err := bfs.repointRootItemBytenrLocked(objID, nodeLog); err != nil {
		t.Fatalf("placeTreeMultiLevelHigh: repoint ROOT_ITEM %d: %v", objID, err)
	}
	setRootItemLevel(t, bfs, objID, 1)
	if objID != extentTreeObjID {
		if err := rebuildExtentTree(bfs.rwa, bfs.partOffset, bfs.sb, bfs.sm); err != nil {
			t.Fatalf("placeTreeMultiLevelHigh: rebuild extent tree: %v", err)
		}
	}
	bfs.invalidateCache()
	return nodeLog
}

// setRootItemLevel rewrites the `level` byte of the ROOT_ITEM (objID, ROOT_ITEM,
// 0) in the root-tree leaf — a test-only surgical edit used when a fixture turns
// a tree multi-level so the kernel's ROOT_ITEM.level ↔ root-node-level cross-check
// passes. Caller holds bfs.mu.
func setRootItemLevel(t *testing.T, bfs *btrfsFS, objID uint64, level byte) {
	t.Helper()
	phys, err := bfs.sb.physAddr(bfs.partOffset, bfs.sb.rootLogAddr)
	if err != nil {
		t.Fatalf("setRootItemLevel: physAddr: %v", err)
	}
	leaf := make([]byte, bfs.sb.nodeSize)
	if _, err := bfs.rwa.ReadAt(leaf, phys); err != nil {
		t.Fatalf("setRootItemLevel: read: %v", err)
	}
	idx := findItemIdx(leaf, objID, typeRootItem, 0)
	if idx < 0 {
		t.Fatalf("setRootItemLevel: ROOT_ITEM %d not found", objID)
	}
	items := parseLeafItems(leaf, parseNodeHeader(leaf).nItems)
	d := items[idx].data(leaf)
	if len(d) <= rootItemOffLevel {
		t.Fatalf("setRootItemLevel: ROOT_ITEM %d too short", objID)
	}
	d[rootItemOffLevel] = level
	updateNodeCRC(leaf)
	if _, err := bfs.rwa.WriteAt(leaf, phys); err != nil {
		t.Fatalf("setRootItemLevel: write: %v", err)
	}
	bfs.invalidateCache()
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

// TestRelocMeta_MultiLevelInteriorInTail builds a genuine TWO-level DEV tree
// whose interior root node AND one child leaf sit high in the [18,24) MiB tail,
// then shrinks so the tail captures them. relocateTailMetadata must path-COW the
// subtree down bottom-up (relocate the in-tail child leaf, repoint the interior
// node's child key-ptr + generation, relocate the interior node, repoint the
// ROOT_ITEM) — keeping the kernel's parent-transid invariant — and every file
// must read back byte-identical. Runs on every CI arch (no kernel).
func TestRelocMeta_MultiLevelInteriorInTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mlinterior.img")
	fs, err := Format(path, 24*1024*1024, FormatConfig{Label: "mlint"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)
	files := map[string][]byte{
		"/a.txt": bytes.Repeat([]byte("AAAA"), 1500),
		"/b.txt": []byte("hello multi-level\n"),
	}
	for name, data := range files {
		if err := fs.WriteFile(name, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	// DEV tree has DEV_ITEM + DEV_EXTENT(s) in its single leaf — enough to split
	// into a real two-level tree. Interior node at 21 MiB, right child leaf at
	// 20 MiB; both inside the [18,24) MiB tail.
	placeTreeMultiLevelHigh(t, bfs, devTreeObjID, 21*1024*1024, 20*1024*1024)

	if err := bfs.Shrink(18 * 1024 * 1024); err != nil {
		t.Fatalf("Shrink with multi-level interior node in tail: %v", err)
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

// TestRemoveChunk_NonEmptyRelocates builds a 2-data-chunk image whose trailing
// chunk holds a real live file, then Shrink-drops that whole chunk: its contents
// must be relocated into the lower chunk and the now-empty chunk removed. The
// relocated file must survive byte-identical and the device must shrink to the
// chunk boundary. Runs on every arch (no kernel).
func TestRemoveChunk_NonEmptyRelocates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonempty-chunk.img")
	fs, err := Format(path, 16*1024*1024, FormatConfig{Label: "nechunk"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)
	if err := fs.WriteFile("/keep.bin", metaFixtureKeep, 0o644); err != nil {
		t.Fatalf("keep: %v", err)
	}
	payload := bytes.Repeat([]byte("RELOC-ME-ALIGNED-"), 256*8)
	newChunkLog := appendDataChunkWithLiveExtent(t, bfs, 8*1024*1024, "/tail.bin", payload)

	if err := bfs.Shrink(int64(newChunkLog)); err != nil {
		t.Fatalf("Shrink dropping non-empty trailing chunk: %v", err)
	}
	if got := readSBTotalBytes(t, bfs); got != newChunkLog {
		t.Errorf("post-shrink total_bytes = %d, want %d", got, newChunkLog)
	}
	// No local chunk may extend past the shrunk device.
	bfs.mu.Lock()
	for _, m := range bfs.sb.sysChunks {
		if m.localStripeIdx >= 0 && m.physStart+m.size > newChunkLog {
			t.Errorf("chunk at phys 0x%X size %d extends past shrunk size %d", m.physStart, m.size, newChunkLog)
		}
	}
	bfs.mu.Unlock()
	assertNoMetaAboveLocked(t, bfs, newChunkLog)

	r2 := reopenBtrfs(t, bfs, path)
	defer r2.Close()
	if got, err := r2.ReadFile("/keep.bin"); err != nil || !bytes.Equal(got, metaFixtureKeep) {
		t.Errorf("/keep.bin mismatch after chunk reloc: err=%v len=%d", err, len(got))
	}
	if got, err := r2.ReadFile("/tail.bin"); err != nil || !bytes.Equal(got, payload) {
		t.Errorf("relocated /tail.bin mismatch: err=%v len=%d", err, len(got))
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
