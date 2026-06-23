package filesystem_btrfs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// extentTreeLevel returns the node-header level of the EXTENT_TREE root, read via
// its ROOT_ITEM in the (possibly multi-level) root tree.
func extentTreeLevel(t *testing.T, bfs *btrfsFS) uint8 {
	t.Helper()
	bfs.mu.Lock()
	defer bfs.mu.Unlock()
	root, err := extentTreeRoot(bfs.rwa, bfs.partOffset, bfs.sb)
	if err != nil {
		t.Fatalf("extentTreeRoot: %v", err)
	}
	node, err := readNode(bfs.rwa, bfs.partOffset, bfs.sb, root)
	if err != nil {
		t.Fatalf("read extent root: %v", err)
	}
	return parseNodeHeader(node).level
}

// countLiveExtentItems walks the EXTENT_TREE and tallies METADATA_ITEM,
// EXTENT_ITEM and BLOCK_GROUP_ITEM records — proving a multi-level extent tree is
// fully traversable (interior nodes index real leaves).
func countLiveExtentItems(t *testing.T, bfs *btrfsFS) (meta, data, bg int) {
	t.Helper()
	bfs.mu.Lock()
	defer bfs.mu.Unlock()
	root, err := extentTreeRoot(bfs.rwa, bfs.partOffset, bfs.sb)
	if err != nil {
		t.Fatalf("extentTreeRoot: %v", err)
	}
	_ = walkLeaves(bfs.rwa, bfs.partOffset, bfs.sb, root, func(buf []byte, items []leafItem) error {
		for _, it := range items {
			switch it.k.typ {
			case typeMetadataItem:
				meta++
			case typeExtentItem:
				data++
			case typeBlockGroupItem:
				bg++
			}
		}
		return nil
	})
	return
}

// TestMultiLevelExtentTree_ManyFiles writes enough distinct-extent files to
// overflow a single 4 KiB extent leaf, forcing rebuildExtentTree to emit a
// genuine MULTI-LEVEL extent tree (interior node indexing several leaves). It
// asserts the tree is multi-level, fully walkable, every file reads back
// byte-identical, and the live-block bookkeeping is internally consistent. Runs
// on every CI arch (no kernel).
func TestMultiLevelExtentTree_ManyFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mlmany.img")
	fs, err := Format(path, 64*1024*1024, FormatConfig{Label: "mlmany"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)

	const n = 150
	want := map[string][]byte{}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("/f%04d.bin", i)
		// Distinct, sector-aligned payload so each file owns one regular data extent.
		body := bytes.Repeat([]byte(fmt.Sprintf("E%04d-", i)), 4096/6+1)[:4096]
		if err := fs.WriteFile(name, body, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		want[name] = body
	}

	if lvl := extentTreeLevel(t, bfs); lvl == 0 {
		t.Fatalf("extent tree is still single-leaf (level 0); expected multi-level for %d files", n)
	}
	meta, data, bg := countLiveExtentItems(t, bfs)
	if data < n {
		t.Errorf("walked %d EXTENT_ITEMs, want >= %d (multi-level walk missed leaves)", data, n)
	}
	if meta == 0 || bg == 0 {
		t.Errorf("multi-level extent tree missing records: meta=%d bg=%d", meta, bg)
	}

	// Reopen and verify every file byte-identical (exercises the read path against
	// the rebuilt multi-level extent tree's data extents).
	r2 := reopenBtrfs(t, bfs, path)
	defer r2.Close()
	for name, body := range want {
		got, err := r2.ReadFile(name)
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("after rebuild %s mismatch: err=%v len=%d want %d", name, err, len(got), len(body))
		}
	}
}

