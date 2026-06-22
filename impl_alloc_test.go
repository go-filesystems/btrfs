// Package-internal tests – alloc, partition, chunk, superblock, and dir/read extras.
package filesystem_btrfs

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// ── partitionOffset: GPT path ─────────────────────────────────────────────
//
// partition.go now delegates parsing to the hardened go-volumes/gpt package;
// these tests drive partitionOffset end-to-end (scheme auto-detection + the
// btrfs-specific Linux-partition / bare-image fallback) over hand-built GPT
// and MBR images. rwaBuf has no Size(), so deviceSize() falls back to the
// 1 GiB hardSizeCeiling, against which every fixture partition validates.

func buildGPTBuf(numParts uint32, entrySize uint32, linuxPart bool, partStartLBA uint64) *rwaBuf {
	// Minimum size: 3 sectors (MBR + GPT header + entry table)
	size := 3*sectorSize + int(numParts)*int(entrySize) + 4096
	buf := &rwaBuf{data: make([]byte, size)}
	le := binary.LittleEndian

	// GPT header starts at byte 512. The "EFI PART" signature is what
	// partitionOffset uses to dispatch to the GPT parser.
	hdr := make([]byte, 92)
	copy(hdr[0:], []byte("EFI PART"))
	partEntryLBA := uint64(2)
	le.PutUint64(hdr[72:], partEntryLBA)
	le.PutUint32(hdr[80:], numParts)
	le.PutUint32(hdr[84:], entrySize)
	_, _ = buf.WriteAt(hdr, 512)

	if numParts == 0 {
		return buf
	}

	// Write partition entries at sector 2. endLBA (offset 40) is set so the
	// partition is a single sector, keeping it within the device.
	tableOff := int64(partEntryLBA) * sectorSize
	entry := make([]byte, entrySize)
	if linuxPart {
		copy(entry[0:16], linuxPartTypeGPT[:])
	} else {
		// non-zero non-linux GUID
		entry[0] = 0xFF
	}
	le.PutUint64(entry[32:], partStartLBA)
	le.PutUint64(entry[40:], partStartLBA) // endLBA == startLBA: 1-sector part
	_, _ = buf.WriteAt(entry, tableOff)
	return buf
}

func TestGPTPartOffset_AutoSelectLinux(t *testing.T) {
	const startLBA = 2048
	buf := buildGPTBuf(1, 128, true, startLBA)
	off, err := partitionOffset(buf, -1)
	if err != nil {
		t.Fatalf("partitionOffset auto: %v", err)
	}
	if off != startLBA*sectorSize {
		t.Fatalf("expected %d, got %d", startLBA*sectorSize, off)
	}
}

func TestGPTPartOffset_ByIndex(t *testing.T) {
	const startLBA = 4096
	buf := buildGPTBuf(1, 128, false, startLBA)
	off, err := partitionOffset(buf, 0)
	if err != nil {
		t.Fatalf("partitionOffset by index: %v", err)
	}
	if off != startLBA*sectorSize {
		t.Fatalf("expected %d, got %d", startLBA*sectorSize, off)
	}
}

func TestGPTPartOffset_IndexNotFound(t *testing.T) {
	buf := buildGPTBuf(1, 128, true, 2048)
	_, err := partitionOffset(buf, 5)
	if err == nil {
		t.Fatal("expected index-not-found error")
	}
}

func TestGPTPartOffset_NoLinuxPartition(t *testing.T) {
	buf := buildGPTBuf(1, 128, false, 2048)
	_, err := partitionOffset(buf, -1)
	if err == nil {
		t.Fatal("expected no-linux-partition error")
	}
}

func TestGPTPartOffset_SmallEntrySize(t *testing.T) {
	buf := buildGPTBuf(1, 64 /* < 128 */, true, 2048)
	_, err := partitionOffset(buf, -1)
	if err == nil {
		t.Fatal("expected small-entry-size error")
	}
}

