// Package-internal tests – leaf-level, B-tree, and COW mechanics.
package filesystem_btrfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// ── in-memory ReadAt / WriteAt mock ──────────────────────────────────────

type rwaBuf struct{ data []byte }

func (r *rwaBuf) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || int(off) >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
func (r *rwaBuf) WriteAt(p []byte, off int64) (int, error) {
	end := int(off) + len(p)
	if end > len(r.data) {
		r.data = append(r.data, make([]byte, end-len(r.data))...)
	}
	copy(r.data[off:], p)
	return len(p), nil
}

// failWriterAt always fails at WriteAt.
type failWriterAt struct {
	inner *rwaBuf
	err   error
}

func (f *failWriterAt) ReadAt(p []byte, off int64) (int, error)  { return f.inner.ReadAt(p, off) }
func (f *failWriterAt) WriteAt(p []byte, off int64) (int, error) { return 0, f.err }

// failAfterWriter fails WriteAt after N successful writes.
type failAfterWriter struct {
	inner     *rwaBuf
	count     int
	failAfter int
	err       error
}

func (f *failAfterWriter) ReadAt(p []byte, off int64) (int, error) { return f.inner.ReadAt(p, off) }
func (f *failAfterWriter) WriteAt(p []byte, off int64) (int, error) {
	f.count++
	if f.count > f.failAfter {
		return 0, f.err
	}
	return f.inner.WriteAt(p, off)
}

// failReaderAt fails ReadAt at a given physical offset.
type failReaderAt struct {
	inner  *rwaBuf
	failAt int64
	err    error
}

func (f *failReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off == f.failAt {
		return 0, f.err
	}
	return f.inner.ReadAt(p, off)
}
func (f *failReaderAt) WriteAt(p []byte, off int64) (int, error) { return f.inner.WriteAt(p, off) }

// ── Build minimal superblock for unit tests ───────────────────────────────

// buildMinimalSB creates a superblock with one chunk covering [0, imageSize).
func buildMinimalSB(nodeSize uint32, imageSize uint64) *superblock {
	return &superblock{
		nodeSize:     nodeSize,
		sectorSize:   nodeSize,
		leafSize:     nodeSize,
		stripeSize:   nodeSize,
		totalBytes:   imageSize,
		rootLogAddr:  testRootPhys,
		chunkLogAddr: testChunkPhys,
		generation:   1,
		sysChunks: []chunkMapping{
			{logStart: 0, size: imageSize, physStart: 0},
		},
	}
}

// ── Tests: parseLeafItems truncated break ─────────────────────────────────

func TestParseLeafItems_TruncatedBreak(t *testing.T) {
	buf := make([]byte, testNodeSize)
	le := binary.LittleEndian
	// Claim 500 items but the buffer only fits ~159 (nodeHdrSize+n*itemSize <= 4096)
	le.PutUint32(buf[0x60:], 500)
	items := parseLeafItems(buf, 500)
	// Should stop early due to buffer bounds
	if len(items) >= 500 {
		t.Fatalf("expected fewer than 500 items, got %d", len(items))
	}
}

// ── Tests: leafItem.data out of range ─────────────────────────────────────

func TestItemData_OutOfRange(t *testing.T) {
	it := leafItem{
		dataOff:  uint32(testNodeSize), // past end of buffer
		dataSize: 10,
	}
	buf := make([]byte, testNodeSize)
	if it.data(buf) != nil {
		t.Fatal("expected nil for out-of-range data")
	}
}

// ── Tests: leafInsertItem ─────────────────────────────────────────────────

func makeEmptyLeaf() []byte {
	buf := make([]byte, testNodeSize)
	le := binary.LittleEndian
	le.PutUint64(buf[0x30:], 0x1000)
	le.PutUint64(buf[0x38:], 1) // flags = leaf
	le.PutUint32(buf[0x60:], 0)
	buf[0x64] = 0
	return buf
}