// TestMultiLevelExtentTree_ShrinkRelocates writes many files into a large image,
// then Shrinks it. The extent tree is multi-level, so the shrink must rebuild it
// multi-level and entirely below the new size, leaving no live metadata in the
// removed tail and every file intact. Runs on every CI arch (no kernel).
func TestMultiLevelExtentTree_ShrinkRelocates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mlshrink.img")
	fs, err := Format(path, 64*1024*1024, FormatConfig{Label: "mlshrink"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)

	const n = 120
	want := map[string][]byte{}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("/g%04d.bin", i)
		body := bytes.Repeat([]byte(fmt.Sprintf("S%04d-", i)), 4096/6+1)[:4096]
		if err := fs.WriteFile(name, body, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		want[name] = body
	}
	if lvl := extentTreeLevel(t, bfs); lvl == 0 {
		t.Skipf("extent tree did not reach multi-level (%d files); nothing to validate", n)
	}

	// Shrink to 48 MiB — the data + metadata comfortably fit, and the rebuilt
	// extent tree must be reconstructed below the new size.
	newSize := int64(48 * 1024 * 1024)
	if err := bfs.Shrink(newSize); err != nil {
		t.Fatalf("Shrink with multi-level extent tree: %v", err)
	}
	if got := readSBTotalBytes(t, bfs); got != uint64(newSize) {
		t.Errorf("post-shrink total_bytes = %d, want %d", got, newSize)
	}
	assertNoMetaAboveLocked(t, bfs, uint64(newSize))

	r2 := reopenBtrfs(t, bfs, path)
	defer r2.Close()
	for name, body := range want {
		got, err := r2.ReadFile(name)
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("after shrink %s mismatch: err=%v len=%d want %d", name, err, len(got), len(body))
		}
	}
}

// TestPartitionLeaves_PacksByBytes is a unit check on the leaf partitioner: items
// are packed greedily until the byte budget is exceeded, never splitting an item.
func TestPartitionLeaves_PacksByBytes(t *testing.T) {
	mk := func(n int) []itemRec {
		out := make([]itemRec, n)
		for i := range out {
			out[i] = itemRec{k: key{uint64(i), typeMetadataItem, 0}, data: make([]byte, 33)}
		}
		return out
	}
	// capBytes admits exactly 2 items of size itemSize+33 = 58.
	ranges := partitionLeaves(mk(5), 2*(itemSize+33))
	if len(ranges) != 3 { // 2 + 2 + 1
		t.Fatalf("got %d leaves, want 3: %v", len(ranges), ranges)
	}
	if ranges[0] != [2]int{0, 2} || ranges[1] != [2]int{2, 4} || ranges[2] != [2]int{4, 5} {
		t.Errorf("unexpected ranges: %v", ranges)
	}
}

// TestInteriorShape_Heights checks the interior-node sizing for a few fan-outs.
func TestInteriorShape_Heights(t *testing.T) {
	// icap = 3: 1 leaf -> no interior; 3 -> 1 root; 4 -> 2 + 1 root (2 levels).
	cases := []struct {
		leaves, icap, total int
		rootLevel           uint8
	}{
		{1, 3, 0, 0},
		{2, 3, 1, 1},
		{3, 3, 1, 1},
		{4, 3, 3, 2},
		{9, 3, 4, 2},
		{10, 3, 7, 3}, // 4 (ceil 10/3) + 2 (ceil 4/3) + 1 root
	}
	for _, c := range cases {
		_, total, rl := interiorShape(c.leaves, c.icap)
		if total != c.total || rl != c.rootLevel {
			t.Errorf("interiorShape(%d,%d) = total %d level %d, want total %d level %d",
				c.leaves, c.icap, total, rl, c.total, c.rootLevel)
		}
	}
}

// TestRepointExtentRoot_Missing asserts repointExtentRoot errors when the
// EXTENT_TREE ROOT_ITEM is absent from the root tree (here: a fixture whose root
// tree never carried one is unusual, so we simulate by deleting it).
func TestRepointExtentRoot_Missing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "noext.img")
	fs, err := Format(path, 16*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)
	defer bfs.Close()
	bfs.mu.Lock()
	defer bfs.mu.Unlock()
	// Delete the EXTENT_TREE ROOT_ITEM from the single-leaf root tree in place.
	phys, err := bfs.sb.physAddr(bfs.partOffset, bfs.sb.rootLogAddr)
	if err != nil {
		t.Fatalf("physAddr: %v", err)
	}
	leaf := make([]byte, bfs.sb.nodeSize)
	if _, err := bfs.rwa.ReadAt(leaf, phys); err != nil {
		t.Fatalf("read: %v", err)
	}
	idx := findItemIdx(leaf, extentTreeObjID, typeRootItem, 0)
	if idx < 0 {
		t.Fatalf("EXTENT_TREE ROOT_ITEM not present to delete")
	}
	leafDeleteItem(leaf, idx)
	updateNodeCRC(leaf)
	if _, err := bfs.rwa.WriteAt(leaf, phys); err != nil {
		t.Fatalf("write: %v", err)
	}
	bfs.invalidateCache()
	if err := repointExtentRoot(bfs.rwa, bfs.partOffset, bfs.sb, 0x500000, 1); err == nil ||
		!bytes.Contains([]byte(err.Error()), []byte("ROOT_ITEM not found")) {
		t.Errorf("expected ROOT_ITEM not-found error, got: %v", err)
	}
}