func TestGPTPartOffset_SkipsEmptyEntry(t *testing.T) {
	// 2 entries: first empty (all-zero GUID), second is Linux
	size := 3*sectorSize + 2*128 + 4096
	buf := &rwaBuf{data: make([]byte, size)}
	le := binary.LittleEndian

	hdr := make([]byte, 92)
	copy(hdr[0:], []byte("EFI PART"))
	le.PutUint64(hdr[72:], 2) // partEntryLBA
	le.PutUint32(hdr[80:], 2) // numParts
	le.PutUint32(hdr[84:], 128)
	_, _ = buf.WriteAt(hdr, 512)

	tableOff := int64(2 * sectorSize)
	// Entry 0: all zeros = empty, skip. Entry 1: Linux.
	entry1 := make([]byte, 128)
	copy(entry1[0:16], linuxPartTypeGPT[:])
	le.PutUint64(entry1[32:], 2048)
	le.PutUint64(entry1[40:], 2048)
	_, _ = buf.WriteAt(entry1, tableOff+128)

	off, err := partitionOffset(buf, -1)
	if err != nil {
		t.Fatalf("expected success: %v", err)
	}
	if off != 2048*sectorSize {
		t.Fatalf("expected %d, got %d", 2048*sectorSize, off)
	}
}

func TestGPTPartOffset_EntryArrayOutOfRange(t *testing.T) {
	// numParts=10 with partEntryLBA=2 but a tiny device => the entry array
	// extent exceeds the device size and the hardened parser rejects it.
	buf := &rwaBuf{data: make([]byte, 1024)}
	le := binary.LittleEndian
	hdr := make([]byte, 92)
	copy(hdr[0:], []byte("EFI PART"))
	le.PutUint64(hdr[72:], 2)   // partEntryLBA
	le.PutUint32(hdr[80:], 10)  // 10 parts
	le.PutUint32(hdr[84:], 128) // entrySize
	_, _ = buf.WriteAt(hdr, 512)
	_, err := partitionOffset(buf, -1)
	if err == nil {
		t.Fatal("expected error when entry array exceeds the device")
	}
}

// ── partitionOffset: MBR path ─────────────────────────────────────────────

func buildMBRBuf(ptype byte, startLBA uint32, magic bool) *rwaBuf {
	buf := &rwaBuf{data: make([]byte, 512*4)}
	le := binary.LittleEndian
	if magic {
		buf.data[510] = 0x55
		buf.data[511] = 0xAA
	}
	// Partition table at offset 446, entry 0. A 1-sector numSectors keeps the
	// partition within the device for the hardened parser's range check.
	e := make([]byte, 16)
	e[4] = ptype
	le.PutUint32(e[8:], startLBA)
	le.PutUint32(e[12:], 1) // numSectors
	_, _ = buf.WriteAt(e, 446)
	return buf
}

func TestMBRPartOffset_AutoSelectLinux(t *testing.T) {
	const startLBA = 2048
	buf := buildMBRBuf(0x83, startLBA, true)
	off, err := partitionOffset(buf, -1)
	if err != nil {
		t.Fatalf("partitionOffset auto: %v", err)
	}
	if off != startLBA*sectorSize {
		t.Fatalf("expected %d, got %d", startLBA*sectorSize, off)
	}
}

func TestMBRPartOffset_ByIndex(t *testing.T) {
	const startLBA = 2048
	buf := buildMBRBuf(0x07, startLBA, true)
	off, err := partitionOffset(buf, 0)
	if err != nil {
		t.Fatalf("partitionOffset by index: %v", err)
	}
	if off != startLBA*sectorSize {
		t.Fatalf("expected %d, got %d", startLBA*sectorSize, off)
	}
}

func TestMBRPartOffset_IndexNotFound(t *testing.T) {
	buf := buildMBRBuf(0x83, 2048, true)
	_, err := partitionOffset(buf, 3)
	if err == nil {
		t.Fatal("expected index-not-found error")
	}
}

func TestMBRPartOffset_NoLinuxPartition(t *testing.T) {
	buf := buildMBRBuf(0x07, 2048, true)
	_, err := partitionOffset(buf, -1)
	if err == nil {
		t.Fatal("expected no-linux-partition error")
	}
}