func TestLeafInsertItem_Basic(t *testing.T) {
	buf := makeEmptyLeaf()
	data := []byte("hello")
	k := key{objID: 10, typ: 1, offset: 0}
	if err := leafInsertItem(buf, k, data); err != nil {
		t.Fatalf("insert: %v", err)
	}
	hdr := parseNodeHeader(buf)
	if hdr.nItems != 1 {
		t.Fatalf("expected 1 item, got %d", hdr.nItems)
	}
	items := parseLeafItems(buf, 1)
	if !bytes.Equal(items[0].data(buf), data) {
		t.Fatalf("data mismatch: got %q", items[0].data(buf))
	}
}

func TestLeafInsertItem_Full(t *testing.T) {
	buf := makeEmptyLeaf()
	// Fill the leaf
	bigData := make([]byte, testNodeSize/2)
	_ = leafInsertItem(buf, key{1, 1, 0}, bigData)
	// Now insert more than will fit
	err := leafInsertItem(buf, key{2, 1, 0}, bigData)
	if err == nil {
		t.Fatal("expected leaf full error")
	}
}

func TestLeafInsertItem_UnsortedInsertion(t *testing.T) {
	buf := makeEmptyLeaf()
	// Insert in reverse order; they should be sorted in the leaf
	_ = leafInsertItem(buf, key{20, 1, 0}, []byte("z"))
	_ = leafInsertItem(buf, key{5, 1, 0}, []byte("a"))
	_ = leafInsertItem(buf, key{10, 1, 0}, []byte("m"))
	items := parseLeafItems(buf, 3)
	if items[0].k.objID != 5 || items[1].k.objID != 10 || items[2].k.objID != 20 {
		t.Fatalf("items not sorted by objID: got %v %v %v",
			items[0].k.objID, items[1].k.objID, items[2].k.objID)
	}
}

// ── Tests: leafDeleteItem ─────────────────────────────────────────────────

func TestLeafDeleteItem_Basic(t *testing.T) {
	buf := makeEmptyLeaf()
	_ = leafInsertItem(buf, key{1, 1, 0}, []byte("abc"))
	leafDeleteItem(buf, 0)
	hdr := parseNodeHeader(buf)
	if hdr.nItems != 0 {
		t.Fatalf("expected 0 items after delete, got %d", hdr.nItems)
	}
}

func TestLeafDeleteItem_ShiftsDataOffsets(t *testing.T) {
	buf := makeEmptyLeaf()
	_ = leafInsertItem(buf, key{1, 1, 0}, []byte("first"))
	_ = leafInsertItem(buf, key{2, 1, 0}, []byte("second"))
	_ = leafInsertItem(buf, key{3, 1, 0}, []byte("third"))
	// Delete middle
	leafDeleteItem(buf, 1)
	hdr := parseNodeHeader(buf)
	if hdr.nItems != 2 {
		t.Fatalf("expected 2 items, got %d", hdr.nItems)
	}
	items := parseLeafItems(buf, 2)
	if string(items[0].data(buf)) != "first" || string(items[1].data(buf)) != "third" {
		t.Fatalf("unexpected data after delete: %q %q",
			items[0].data(buf), items[1].data(buf))
	}
}

// ── Tests: leafReplaceItemData ────────────────────────────────────────────

func TestLeafReplaceItemData_SameSize(t *testing.T) {
	buf := makeEmptyLeaf()
	_ = leafInsertItem(buf, key{1, 1, 0}, []byte("hello"))
	if err := leafReplaceItemData(buf, 0, []byte("world")); err != nil {
		t.Fatalf("replace same size: %v", err)
	}
	items := parseLeafItems(buf, 1)
	if string(items[0].data(buf)) != "world" {
		t.Fatalf("got %q", items[0].data(buf))
	}
}

func TestLeafReplaceItemData_Grow(t *testing.T) {
	buf := makeEmptyLeaf()
	_ = leafInsertItem(buf, key{1, 1, 0}, []byte("hi"))
	if err := leafReplaceItemData(buf, 0, []byte("hello world")); err != nil {
		t.Fatalf("replace grow: %v", err)
	}
	items := parseLeafItems(buf, 1)
	if string(items[0].data(buf)) != "hello world" {
		t.Fatalf("got %q", items[0].data(buf))
	}
}

func TestLeafReplaceItemData_Shrink(t *testing.T) {
	buf := makeEmptyLeaf()
	_ = leafInsertItem(buf, key{1, 1, 0}, []byte("hello world"))
	if err := leafReplaceItemData(buf, 0, []byte("hi")); err != nil {
		t.Fatalf("replace shrink: %v", err)
	}
	items := parseLeafItems(buf, 1)
	if string(items[0].data(buf)) != "hi" {
		t.Fatalf("got %q", items[0].data(buf))
	}
}

