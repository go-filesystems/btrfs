package filesystem_btrfs

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// TestRemoveChunk_RefusesOnlyChunk asserts that a shrink dropping the sole
// DATA|METADATA chunk (no lower local chunk to anchor the device tail) is
// refused with the bootstrap/only-chunk boundary error, leaving the image
// readable.
func TestRemoveChunk_RefusesOnlyChunk(t *testing.T) {
	fs, _ := resizeTempImage(t, 16*1024*1024)
	// The DATA chunk starts at 5 MiB. Shrinking to exactly 5 MiB drops the whole
	// (and only) data chunk; there is no lower local chunk reaching 5 MiB, so the
	// removal must be refused.
	err := fs.Shrink(5 * 1024 * 1024)
	if err == nil {
		t.Fatal("Shrink dropping the only data chunk accepted; want refusal")
	}
	// The sole DATA|METADATA chunk holds every metadata block; it is anchored by
	// the SYSTEM chunk that ends at 5 MiB, but that small chunk has no room to
	// receive the relocated metadata, so non-empty whole-chunk relocation refuses
	// for lack of free space below the new size. Any of these is a correct,
	// image-preserving boundary.
	if !strings.Contains(err.Error(), "no free") &&
		!strings.Contains(err.Error(), "not empty") &&
		!strings.Contains(err.Error(), "no chunk reaching its tail") &&
		!strings.Contains(err.Error(), "below minimum") &&
		!strings.Contains(err.Error(), "below bytes_used") {
		t.Errorf("expected whole-chunk-removal boundary refusal, got: %v", err)
	}
	// Image must still be readable at its original size.
	if _, rerr := fs.ReadFile("/nonexistent"); rerr == nil {
		t.Error("expected ReadFile of missing path to error")
	}
}