// TestExtentHeaderTemplate_OwnerReset checks that extentHeaderTemplate copies a
// reachable node's header and resets the owner field to EXTENT_TREE.
func TestExtentHeaderTemplate_OwnerReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hdr.img")
	fs, err := Format(path, 16*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)
	defer bfs.Close()
	bfs.mu.Lock()
	defer bfs.mu.Unlock()
	tpl, err := extentHeaderTemplate(bfs.rwa, bfs.partOffset, bfs.sb)
	if err != nil {
		t.Fatalf("extentHeaderTemplate: %v", err)
	}
	if got := binary.LittleEndian.Uint64(tpl[0x58:]); got != extentTreeObjID {
		t.Errorf("template owner = %d, want %d", got, extentTreeObjID)
	}
	if len(tpl) != int(bfs.sb.nodeSize) {
		t.Errorf("template size = %d, want %d", len(tpl), bfs.sb.nodeSize)
	}
}

// TestRootItemLeafLogAddr_MultiLevel checks that rootItemLeafLogAddr descends a
// genuine multi-level root tree to the leaf carrying a given ROOT_ITEM.
func TestRootItemLeafLogAddr_MultiLevel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mlrootaddr.img")
	fs, err := Format(path, 24*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)
	defer bfs.Close()
	if err := fs.WriteFile("/x", []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	placeRootTreeMultiLevelHigh(t, bfs, 21*1024*1024, 20*1024*1024)
	bfs.mu.Lock()
	defer bfs.mu.Unlock()
	// The root tree is now level 1; the helper must return a level-0 leaf that
	// actually contains the FS_TREE ROOT_ITEM.
	leafLog, err := rootItemLeafLogAddr(bfs.rwa, bfs.partOffset, bfs.sb, fsTreeObjID)
	if err != nil {
		t.Fatalf("rootItemLeafLogAddr: %v", err)
	}
	node, err := readNode(bfs.rwa, bfs.partOffset, bfs.sb, leafLog)
	if err != nil {
		t.Fatalf("read leaf: %v", err)
	}
	if parseNodeHeader(node).level != 0 {
		t.Errorf("returned node is not a leaf (level %d)", parseNodeHeader(node).level)
	}
	if findItemIdx(node, fsTreeObjID, typeRootItem, 0) < 0 {
		t.Errorf("FS_TREE ROOT_ITEM not in returned leaf 0x%X", leafLog)
	}
}

// TestMultiLevelExtentTree_AtScale writes many small (inline) files so the FS
// tree grows large and the EXTENT_TREE goes multi-level under a heavy METADATA_ITEM
// load, exercising the multi-leaf build and a full interior-indexed walk and
// readback at scale.
func TestMultiLevelExtentTree_AtScale(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test is slow; skipped in -short")
	}
	path := filepath.Join(t.TempDir(), "ml3.img")
	fs, err := Format(path, 256*1024*1024, FormatConfig{Label: "ml3"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)

	const n = 2000
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("/t%05d.bin", i)
		if err := fs.WriteFile(name, []byte(fmt.Sprintf("body-%05d\n", i)), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	lvl := extentTreeLevel(t, bfs)
	if lvl < 1 {
		t.Fatalf("extent tree level %d; expected multi-level", lvl)
	}
	t.Logf("extent tree level = %d", lvl)
	// These files are tiny (inline EXTENT_DATA, no data extent), so the extent
	// tree is dominated by METADATA_ITEMs for the large FS tree. Assert the walk
	// reaches a substantial number of records (proving interior nodes index real
	// leaves end to end).
	meta, _, bg := countLiveExtentItems(t, bfs)
	if meta < 200 || bg == 0 {
		t.Errorf("multi-level extent walk too small: meta=%d bg=%d", meta, bg)
	}
	r2 := reopenBtrfs(t, bfs, path)
	defer r2.Close()
	// Spot-check a sample across the key space.
	for _, i := range []int{0, 1, n / 2, n - 2, n - 1} {
		name := fmt.Sprintf("/t%05d.bin", i)
		want := []byte(fmt.Sprintf("body-%05d\n", i))
		if got, err := r2.ReadFile(name); err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s mismatch: err=%v got=%q", name, err, got)
		}
	}
}