func TestLeafReplaceItemData_MultipleItems(t *testing.T) {
	buf := makeEmptyLeaf()
	_ = leafInsertItem(buf, key{1, 1, 0}, []byte("aaa"))
	_ = leafInsertItem(buf, key{2, 1, 0}, []byte("bbb"))
	_ = leafInsertItem(buf, key{3, 1, 0}, []byte("ccc"))
	// Replace middle item with different size
	if err := leafReplaceItemData(buf, 1, []byte("b-expanded")); err != nil {
		t.Fatalf("replace: %v", err)
	}
	items := parseLeafItems(buf, 3)
	if string(items[0].data(buf)) != "aaa" {
		t.Fatalf("item[0] got %q", items[0].data(buf))
	}
	if string(items[1].data(buf)) != "b-expanded" {
		t.Fatalf("item[1] got %q", items[1].data(buf))
	}
	if string(items[2].data(buf)) != "ccc" {
		t.Fatalf("item[2] got %q", items[2].data(buf))
	}
}

func TestLeafReplaceItemData_NoSpace(t *testing.T) {
	buf := makeEmptyLeaf()
	// Fill leaf with large data (leaves only ~70 bytes free after header + descriptor)
	// nodeSize=4096, nodeHdrSize=101, itemSize=25: max data = 4096-101-25 = 3970
	// Use 3900 bytes: free after insert = 4096-101-25-3900 = 70
	large := make([]byte, 3900)
	if err := leafInsertItem(buf, key{1, 1, 0}, large); err != nil {
		t.Fatalf("insert setup: %v", err)
	}
	// Try to expand by testNodeSize bytes (delta=196 > free=70)
	huge := make([]byte, testNodeSize)
	err := leafReplaceItemData(buf, 0, huge)
	if err == nil {
		t.Fatal("expected no-space error")
	}
}

// ── Tests: leafUpdateItemSameSize ─────────────────────────────────────────

func TestLeafUpdateItemSameSize_SizeMismatch(t *testing.T) {
	buf := makeEmptyLeaf()
	_ = leafInsertItem(buf, key{1, 1, 0}, []byte("abc"))
	err := leafUpdateItemSameSize(buf, 0, []byte("abcd"))
	if err == nil {
		t.Fatal("expected size mismatch error")
	}
}

// ── Tests: findItemIdx ────────────────────────────────────────────────────

func TestFindItemIdx_NotFound(t *testing.T) {
	buf := makeEmptyLeaf()
	_ = leafInsertItem(buf, key{1, 1, 0}, []byte("data"))
	idx := findItemIdx(buf, 99, 1, 0)
	if idx != -1 {
		t.Fatalf("expected -1, got %d", idx)
	}
}

func TestFindItemIdx_Found(t *testing.T) {
	buf := makeEmptyLeaf()
	_ = leafInsertItem(buf, key{1, 1, 0}, []byte("data"))
	idx := findItemIdx(buf, 1, 1, 0)
	if idx != 0 {
		t.Fatalf("expected 0, got %d", idx)
	}
}

// ── Tests: searchTree ────────────────────────────────────────────────────