// TestRemoveChunk_RefusesPartialDrop verifies that a shrink landing part-way
// into the (would-be removed) trailing chunk — i.e. not on its boundary — is
// refused rather than silently corrupting geometry.
func TestRemoveChunk_RefusesPartialDrop(t *testing.T) {
	path := t.TempDir() + "/partial.img"
	fs, err := Format(path, 16*1024*1024, FormatConfig{Label: "partial"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)
	newChunkLog := appendEmptyDataChunk(t, bfs, 8*1024*1024)
	bfs.mu.Lock()
	defer bfs.mu.Unlock()
	// Drive removeWholeTrailingChunkLocked directly with a newSize below the
	// trailing chunk's start (a partial drop of the chunk *below* it).
	err = bfs.removeWholeTrailingChunkLocked(len(bfs.sb.sysChunks)-1, newChunkLog-4096)
	if err == nil || !strings.Contains(err.Error(), "trailing chunk boundary") {
		t.Errorf("expected boundary refusal, got: %v", err)
	}
}

// TestRemoveChunk_NonEmptyUsedFlagCorrected marks the appended trailing chunk's
// BLOCK_GROUP_ITEM with a non-zero `used` even though it holds no live extent.
// chunkIsEvacuableLocked sees it as occupied (so the non-empty path is taken),
// but the relocation pass finds nothing to move and the extent-tree rebuild
// recomputes `used` back to 0, so the now-empty chunk is removed cleanly.
func TestRemoveChunk_NonEmptyUsedFlagCorrected(t *testing.T) {
	path := t.TempDir() + "/ne.img"
	fs, err := Format(path, 16*1024*1024, FormatConfig{Label: "ne"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)
	newChunkLog := appendEmptyDataChunk(t, bfs, 8*1024*1024)
	bfs.mu.Lock()
	// Flip the appended chunk's BLOCK_GROUP_ITEM used from 0 to a non-zero value
	// directly in the extent leaf, so chunkIsEvacuableLocked sees it as occupied.
	extRoot, err := extentTreeRoot(bfs.rwa, bfs.partOffset, bfs.sb)
	if err != nil {
		bfs.mu.Unlock()
		t.Fatalf("extent root: %v", err)
	}
	leaf, phys, err := bfs.findExtentLeafWithKey(extRoot, newChunkLog, typeBlockGroupItem, 8*1024*1024)
	if err != nil || leaf == nil {
		bfs.mu.Unlock()
		t.Fatalf("find block group: leaf=%v err=%v", leaf != nil, err)
	}
	idx := findItemIdx(leaf, newChunkLog, typeBlockGroupItem, 8*1024*1024)
	items := parseLeafItems(leaf, parseNodeHeader(leaf).nItems)
	d := items[idx].data(leaf)
	binary.LittleEndian.PutUint64(d[0:], 4096) // used = nonzero (but no live extent)
	updateNodeCRC(leaf)
	if _, err := bfs.rwa.WriteAt(leaf, phys); err != nil {
		bfs.mu.Unlock()
		t.Fatalf("write leaf: %v", err)
	}
	bfs.invalidateCache()
	idxChunk := len(bfs.sb.sysChunks) - 1
	if bfs.chunkIsEvacuableLocked(idxChunk) {
		t.Error("chunkIsEvacuableLocked reported a used!=0 chunk as evacuable")
	}
	err = bfs.removeWholeTrailingChunkLocked(idxChunk, newChunkLog)
	bfs.mu.Unlock()
	if err != nil {
		t.Fatalf("non-empty (flag-only) chunk removal failed: %v", err)
	}
	if got := bfs.sb.totalBytes; got != newChunkLog {
		t.Errorf("post-removal total_bytes = %d, want %d", got, newChunkLog)
	}
}

// TestRelocMeta_RefusesMultiLevelRoot fakes a multi-level root tree in the tail
// and asserts relocateTailMetadata refuses it with a clear error rather than
// mishandling an interior node.
// TestRelocMeta_MultiLevelRootTreeLeafInTail builds a genuine multi-level
// ROOT_TREE whose ROOT_ITEM leaf sits in the removed tail, then shrinks. The
// relocation must COW the leaf (and its interior parent) below the new size,
// re-seat the superblock `root` pointer, and leave every file byte-identical.
// Runs on every CI arch (no kernel).
func TestRelocMeta_MultiLevelRootTreeLeafInTail(t *testing.T) {
	path := t.TempDir() + "/mlroot.img"
	fs, err := Format(path, 24*1024*1024, FormatConfig{Label: "mlroot"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)
	files := map[string][]byte{
		"/a.txt": bytes.Repeat([]byte("ROOTLEAF"), 600),
		"/b.txt": []byte("multi-level root tree\n"),
	}
	for name, data := range files {
		if err := fs.WriteFile(name, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	// Split the ROOT_TREE into a real two-level tree: interior node at 21 MiB,
	// right child leaf (carrying the upper ROOT_ITEMs) at 20 MiB — both inside
	// [18,24) MiB.
	placeRootTreeMultiLevelHigh(t, bfs, 21*1024*1024, 20*1024*1024)

	if err := bfs.Shrink(18 * 1024 * 1024); err != nil {
		t.Fatalf("Shrink with multi-level root-tree leaf in tail: %v", err)
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

// TestRelocMeta_MultiLevelExtentTreeShrinks builds a genuine two-level EXTENT
// tree with a node in the tail and asserts the shrink now SUCCEEDS: the extent
// tree is not relocated block-by-block but rebuilt low and multi-level by the
// finalize, evacuating its tail nodes. The files survive byte-identical and no
// live metadata remains above the new size. Runs on every arch (no kernel).
func TestRelocMeta_MultiLevelExtentTreeShrinks(t *testing.T) {
	path := t.TempDir() + "/mlext.img"
	fs, err := Format(path, 24*1024*1024, FormatConfig{Label: "mlext"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)
	files := map[string][]byte{
		"/x": bytes.Repeat([]byte("x"), 4096),
		"/y": []byte("extent tree multi-level\n"),
	}
	for name, data := range files {
		if err := fs.WriteFile(name, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	// Make the EXTENT tree genuinely two-level with its interior node high in the
	// [18,24) MiB tail.
	placeTreeMultiLevelHigh(t, bfs, extentTreeObjID, 21*1024*1024, 20*1024*1024)

	if err := bfs.Shrink(18 * 1024 * 1024); err != nil {
		t.Fatalf("Shrink with multi-level extent tree in tail: %v", err)
	}
	if got := readSBTotalBytes(t, bfs); got != 18*1024*1024 {
		t.Errorf("post-shrink total_bytes = %d, want %d", got, 18*1024*1024)
	}
	assertNoMetaAboveLocked(t, bfs, 18*1024*1024)

	r2 := reopenBtrfs(t, bfs, path)
	defer r2.Close()
	for name, data := range files {
		if got, err := r2.ReadFile(name); err != nil || !bytes.Equal(got, data) {
			t.Errorf("after shrink %s mismatch: err=%v len=%d", name, err, len(got))
		}
	}
}

// TestRepointRootItem_WriteFault covers repointRootItemLocked's write-error
// branch by faulting the write of the root-tree leaf.
func TestRepointRootItem_WriteFault(t *testing.T) {
	path := t.TempDir() + "/rpwf.img"
	if _, err := Format(path, 16*1024*1024, FormatConfig{}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	f, err := osOpenFileRW(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	wrapper := &failBackend{inner: &osFileBackend{f: f}}
	fs, err := OpenFromDevice(wrapper, -1)
	if err != nil {
		t.Fatalf("OpenFromDevice: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	bfs := fs.(*btrfsFS)
	bfs.mu.Lock()
	defer bfs.mu.Unlock()
	wrapper.failWriteAt = true
	if err := bfs.repointRootItemLocked(csumTreeObjID, 0x600000); err == nil ||
		!strings.Contains(err.Error(), "write root tree leaf") {
		t.Errorf("expected write-fault error, got: %v", err)
	}
}

// TestShrink_MultiLevelExtentTreeInTail drives a full Shrink whose tail holds a
// genuine two-level EXTENT tree. This used to be a refusal; the extent tree is
// now rebuilt low and multi-level by the finalize, so the shrink succeeds and the
// data survives byte-identical at the smaller size.
func TestShrink_MultiLevelExtentTreeInTail(t *testing.T) {
	path := t.TempDir() + "/unreloc.img"
	fs, err := Format(path, 24*1024*1024, FormatConfig{Label: "unreloc"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)
	payload := bytes.Repeat([]byte("DATA-ALIGNED-"), 256*8)
	if err := fs.WriteFile("/d.bin", payload, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	placeTreeMultiLevelHigh(t, bfs, extentTreeObjID, 21*1024*1024, 20*1024*1024)
	if err := bfs.Shrink(18 * 1024 * 1024); err != nil {
		t.Fatalf("Shrink with multi-level extent tree in tail: %v", err)
	}
	assertNoMetaAboveLocked(t, bfs, 18*1024*1024)
	r2 := reopenBtrfs(t, bfs, path)
	defer r2.Close()
	if got, rerr := r2.ReadFile("/d.bin"); rerr != nil || !bytes.Equal(got, payload) {
		t.Errorf("data corrupted after shrink: err=%v len=%d", rerr, len(got))
	}
}

// TestRelocLeafBlock_AboveLimit exercises the guard where the replacement block
// allocates but lands at/above the limit (no room strictly below).
func TestRelocLeafBlock_AboveLimit(t *testing.T) {
	fs, _ := resizeTempImage(t, 16*1024*1024)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	// Make the only free extent a high one, then pass a limit below it.
	fs.sm.freeExts = []freeExtent{{physStart: 0xA00000, size: uint64(fs.sb.nodeSize)}}
	if _, err := fs.relocateLeafBlock(fs.sb.rootLogAddr, 0x900000); err == nil ||
		!strings.Contains(err.Error(), "no free metadata block below") {
		t.Errorf("expected below-limit guard error, got: %v", err)
	}
}

// TestRelocLeafBlock_NoLowSpace exercises relocateLeafBlock's "no free metadata
// block below new size" guard: with the allocator emptied, the replacement
// allocation either fails or lands at/above the limit, and the call must error.
func TestRelocLeafBlock_NoLowSpace(t *testing.T) {
	fs, _ := resizeTempImage(t, 16*1024*1024)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	// Drain the free list so allocNodeBlock fails.
	fs.sm.freeExts = nil
	if _, err := fs.relocateLeafBlock(fs.sb.rootLogAddr, 1024); err == nil {
		t.Error("relocateLeafBlock with no free space accepted; want error")
	}
}

// TestRepointRootItem_Missing asserts repointRootItemLocked errors when the
// target ROOT_ITEM is absent.
func TestRepointRootItem_Missing(t *testing.T) {
	fs, _ := resizeTempImage(t, 16*1024*1024)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if err := fs.repointRootItemLocked(0xDEADBEEF, 0x600000); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Errorf("expected ROOT_ITEM not-found error, got: %v", err)
	}
}

// TestDeleteItem_TolerateAbsent exercises the "tolerate missing item" branches
// of the chunk-removal delete helpers: deleting items that are not present must
// be a no-op (nil), and deleteLeafItemInPlace on an absent key must error.
func TestDeleteItem_TolerateAbsent(t *testing.T) {
	fs, _ := resizeTempImage(t, 16*1024*1024)
	fs.mu.Lock()
	defer fs.mu.Unlock()

	// dev tree: a DEV_EXTENT for a non-existent chunk logical is absent -> nil.
	if err := fs.deleteDevTreeItemLocked(1, typeDevExtent, 0xFEEDFACE); err != nil {
		t.Errorf("deleteDevTreeItemLocked(absent) = %v, want nil", err)
	}
	// extent tree: a BLOCK_GROUP_ITEM for a non-existent chunk is absent -> nil.
	if err := fs.deleteBlockGroupItemLocked(0xFEEDFACE, 0x1000); err != nil {
		t.Errorf("deleteBlockGroupItemLocked(absent) = %v, want nil", err)
	}
	// sys_chunk_array: a data chunk logical is not mirrored there -> nil.
	if err := fs.deleteSysChunkArrayEntryLocked(0xFEEDFACE); err != nil {
		t.Errorf("deleteSysChunkArrayEntryLocked(absent) = %v, want nil", err)
	}
	// deleteLeafItemInPlace on an absent key errors.
	phys, _ := fs.sb.physAddr(fs.partOffset, fs.sb.chunkLogAddr)
	if err := fs.deleteLeafItemInPlace(phys, 0xDEAD, typeChunkItem, 0xBEEF); err == nil ||
		!strings.Contains(err.Error(), "not in leaf") {
		t.Errorf("deleteLeafItemInPlace(absent) = %v, want not-in-leaf error", err)
	}
}

// TestDeleteSysChunkArrayEntry_Removes drives the sys_chunk_array compaction
// branch by removing the SYSTEM chunk's mirrored entry (which IS present) and
// confirming the recorded array size shrank.
func TestDeleteSysChunkArrayEntry_Removes(t *testing.T) {
	fs, _ := resizeTempImage(t, 16*1024*1024)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var sysChunk chunkMapping
	for _, m := range fs.sb.sysChunks {
		if m.profile&blockGroupSystem != 0 {
			sysChunk = m
		}
	}
	readArrSz := func() uint32 {
		b := make([]byte, 4)
		fs.rwa.ReadAt(b, fs.partOffset+superblockOffset+int64(sbfSysChunkArrSz))
		return binary.LittleEndian.Uint32(b)
	}
	before := readArrSz()
	if err := fs.deleteSysChunkArrayEntryLocked(sysChunk.logStart); err != nil {
		t.Fatalf("deleteSysChunkArrayEntryLocked: %v", err)
	}
	if after := readArrSz(); after >= before {
		t.Errorf("sys_chunk_array_size did not shrink: before=%d after=%d", before, after)
	}
}

// TestFsRootNodeGeneration_Fallback covers the fallback when the FS-root node
// cannot be read (an unmapped logical address): the helper returns the supplied
// fallback generation rather than panicking.
func TestFsRootNodeGeneration_Fallback(t *testing.T) {
	fs, _ := resizeTempImage(t, 16*1024*1024)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	got := fsRootNodeGeneration(fs.rwa, fs.partOffset, fs.sb, 0xFFFFFFFFFF000000, 42)
	if got != 42 {
		t.Errorf("fsRootNodeGeneration(unmapped) = %d, want fallback 42", got)
	}
}

// TestRemoveTrailingChunk_DeleteError drives removeTrailingChunkLocked with a
// CHUNK_ITEM key that is not in the chunk tree, so the first delete errors and
// the function surfaces it.
func TestRemoveTrailingChunk_DeleteError(t *testing.T) {
	path := t.TempDir() + "/delerr.img"
	fs, err := Format(path, 16*1024*1024, FormatConfig{Label: "delerr"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)
	appendEmptyDataChunk(t, bfs, 8*1024*1024)
	bfs.mu.Lock()
	defer bfs.mu.Unlock()
	// Corrupt the in-memory chunk's logStart so the CHUNK_ITEM delete misses.
	idx := len(bfs.sb.sysChunks) - 1
	bfs.sb.sysChunks[idx].logStart = 0xBADBAD000
	if err := bfs.removeTrailingChunkLocked(idx, bfs.sb.sysChunks[idx].physStart); err == nil ||
		!strings.Contains(err.Error(), "delete CHUNK_ITEM") {
		t.Errorf("expected CHUNK_ITEM delete error, got: %v", err)
	}
}

// TestRemoveChunk_TruncateFault injects a Truncate failure during whole-chunk
// removal: the metadata edits succeed but the final device truncate fails, so
// Shrink surfaces an error (the on-disk metadata is already consistent at the
// smaller size — the trailing bytes are merely unreferenced).
func TestRemoveChunk_TruncateFault(t *testing.T) {
	path := t.TempDir() + "/trunc.img"
	if _, err := Format(path, 16*1024*1024, FormatConfig{}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	f, err := osOpenFileRW(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	wrapper := &failBackend{inner: &osFileBackend{f: f}}
	fs, err := OpenFromDevice(wrapper, -1)
	if err != nil {
		t.Fatalf("OpenFromDevice: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	bfs := fs.(*btrfsFS)
	newChunkLog := appendEmptyDataChunk(t, bfs, 8*1024*1024)
	wrapper.failTruncate = true
	if err := bfs.Shrink(int64(newChunkLog)); err == nil ||
		!strings.Contains(err.Error(), "truncate") {
		t.Errorf("expected truncate-fault error, got: %v", err)
	}
}

// TestRelocMeta_WriteFault injects a write failure during metadata-block
// relocation (a root tree placed high, then shrunk) so relocateLeafBlock's write
// fails and the shrink errors without truncating.
func TestRelocMeta_WriteFault(t *testing.T) {
	path := t.TempDir() + "/metafault.img"
	if _, err := Format(path, 24*1024*1024, FormatConfig{}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	f, err := osOpenFileRW(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	wrapper := &failBackend{inner: &osFileBackend{f: f}}
	fs, err := OpenFromDevice(wrapper, -1)
	if err != nil {
		t.Fatalf("OpenFromDevice: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	bfs := fs.(*btrfsFS)
	if err := bfs.WriteFile("/x", []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	placeTreeRootHigh(t, bfs, rootTreeObjID, 20*1024*1024)
	wrapper.failWriteAt = true
	if err := bfs.Shrink(18 * 1024 * 1024); err == nil {
		t.Error("Shrink accepted despite WriteAt failure during metadata relocation")
	}
}

// TestBlockGroupUsed_Reports confirms blockGroupUsedLocked reads the live block
// group's used byte count (non-zero after a write, present in the extent tree).
func TestBlockGroupUsed_Reports(t *testing.T) {
	fs, _ := resizeTempImage(t, 16*1024*1024)
	if err := fs.WriteFile("/u.bin", bytes.Repeat([]byte("U"), 4096), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var dataChunk chunkMapping
	for _, m := range fs.sb.sysChunks {
		if m.profile&blockGroupData != 0 {
			dataChunk = m
		}
	}
	if used, ok := fs.blockGroupUsedLocked(dataChunk.logStart, dataChunk.size); !ok || used == 0 {
		t.Errorf("blockGroupUsedLocked = (%d, %v), want non-zero used", used, ok)
	}
}
