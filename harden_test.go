// Security-hardening tests: a malicious or corrupt on-disk image must never
// panic the host, read out of bounds, integer-overflow into a bad
// alloc/slice, loop forever, or OOM — every finding must surface as a
// graceful error. These tests feed the exact attack vectors from the threat
// model (sys_chunk_array size=0xFFFFFFFF, self-referential / cyclic btree
// block ptr, nodeSize 0/1/huge, in.size 2^63, fileOffset > in.size,
// ram_bytes 2^60) plus a fuzz harness over arbitrary mutations.
package filesystem_btrfs

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/go-volumes/safeio"
)

// sizedBuf is an rwaBuf that also reports a Size(), so deviceSize() derives a
// real ceiling from it (exercising the sizer branch of deviceSize).
type sizedBuf struct {
	*rwaBuf
	size int64
}

func (s *sizedBuf) Size() (int64, error) { return s.size, nil }

// memBackend is a full blockBackend over an in-memory byte slice, so a fuzzed
// image can be mounted via OpenFromDevice without touching the filesystem.
// Size() reports the slice length, exercising the deviceSize sizer path.
type memBackend struct{ *rwaBuf }

func (m *memBackend) Sync() error          { return nil }
func (m *memBackend) Truncate(int64) error { return nil }
func (m *memBackend) Close() error         { return nil }
func (m *memBackend) Size() (int64, error) { return int64(len(m.rwaBuf.data)), nil }

// mountBytes attempts to open an in-memory image, asserting only that the
// call returns (never panics). A nil-or-error result is fine; the point is
// "no panic, no hang, no OOB".
func mountBytes(t *testing.T, img []byte) {
	t.Helper()
	fs, err := OpenFromDevice(&memBackend{&rwaBuf{data: img}}, -1)
	if err == nil && fs != nil {
		// A successful open on a mutated image is allowed; exercise a couple
		// of read paths then close, all of which must also stay panic-free.
		_, _ = fs.ReadFile("/hello.txt")
		_, _ = fs.ListDir("/")
		_ = fs.Close()
	}
}

// ── C1: sys_chunk_array size = 0xFFFFFFFF ──────────────────────────────────

func TestHarden_SysChunkArrayHugeSize(t *testing.T) {
	img := buildTestImageBytes()
	le := binary.LittleEndian
	sb := img[superblockOffset : superblockOffset+sbfSize]
	le.PutUint32(sb[sbfSysChunkArrSz:], 0xFFFFFFFF) // way past the 0x1000 buffer
	updateSuperblockCRC(sb)

	_, err := readSuperblock(&rwaBuf{data: img}, 0)
	if err == nil {
		t.Fatal("expected error for oversized sys_chunk_array, got nil")
	}
	if !errors.Is(err, safeio.ErrOutOfBounds) {
		t.Fatalf("expected ErrOutOfBounds, got %v", err)
	}
}

// ── C3: nodeSize 0 / 1 / huge ──────────────────────────────────────────────

func TestHarden_NodeSizeRejected(t *testing.T) {
	cases := []struct {
		name     string
		nodeSize uint32
	}{
		{"zero", 0},
		{"one", 1},
		{"below-header", nodeHdrSize - 1},
		{"huge", 0xFFFFFFFF},
		{"over-max", maxNodeSize + testNodeSize},
		{"not-multiple", testNodeSize + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			img := buildTestImageBytes()
			le := binary.LittleEndian
			sb := img[superblockOffset : superblockOffset+sbfSize]
			le.PutUint32(sb[sbfNodeSize:], tc.nodeSize)
			updateSuperblockCRC(sb)

			_, err := readSuperblock(&rwaBuf{data: img}, 0)
			if err == nil {
				t.Fatalf("expected error for nodeSize=%d, got nil", tc.nodeSize)
			}
		})
	}
}

