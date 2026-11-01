package filesystem_btrfs

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"io"
	"runtime"
	"testing"
)

// Memory-bomb regression tests: every input-driven allocation must be refused
// or bounded BEFORE it happens, so each asserts that the whole call allocated
// little (runtime.MemStats.TotalAlloc is cumulative: an allocation that is
// freed afterwards still shows).

const bombAllocLimit = 1 << 20

func allocDelta(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

// zstdFrame is an empty zstd frame (one last, empty raw block) whose header
// declares a window of 2^exp bytes.
func zstdFrame(exp uint) []byte {
	return []byte{0x28, 0xB5, 0x2F, 0xFD, 0x00, byte((exp - 10) << 3), 0x01, 0x00, 0x00}
}

// A 9-byte frame declaring a 256 MiB window allocated 268 MiB; the kernel
// decodes btrfs extents with a 128 KiB window (zstdMaxWindow).
func TestBomb_ZstdDeclaredWindow(t *testing.T) {
	var err error
	got := allocDelta(func() { _, err = decompressZstd(zstdFrame(28), 4096) })
	if err == nil {
		t.Fatal("a 256 MiB window was accepted")
	}
	if got > bombAllocLimit {
		t.Fatalf("allocated %d bytes for a 9-byte frame", got)
	}
}

// The window bound is exact: 2^17 is decoded, 2^18 is refused.
func TestBomb_ZstdWindowExact(t *testing.T) {
	if _, err := decompressZstd(zstdFrame(17), 4096); err != nil {
		t.Fatalf("window 2^17: %v", err)
	}
	if _, err := decompressZstd(zstdFrame(18), 4096); err == nil {
		t.Fatal("window 2^18 accepted")
	}
}

// An 8-byte LZO extent with ram_bytes = 1 GiB pre-allocated 1 GiB.
func TestBomb_LzoPreallocation(t *testing.T) {
	src := make([]byte, 8)
	binary.LittleEndian.PutUint32(src[0:], 8)
	var err error
	got := allocDelta(func() { _, err = decompressLzo(src, 1<<30) })
	if err != nil {
		t.Fatal(err)
	}
	if got > bombAllocLimit {
		t.Fatalf("allocated %d bytes for an 8-byte extent", got)
	}
}

// zlib: a 526 KiB stream of zeros inflates to 256 MiB; the declared ram_bytes
// of 1 GiB used to be the only bound. Output is now capped at the kernel's
// 128 KiB per extent (plus the one byte that detects overflow).
func TestBomb_ZlibInflation(t *testing.T) {
	var zb bytes.Buffer
	zw := zlib.NewWriter(&zb)
	chunk := make([]byte, 1<<20)
	for i := 0; i < 64; i++ {
		zw.Write(chunk)
	}
	zw.Close()
	var out []byte
	var err error
	got := allocDelta(func() { out, err = decompressExtent(zb.Bytes(), compressionZlib, 1<<30) })
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != maxDecompressRAM+1 {
		t.Fatalf("len = %d, want the cap %d", len(out), maxDecompressRAM+1)
	}
	if got > bombAllocLimit {
		t.Fatalf("allocated %d bytes", got)
	}
}

// End to end through readFileData: a regular zlib extent with a hostile
// extent offset (so the output window is not what bounds the decode) and
// ram_bytes = 1 GiB, backed by 64 MiB of zeros in 64 KiB of image.
func TestBomb_ReadFileDataCompressedRamBytes(t *testing.T) {
	var zb bytes.Buffer
	zw := zlib.NewWriter(&zb)
	chunk := make([]byte, 1<<20)
	for i := 0; i < 64; i++ {
		zw.Write(chunk)
	}
	zw.Close()
	compressed := zb.Bytes()

	const inoNum = uint64(611)
	const fileSize = uint64(4096)
	imgBuf, sb := buildSingleLeafImage(t, func(leaf []byte) {
		le := binary.LittleEndian
		inodeBuf := make([]byte, inodeItemSize)
		le.PutUint64(inodeBuf[inodeOffSize:], fileSize)
		le.PutUint32(inodeBuf[inodeOffMode:], 0x81A4)
		_ = leafInsertItem(leaf, key{inoNum, typeInodeItem, 0}, inodeBuf)
		ext := encodeRegularExtentData(payloadPhys, uint64(len(compressed)), 1<<30, 1<<40, fileSize, 1, compressionZlib)
		_ = leafInsertItem(leaf, key{inoNum, typeExtentData, 0}, ext)
	})
	if _, err := imgBuf.WriteAt(compressed, payloadPhys); err != nil {
		t.Fatal(err)
	}
	in := &inodeItem{num: inoNum, size: fileSize, mode: 0x8000}
	var err error
	got := allocDelta(func() { _, err = readFileData(imgBuf, 0, sb, testFsPhys, in) })
	if err == nil {
		t.Fatal("window outside the decompressed extent accepted")
	}
	if got > 4*bombAllocLimit {
		t.Fatalf("allocated %d bytes", got)
	}
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.ReaderAt
	n int64
}

func (c *countingReader) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.n += int64(n)
	return n, err
}