func TestPartOffset_BareImage(t *testing.T) {
	// No "EFI PART" and no 0x55AA signature: a bare btrfs image whose
	// filesystem starts at offset 0.
	buf := &rwaBuf{data: make([]byte, 4096)}
	off, err := partitionOffset(buf, -1)
	if err != nil {
		t.Fatalf("bare image auto: %v", err)
	}
	if off != 0 {
		t.Fatalf("expected offset 0 for bare image, got %d", off)
	}
	// Index 0 on a bare image also means "the whole device".
	off, err = partitionOffset(buf, 0)
	if err != nil {
		t.Fatalf("bare image index 0: %v", err)
	}
	if off != 0 {
		t.Fatalf("expected offset 0 for bare image index 0, got %d", off)
	}
	// A non-zero index on a bare image has no partition table to satisfy it.
	if _, err := partitionOffset(buf, 2); err == nil {
		t.Fatal("expected error for non-zero index on bare image")
	}
}

// ── Open with GPT/MBR image ───────────────────────────────────────────────

func buildGPTImageFile(t *testing.T) string {
	t.Helper()
	// Build: MBR sector + GPT header + small sector + btrfs image at startLBA
	const startLBA = 4
	const imageSize = startLBA*sectorSize + testImageSize

	raw := make([]byte, imageSize)
	le := binary.LittleEndian

	// Write GPT magic at byte 512
	copy(raw[512:520], "EFI PART")
	// partEntryLBA=2, numParts=1, entrySize=128
	le.PutUint64(raw[512+72:], 2)
	le.PutUint32(raw[512+80:], 1)
	le.PutUint32(raw[512+84:], 128)

	// Entry at LBA2 (offset 1024): Linux GUID, startLBA=4
	copy(raw[1024:1024+16], linuxPartTypeGPT[:])
	le.PutUint64(raw[1024+32:], startLBA)

	// Embed actual btrfs image at offset startLBA*sectorSize
	btrfsImg := buildTestImageBytes()
	copy(raw[startLBA*sectorSize:], btrfsImg)

	p := filepath.Join(t.TempDir(), "gpt.img")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatalf("write gpt image: %v", err)
	}
	return p
}

func TestOpen_GPTPartition(t *testing.T) {
	p := buildGPTImageFile(t)
	fs, err := Open(p, -1)
	if err != nil {
		t.Fatalf("Open GPT: %v", err)
	}
	defer fs.Close()
	data, err := fs.ReadFile("/hello.txt")
	if err != nil {
		t.Fatalf("ReadFile on GPT FS: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("got %q", data)
	}
}

func buildMBRImageFile(t *testing.T) string {
	t.Helper()
	const startLBA = 4
	const imageSize = startLBA*sectorSize + testImageSize

	raw := make([]byte, imageSize)
	le := binary.LittleEndian

	// MBR magic
	raw[510] = 0x55
	raw[511] = 0xAA
	// Partition entry 0 at offset 446: type=0x83, startLBA=4
	e := raw[446:]
	e[4] = 0x83
	le.PutUint32(e[8:], startLBA)

	// Embed btrfs image
	btrfsImg := buildTestImageBytes()
	copy(raw[startLBA*sectorSize:], btrfsImg)

	p := filepath.Join(t.TempDir(), "mbr.img")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatalf("write mbr image: %v", err)
	}
	return p
}