// readNode must allocate through MakeBytes; a superblock that somehow carries
// an out-of-range nodeSize (bypassing readSuperblock) still cannot drive a
// giant allocation.
func TestHarden_ReadNodeBoundedAlloc(t *testing.T) {
	sb := buildMinimalSB(maxNodeSize*2, testImageSize) // > maxNodeSize
	_, err := readNode(&rwaBuf{data: make([]byte, testImageSize)}, 0, sb, testFsPhys)
	if err == nil {
		t.Fatal("expected error for oversized nodeSize in readNode")
	}
	if !errors.Is(err, safeio.ErrTooLarge) {
		t.Fatalf("expected ErrTooLarge, got %v", err)
	}
}

// ── C2: self-referential / cyclic btree block pointer ──────────────────────

// buildCyclicTree writes an internal node at logAddr whose only child points
// back at itself, then returns an image + superblock that route every tree to
// it. Every traversal must terminate with a cycle/loop error, not hang.
func buildCyclicImage(t *testing.T) ([]byte, *superblock) {
	t.Helper()
	img := make([]byte, testImageSize)
	le := binary.LittleEndian
	const selfPhys = testRootPhys
	node := img[selfPhys : selfPhys+testNodeSize]
	le.PutUint64(node[0x30:], selfPhys) // bytenr
	le.PutUint64(node[0x38:], 0)        // flags: internal (not leaf)
	le.PutUint64(node[0x50:], 1)        // generation
	le.PutUint32(node[0x60:], 1)        // nritems = 1
	node[0x64] = 1                      // level 1 (internal)
	// single key-ptr whose block pointer is the node itself
	off := nodeHdrSize
	le.PutUint64(node[off:], 1)           // objid
	node[off+8] = 0                       // type
	le.PutUint64(node[off+9:], 0)         // offset
	le.PutUint64(node[off+17:], selfPhys) // child blockptr == self
	le.PutUint64(node[off+25:], 1)        // generation
	updateNodeCRC(node)

	sb := buildMinimalSB(testNodeSize, testImageSize)
	sb.rootLogAddr = selfPhys
	sb.chunkLogAddr = selfPhys
	return img, sb
}

func TestHarden_CyclicBtree_searchTree(t *testing.T) {
	img, sb := buildCyclicImage(t)
	_, _, err := searchTree(&rwaBuf{data: img}, 0, sb, sb.rootLogAddr, 999, 0x84, 0)
	if err == nil {
		t.Fatal("expected error walking a self-referential tree")
	}
}

func TestHarden_CyclicBtree_walkNode(t *testing.T) {
	img, sb := buildCyclicImage(t)
	err := walkNode(&rwaBuf{data: img}, 0, sb, sb.chunkLogAddr,
		func(buf []byte, items []leafItem) error { return nil })
	if !errors.Is(err, safeio.ErrCycle) {
		t.Fatalf("expected ErrCycle from walkNode, got %v", err)
	}
}

func TestHarden_CyclicBtree_walkPrefixLeaves(t *testing.T) {
	img, sb := buildCyclicImage(t)
	err := walkPrefixLeaves(&rwaBuf{data: img}, 0, sb, sb.rootLogAddr, 1, 0,
		func(buf []byte, it leafItem) bool { return true })
	if !errors.Is(err, safeio.ErrCycle) {
		t.Fatalf("expected ErrCycle from walkPrefixLeaves, got %v", err)
	}
}

func TestHarden_CyclicBtree_walkAllLeaves(t *testing.T) {
	img, sb := buildCyclicImage(t)
	err := walkAllLeaves(&rwaBuf{data: img}, 0, sb, sb.rootLogAddr,
		func(buf []byte, it leafItem) bool { return true })
	if !errors.Is(err, safeio.ErrCycle) {
		t.Fatalf("expected ErrCycle from walkAllLeaves, got %v", err)
	}
}