// buildTwoLevelTree creates an in-memory two-level B-tree:
//   - internal node at physInternal pointing to leaf at physLeaf
//   - leaf has one item: (wantObj, wantType, wantOffset) with data "testdata"
func buildTwoLevelTree(sb *superblock, physInternal, physLeaf int64, wantObj uint64, wantType uint8, wantOffset uint64) *rwaBuf {
	imgBuf := &rwaBuf{data: make([]byte, testImageSize)}
	le := binary.LittleEndian

	// Build leaf at physLeaf
	leaf := make([]byte, testNodeSize)
	le.PutUint64(leaf[0x30:], uint64(physLeaf))
	le.PutUint64(leaf[0x38:], 1) // leaf flags
	le.PutUint32(leaf[0x60:], 0)
	leaf[0x64] = 0
	_ = leafInsertItem(leaf, key{wantObj, wantType, wantOffset}, []byte("testdata"))
	updateNodeCRC(leaf)
	_, _ = imgBuf.WriteAt(leaf, physLeaf)

	// Build internal node at physInternal pointing to leaf
	internal := make([]byte, testNodeSize)
	le.PutUint64(internal[0x30:], uint64(physInternal))
	le.PutUint64(internal[0x38:], 2) // internal flags
	internal[0x64] = 1               // level = 1
	le.PutUint32(internal[0x60:], 1) // 1 key-pointer
	// key-ptr[0]: key=(wantObj, wantType, wantOffset), blockptr=physLeaf, gen=1
	off := nodeHdrSize
	le.PutUint64(internal[off:], wantObj)
	internal[off+8] = wantType
	le.PutUint64(internal[off+9:], wantOffset)
	le.PutUint64(internal[off+17:], uint64(physLeaf)) // blockptr
	le.PutUint64(internal[off+25:], 1)                // generation
	updateNodeCRC(internal)
	_, _ = imgBuf.WriteAt(internal, physInternal)

	return imgBuf
}

func TestSearchTree_InternalNodePath(t *testing.T) {
	physInternal := int64(testRootPhys)
	physLeaf := int64(testFsPhys)
	sb := buildMinimalSB(testNodeSize, testImageSize)
	imgBuf := buildTwoLevelTree(sb, physInternal, physLeaf, 42, 1, 0)

	_, it, err := searchTree(imgBuf, 0, sb, uint64(physInternal), 42, 1, 0)
	if err != nil {
		t.Fatalf("searchTree two-level: %v", err)
	}
	if it.k.objID != 42 {
		t.Fatalf("wrong objID: %d", it.k.objID)
	}
}

