// Package-internal tests – error injection and edge-case coverage.
package filesystem_btrfs

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// ── Open: reopen after large write (exercises reserveDataExtents) ─────────

func TestOpen_ReopenAfterLargeWrite(t *testing.T) {
	p := buildTestImageFile(t)

	// First open: write a large file creating regular physical extents
	fs, err := Open(p, 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	largeData := make([]byte, testNodeSize*3)
	for i := range largeData {
		largeData[i] = byte(i & 0xFF)
	}
	if err := fs.WriteFile("/large.dat", largeData, 0o644); err != nil {
		t.Fatalf("WriteFile large: %v", err)
	}
	fs.Close()

	// Second open: buildSpaceManager finds regular extents → covers reserveDataExtents
	fs2, err := Open(p, 0)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer fs2.Close()

	got, err := fs2.ReadFile("/large.dat")
	if err != nil {
		t.Fatalf("ReadFile after reopen: %v", err)
	}
	if len(got) != len(largeData) {
		t.Fatalf("size mismatch: got %d, want %d", len(got), len(largeData))
	}
}

// ── Open: error paths ─────────────────────────────────────────────────────

func TestOpen_PartitionOffsetError(t *testing.T) {
	// Build a GPT image where gptPartOffset will fail (small entrySize)
	raw := make([]byte, 3*sectorSize+512)
	le := binary.LittleEndian

	// GPT magic at byte 512
	copy(raw[512:520], "EFI PART")
	le.PutUint64(raw[512+72:], 2)  // partEntryLBA
	le.PutUint32(raw[512+80:], 1)  // numParts
	le.PutUint32(raw[512+84:], 63) // entrySize < 128 → error

	p := filepath.Join(t.TempDir(), "bad_gpt.img")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Open(p, -1)
	if err == nil {
		t.Fatal("expected error for bad GPT entry size")
	}
}

// ── Open: buildSpaceManager error ─────────────────────────────────────────

func TestOpen_BuildSpaceManagerError(t *testing.T) {
	// Create a modified image where the FS leaf has a regular extent
	// pointing to a DISK_BYTENR that is logically valid BUT the image
	// doesn't have corresponding data bytes at that location.
	// buildSpaceManager calls reserveDataExtents which calls walkLeaves.
	// We make the image have a regular extent pointing to an unmapped
	// logical address. buildSpaceManager will just continue on logToPhys err,
	// so it won't error from that alone.
	//
	// Actually, buildSpaceManager only errors if reserveDataExtents returns
	// a non-nil error, which only happens when walkLeaves itself errors.
	// walkLeaves errors when readNode fails for the root.
	//
	// To trigger buildSpaceManager error: use an image where the FS leaf
	// is at an unmapped logical address.
	img := buildTestImageBytes()
	le := binary.LittleEndian

	// Break the ROOT_ITEM so that fsTreeRoot points to 0x999000 (unmapped)
	rootLeaf := img[testRootPhys : testRootPhys+testNodeSize]
	items := parseLeafItems(rootLeaf, parseNodeHeader(rootLeaf).nItems)
	for _, it := range items {
		if it.k.objID == fsTreeObjID && it.k.typ == typeRootItem {
			// bytenr lives at offset 0xB0 in btrfs_root_item.
			d := it.data(rootLeaf)
			le.PutUint64(d[0xB0:0xB8], 0x999000) // unmapped address
			break
		}
	}
	updateNodeCRC(rootLeaf)

	p := filepath.Join(t.TempDir(), "bad_fstree.img")
	if err := os.WriteFile(p, img, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Open(p, 0)
	if err == nil {
		t.Fatal("expected error when fs tree root is at unmapped address")
	}
}

// ── readFileData edge paths ───────────────────────────────────────────────

// buildLeafWithExtents creates an in-memory image with a custom FS leaf.
// The caller provides a list of item insertions to perform.
func buildSingleLeafImage(t *testing.T, setupFn func(leafBuf []byte)) (*rwaBuf, *superblock) {
	t.Helper()
	sb := buildMinimalSB(testNodeSize, testImageSize)
	imgBuf := &rwaBuf{data: make([]byte, testImageSize)}
	le := binary.LittleEndian

	leaf := makeEmptyLeaf()
	le.PutUint64(leaf[0x30:], testFsPhys)
	setupFn(leaf)
	updateNodeCRC(leaf)
	_, _ = imgBuf.WriteAt(leaf, testFsPhys)
	return imgBuf, sb
}

func TestReadFileData_ShortExtentData(t *testing.T) {
	// Extent item with data shorter than extDataOffType (0x14)
	const inoNum = uint64(500)
	const fileSize = uint64(10)

	imgBuf, sb := buildSingleLeafImage(t, func(leaf []byte) {
		inodeBuf := make([]byte, inodeItemSize)
		le := binary.LittleEndian
		le.PutUint64(inodeBuf[inodeOffSize:], fileSize)
		le.PutUint32(inodeBuf[inodeOffMode:], 0x81A4)
		_ = leafInsertItem(leaf, key{inoNum, typeInodeItem, 0}, inodeBuf)
		// Insert a very short EXTENT_DATA item (< extDataOffType bytes)
		_ = leafInsertItem(leaf, key{inoNum, typeExtentData, 0}, []byte("short"))
	})

	in := &inodeItem{num: inoNum, size: fileSize, mode: 0x8000}
	got, err := readFileData(imgBuf, 0, sb, testFsPhys, in)
	if err != nil {
		t.Fatalf("readFileData: %v", err)
	}
	// Short extent data is skipped, result is zero-filled to fileSize
	if len(got) != int(fileSize) {
		t.Fatalf("expected %d bytes, got %d", fileSize, len(got))
	}
}

func TestReadFileData_SparseExtentExternal(t *testing.T) {
	// Regular extent with diskBytenr=0 → sparse → all zeros
	const inoNum = uint64(501)
	const fileSize = uint64(4096)

	imgBuf, sb := buildSingleLeafImage(t, func(leaf []byte) {
		le := binary.LittleEndian
		inodeBuf := make([]byte, inodeItemSize)
		le.PutUint64(inodeBuf[inodeOffSize:], fileSize)
		le.PutUint32(inodeBuf[inodeOffMode:], 0x81A4)
		_ = leafInsertItem(leaf, key{inoNum, typeInodeItem, 0}, inodeBuf)

		// Regular extent with diskBytenr=0
		extBuf := make([]byte, extDataRegularSize)
		le.PutUint64(extBuf[0x00:], 1)        // generation
		le.PutUint64(extBuf[0x08:], fileSize) // ram_bytes
		extBuf[0x14] = extentDataRegular      // type
		le.PutUint64(extBuf[0x15:], 0)        // diskBytenr = 0 → sparse
		le.PutUint64(extBuf[0x1D:], fileSize) // diskNumBytes
		le.PutUint64(extBuf[0x25:], 0)        // fileOffset
		le.PutUint64(extBuf[0x2D:], fileSize) // numBytes
		_ = leafInsertItem(leaf, key{inoNum, typeExtentData, 0}, extBuf)
	})

	in := &inodeItem{num: inoNum, size: fileSize, mode: 0x8000}
	got, err := readFileData(imgBuf, 0, sb, testFsPhys, in)
	if err != nil {
		t.Fatalf("readFileData sparse: %v", err)
	}
	if len(got) != int(fileSize) {
		t.Fatalf("expected %d bytes, got %d", fileSize, len(got))
	}
	for i, b := range got {
		if b != 0 {
			t.Fatalf("expected zero at byte %d, got %d", i, b)
		}
	}
}

func TestReadFileData_ShortRegularExtent(t *testing.T) {
	// Regular extent with too-short data (< extDataRegularSize) → skipped
	const inoNum = uint64(502)
	const fileSize = uint64(4096)

	imgBuf, sb := buildSingleLeafImage(t, func(leaf []byte) {
		le := binary.LittleEndian
		inodeBuf := make([]byte, inodeItemSize)
		le.PutUint64(inodeBuf[inodeOffSize:], fileSize)
		le.PutUint32(inodeBuf[inodeOffMode:], 0x81A4)
		_ = leafInsertItem(leaf, key{inoNum, typeInodeItem, 0}, inodeBuf)

		// Regular extent with insufficient header size → skipped (< extDataRegularSize)
		extBuf := make([]byte, extDataHdrSize+5) // 0x1A bytes, less than extDataRegularSize (0x35)
		extBuf[extDataOffType] = extentDataRegular
		_ = leafInsertItem(leaf, key{inoNum, typeExtentData, 0}, extBuf)
	})

	in := &inodeItem{num: inoNum, size: fileSize, mode: 0x8000}
	got, err := readFileData(imgBuf, 0, sb, testFsPhys, in)
	if err != nil {
		t.Fatalf("readFileData short-regular: %v", err)
	}
	_ = got
}

func TestReadFileData_RegularExtentPhysAddrError(t *testing.T) {
	// Regular extent with diskBytenr pointing to an unmapped logical address
	const inoNum = uint64(503)
	const fileSize = uint64(4096)

	sb := &superblock{
		nodeSize:   testNodeSize,
		sectorSize: testNodeSize,
		generation: 1,
		// Only map [0, testImageSize/2)
		sysChunks: []chunkMapping{{logStart: 0, size: testImageSize / 2, physStart: 0}},
	}
	imgBuf := &rwaBuf{data: make([]byte, testImageSize)}
	le := binary.LittleEndian

	leaf := makeEmptyLeaf()
	le.PutUint64(leaf[0x30:], testFsPhys)

	inodeBuf := make([]byte, inodeItemSize)
	le.PutUint64(inodeBuf[inodeOffSize:], fileSize)
	le.PutUint32(inodeBuf[inodeOffMode:], 0x81A4)
	_ = leafInsertItem(leaf, key{inoNum, typeInodeItem, 0}, inodeBuf)

	// Regular extent pointing to unmapped diskBytenr (0x999000 > testImageSize/2)
	extBuf := make([]byte, extDataRegularSize)
	le.PutUint64(extBuf[0x00:], 1)
	le.PutUint64(extBuf[0x08:], fileSize)
	extBuf[0x14] = extentDataRegular
	le.PutUint64(extBuf[0x15:], 0x999000) // unmapped diskBytenr
	le.PutUint64(extBuf[0x1D:], fileSize)
	le.PutUint64(extBuf[0x2D:], fileSize)
	_ = leafInsertItem(leaf, key{inoNum, typeExtentData, 0}, extBuf)
	updateNodeCRC(leaf)
	_, _ = imgBuf.WriteAt(leaf, testFsPhys)

	in := &inodeItem{num: inoNum, size: fileSize, mode: 0x8000}
	_, err := readFileData(imgBuf, 0, sb, testFsPhys, in)
	if err == nil {
		t.Fatal("expected error for unmapped physAddr")
	}
}

func TestReadFileData_RegularExtentReadError(t *testing.T) {
	// Regular extent where ReadAt fails
	const inoNum = uint64(504)
	const fileSize = uint64(4096)
	const extPhysAddr = uint64(0x030000) // physical address for the data

	sb := buildMinimalSB(testNodeSize, testImageSize)
	inner := &rwaBuf{data: make([]byte, testImageSize)}
	le := binary.LittleEndian

	leaf := makeEmptyLeaf()
	le.PutUint64(leaf[0x30:], testFsPhys)

	inodeBuf := make([]byte, inodeItemSize)
	le.PutUint64(inodeBuf[inodeOffSize:], fileSize)
	le.PutUint32(inodeBuf[inodeOffMode:], 0x81A4)
	_ = leafInsertItem(leaf, key{inoNum, typeInodeItem, 0}, inodeBuf)

	// Regular extent with diskBytenr = logAddr for extPhysAddr (chunk maps 1:1)
	extBuf := make([]byte, extDataRegularSize)
	le.PutUint64(extBuf[0x00:], 1)
	le.PutUint64(extBuf[0x08:], fileSize)
	extBuf[0x14] = extentDataRegular
	le.PutUint64(extBuf[0x15:], extPhysAddr) // diskBytenr (log==phys in 1:1 map)
	le.PutUint64(extBuf[0x1D:], fileSize)
	le.PutUint64(extBuf[0x2D:], fileSize)
	_ = leafInsertItem(leaf, key{inoNum, typeExtentData, 0}, extBuf)
	updateNodeCRC(leaf)
	_, _ = inner.WriteAt(leaf, testFsPhys)

	// Now make ReadAt fail for the data extent
	diskErr := errors.New("disk read error on extent data")
	failRdr := &failReaderAt{inner: inner, failAt: int64(extPhysAddr), err: diskErr}

	in := &inodeItem{num: inoNum, size: fileSize, mode: 0x8000}
	_, err := readFileData(failRdr, 0, sb, testFsPhys, in)
	if err == nil {
		t.Fatal("expected read error for extent data")
	}
}

func TestReadFileData_SearchTreeError(t *testing.T) {
	// File with size>0 but no EXTENT_DATA items → searchTreePrefix returns error
	// readFileData returns error
	const inoNum = uint64(505)
	const fileSize = uint64(100)

	imgBuf, sb := buildSingleLeafImage(t, func(leaf []byte) {
		le := binary.LittleEndian
		inodeBuf := make([]byte, inodeItemSize)
		le.PutUint64(inodeBuf[inodeOffSize:], fileSize)
		le.PutUint32(inodeBuf[inodeOffMode:], 0x81A4)
		// Insert only INODE_ITEM, no EXTENT_DATA → searchTreePrefix returns error
		_ = leafInsertItem(leaf, key{inoNum, typeInodeItem, 0}, inodeBuf)
	})

	in := &inodeItem{num: inoNum, size: fileSize, mode: 0x8000}
	_, err := readFileData(imgBuf, 0, sb, testFsPhys, in)
	if err == nil {
		t.Fatal("expected error when file has no extent data items")
	}
}

func TestReadFileData_ExtentTruncated(t *testing.T) {
	// Test fileOffset+numBytes > in.size path (extent claims more than file size)
	const inoNum = uint64(506)
	const fileSize = uint64(10) // claimed file size
	const extPhysAddr = uint64(0x030000)

	sb := buildMinimalSB(testNodeSize, testImageSize)
	inner := &rwaBuf{data: make([]byte, testImageSize)}
	le := binary.LittleEndian

	leaf := makeEmptyLeaf()
	le.PutUint64(leaf[0x30:], testFsPhys)

	inodeBuf := make([]byte, inodeItemSize)
	le.PutUint64(inodeBuf[inodeOffSize:], fileSize)
	le.PutUint32(inodeBuf[inodeOffMode:], 0x81A4)
	_ = leafInsertItem(leaf, key{inoNum, typeInodeItem, 0}, inodeBuf)

	// Extent numBytes (4096) > remaining size (10) → truncation path
	extBuf := make([]byte, extDataRegularSize)
	le.PutUint64(extBuf[0x00:], 1)
	le.PutUint64(extBuf[0x08:], fileSize)
	extBuf[0x14] = extentDataRegular
	le.PutUint64(extBuf[0x15:], extPhysAddr)
	le.PutUint64(extBuf[0x1D:], 4096) // diskNumBytes
	le.PutUint64(extBuf[0x2D:], 4096) // numBytes > fileSize → truncated
	_ = leafInsertItem(leaf, key{inoNum, typeExtentData, 0}, extBuf)
	updateNodeCRC(leaf)
	_, _ = inner.WriteAt(leaf, testFsPhys)

	in := &inodeItem{num: inoNum, size: fileSize, mode: 0x8000}
	got, err := readFileData(inner, 0, sb, testFsPhys, in)
	if err != nil {
		t.Fatalf("readFileData truncated: %v", err)
	}
	if len(got) != int(fileSize) {
		t.Fatalf("expected %d bytes, got %d", fileSize, len(got))
	}
}

// ── readSymlink error ─────────────────────────────────────────────────────

func TestReadSymlink_ExtentError(t *testing.T) {
	// Symlink with size>0 but no EXTENT_DATA → readFileData errors → readSymlink errors
	const inoNum = uint64(507)
	const fileSize = uint64(9)

	imgBuf, sb := buildSingleLeafImage(t, func(leaf []byte) {
		le := binary.LittleEndian
		inodeBuf := make([]byte, inodeItemSize)
		le.PutUint64(inodeBuf[inodeOffSize:], fileSize)
		le.PutUint32(inodeBuf[inodeOffMode:], 0xA1FF)
		_ = leafInsertItem(leaf, key{inoNum, typeInodeItem, 0}, inodeBuf)
		// No EXTENT_DATA → readFileData will error
	})

	in := &inodeItem{num: inoNum, size: fileSize, mode: 0xA000}
	_, err := readSymlink(imgBuf, 0, sb, testFsPhys, in)
	if err == nil {
		t.Fatal("expected error for symlink with missing extent data")
	}
}

// ── readNode ReadAt error ─────────────────────────────────────────────────

func TestReadNode_ReadAtError(t *testing.T) {
	sb := buildMinimalSB(testNodeSize, testImageSize)
	inner := &rwaBuf{data: make([]byte, testImageSize)}
	diskErr := errors.New("io error")
	// physAddr for testFsPhys = partOff + physFromChunk(testFsPhys) = 0 + testFsPhys
	failRdr := &failReaderAt{inner: inner, failAt: testFsPhys, err: diskErr}
	_, err := readNode(failRdr, 0, sb, testFsPhys)
	if err == nil {
		t.Fatal("expected readNode ReadAt error")
	}
}

// ── parseDirItems / parseDirItemsAll truncation break ─────────────────────

func buildDirItemData(name string, childObjID uint64) []byte {
	nameBytes := []byte(name)
	buf := make([]byte, dirItemHdrSize+len(nameBytes))
	le := binary.LittleEndian
	le.PutUint64(buf[0:], childObjID)
	buf[0x08] = typeInodeItem
	le.PutUint16(buf[0x1B:], uint16(len(nameBytes)))
	buf[0x1D] = ftRegFile
	copy(buf[0x1E:], nameBytes)
	return buf
}

func TestParseDirItems_TruncatedName(t *testing.T) {
	// nameLen says 100 bytes but data only has 10 bytes after header
	data := make([]byte, dirItemHdrSize+10) // 30+10 = 40 bytes total
	le := binary.LittleEndian
	le.PutUint64(data[0:], 42)     // childObjID
	le.PutUint16(data[0x1B:], 100) // nameLen >> available data
	data[0x1D] = ftRegFile

	_, _, err := parseDirItems(data, "test-name")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for truncated name, got %v", err)
	}
}

func TestParseDirItemsAll_TruncatedName(t *testing.T) {
	// nameLen overflow → break
	data := make([]byte, dirItemHdrSize+5)
	le := binary.LittleEndian
	le.PutUint64(data[0:], 42)
	le.PutUint16(data[0x1B:], 500) // way too long
	data[0x1D] = ftRegFile

	entries := parseDirItemsAll(data)
	if len(entries) != 0 {
		t.Fatalf("expected no entries from truncated data, got %d", len(entries))
	}
}

// ── readDir empty dir (ErrNotFound path) ──────────────────────────────────

func TestReadDir_NoDirIndexItems(t *testing.T) {
	// readDir calls searchTreePrefix for typeDirIndex. If it returns ErrNotFound,
	// readDir returns nil, nil. Use a leaf with no typeDirIndex items.
	const dirIno = uint64(300)
	imgBuf, sb := buildSingleLeafImage(t, func(leaf []byte) {
		le := binary.LittleEndian
		inodeBuf := make([]byte, inodeItemSize)
		le.PutUint32(inodeBuf[inodeOffMode:], 0x41ED)
		_ = leafInsertItem(leaf, key{dirIno, typeInodeItem, 0}, inodeBuf)
		// no typeDirIndex items → searchTreePrefix returns ErrNotFound
	})

	entries, err := readDir(imgBuf, 0, sb, testFsPhys, dirIno)
	if err != nil {
		t.Fatalf("readDir: %v", err)
	}
	if entries != nil {
		t.Fatalf("expected nil entries, got %v", entries)
	}
}

// ── writeSuperblock error paths ────────────────────────────────────────────

func TestWriteSuperblock_ReadError(t *testing.T) {
	sb := buildMinimalSB(testNodeSize, testImageSize)
	inner := &rwaBuf{data: make([]byte, testImageSize)}
	diskErr := errors.New("superblock read error")
	// Fail ReadAt at the superblock offset (superblockOffset = 0x10000)
	failRdr := &failReaderAt{inner: inner, failAt: superblockOffset, err: diskErr}
	err := writeSuperblock(failRdr, 0, sb, testFsPhys)
	if err == nil {
		t.Fatal("expected error when superblock ReadAt fails")
	}
}

func TestWriteSuperblock_WriteError(t *testing.T) {
	// Build a valid superblock in the buffer, then fail on WriteAt
	img := buildTestImageBytes()
	inner := &rwaBuf{data: img}
	sb := buildMinimalSB(testNodeSize, testImageSize)
	diskErr := errors.New("superblock write error")
	// Fail WriteAt on the first call (writing the superblock)
	failWriter := &failAfterWriter{inner: inner, failAfter: 0, err: diskErr}
	err := writeSuperblock(failWriter, 0, sb, testFsPhys)
	if err == nil {
		t.Fatal("expected error when superblock WriteAt fails")
	}
}

// ── freeInodeExtents edges ────────────────────────────────────────────────

func TestFreeInodeExtents_SparseExtent(t *testing.T) {
	// Regular extent with diskBytenr=0 → skip (sparse)
	const inoNum = uint64(510)
	imgBuf, sb := buildSingleLeafImage(t, func(leaf []byte) {
		le := binary.LittleEndian
		extBuf := make([]byte, extDataRegularSize)
		le.PutUint64(extBuf[0x00:], 1)
		extBuf[0x14] = extentDataRegular
		le.PutUint64(extBuf[0x15:], 0) // diskBytenr=0 → sparse
		le.PutUint64(extBuf[0x1D:], 4096)
		_ = leafInsertItem(leaf, key{inoNum, typeExtentData, 0}, extBuf)
	})
	sm := &spaceManager{nodeSize: testNodeSize, freeExts: []freeExtent{{physStart: 0, size: testImageSize}}}
	prevLen := len(sm.freeExts)
	freeInodeExtents(imgBuf, 0, sb, sm, testFsPhys, inoNum)
	// Sparse extent → no freeRange call → freeExts unchanged
	if len(sm.freeExts) != prevLen {
		t.Fatalf("freeExts changed on sparse extent: %d → %d", prevLen, len(sm.freeExts))
	}
}

func TestFreeInodeExtents_UnmappedLogAddr(t *testing.T) {
	// Regular extent with diskBytenr outside chunk mapping → logToPhys err → continue
	const inoNum = uint64(511)
	sb := &superblock{
		nodeSize:   testNodeSize,
		sectorSize: testNodeSize,
		generation: 1,
		sysChunks:  []chunkMapping{{logStart: 0, size: testImageSize / 2, physStart: 0}},
	}
	imgBuf := &rwaBuf{data: make([]byte, testImageSize)}
	le := binary.LittleEndian

	leaf := makeEmptyLeaf()
	le.PutUint64(leaf[0x30:], testFsPhys)
	extBuf := make([]byte, extDataRegularSize)
	le.PutUint64(extBuf[0x00:], 1)
	extBuf[0x14] = extentDataRegular
	le.PutUint64(extBuf[0x15:], 0x999000) // out-of-range diskBytenr
	le.PutUint64(extBuf[0x1D:], 4096)
	_ = leafInsertItem(leaf, key{inoNum, typeExtentData, 0}, extBuf)
	updateNodeCRC(leaf)
	_, _ = imgBuf.WriteAt(leaf, testFsPhys)

	sm := &spaceManager{nodeSize: testNodeSize, freeExts: []freeExtent{{physStart: 0, size: testImageSize / 2}}}
	initialLen := len(sm.freeExts)
	freeInodeExtents(imgBuf, 0, sb, sm, testFsPhys, inoNum)
	// logToPhys fails for 0x999000 → continue → freeExts unchanged
	if len(sm.freeExts) != initialLen {
		t.Fatalf("freeExts changed unexpectedly: %d → %d", initialLen, len(sm.freeExts))
	}
}

func TestFreeInodeExtents_ShortExtentData(t *testing.T) {
	// Extent with too-short data → len(d) <= extDataOffType → continue
	const inoNum = uint64(512)
	imgBuf, sb := buildSingleLeafImage(t, func(leaf []byte) {
		// Very short extent data
		_ = leafInsertItem(leaf, key{inoNum, typeExtentData, 0}, []byte("hi"))
	})
	sm := &spaceManager{nodeSize: testNodeSize}
	freeInodeExtents(imgBuf, 0, sb, sm, testFsPhys, inoNum)
	// No panics
}

func TestFreeInodeExtents_ShortRegularExtent(t *testing.T) {
	// Extent data with type=regular but len < extDataRegularSize → continue
	const inoNum = uint64(513)
	imgBuf, sb := buildSingleLeafImage(t, func(leaf []byte) {
		extBuf := make([]byte, extDataHdrSize+5) // too short for extDataRegularSize
		extBuf[extDataOffType] = extentDataRegular
		_ = leafInsertItem(leaf, key{inoNum, typeExtentData, 0}, extBuf)
	})
	sm := &spaceManager{nodeSize: testNodeSize}
	freeInodeExtents(imgBuf, 0, sb, sm, testFsPhys, inoNum)
	// No panics
}

// ── lookupDirEntry fallback via DIR_ITEM ──────────────────────────────────

func TestLookupDirEntry_FallbackToDirItem(t *testing.T) {
	// dirIno has no typeDirIndex items but has typeDirItem items
	const dirIno = uint64(300)
	imgBuf, sb := buildSingleLeafImage(t, func(leaf []byte) {
		le := binary.LittleEndian
		// Insert a DIR_ITEM (not DIR_INDEX) entry for "myfile"
		dirItemBuf := buildDirItemData("myfile", 400)
		le.PutUint64(dirItemBuf[0:], 400)
		_ = leafInsertItem(leaf, key{dirIno, typeDirItem, hashDirName("myfile")}, dirItemBuf)
	})
	objID, ftype, err := lookupDirEntry(imgBuf, 0, sb, testFsPhys, dirIno, "myfile")
	if err != nil {
		t.Fatalf("lookupDirEntry fallback: %v", err)
	}
	if objID != 400 {
		t.Fatalf("expected objID=400, got %d", objID)
	}
	if ftype != ftRegFile {
		t.Fatalf("expected ftRegFile, got %d", ftype)
	}
}

func TestLookupDirEntry_NotFoundInDirItem(t *testing.T) {
	// dirIno has a DIR_ITEM for "other" but not "myfile"
	const dirIno = uint64(300)
	imgBuf, sb := buildSingleLeafImage(t, func(leaf []byte) {
		dirItemBuf := buildDirItemData("other", 400)
		_ = leafInsertItem(leaf, key{dirIno, typeDirItem, hashDirName("other")}, dirItemBuf)
	})
	_, _, err := lookupDirEntry(imgBuf, 0, sb, testFsPhys, dirIno, "myfile")
	if err == nil {
		t.Fatal("expected not-found error")
	}
}

// ── pathLookup readInode error after traversal ────────────────────────────

func TestPathLookup_ReadInodeError(t *testing.T) {
	// pathLookup traverses /subdir → finds childObjID → calls readInode → inode too short
	const dirIno = uint64(rootDirObjID)
	const childIno = uint64(300)

	imgBuf, sb := buildSingleLeafImage(t, func(leaf []byte) {
		le := binary.LittleEndian

		// Root dir inode
		rootInodeBuf := make([]byte, inodeItemSize)
		le.PutUint32(rootInodeBuf[inodeOffMode:], 0x41ED)
		le.PutUint32(rootInodeBuf[inodeOffNLink:], 1)
		_ = leafInsertItem(leaf, key{dirIno, typeInodeItem, 0}, rootInodeBuf)

		// DIR_INDEX entry for "subdir" → points to childIno
		dirItemBuf := buildDirItemData("subdir", childIno)
		dirItemBuf[0x1D] = ftDir
		le.PutUint64(dirItemBuf[0:], childIno)
		_ = leafInsertItem(leaf, key{dirIno, typeDirIndex, 3}, dirItemBuf)

		// Child inode with too-short data (< inodeItemSize)
		_ = leafInsertItem(leaf, key{childIno, typeInodeItem, 0}, []byte("short"))
	})

	_, err := pathLookup(imgBuf, 0, sb, testFsPhys, "/subdir")
	if err == nil {
		t.Fatal("expected error for invalid child inode")
	}
}

// ── resolveRootTree short ROOT_ITEM ──────────────────────────────────────

func TestResolveRootTree_ShortRootItem(t *testing.T) {
	// resolveRootTree uses sb.rootLogAddr → must write leaf at testRootPhys
	sb := buildMinimalSB(testNodeSize, testImageSize)
	imgBuf := &rwaBuf{data: make([]byte, testImageSize)}
	le := binary.LittleEndian

	leaf := makeEmptyLeaf()
	le.PutUint64(leaf[0x30:], testRootPhys) // leaf at root phys address
	// ROOT_ITEM for fsTreeObjID with only 4 bytes (< 8 required)
	_ = leafInsertItem(leaf, key{fsTreeObjID, typeRootItem, 0}, []byte{1, 2, 3, 4})
	updateNodeCRC(leaf)
	_, _ = imgBuf.WriteAt(leaf, testRootPhys)

	_, err := resolveRootTree(imgBuf, 0, sb)
	if err == nil {
		t.Fatal("expected error for short ROOT_ITEM")
	}
}

// ── btree_write.go: cowMutate internal node write fails ───────────────────

func TestCowMutate_AllocInternalNodeFails(t *testing.T) {
	// Only 1 free block: enough for the new leaf but NOT for the new internal node.
	// This covers btree_write.go 142-144 (allocNodeBlock on internal fails).
	physInternal := int64(testRootPhys)
	physLeaf := int64(testFsPhys)
	sb := buildMinimalSB(testNodeSize, testImageSize)
	inner := buildTwoLevelTree(sb, physInternal, physLeaf, 42, 1, 5)

	sm := &spaceManager{
		nodeSize: testNodeSize,
		// Exactly 1 free block: leaf gets it, internal page allocation fails.
		freeExts: []freeExtent{{physStart: 0x030000, size: uint64(testNodeSize)}},
	}

	_, err := cowMutate(inner, 0, sb, sm, uint64(physInternal),
		key{}, func(leaf []byte) error { return nil })
	if err == nil {
		t.Fatal("expected error: no space for internal node")
	}
}

// ── btree.go: searchTree/searchTreePrefix chosen==0 ────────────────────────

func TestSearchTree_ExactFirstKeyMatch(t *testing.T) {
	// Key that exactly equals the first (and only) key in internal node → chosen updates
	physInternal := int64(testRootPhys)
	physLeaf := int64(testFsPhys)
	sb := buildMinimalSB(testNodeSize, testImageSize)
	// All keys in internal node have objID=42; search for (42,1,5) exactly
	imgBuf := buildTwoLevelTree(sb, physInternal, physLeaf, 42, 1, 5)

	_, it, err := searchTree(imgBuf, 0, sb, uint64(physInternal), 42, 1, 5)
	if err != nil {
		t.Fatalf("searchTree exact match: %v", err)
	}
	if it.k.objID != 42 {
		t.Fatalf("wrong key: %v", it.k)
	}
}

// ── allocDataBytes exact-fit ──────────────────────────────────────────────

func TestAllocDataBytes_ExactFit(t *testing.T) {
	sm := &spaceManager{
		nodeSize: testNodeSize,
		freeExts: []freeExtent{{physStart: 0x1000, size: 512}}, // exactly 512 bytes
	}
	// Request exactly 512 bytes with 512-byte sectorSize
	phys, size, err := sm.allocDataBytes(512, 512)
	if err != nil {
		t.Fatalf("allocDataBytes exact fit: %v", err)
	}
	if phys != 0x1000 || size != 512 {
		t.Fatalf("unexpected result: phys=%#x size=%d", phys, size)
	}
	// Extent should be removed (exact fit)
	if len(sm.freeExts) != 0 {
		t.Fatalf("expected 0 free extents after exact fit, got %d", len(sm.freeExts))
	}
}

// ── remove: no-overlap path ──────────────────────────────────────────────

func TestRemove_NoOverlap(t *testing.T) {
	sm := &spaceManager{
		nodeSize: testNodeSize,
		freeExts: []freeExtent{
			{physStart: 0x1000, size: 0x1000},
			{physStart: 0x5000, size: 0x1000},
		},
	}
	// Remove a range that doesn't overlap with any free extent
	sm.remove(0x3000, 0x1000)
	if len(sm.freeExts) != 2 {
		t.Fatalf("expected 2 extents after non-overlapping remove, got %d", len(sm.freeExts))
	}
}

// ── writeFile: parent not found for bare filename ─────────────────────────

func TestWriteFile_BareFileName(t *testing.T) {
	// WriteFile with path="/file.txt" should work (parent=/)
	fs := openTestFS(t)
	if err := fs.WriteFile("file.txt", []byte("ok"), 0o644); err != nil {
		t.Fatalf("WriteFile bare: %v", err)
	}
}

// ── searchTree / searchTreePrefix: chosen==0 paths ───────────────────────

func TestSearchTreePrefix_ExactFirstKey(t *testing.T) {
	physInternal := int64(testRootPhys)
	physLeaf := int64(testFsPhys)
	sb := buildMinimalSB(testNodeSize, testImageSize)
	imgBuf := buildTwoLevelTree(sb, physInternal, physLeaf, 42, 1, 5)

	_, items, err := searchTreePrefix(imgBuf, 0, sb, uint64(physInternal), 42, 1)
	if err != nil {
		t.Fatalf("searchTreePrefix exact: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("expected items")
	}
}

// ── delete: error paths ───────────────────────────────────────────────────

func TestDeleteFile_DeleteFileNotRegular(t *testing.T) {
	// DeleteFile must reject directories — they have to go through
	// DeleteDir so the recursive cleanup runs. Symlinks are now accepted
	// by DeleteFile (Unix-style: unlink(2) handles regular files AND
	// symlinks), so the test target must be a directory we materialise.
	fs := openTestFS(t)
	if err := fs.MkDir("/test-dir", 0o755); err != nil {
		t.Fatalf("MkDir: %v", err)
	}
	if err := fs.DeleteFile("/test-dir"); err == nil {
		t.Fatal("expected error deleting a directory as a file")
	}
}

func TestDeleteFile_ParentNotFound(t *testing.T) {
	// deleteFile: parent dir lookup fails (covers lines 13-15 of delete.go)
	fs := openTestFS(t)
	err := fs.DeleteFile("/nonexistent_parent/file.txt")
	if err == nil {
		t.Fatal("expected error for nonexistent parent dir")
	}
}

func TestDeleteDir_DeleteFileNotDir(t *testing.T) {
	// deleteDir should error when target is a regular file
	fs := openTestFS(t)
	err := fs.DeleteDir("/hello.txt")
	if err == nil {
		t.Fatal("expected error deleting file as dir")
	}
}

func TestDeleteDir_ParentNotFound(t *testing.T) {
	// deleteDir: parent dir lookup fails (covers lines 31-33 of delete.go)
	fs := openTestFS(t)
	err := fs.DeleteDir("/nonexistent_parent/subdir")
	if err == nil {
		t.Fatal("expected error for nonexistent parent dir")
	}
}

// ── mkdir: error and deeper paths ────────────────────────────────────────

func TestMkDir_RootButNoSlash(t *testing.T) {
	// MkDir with just a name (no prefix slash) should still work
	fs := openTestFS(t)
	if err := fs.MkDir("newdir2", 0o755); err != nil {
		t.Fatalf("MkDir no-slash: %v", err)
	}
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Name() == "newdir2" {
			found = true
		}
	}
	if !found {
		t.Error("newdir2 not found after MkDir")
	}
}

// ── rename deeper paths ───────────────────────────────────────────────────

func TestRename_FileToNewDirPath(t *testing.T) {
	fs := openTestFS(t)
	_ = fs.MkDir("/subdir3", 0o755)
	if err := fs.Rename("/hello.txt", "/subdir3/hello.txt"); err != nil {
		t.Fatalf("Rename cross-dir: %v", err)
	}
	data, err := fs.ReadFile("/subdir3/hello.txt")
	if err != nil {
		t.Fatalf("ReadFile after cross-dir rename: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("got %q", data)
	}
}

func TestRename_OverwriteExistingLargeFile(t *testing.T) {
	fs := openTestFS(t)
	large := make([]byte, testNodeSize*2)
	_ = fs.WriteFile("/source.txt", large, 0o644)
	_ = fs.WriteFile("/dest.txt", []byte("old"), 0o644)
	if err := fs.Rename("/source.txt", "/dest.txt"); err != nil {
		t.Fatalf("Rename overwrite large: %v", err)
	}
	got, err := fs.ReadFile("/dest.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != len(large) {
		t.Fatalf("size mismatch: %d vs %d", len(got), len(large))
	}
}

// ── splitPath edge cases ──────────────────────────────────────────────────

func TestSplitPath_NoSlash(t *testing.T) {
	dir, name := splitPath("filename")
	if dir != "/" || name != "filename" {
		t.Fatalf("got dir=%q name=%q", dir, name)
	}
}

func TestSplitPath_Root(t *testing.T) {
	dir, name := splitPath("/")
	if dir != "/" || name != "" {
		t.Fatalf("got dir=%q name=%q", dir, name)
	}
}

// ── readSuperblock error (bad parseSysChunkArray) ─────────────────────────

func TestReadSuperblock_BadSysChunks(t *testing.T) {
	img := buildTestImageBytes()
	le := binary.LittleEndian

	// The superblock is at offset superblockOffset (0x10000) in the image.
	// Corrupt the sysChunkArr so parseSysChunkArray returns error:
	// put a key with type != 0xE4 → "unexpected key type" error.
	sbBase := int(superblockOffset) // 0x10000
	// Set arrSz to keySize (17): one entry to parse
	le.PutUint32(img[sbBase+sbfSysChunkArrSz:], uint32(keySize))
	// Clear the key bytes and set key type = 0x01 (not 0xE4)
	copy(img[sbBase+sbfSysChunkArr:sbBase+sbfSysChunkArr+keySize], make([]byte, keySize))
	img[sbBase+sbfSysChunkArr+8] = 0x01 // key type = 0x01 → error

	r := &rwaBuf{data: img}
	_, err := readSuperblock(r, 0)
	if err == nil {
		t.Fatal("expected error for bad sys_chunk_array key type")
	}
}

// ── walkNodeAddrs truncated internal break ────────────────────────────────

func TestWalkNodeAddrs_InternalTruncated(t *testing.T) {
	// Internal node claims 200 items but only 1 fits; walkNodeAddrs should process whatever fits
	sb := buildMinimalSB(testNodeSize, testImageSize)
	imgBuf := &rwaBuf{data: make([]byte, testImageSize)}
	le := binary.LittleEndian

	physLeaf := int64(testFsPhys)
	leaf := makeEmptyLeaf()
	le.PutUint64(leaf[0x30:], uint64(physLeaf))
	updateNodeCRC(leaf)
	_, _ = imgBuf.WriteAt(leaf, physLeaf)

	physInternal := int64(testRootPhys)
	internal := make([]byte, testNodeSize)
	le.PutUint64(internal[0x30:], uint64(physInternal))
	le.PutUint64(internal[0x38:], 2)
	internal[0x64] = 1                 // level = 1
	le.PutUint32(internal[0x60:], 200) // too many items to fit
	off := nodeHdrSize
	le.PutUint64(internal[off:], 42)
	internal[off+8] = 1
	le.PutUint64(internal[off+9:], 5)
	le.PutUint64(internal[off+17:], uint64(physLeaf))
	le.PutUint64(internal[off+25:], 1)
	updateNodeCRC(internal)
	_, _ = imgBuf.WriteAt(internal, physInternal)

	var visited []uint64
	err := walkNodeAddrs(imgBuf, 0, sb, uint64(physInternal), func(a uint64) error {
		visited = append(visited, a)
		return nil
	})
	if err != nil {
		t.Fatalf("walkNodeAddrs truncated internal: %v", err)
	}
	// Should visit internal + leaf(s) it can reach
	if len(visited) < 2 {
		t.Fatalf("expected >=2 visited, got %d", len(visited))
	}
}

// ── reserveTreeNodes empty-chunk path ─────────────────────────────────────

func TestReserveTreeNodes_UnmappedNode(t *testing.T) {
	// reserveTreeNodes calls walkNodeAddrs and then logToPhys.
	// If logToPhys fails (unmapped address), it just returns nil (ignore).
	sb := &superblock{
		nodeSize:   testNodeSize,
		sectorSize: testNodeSize,
		generation: 1,
		// Only map a small range, so logToPhys for out-of-range fails
		sysChunks: []chunkMapping{{logStart: testFsPhys, size: uint64(testNodeSize), physStart: testFsPhys}},
	}
	imgBuf := &rwaBuf{data: buildTestImageBytes()}
	sm := &spaceManager{nodeSize: testNodeSize}
	// reserveTreeNodes called for root tree root = testRootPhys which is OUTSIDE the chunk
	err := sm.reserveTreeNodes(imgBuf, 0, sb, testRootPhys)
	if err != nil {
		t.Fatalf("expected nil (ignore unmapped), got %v", err)
	}
}