func TestHarden_CyclicBtree_searchTreePrefix(t *testing.T) {
	img, sb := buildCyclicImage(t)
	_, _, err := searchTreePrefix(&rwaBuf{data: img}, 0, sb, sb.rootLogAddr, 1, 0)
	if err == nil {
		t.Fatal("expected error from searchTreePrefix on a cyclic tree")
	}
}

func TestHarden_CyclicBtree_walkNodeAddrs(t *testing.T) {
	img, sb := buildCyclicImage(t)
	visited := 0
	err := walkNodeAddrs(&rwaBuf{data: img}, 0, sb, sb.rootLogAddr, func(uint64) error {
		visited++
		return nil
	})
	// walkNodeAddrs tolerates the cycle by stopping on revisit (returns nil)
	// rather than erroring; the key property is that it terminates.
	if err != nil {
		t.Fatalf("walkNodeAddrs should terminate cleanly on a cycle, got %v", err)
	}
	if visited == 0 {
		t.Fatal("expected at least the root to be visited")
	}
}

func TestHarden_CyclicBtree_tracePath(t *testing.T) {
	img, sb := buildCyclicImage(t)
	_, err := tracePath(&rwaBuf{data: img}, 0, sb, sb.rootLogAddr, key{objID: 1})
	if !errors.Is(err, safeio.ErrCycle) {
		t.Fatalf("expected ErrCycle from tracePath, got %v", err)
	}
}

func TestHarden_CyclicBtree_findLeafContainingPrefix(t *testing.T) {
	img, sb := buildCyclicImage(t)
	_, _, err := findLeafContainingPrefix(&rwaBuf{data: img}, 0, sb, sb.rootLogAddr, 1, 0)
	if !errors.Is(err, safeio.ErrCycle) {
		t.Fatalf("expected ErrCycle from findLeafContainingPrefix, got %v", err)
	}
}

// A self-referential chunk tree at mount time (pre-auth) must not hang.
func TestHarden_CyclicChunkTree_loadChunkTree(t *testing.T) {
	img, sb := buildCyclicImage(t)
	err := loadChunkTree(&rwaBuf{data: img}, 0, sb)
	if err == nil {
		t.Fatal("expected error loading a self-referential chunk tree")
	}
}

// ── H1: in.size = 2^63 ─────────────────────────────────────────────────────

func TestHarden_InodeSizeHuge(t *testing.T) {
	img := buildTestImageBytes()
	in := &inodeItem{num: 257, size: 1 << 63, mode: 0x81A4}
	// rwaBuf has no Size(), so the ceiling is hardSizeCeiling (1 GiB); a 2^63
	// declared size must be rejected, not allocated.
	_, err := readFileData(&rwaBuf{data: img}, 0, buildMinimalSB(testNodeSize, testImageSize), testFsPhys, in)
	if err == nil {
		t.Fatal("expected error for inode size 2^63")
	}
	if !errors.Is(err, safeio.ErrTooLarge) {
		t.Fatalf("expected ErrTooLarge, got %v", err)
	}
}

// ── H2: fileOffset > in.size (key offset past EOF) ─────────────────────────

func TestHarden_ExtentOffsetPastEOF(t *testing.T) {
	// An EXTENT_DATA key whose offset exceeds the inode size would underflow
	// `in.size - fileOffset` and slice out of bounds; readFileData must skip
	// it and return the (zero-filled) file without panicking.
	img := make([]byte, testImageSize)
	le := binary.LittleEndian
	sb := buildMinimalSB(testNodeSize, testImageSize)

	leaf := img[testFsPhys : testFsPhys+testNodeSize]
	le.PutUint64(leaf[0x30:], testFsPhys)
	le.PutUint64(leaf[0x38:], 1) // leaf
	le.PutUint64(leaf[0x50:], 1)
	leaf[0x64] = 0
	// EXTENT_DATA item for inode 257 with KEY offset 1<<40 (far past EOF).
	ext := encodeExtentDataInline([]byte("AAAA"), 1)
	_ = leafInsertItem(leaf, key{257, typeExtentData, 1 << 40}, ext)
	updateNodeCRC(leaf)

	in := &inodeItem{num: 257, size: 4, mode: 0x81A4}
	out, err := readFileData(&rwaBuf{data: img}, 0, sb, testFsPhys, in)
	if err != nil {
		t.Fatalf("expected graceful read, got %v", err)
	}
	if len(out) != 4 {
		t.Fatalf("expected 4-byte output, got %d", len(out))
	}
}