func TestSearchTree_InternalNodeChosen0(t *testing.T) {
	// Key in search is SMALLER than all keys in internal node => chosen stays 0 => ErrNotFound
	physInternal := int64(testRootPhys)
	physLeaf := int64(testFsPhys)
	sb := buildMinimalSB(testNodeSize, testImageSize)
	// Internal node key = (100, 1, 0), but we search for (1, 1, 0)
	imgBuf := buildTwoLevelTree(sb, physInternal, physLeaf, 100, 1, 0)

	_, _, err := searchTree(imgBuf, 0, sb, uint64(physInternal), 1, 1, 0)
	if err == nil {
		t.Fatal("expected ErrNotFound for key smaller than all internal keys")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestSearchTree_LeafNotFound(t *testing.T) {
	sb := buildMinimalSB(testNodeSize, testImageSize)
	imgBuf := &rwaBuf{data: buildTestImageBytes()}
	// Flat single-leaf tree; search for a key that doesn't exist
	_, _, err := searchTree(imgBuf, 0, sb, testFsPhys, 9999, 1, 0)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestSearchTree_InternalTruncatedBreak(t *testing.T) {
	// Internal node with nItems = 200 but buffer only fits ~3 => break out early
	sb := buildMinimalSB(testNodeSize, testImageSize)
	imgBuf := &rwaBuf{data: make([]byte, testImageSize)}
	le := binary.LittleEndian

	internal := make([]byte, testNodeSize)
	le.PutUint64(internal[0x30:], testRootPhys)
	le.PutUint64(internal[0x38:], 2)
	internal[0x64] = 1
	le.PutUint32(internal[0x60:], 200) // Too many to fit
	// Put a valid key-ptr at index 0 pointing to a non-existent leaf
	off := nodeHdrSize
	le.PutUint64(internal[off:], 5)
	internal[off+8] = 0
	le.PutUint64(internal[off+9:], 0)
	le.PutUint64(internal[off+17:], 0x999000) // blockptr to nowhere
	le.PutUint64(internal[off+25:], 1)
	updateNodeCRC(internal)
	_, _ = imgBuf.WriteAt(internal, testRootPhys)

	_, _, err := searchTree(imgBuf, 0, sb, testRootPhys, 5, 0, 0)
	// Will get "logical addr not in chunk" or read error
	if err == nil {
		t.Fatal("expected error traversing truncated internal node")
	}
}

// ── Tests: searchTreePrefix ──────────────────────────────────────────────

func TestSearchTreePrefix_LeafNotFound(t *testing.T) {
	sb := buildMinimalSB(testNodeSize, testImageSize)
	imgBuf := &rwaBuf{data: buildTestImageBytes()}
	_, _, err := searchTreePrefix(imgBuf, 0, sb, testFsPhys, 9999, 1)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestSearchTreePrefix_InternalNodePath(t *testing.T) {
	physInternal := int64(testRootPhys)
	physLeaf := int64(testFsPhys)
	sb := buildMinimalSB(testNodeSize, testImageSize)
	imgBuf := buildTwoLevelTree(sb, physInternal, physLeaf, 42, 1, 5)

	_, items, err := searchTreePrefix(imgBuf, 0, sb, uint64(physInternal), 42, 1)
	if err != nil {
		t.Fatalf("searchTreePrefix two-level: %v", err)
	}
	if len(items) != 1 || items[0].k.objID != 42 {
		t.Fatalf("unexpected items: %v", items)
	}
}

func TestSearchTreePrefix_InternalChosen0(t *testing.T) {
	// Search for objID smaller than all keys in internal => chosen stays 0 => NotFound
	physInternal := int64(testRootPhys)
	physLeaf := int64(testFsPhys)
	sb := buildMinimalSB(testNodeSize, testImageSize)
	imgBuf := buildTwoLevelTree(sb, physInternal, physLeaf, 100, 1, 0)
	_, _, err := searchTreePrefix(imgBuf, 0, sb, uint64(physInternal), 1, 1)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestSearchTreePrefix_InternalTruncatedBreak(t *testing.T) {
	sb := buildMinimalSB(testNodeSize, testImageSize)
	imgBuf := &rwaBuf{data: make([]byte, testImageSize)}
	le := binary.LittleEndian

	internal := make([]byte, testNodeSize)
	le.PutUint64(internal[0x30:], testRootPhys)
	le.PutUint64(internal[0x38:], 2)
	internal[0x64] = 1
	le.PutUint32(internal[0x60:], 200)
	off := nodeHdrSize
	le.PutUint64(internal[off:], 3)
	internal[off+8] = 0
	le.PutUint64(internal[off+9:], 0)
	le.PutUint64(internal[off+17:], 0x999000)
	le.PutUint64(internal[off+25:], 1)
	updateNodeCRC(internal)
	_, _ = imgBuf.WriteAt(internal, testRootPhys)

	_, _, err := searchTreePrefix(imgBuf, 0, sb, testRootPhys, 3, 0)
	if err == nil {
		t.Fatal("expected error traversing truncated internal node")
	}
}

// ── Tests: readNode error ─────────────────────────────────────────────────

func TestReadNode_UnmappedLogAddr(t *testing.T) {
	sb := buildMinimalSB(testNodeSize, testImageSize)
	// Use a log addr far out of the chunk map range
	_, err := readNode(&rwaBuf{data: make([]byte, testImageSize)}, 0, sb, 0x9999_0000)
	if err == nil {
		t.Fatal("expected unmapped-log-addr error")
	}
}

// ── Tests: cowCtx.writeNodeAtPhys ─────────────────────────────────────────

func TestCowCtx_WriteNodeAtPhys_OK(t *testing.T) {
	buf := make([]byte, testNodeSize)
	inner := &rwaBuf{data: make([]byte, testImageSize)}
	ctx := &cowCtx{rwa: inner, sb: buildMinimalSB(testNodeSize, testImageSize)}
	if err := ctx.writeNodeAtPhys(0, buf); err != nil {
		t.Fatalf("writeNodeAtPhys: %v", err)
	}
}

func TestCowCtx_WriteNodeAtPhys_Error(t *testing.T) {
	buf := make([]byte, testNodeSize)
	fail := &failWriterAt{inner: &rwaBuf{data: make([]byte, testImageSize)}, err: errors.New("disk full")}
	ctx := &cowCtx{rwa: fail, sb: buildMinimalSB(testNodeSize, testImageSize)}
	if err := ctx.writeNodeAtPhys(0, buf); err == nil {
		t.Fatal("expected write error")
	}
}

// ── Tests: cowCtx.cowNode ─────────────────────────────────────────────────

func TestCowCtx_CowNode_OK(t *testing.T) {
	imgData := buildTestImageBytes()
	inner := &rwaBuf{data: imgData}
	sb := buildMinimalSB(testNodeSize, testImageSize)
	sm := &spaceManager{
		nodeSize: testNodeSize,
		freeExts: []freeExtent{{physStart: 0x030000, size: testNodeSize * 10}},
	}
	ctx := &cowCtx{rwa: inner, sb: sb, sm: sm, generation: 2}

	newLog, newPhys, newBuf, err := ctx.cowNode(testFsPhys)
	if err != nil {
		t.Fatalf("cowNode: %v", err)
	}
	if newLog == 0 || newPhys == 0 || len(newBuf) == 0 {
		t.Fatal("unexpected zero return values")
	}
}

func TestCowCtx_CowNode_BadLogAddr(t *testing.T) {
	inner := &rwaBuf{data: make([]byte, testImageSize)}
	// Use a log addr outside the sys_chunk_array mapping
	badSB := &superblock{
		nodeSize:   testNodeSize,
		sectorSize: testNodeSize,
		sysChunks:  []chunkMapping{{logStart: 0x100000, size: 0x1000, physStart: 0x100000}},
	}
	sm := &spaceManager{nodeSize: testNodeSize, freeExts: []freeExtent{{physStart: 0x030000, size: 0x10000}}}
	ctx := &cowCtx{rwa: inner, sb: badSB, sm: sm}
	// 0x999000 is outside the chunk
	_, _, _, err := ctx.cowNode(0x999000)
	if err == nil {
		t.Fatal("expected error for unmapped log addr")
	}
}

func TestCowCtx_CowNode_ReadError(t *testing.T) {
	inner := &rwaBuf{data: make([]byte, testImageSize)}
	sb := buildMinimalSB(testNodeSize, testImageSize)
	diskErr := errors.New("disk IO error")
	failRdr := &failReaderAt{inner: inner, failAt: testFsPhys, err: diskErr}
	sm := &spaceManager{nodeSize: testNodeSize, freeExts: []freeExtent{{physStart: 0x030000, size: 0x10000}}}
	ctx := &cowCtx{rwa: failRdr, sb: sb, sm: sm}
	_, _, _, err := ctx.cowNode(testFsPhys)
	if err == nil {
		t.Fatal("expected read error")
	}
}

func TestCowCtx_CowNode_AllocError(t *testing.T) {
	imgData := buildTestImageBytes()
	inner := &rwaBuf{data: imgData}
	sb := buildMinimalSB(testNodeSize, testImageSize)
	sm := &spaceManager{nodeSize: testNodeSize} // no free extents
	ctx := &cowCtx{rwa: inner, sb: sb, sm: sm}
	_, _, _, err := ctx.cowNode(testFsPhys)
	if err == nil {
		t.Fatal("expected alloc error")
	}
}

// ── Tests: tracePath ─────────────────────────────────────────────────────

func TestTracePath_ReadNodeError(t *testing.T) {
	sb := buildMinimalSB(testNodeSize, testImageSize)
	// Use an image where the log addr is not accessible
	_, err := tracePath(&rwaBuf{data: make([]byte, testImageSize)}, 0, sb, 0x999000, key{})
	if err == nil {
		t.Fatal("expected error from tracePath with unmapped addr")
	}
}

func TestTracePath_SingleLeaf(t *testing.T) {
	sb := buildMinimalSB(testNodeSize, testImageSize)
	imgBuf := &rwaBuf{data: buildTestImageBytes()}
	path, err := tracePath(imgBuf, 0, sb, testFsPhys, key{})
	if err != nil {
		t.Fatalf("tracePath: %v", err)
	}
	if len(path) != 1 {
		t.Fatalf("expected 1 path entry, got %d", len(path))
	}
	if path[0].level != 0 {
		t.Fatalf("expected leaf level 0, got %d", path[0].level)
	}
}

// ── Tests: cowMutate ─────────────────────────────────────────────────────

// buildSmImageWithFreespace returns a rwaBuf image + superblock + spaceManager
// ready for cowMutate calls, with free extents for node allocation.
func buildCowCtxFromImage() (*rwaBuf, *superblock, *spaceManager) {
	img := buildTestImageBytes()
	rwa := &rwaBuf{data: img}
	sb := buildMinimalSB(testNodeSize, testImageSize)
	// Reserves: image has test nodes at testChunkPhys, testRootPhys, testFsPhys
	// Free starting at 0x030000
	sm := &spaceManager{
		nodeSize: testNodeSize,
		freeExts: []freeExtent{{physStart: 0x030000, size: testImageSize - 0x030000}},
	}
	return rwa, sb, sm
}

func TestCowMutate_FnError(t *testing.T) {
	rwa, sb, sm := buildCowCtxFromImage()
	fnErr := errors.New("fn deliberate error")
	_, err := cowMutate(rwa, 0, sb, sm, testFsPhys, key{}, func(leaf []byte) error {
		return fnErr
	})
	if !errors.Is(err, fnErr) {
		t.Fatalf("expected fn error, got %v", err)
	}
}

func TestCowMutate_NoSpace(t *testing.T) {
	img := buildTestImageBytes()
	rwa := &rwaBuf{data: img}
	sb := buildMinimalSB(testNodeSize, testImageSize)
	sm := &spaceManager{nodeSize: testNodeSize} // no free extents
	_, err := cowMutate(rwa, 0, sb, sm, testFsPhys, key{}, func(leaf []byte) error { return nil })
	if err == nil {
		t.Fatal("expected alloc error with no free space")
	}
}

func TestCowMutate_WriteLeafFails(t *testing.T) {
	img := buildTestImageBytes()
	inner := &rwaBuf{data: img}
	sb := buildMinimalSB(testNodeSize, testImageSize)
	sm := &spaceManager{
		nodeSize: testNodeSize,
		freeExts: []freeExtent{{physStart: 0x030000, size: testImageSize - 0x030000}},
	}
	diskErr := errors.New("disk write failure")
	failRwa := &failWriterAt{inner: inner, err: diskErr}
	_, err := cowMutate(failRwa, 0, sb, sm, testFsPhys, key{}, func(leaf []byte) error { return nil })
	if err == nil {
		t.Fatal("expected write error for leaf")
	}
}

func TestCowMutate_TracePathError(t *testing.T) {
	sb := buildMinimalSB(testNodeSize, testImageSize)
	sm := &spaceManager{nodeSize: testNodeSize, freeExts: []freeExtent{{physStart: 0x030000, size: 0x10000}}}
	// Pass rootLogAddr that is unmapped
	_, err := cowMutate(&rwaBuf{data: make([]byte, testImageSize)}, 0, sb, sm, 0x999000,
		key{}, func(leaf []byte) error { return nil })
	if err == nil {
		t.Fatal("expected tracePath error for unmapped root")
	}
}

func TestCowMutate_WithTwoLevelTree_WriteInternal(t *testing.T) {
	physInternal := int64(testRootPhys)
	physLeaf := int64(testFsPhys)
	sb := buildMinimalSB(testNodeSize, testImageSize)
	imgBuf := buildTwoLevelTree(sb, physInternal, physLeaf, 42, 1, 5)

	sm := &spaceManager{
		nodeSize: testNodeSize,
		freeExts: []freeExtent{{physStart: 0x030000, size: testImageSize - 0x030000}},
	}

	// This exercises the internal node path in cowMutate
	newRoot, err := cowMutate(imgBuf, 0, sb, sm, uint64(physInternal),
		key{}, func(leaf []byte) error { return nil })
	if err != nil {
		t.Fatalf("cowMutate two-level: %v", err)
	}
	if newRoot == 0 {
		t.Fatal("expected non-zero new root")
	}
}

func TestCowMutate_WriteInternalFails(t *testing.T) {
	physInternal := int64(testRootPhys)
	physLeaf := int64(testFsPhys)
	sb := buildMinimalSB(testNodeSize, testImageSize)
	inner := buildTwoLevelTree(sb, physInternal, physLeaf, 42, 1, 5)

	sm := &spaceManager{
		nodeSize: testNodeSize,
		freeExts: []freeExtent{{physStart: 0x030000, size: testImageSize - 0x030000}},
	}
	diskErr := errors.New("disk error on internal")
	// Fail after 1st write (leaf write succeeds, internal write fails)
	failRwa := &failAfterWriter{inner: inner, failAfter: 1, err: diskErr}

	_, err := cowMutate(failRwa, 0, sb, sm, uint64(physInternal),
		key{}, func(leaf []byte) error { return nil })
	if err == nil {
		t.Fatal("expected write error for internal node")
	}
}

// ── Tests: cowUpdate / cowDelete ─────────────────────────────────────────

func TestCowUpdate_KeyNotFound(t *testing.T) {
	rwa, sb, sm := buildCowCtxFromImage()
	_, err := cowUpdate(nil, rwa, 0, sb, sm, testFsPhys,
		key{9999, 1, 0}, []byte("data"))
	if err == nil {
		t.Fatal("expected key-not-found error from cowUpdate")
	}
}

func TestCowDelete_KeyNotFound(t *testing.T) {
	rwa, sb, sm := buildCowCtxFromImage()
	_, err := cowDelete(nil, rwa, 0, sb, sm, testFsPhys,
		key{9999, 1, 0})
	if err == nil {
		t.Fatal("expected key-not-found error from cowDelete")
	}
}

// ── Tests: physToLog / physFromLog ────────────────────────────────────────

func TestPhysToLog_Mapped(t *testing.T) {
	sb := buildMinimalSB(testNodeSize, testImageSize)
	log := physToLog(sb, testFsPhys) // testFsPhys = 0x022000, logStart=0 => log==phys
	if log != testFsPhys {
		t.Fatalf("expected %#x, got %#x", testFsPhys, log)
	}
}

func TestPhysToLog_Fallback(t *testing.T) {
	// sb with no matching chunk => falls back to phys
	sb := &superblock{
		nodeSize: testNodeSize,
		sysChunks: []chunkMapping{
			{logStart: 0xAA0000, size: 0x1000, physStart: 0xBB0000},
		},
	}
	phys := uint64(0x123456)
	log := physToLog(sb, phys)
	if log != phys {
		t.Fatalf("fallback: expected log=phys=%#x, got %#x", phys, log)
	}
}

func TestPhysFromLog_Mapped(t *testing.T) {
	sb := buildMinimalSB(testNodeSize, testImageSize)
	phys := physFromLog(sb, testFsPhys)
	if phys != testFsPhys {
		t.Fatalf("expected %#x, got %#x", testFsPhys, phys)
	}
}

func TestPhysFromLog_Fallback(t *testing.T) {
	sb := &superblock{
		nodeSize: testNodeSize,
		sysChunks: []chunkMapping{
			{logStart: 0xAA0000, size: 0x1000, physStart: 0xBB0000},
		},
	}
	logAddr := uint64(0x123456)
	phys := physFromLog(sb, logAddr)
	if phys != logAddr {
		t.Fatalf("fallback: expected phys=log=%#x, got %#x", logAddr, phys)
	}
}

// ── Tests: misc helper functions ─────────────────────────────────────────

func TestIsNotFoundErr(t *testing.T) {
	if !isNotFoundErr(ErrNotFound) {
		t.Fatal("expected true for ErrNotFound")
	}
	if isNotFoundErr(nil) {
		t.Fatal("expected false for nil")
	}
	if isNotFoundErr(errors.New("other")) {
		t.Fatal("expected false for other error")
	}
}

func TestSortExtentItems(t *testing.T) {
	items := []leafItem{
		{k: key{objID: 1, typ: typeDirIndex, offset: 20}},
		{k: key{objID: 1, typ: typeDirIndex, offset: 5}},
		{k: key{objID: 1, typ: typeDirIndex, offset: 12}},
	}
	sortExtentItems(items)
	if items[0].k.offset != 5 || items[1].k.offset != 12 || items[2].k.offset != 20 {
		t.Fatalf("sortExtentItems: got offsets %d %d %d", items[0].k.offset, items[1].k.offset, items[2].k.offset)
	}
}

func TestHashDirName(t *testing.T) {
	h := hashDirName("hello")
	if h == 0 {
		t.Fatal("expected non-zero hash")
	}
	// Same input should produce same hash
	if hashDirName("hello") != h {
		t.Fatal("hash not deterministic")
	}
	// Different inputs should produce different hashes
	if hashDirName("world") == h {
		t.Fatal("collision: hello and world have same hash")
	}
}