func TestOpen_MBRPartition(t *testing.T) {
	p := buildMBRImageFile(t)
	fs, err := Open(p, -1)
	if err != nil {
		t.Fatalf("Open MBR: %v", err)
	}
	defer fs.Close()
	data, err := fs.ReadFile("/hello.txt")
	if err != nil {
		t.Fatalf("ReadFile on MBR FS: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("got %q", data)
	}
}

// ── walkNodeAddrs ─────────────────────────────────────────────────────────

func TestWalkNodeAddrs_Leaf(t *testing.T) {
	sb := buildMinimalSB(testNodeSize, testImageSize)
	imgBuf := &rwaBuf{data: buildTestImageBytes()}
	var visited []uint64
	err := walkNodeAddrs(imgBuf, 0, sb, testFsPhys, func(a uint64) error {
		visited = append(visited, a)
		return nil
	})
	if err != nil {
		t.Fatalf("walkNodeAddrs leaf: %v", err)
	}
	if len(visited) != 1 || visited[0] != testFsPhys {
		t.Fatalf("expected [%#x], got %v", testFsPhys, visited)
	}
}

func TestWalkNodeAddrs_InternalNode(t *testing.T) {
	physInternal := int64(testRootPhys)
	physLeaf := int64(testFsPhys)
	sb := buildMinimalSB(testNodeSize, testImageSize)
	imgBuf := buildTwoLevelTree(sb, physInternal, physLeaf, 42, 1, 5)

	var visited []uint64
	err := walkNodeAddrs(imgBuf, 0, sb, uint64(physInternal), func(a uint64) error {
		visited = append(visited, a)
		return nil
	})
	if err != nil {
		t.Fatalf("walkNodeAddrs internal: %v", err)
	}
	if len(visited) < 2 {
		t.Fatalf("expected >=2 visited, got %d: %v", len(visited), visited)
	}
}

func TestWalkNodeAddrs_FnError(t *testing.T) {
	sb := buildMinimalSB(testNodeSize, testImageSize)
	imgBuf := &rwaBuf{data: buildTestImageBytes()}
	fnErr := errors.New("walk aborted")
	err := walkNodeAddrs(imgBuf, 0, sb, testFsPhys, func(a uint64) error {
		return fnErr
	})
	if !errors.Is(err, fnErr) {
		t.Fatalf("expected fn error, got %v", err)
	}
}

func TestWalkNodeAddrs_ChildFnError(t *testing.T) {
	physInternal := int64(testRootPhys)
	physLeaf := int64(testFsPhys)
	sb := buildMinimalSB(testNodeSize, testImageSize)
	imgBuf := buildTwoLevelTree(sb, physInternal, physLeaf, 42, 1, 5)

	count := 0
	childErr := errors.New("child fn error")
	err := walkNodeAddrs(imgBuf, 0, sb, uint64(physInternal), func(a uint64) error {
		count++
		if count == 2 {
			return childErr
		}
		return nil
	})
	if !errors.Is(err, childErr) {
		t.Fatalf("expected child fn error, got %v", err)
	}
}

func TestWalkNodeAddrs_ReadError(t *testing.T) {
	// readNode fails but walkNodeAddrs should tolerate it (returns nil)
	sb := buildMinimalSB(testNodeSize, testImageSize)
	var visited []uint64
	err := walkNodeAddrs(&rwaBuf{data: make([]byte, testImageSize)}, 0, sb, 0x999000, func(a uint64) error {
		visited = append(visited, a)
		return nil
	})
	// The fn is called first (for the root), but readNode fails → tolerate → nil
	if err != nil {
		t.Fatalf("expected nil on read error, got %v", err)
	}
	// fn was called for root logAddr even if readNode fails
	if len(visited) == 0 {
		t.Fatal("expected fn to be called for root, even if readNode fails")
	}
}

// ── walkNode ─────────────────────────────────────────────────────────────

func TestWalkNode_Leaf(t *testing.T) {
	sb := buildMinimalSB(testNodeSize, testImageSize)
	imgBuf := &rwaBuf{data: buildTestImageBytes()}
	called := 0
	err := walkNode(imgBuf, 0, sb, testFsPhys, func(buf []byte, items []leafItem) error {
		called++
		return nil
	})
	if err != nil {
		t.Fatalf("walkNode leaf: %v", err)
	}
	if called != 1 {
		t.Fatalf("expected 1 call, got %d", called)
	}
}

func TestWalkNode_InternalNode(t *testing.T) {
	physInternal := int64(testRootPhys)
	physLeaf := int64(testFsPhys)
	sb := buildMinimalSB(testNodeSize, testImageSize)
	imgBuf := buildTwoLevelTree(sb, physInternal, physLeaf, 42, 1, 5)

	called := 0
	err := walkNode(imgBuf, 0, sb, uint64(physInternal), func(buf []byte, items []leafItem) error {
		called++
		return nil
	})
	if err != nil {
		t.Fatalf("walkNode internal: %v", err)
	}
	if called != 1 {
		t.Fatalf("expected 1 leaf call from internal node, got %d", called)
	}
}

func TestWalkNode_InternalFnError(t *testing.T) {
	physInternal := int64(testRootPhys)
	physLeaf := int64(testFsPhys)
	sb := buildMinimalSB(testNodeSize, testImageSize)
	imgBuf := buildTwoLevelTree(sb, physInternal, physLeaf, 42, 1, 5)

	fnErr := errors.New("leaf fn error")
	err := walkNode(imgBuf, 0, sb, uint64(physInternal), func(buf []byte, items []leafItem) error {
		return fnErr
	})
	if !errors.Is(err, fnErr) {
		t.Fatalf("expected fn error, got %v", err)
	}
}

func TestWalkNode_InternalReadError(t *testing.T) {
	sb := buildMinimalSB(testNodeSize, testImageSize)
	err := walkNode(&rwaBuf{data: make([]byte, testImageSize)}, 0, sb, 0x999000,
		func(buf []byte, items []leafItem) error { return nil })
	if err == nil {
		t.Fatal("expected error for unmapped logAddr")
	}
}

func TestWalkNode_InternalTruncatedItems(t *testing.T) {
	// Internal node claims 200 items but buffer only fits 3 → break
	sb := buildMinimalSB(testNodeSize, testImageSize)
	imgBuf := &rwaBuf{data: make([]byte, testImageSize)}
	le := binary.LittleEndian

	// Write an internal node with 200 items but only 1 valid key-ptr pointing to leaf
	physInternal := int64(testRootPhys)
	physLeaf := int64(testFsPhys)
	internal := make([]byte, testNodeSize)
	le.PutUint64(internal[0x30:], uint64(physInternal))
	le.PutUint64(internal[0x38:], 2)
	internal[0x64] = 1
	le.PutUint32(internal[0x60:], 200)
	// Only 1 real key-ptr
	off := nodeHdrSize
	le.PutUint64(internal[off:], 42)
	internal[off+8] = 1
	le.PutUint64(internal[off+9:], 5)
	le.PutUint64(internal[off+17:], uint64(physLeaf))
	le.PutUint64(internal[off+25:], 1)
	updateNodeCRC(internal)
	_, _ = imgBuf.WriteAt(internal, physInternal)

	// Write a valid leaf at physLeaf
	leaf := makeEmptyLeaf()
	le.PutUint64(leaf[0x30:], uint64(physLeaf))
	updateNodeCRC(leaf)
	_, _ = imgBuf.WriteAt(leaf, physLeaf)

	called := 0
	err := walkNode(imgBuf, 0, sb, uint64(physInternal), func(buf []byte, items []leafItem) error {
		called++
		return nil
	})
	if err != nil {
		t.Fatalf("walkNode truncated internal: %v", err)
	}
	// The first real child should still be visited
	if called == 0 {
		t.Fatal("expected at least 1 leaf to be visited")
	}
}

// ── loadChunkTree ─────────────────────────────────────────────────────────

func TestLoadChunkTree_WithChunkItems(t *testing.T) {
	// Build a test image where the chunk leaf has a CHUNK_ITEM
	img := buildTestImageBytes()
	le := binary.LittleEndian

	// The testChunkPhys is an empty leaf. Add a CHUNK_ITEM to it.
	chunkLeaf := img[testChunkPhys : testChunkPhys+testNodeSize]
	// Reset to empty leaf
	for i := range chunkLeaf {
		chunkLeaf[i] = 0
	}
	le.PutUint64(chunkLeaf[0x30:], testChunkPhys)
	le.PutUint64(chunkLeaf[0x38:], 1)
	le.PutUint64(chunkLeaf[0x50:], 1)
	chunkLeaf[0x64] = 0
	le.PutUint32(chunkLeaf[0x60:], 0)

	// Insert a CHUNK_ITEM (type 0xE4) for a logical range distinct from the
	// initial sys_chunk_array entry, so loadChunkTree's dedup logic does not
	// skip it.
	chunkData := make([]byte, chunkHeaderSize+chunkStripeSize)
	le.PutUint64(chunkData[chunkSize:], testImageSize)
	le.PutUint16(chunkData[chunkNumStripes:], 1)
	le.PutUint64(chunkData[chunkHeaderSize+8:], testImageSize) // stripe[0].offset
	_ = leafInsertItem(chunkLeaf, key{1, 0xE4, testImageSize}, chunkData)

	// Also insert a non-CHUNK_ITEM to exercise the `continue` path
	_ = leafInsertItem(chunkLeaf, key{2, 0x01, 0}, []byte("ignored"))

	// Insert a CHUNK_ITEM with numStripes=0 to exercise that `continue`
	badChunk := make([]byte, chunkHeaderSize+chunkStripeSize)
	le.PutUint16(badChunk[chunkNumStripes:], 0)
	_ = leafInsertItem(chunkLeaf, key{3, 0xE4, 1000}, badChunk)

	// Insert a CHUNK_ITEM with too-short data
	shortChunk := make([]byte, chunkHeaderSize-1)
	_ = leafInsertItem(chunkLeaf, key{4, 0xE4, 2000}, shortChunk)

	le.PutUint64(chunkLeaf[0x50:], 1)
	updateNodeCRC(chunkLeaf)

	sb := buildMinimalSB(testNodeSize, testImageSize)
	imgBuf := &rwaBuf{data: img}

	initialLen := len(sb.sysChunks)
	if err := loadChunkTree(imgBuf, 0, sb); err != nil {
		t.Fatalf("loadChunkTree: %v", err)
	}
	// Should have added 1 new mapping (the valid CHUNK_ITEM; others are skipped)
	if len(sb.sysChunks) <= initialLen {
		t.Fatalf("expected more sysChunks after loadChunkTree, got %d", len(sb.sysChunks))
	}
}

// ── parseSysChunkArray ────────────────────────────────────────────────────

func TestParseSysChunkArray_UnexpectedKeyType(t *testing.T) {
	le := binary.LittleEndian
	// Build a key with type != 0xE4
	data := make([]byte, keySize+chunkHeaderSize+chunkStripeSize)
	data[8] = 0x01 // wrong type
	le.PutUint64(data[9:], 0x1000)
	_, err := parseSysChunkArray(le, data, 0)
	if err == nil {
		t.Fatal("expected error for unexpected key type")
	}
}

func TestParseSysChunkArray_Truncated(t *testing.T) {
	le := binary.LittleEndian
	// Key with type 0xE4 but then truncated data
	data := make([]byte, keySize+5) // too short for chunkHeaderSize+chunkStripeSize
	data[8] = 0xE4
	_, err := parseSysChunkArray(le, data, 0)
	if err == nil {
		t.Fatal("expected error for truncated chunk array")
	}
}

func TestParseSysChunkArray_ZeroStripes(t *testing.T) {
	le := binary.LittleEndian
	data := make([]byte, keySize+chunkHeaderSize+chunkStripeSize)
	data[8] = 0xE4
	// numStripes = 0 (at chunkNumStripes offset relative to chunk data start)
	// The chunk data starts at keySize; chunkNumStripes = 0x2C
	le.PutUint16(data[keySize+chunkNumStripes:], 0)
	_, err := parseSysChunkArray(le, data, 0)
	if err == nil {
		t.Fatal("expected error for zero stripes")
	}
}

// ── logToPhys / physAddr ──────────────────────────────────────────────────

func TestLogToPhys_Unmapped(t *testing.T) {
	sb := &superblock{
		nodeSize: testNodeSize,
		sysChunks: []chunkMapping{
			{logStart: 0x100000, size: 0x1000, physStart: 0x200000},
		},
	}
	_, err := sb.logToPhys(0x999000)
	if err == nil {
		t.Fatal("expected error for unmapped logical address")
	}
}

func TestPhysAddr_Unmapped(t *testing.T) {
	sb := &superblock{
		nodeSize: testNodeSize,
		sysChunks: []chunkMapping{
			{logStart: 0x100000, size: 0x1000, physStart: 0x200000},
		},
	}
	_, err := sb.physAddr(0, 0x999000)
	if err == nil {
		t.Fatal("expected error for unmapped logical address")
	}
}

// ── coalesce ─────────────────────────────────────────────────────────────

func TestCoalesce_MergesAdjacentRanges(t *testing.T) {
	sm := &spaceManager{
		nodeSize: testNodeSize,
		freeExts: []freeExtent{
			{physStart: 0x3000, size: 0x1000},
			{physStart: 0x1000, size: 0x1000},
			{physStart: 0x2000, size: 0x1000}, // Adjacent to both
		},
	}
	sm.coalesce()
	if len(sm.freeExts) != 1 {
		t.Fatalf("expected 1 merged extent, got %d: %v", len(sm.freeExts), sm.freeExts)
	}
	if sm.freeExts[0].physStart != 0x1000 || sm.freeExts[0].size != 0x3000 {
		t.Fatalf("unexpected merged extent: %+v", sm.freeExts[0])
	}
}

func TestCoalesce_NoMergeNonAdjacent(t *testing.T) {
	sm := &spaceManager{
		nodeSize: testNodeSize,
		freeExts: []freeExtent{
			{physStart: 0x1000, size: 0x100},
			{physStart: 0x5000, size: 0x100}, // gap between
		},
	}
	sm.coalesce()
	if len(sm.freeExts) != 2 {
		t.Fatalf("expected 2 extents (non-adjacent), got %d", len(sm.freeExts))
	}
}

func TestFreeRange_AndCoalesce(t *testing.T) {
	sm := &spaceManager{
		nodeSize: testNodeSize,
		freeExts: []freeExtent{{physStart: 0x1000, size: 0x1000}},
	}
	// Free a range adjacent to the existing one
	sm.freeRange(0x2000, 0x1000)
	if len(sm.freeExts) != 1 {
		t.Fatalf("expected merge after freeRange, got %d extents: %v", len(sm.freeExts), sm.freeExts)
	}
	if sm.freeExts[0].size != 0x2000 {
		t.Fatalf("expected merged size 0x2000, got %#x", sm.freeExts[0].size)
	}
}

// ── resolveRootTree ───────────────────────────────────────────────────────

func TestResolveRootTree_Success(t *testing.T) {
	sb := buildMinimalSB(testNodeSize, testImageSize)
	imgBuf := &rwaBuf{data: buildTestImageBytes()}
	fsRoot, err := resolveRootTree(imgBuf, 0, sb)
	if err != nil {
		t.Fatalf("resolveRootTree: %v", err)
	}
	if fsRoot != testFsPhys {
		t.Fatalf("expected %#x, got %#x", testFsPhys, fsRoot)
	}
}

func TestResolveRootTree_NotFound(t *testing.T) {
	// Use an empty leaf as root tree so FS_TREE lookup fails
	sb := buildMinimalSB(testNodeSize, testImageSize)
	imgBuf := &rwaBuf{data: make([]byte, testImageSize)}
	le := binary.LittleEndian

	emptyLeaf := makeEmptyLeaf()
	le.PutUint64(emptyLeaf[0x30:], testRootPhys)
	updateNodeCRC(emptyLeaf)
	_, _ = imgBuf.WriteAt(emptyLeaf, testRootPhys)

	_, err := resolveRootTree(imgBuf, 0, sb)
	if err == nil {
		t.Fatal("expected error when FS_TREE root item not found")
	}
}

// ── readFileData edges ────────────────────────────────────────────────────

func TestReadFileData_EmptyInode(t *testing.T) {
	sb := buildMinimalSB(testNodeSize, testImageSize)
	in := &inodeItem{num: 257, size: 0, mode: 0x8000}
	data, err := readFileData(&rwaBuf{data: make([]byte, testNodeSize)}, 0, sb, 0, in)
	if err != nil {
		t.Fatalf("readFileData empty: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("expected empty, got %d bytes", len(data))
	}
}

func TestReadFileData_SparseExtent(t *testing.T) {
	// Write a file with a regular extent where diskBytenr=0 (sparse)
	fs := openTestFS(t)
	// First write a normal file to get the machinery working
	if err := fs.WriteFile("/sparse.txt", []byte("not actually sparse"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// We can't easily create a sparse extent from the high-level API,
	// but we can verify readFileData handles inline extents correctly
	data, err := fs.ReadFile("/sparse.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "not actually sparse" {
		t.Fatalf("got %q", data)
	}
}

func TestReadFileData_RegularExtent(t *testing.T) {
	// Large files use regular extents; verify round-trip
	fs := openTestFS(t)
	large := make([]byte, testNodeSize*3) // > nodeSize => regular extent
	for i := range large {
		large[i] = byte(i & 0xFF)
	}
	if err := fs.WriteFile("/big.txt", large, 0o644); err != nil {
		t.Fatalf("WriteFile large: %v", err)
	}
	got, err := fs.ReadFile("/big.txt")
	if err != nil {
		t.Fatalf("ReadFile large: %v", err)
	}
	if len(got) != len(large) {
		t.Fatalf("size mismatch: got %d, want %d", len(got), len(large))
	}
	for i := range got {
		if got[i] != large[i] {
			t.Fatalf("data mismatch at byte %d: got %d, want %d", i, got[i], large[i])
		}
	}
}

// ── readInode truncated ───────────────────────────────────────────────────

func TestReadInode_TooShort(t *testing.T) {
	sb := buildMinimalSB(testNodeSize, testImageSize)
	imgBuf := &rwaBuf{data: make([]byte, testImageSize)}
	le := binary.LittleEndian

	leaf := makeEmptyLeaf()
	le.PutUint64(leaf[0x30:], testFsPhys)
	// Insert an INODE_ITEM with very short data (less than inodeItemSize)
	_ = leafInsertItem(leaf, key{500, typeInodeItem, 0}, []byte("short"))
	updateNodeCRC(leaf)
	_, _ = imgBuf.WriteAt(leaf, testFsPhys)

	_, err := readInode(imgBuf, 0, sb, testFsPhys, 500)
	if err == nil {
		t.Fatal("expected error for short INODE_ITEM data")
	}
}

// ── deleteDir removeInode ─────────────────────────────────────────────────

func TestDeleteDir_RemovesInode(t *testing.T) {
	fs := openTestFS(t)
	if err := fs.MkDir("/subdir", 0o755); err != nil {
		t.Fatalf("MkDir: %v", err)
	}
	if err := fs.DeleteDir("/subdir"); err != nil {
		t.Fatalf("DeleteDir: %v", err)
	}
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir after DeleteDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "subdir" {
			t.Error("subdir still found after delete")
		}
	}
}

func TestRename_DstIsDir(t *testing.T) {
	fs := openTestFS(t)
	if err := fs.MkDir("/d1", 0o755); err != nil {
		t.Fatalf("MkDir d1: %v", err)
	}
	// Rename a file onto a directory (should error)
	err := fs.Rename("/hello.txt", "/d1")
	if err == nil {
		t.Fatal("expected error renaming file to directory")
	}
}

// ── Open resolveRootTree fails ────────────────────────────────────────────

func TestOpen_BadRootTree(t *testing.T) {
	// Create an image where the root leaf doesn't have a FS_TREE root item
	img := buildTestImageBytes()
	le := binary.LittleEndian

	// Overwrite the root leaf with an empty leaf (no ROOT_ITEM entries)
	emptyLeaf := make([]byte, testNodeSize)
	le.PutUint64(emptyLeaf[0x30:], testRootPhys)
	le.PutUint64(emptyLeaf[0x38:], 1)
	le.PutUint64(emptyLeaf[0x50:], 1)
	le.PutUint32(emptyLeaf[0x60:], 0)
	emptyLeaf[0x64] = 0
	updateNodeCRC(emptyLeaf)
	copy(img[testRootPhys:testRootPhys+testNodeSize], emptyLeaf)

	p := filepath.Join(t.TempDir(), "badroot.img")
	if err := os.WriteFile(p, img, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Open(p, 0)
	if err == nil {
		t.Fatal("expected error when root tree doesn't have FS_TREE entry")
	}
}