// overlappingExtents builds a file of fileSize bytes whose leaf holds `count`
// uncompressed extent items, item i at file offset i*4096, every one claiming
// num_bytes = fileSize: each overlaps all the others. The kernel refuses such a
// leaf (check_extent_data_item, fs/btrfs/tree-checker.c: "file extent end range
// goes beyond start offset of the next file extent").
func overlappingExtents(t *testing.T, count int, fileSize uint64) (*rwaBuf, *superblock, *inodeItem) {
	t.Helper()
	const inoNum = uint64(612)
	imgBuf, sb := buildSingleLeafImage(t, func(leaf []byte) {
		le := binary.LittleEndian
		inodeBuf := make([]byte, inodeItemSize)
		le.PutUint64(inodeBuf[inodeOffSize:], fileSize)
		le.PutUint32(inodeBuf[inodeOffMode:], 0x81A4)
		_ = leafInsertItem(leaf, key{inoNum, typeInodeItem, 0}, inodeBuf)
		for i := 0; i < count; i++ {
			ext := encodeRegularExtentData(payloadPhys, fileSize, fileSize, 0, fileSize, 1, compressionNone)
			if err := leafInsertItem(leaf, key{inoNum, typeExtentData, uint64(i) * 4096}, ext); err != nil {
				t.Fatalf("insert %d: %v", i, err)
			}
		}
	})
	return imgBuf, sb, &inodeItem{num: inoNum, size: fileSize, mode: 0x8000}
}

func TestBomb_OverlappingExtentsRefused(t *testing.T) {
	const fileSize = 128 * 1024
	imgBuf, sb, in := overlappingExtents(t, 40, fileSize)
	cr := &countingReader{r: imgBuf}
	_, err := readFileData(cr, 0, sb, testFsPhys, in)
	t.Logf("read %d bytes for a %d-byte file, err=%v", cr.n, fileSize, err)
	if err == nil {
		t.Fatal("overlapping extents accepted")
	}
	if cr.n > 2*fileSize {
		t.Fatalf("read %d bytes for a %d-byte file", cr.n, fileSize)
	}
}