// TestMultiLevelExtentTree_AllocExhaustion drives the multi-level rebuild with
// the space manager drained (and the old extent tree's freed blocks insufficient)
// so a block allocation fails, surfacing the error rather than silently producing
// a broken tree.
func TestMultiLevelExtentTree_AllocExhaustion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mlexh.img")
	fs, err := Format(path, 64*1024*1024, FormatConfig{Label: "mlexh"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)
	defer bfs.Close()
	// Enough files to force a multi-level extent tree (several leaves + interior).
	for i := 0; i < 120; i++ {
		body := bytes.Repeat([]byte(fmt.Sprintf("X%04d-", i)), 4096/6+1)[:4096]
		if err := fs.WriteFile(fmt.Sprintf("/q%04d.bin", i), body, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	bfs.mu.Lock()
	defer bfs.mu.Unlock()
	// Drain the allocator entirely AND mark the old extent root unreachable so the
	// rebuild cannot reclaim its blocks: every allocation must then fail.
	bfs.sm.freeExts = nil
	// Point the EXTENT_TREE ROOT_ITEM at an out-of-bounds bytenr so extentTreeRoot
	// still resolves but the old-tree walk frees nothing (oldExtSafe == false).
	if err := repointExtentRoot(bfs.rwa, bfs.partOffset, bfs.sb, bfs.sb.totalBytes+uint64(bfs.sb.nodeSize), 0); err != nil {
		t.Fatalf("repoint: %v", err)
	}
	bfs.invalidateCache()
	err = rebuildExtentTree(bfs.rwa, bfs.partOffset, bfs.sb, bfs.sm)
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("alloc extent")) {
		t.Errorf("expected alloc-failure from multi-level rebuild, got: %v", err)
	}
}

// TestRepointExtentRoot_TooShort covers the short-ROOT_ITEM guard.
func TestRepointExtentRoot_TooShort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "short.img")
	fs, err := Format(path, 16*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)
	defer bfs.Close()
	bfs.mu.Lock()
	defer bfs.mu.Unlock()
	phys, err := bfs.sb.physAddr(bfs.partOffset, bfs.sb.rootLogAddr)
	if err != nil {
		t.Fatalf("physAddr: %v", err)
	}
	leaf := make([]byte, bfs.sb.nodeSize)
	if _, err := bfs.rwa.ReadAt(leaf, phys); err != nil {
		t.Fatalf("read: %v", err)
	}
	idx := findItemIdx(leaf, extentTreeObjID, typeRootItem, 0)
	// Truncate the ROOT_ITEM data size to below rootItemOffLevel by shrinking it
	// in place via leafReplaceItemData with a tiny payload.
	if err := leafReplaceItemData(leaf, idx, make([]byte, 8)); err != nil {
		t.Fatalf("shrink ROOT_ITEM: %v", err)
	}
	updateNodeCRC(leaf)
	if _, err := bfs.rwa.WriteAt(leaf, phys); err != nil {
		t.Fatalf("write: %v", err)
	}
	bfs.invalidateCache()
	if err := repointExtentRoot(bfs.rwa, bfs.partOffset, bfs.sb, 0x500000, 1); err == nil ||
		!bytes.Contains([]byte(err.Error()), []byte("too short")) {
		t.Errorf("expected too-short error, got: %v", err)
	}
}

// TestGenerateMultiLevelFixtures writes the multi-level validation images to
// $MLFIXTURE_OUT for out-of-band kernel validation (btrfs check + mount in a VM).
// Skipped unless the env var is set. Not a CI test — a generation utility.
func TestGenerateMultiLevelFixtures(t *testing.T) {
	dir := getenvMLFixture()
	if dir == "" {
		t.Skip("MLFIXTURE_OUT not set")
	}

	// (a) Multi-level root tree whose ROOT_ITEM leaf is in the removed tail.
	{
		img := filepath.Join(dir, "ml-roottree-shrink.img")
		fs, err := Format(img, 24*1024*1024, FormatConfig{Label: "mlroot"})
		if err != nil {
			t.Fatalf("Format: %v", err)
		}
		bfs := fs.(*btrfsFS)
		for i := 0; i < 6; i++ {
			name := fmt.Sprintf("/r%02d.bin", i)
			body := bytes.Repeat([]byte(fmt.Sprintf("R%02d-", i)), 4096/4)[:4096]
			if err := fs.WriteFile(name, body, 0o644); err != nil {
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
		t.Logf("wrote %s", img)
	}
}

func getenvMLFixture() string { return os.Getenv("MLFIXTURE_OUT") }

var _ = binary.LittleEndian