// ── H3 / H4: ram_bytes / disk_num_bytes huge ───────────────────────────────

func TestHarden_RamBytesHuge_clampRAM(t *testing.T) {
	// clampRAM caps both the hard ceiling and the context-specific bound.
	if got := clampRAM(1<<60, 100); got != 100 {
		t.Fatalf("clampRAM(2^60,100)=%d want 100", got)
	}
	if got := clampRAM(1<<60, 1<<40); got != maxDecompressRAM {
		t.Fatalf("clampRAM(2^60,2^40)=%d want %d", got, maxDecompressRAM)
	}
	if got := clampRAM(50, 100); got != 50 {
		t.Fatalf("clampRAM(50,100)=%d want 50", got)
	}
}

func TestHarden_DecompressLimit(t *testing.T) {
	if got := decompressLimit(10); got != 11 {
		t.Fatalf("decompressLimit(10)=%d want 11", got)
	}
	// A value near 2^64 must clamp rather than overflow the int64 limit.
	if got := decompressLimit(1 << 60); got != maxDecompressRAM+1 {
		t.Fatalf("decompressLimit(2^60)=%d want %d", got, maxDecompressRAM+1)
	}
}

func TestHarden_LzoRamBytesHuge(t *testing.T) {
	// A tiny valid-looking lzo frame with an absurd ram_bytes hint must not
	// pre-allocate gigabytes; decompressLzo clamps the capacity.
	src := make([]byte, 8)
	binary.LittleEndian.PutUint32(src[0:], 8) // total_size == len
	binary.LittleEndian.PutUint32(src[4:], 0) // seg len 0 -> loop ends
	out, err := decompressLzo(src, 1<<60)
	if err != nil {
		t.Fatalf("decompressLzo: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("expected empty output, got %d bytes", len(out))
	}
}

// ── deviceSize sizer branch ────────────────────────────────────────────────

func TestHarden_DeviceSizeFromSizer(t *testing.T) {
	sb := &sizedBuf{rwaBuf: &rwaBuf{data: make([]byte, 4096)}, size: 123456}
	if got := deviceSize(sb); got != 123456 {
		t.Fatalf("deviceSize sizer=%d want 123456", got)
	}
	// Non-positive size falls back to the hard ceiling.
	z := &sizedBuf{rwaBuf: &rwaBuf{data: make([]byte, 16)}, size: 0}
	if got := deviceSize(z); got != hardSizeCeiling {
		t.Fatalf("deviceSize(0)=%d want %d", got, hardSizeCeiling)
	}
	// A plain reader without Size() uses the ceiling.
	if got := deviceSize(&rwaBuf{data: make([]byte, 8)}); got != hardSizeCeiling {
		t.Fatalf("deviceSize(no-sizer)=%d want %d", got, hardSizeCeiling)
	}
}

// A sized device makes readFileData accept files up to that device size; one
// just over the device is rejected.
func TestHarden_InodeSizeVsDevice(t *testing.T) {
	img := buildTestImageBytes()
	dev := &sizedBuf{rwaBuf: &rwaBuf{data: img}, size: int64(len(img))}
	sb := buildMinimalSB(testNodeSize, testImageSize)
	in := &inodeItem{num: 257, size: uint64(len(img)) + 1, mode: 0x81A4}
	if _, err := readFileData(dev, 0, sb, testFsPhys, in); !errors.Is(err, safeio.ErrTooLarge) {
		t.Fatalf("expected ErrTooLarge for size > device, got %v", err)
	}
}

// ── Fuzz: arbitrary mutations of a valid image must never panic/hang ───────

func FuzzMountImage(f *testing.F) {
	base := buildTestImageBytes()
	f.Add(base)

	// Seed: the explicit threat-model vectors.
	le := binary.LittleEndian

	// sys_chunk_array size = 0xFFFFFFFF
	v1 := append([]byte(nil), base...)
	sb1 := v1[superblockOffset : superblockOffset+sbfSize]
	le.PutUint32(sb1[sbfSysChunkArrSz:], 0xFFFFFFFF)
	updateSuperblockCRC(sb1)
	f.Add(v1)

	// nodeSize 0 / 1 / huge
	for _, ns := range []uint32{0, 1, 0xFFFFFFFF} {
		v := append([]byte(nil), base...)
		sb := v[superblockOffset : superblockOffset+sbfSize]
		le.PutUint32(sb[sbfNodeSize:], ns)
		updateSuperblockCRC(sb)
		f.Add(v)
	}

	// self-referential root/chunk tree pointer
	v2 := append([]byte(nil), base...)
	sb2 := v2[superblockOffset : superblockOffset+sbfSize]
	le.PutUint64(sb2[sbfRootLogAddr:], testRootPhys)
	le.PutUint64(sb2[sbfChunkLogAddr:], testRootPhys)
	root := v2[testRootPhys : testRootPhys+testNodeSize]
	le.PutUint64(root[0x38:], 0) // internal
	root[0x64] = 1               // level 1
	le.PutUint32(root[0x60:], 1) // 1 item
	le.PutUint64(root[nodeHdrSize+17:], testRootPhys)
	updateNodeCRC(root)
	updateSuperblockCRC(sb2)
	f.Add(v2)

	// A short / truncated image.
	f.Add(base[:superblockOffset+sbfSize])
	f.Add([]byte("not a btrfs image"))

	f.Fuzz(func(t *testing.T, img []byte) {
		mountBytes(t, img)
	})
}

// FuzzReadFileData drives the extent decoder with mutated EXTENT_DATA payloads
// and attacker-chosen inode geometry (size, ram_bytes, fileOffset).
func FuzzReadFileData(f *testing.F) {
	f.Add(uint64(5), uint64(0), []byte("hello"))
	f.Add(uint64(1<<63), uint64(0), []byte{})         // H1
	f.Add(uint64(4), uint64(1<<40), []byte("AAAA"))   // H2: offset past EOF
	f.Add(uint64(8), uint64(0), []byte{0x00, 0x00})   // tiny extent
	f.Add(uint64(1<<20), uint64(0), make([]byte, 64)) // larger declared size

	f.Fuzz(func(t *testing.T, size, keyOffset uint64, payload []byte) {
		img := make([]byte, testImageSize)
		le := binary.LittleEndian
		sb := buildMinimalSB(testNodeSize, testImageSize)
		leaf := img[testFsPhys : testFsPhys+testNodeSize]
		le.PutUint64(leaf[0x30:], testFsPhys)
		le.PutUint64(leaf[0x38:], 1)
		le.PutUint64(leaf[0x50:], 1)
		leaf[0x64] = 0
		// Build an EXTENT_DATA item carrying the fuzzed payload; keep it small
		// enough to fit the leaf so leafInsertItem succeeds.
		if len(payload) > testNodeSize/2 {
			payload = payload[:testNodeSize/2]
		}
		ext := encodeExtentDataInline(payload, 1)
		_ = leafInsertItem(leaf, key{257, typeExtentData, keyOffset}, ext)
		updateNodeCRC(leaf)

		in := &inodeItem{num: 257, size: size, mode: 0x81A4}
		// Must return (never panic / OOB / OOM). Error or success both fine.
		_, _ = readFileData(&rwaBuf{data: img}, 0, sb, testFsPhys, in)
	})
}