// Adjacent extents (end == next start) are the normal case and must still read.
func TestBomb_AdjacentExtentsStillRead(t *testing.T) {
	const inoNum = uint64(613)
	const fileSize = uint64(3 * 4096)
	imgBuf, sb := buildSingleLeafImage(t, func(leaf []byte) {
		le := binary.LittleEndian
		inodeBuf := make([]byte, inodeItemSize)
		le.PutUint64(inodeBuf[inodeOffSize:], fileSize)
		le.PutUint32(inodeBuf[inodeOffMode:], 0x81A4)
		_ = leafInsertItem(leaf, key{inoNum, typeInodeItem, 0}, inodeBuf)
		for i := uint64(0); i < 3; i++ {
			ext := encodeRegularExtentData(payloadPhys+i*4096, 4096, 4096, 0, 4096, 1, compressionNone)
			_ = leafInsertItem(leaf, key{inoNum, typeExtentData, i * 4096}, ext)
		}
	})
	for i := 0; i < 3; i++ {
		if _, err := imgBuf.WriteAt(bytes.Repeat([]byte{byte('a' + i)}, 4096), int64(payloadPhys+i*4096)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := readFileData(imgBuf, 0, sb, testFsPhys, &inodeItem{num: inoNum, size: fileSize, mode: 0x8000})
	if err != nil || len(got) != int(fileSize) || got[0] != 'a' || got[4096] != 'b' || got[8192] != 'c' {
		t.Fatalf("adjacent extents: err=%v len=%d", err, len(got))
	}
}

// readCounter is a blockBackend over zeros that counts ReadAt calls.
type readCounter struct {
	memBackend
	calls int
}

func (r *readCounter) ReadAt(p []byte, off int64) (int, error) {
	r.calls++
	return r.memBackend.ReadAt(p, off)
}

func stripedPool(profile uint64, stripeLen uint64) (*devicePool, *readCounter) {
	dev := &readCounter{memBackend: memBackend{&rwaBuf{data: make([]byte, 1<<20)}}}
	sb := &superblock{devID: 1}
	sb.sysChunks = []chunkMapping{{
		logStart: 0, size: 1 << 20, profile: profile, stripeLen: stripeLen, subStripes: 2,
		localStripeIdx: 0,
		stripes:        []chunkStripe{{devID: 1}, {devID: 2}, {devID: 3}, {devID: 4}},
	}}
	pool := newDevicePool(dev, 0, sb)
	pool.addDevice(2, dev)
	pool.addDevice(3, dev)
	pool.addDevice(4, dev)
	return pool, dev
}

// A chunk_item with stripe_len = 1 turned one read into one device read (and
// one recursion level) per byte. The kernel accepts only BTRFS_STRIPE_LEN
// (64 KiB): check_chunk_item in fs/btrfs/tree-checker.c.
func TestBomb_TinyStripeLen(t *testing.T) {
	for _, profile := range []uint64{blockGroupRAID0, blockGroupRAID10, blockGroupRAID5, blockGroupRAID6} {
		pool, dev := stripedPool(profile, 1)
		buf := make([]byte, 64*1024)
		_, err := pool.ReadAt(buf, 0)
		t.Logf("profile %#x: %d device reads for a %d-byte read, err=%v", profile, dev.calls, len(buf), err)
		if err == nil {
			t.Errorf("profile %#x: stripe_len 1 accepted", profile)
		}
		if dev.calls > 4 {
			t.Errorf("profile %#x: %d device reads", profile, dev.calls)
		}
	}
}

// stripe_len must also not overflow the remainder arithmetic.
func TestBomb_HugeStripeLen(t *testing.T) {
	for _, profile := range []uint64{blockGroupRAID0, blockGroupRAID10} {
		pool, _ := stripedPool(profile, 1<<63+5)
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("profile %#x: panic %v", profile, r)
				}
			}()
			if _, err := pool.ReadAt(make([]byte, 4096), 0); err == nil {
				t.Errorf("profile %#x: stripe_len 2^63+5 accepted", profile)
			}
		}()
	}
}

// The kernel's own stripe length still reads, across stripe boundaries.
func TestBomb_KernelStripeLenStillReads(t *testing.T) {
	pool, dev := stripedPool(blockGroupRAID0, btrfsStripeLen)
	if _, err := pool.ReadAt(make([]byte, 3*btrfsStripeLen), 0); err != nil {
		t.Fatal(err)
	}
	if dev.calls != 3 {
		t.Fatalf("device reads = %d, want 3", dev.calls)
	}
}

// The structural bound itself: nothing the clamps return exceeds one kernel
// compressed extent.
func TestBomb_ClampsAtKernelExtentSize(t *testing.T) {
	if maxDecompressRAM != 128*1024 {
		t.Fatalf("maxDecompressRAM = %d, want 128 KiB (BTRFS_MAX_UNCOMPRESSED)", maxDecompressRAM)
	}
	if got := clampRAM(1<<60, 1<<60); got != maxDecompressRAM {
		t.Fatalf("clampRAM = %d", got)
	}
	if got := clampRAM(maxDecompressRAM, 1<<60); got != maxDecompressRAM {
		t.Fatalf("clampRAM at the bound = %d", got)
	}
	if got := decompressLimit(1 << 60); got != maxDecompressRAM+1 {
		t.Fatalf("decompressLimit = %d", got)
	}
}

// A legitimate 128 KiB compressed extent (the largest the kernel writes)
// still round-trips through every codec's bound.
func TestBomb_FullSizeExtentStillDecodes(t *testing.T) {
	plain := bytes.Repeat([]byte("btrfs extent 0123456789\n"), maxDecompressRAM/24+1)[:maxDecompressRAM]
	out, err := decompressExtent(zlibCompress(t, plain), compressionZlib, uint64(len(plain)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out[:len(plain)], plain) {
		t.Fatal("128 KiB zlib extent corrupted")
	}
}

// The LZO output capacity follows the input ((totalSize/5 + 1) pages), not the
// declared ram_bytes: even the smallest legal frame with a huge ram_bytes
// allocates a couple of pages.
func TestBomb_LzoStructuralBound(t *testing.T) {
	src := make([]byte, 4)
	binary.LittleEndian.PutUint32(src, 4)
	got := allocDelta(func() { _, _ = decompressLzo(src, 1<<30) })
	if got > bombAllocLimit {
		t.Fatalf("allocated %d bytes", got)
	}
}
