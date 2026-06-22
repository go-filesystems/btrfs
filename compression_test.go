package filesystem_btrfs

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// zlibCompress is a small helper that returns the zlib-compressed bytes of
// the given payload.
func zlibCompress(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatalf("zlib write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}
	return buf.Bytes()
}

// encodeRegularExtentData builds the on-disk bytes of an EXTENT_DATA item
// pointing at a compressed extent. The output is a btrfs_file_extent_item
// laid out exactly as readFileData expects.
func encodeRegularExtentData(diskBytenr, diskNumBytes, ramBytes, offset, numBytes, generation uint64, compression uint8) []byte {
	buf := make([]byte, extDataRegularSize)
	le := binary.LittleEndian
	le.PutUint64(buf[0x00:], generation)
	le.PutUint64(buf[extDataOffRamBytes:], ramBytes)
	buf[extDataOffCompression] = compression
	buf[extDataOffType] = extentDataRegular
	le.PutUint64(buf[extDataOffDiskBytenr:], diskBytenr)
	le.PutUint64(buf[extDataOffDiskNumBytes:], diskNumBytes)
	le.PutUint64(buf[extDataOffOffset:], offset)
	le.PutUint64(buf[extDataOffNumBytes:], numBytes)
	return buf
}

// TestZlib_ReadCompressedExtent crafts a file whose data lives in a
// zlib-compressed extent (not produced by our own writer, but matching the
// real btrfs on-disk shape), then reads it back through ReadFile.
func TestZlib_ReadCompressedExtent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, 8*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	// First create the file the normal way so all the parent dir items and
	// the INODE_ITEM exist. We'll overwrite its single EXTENT_DATA item with
	// a hand-crafted compressed one.
	payload := bytes.Repeat([]byte("zlib-compresses-this-well-"), 256) // ~6.7 KiB
	// Start with placeholder content of the same size so the INODE size is right.
	placeholder := make([]byte, len(payload))
	if err := fs.WriteFile("/zfile.bin", placeholder, 0o644); err != nil {
		t.Fatalf("WriteFile placeholder: %v", err)
	}

	st, err := fs.Stat("/zfile.bin")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	ino := st.Inode()

	// Free the placeholder's data extents so we can install a fresh compressed
	// one in their place.
	freeInodeExtents(bf.f, bf.partOffset, bf.sb, bf.sm, bf.fsTreeRoot, ino)
	newRoot, err := cowDeletePrefix(nil, bf.f, bf.partOffset, bf.sb, bf.sm, bf.fsTreeRoot, ino, typeExtentData)
	if err != nil {
		t.Fatalf("cowDeletePrefix: %v", err)
	}
	bf.fsTreeRoot = newRoot

	// Compress the real payload and stage it on disk in a fresh allocation.
	compressed := zlibCompress(t, payload)
	physData, _, err := bf.sm.allocDataBytes(uint64(len(compressed)), uint64(bf.sb.sectorSize))
	if err != nil {
		t.Fatalf("allocDataBytes compressed payload: %v", err)
	}
	if _, err := bf.f.WriteAt(compressed, bf.partOffset+int64(physData)); err != nil {
		t.Fatalf("WriteAt compressed payload: %v", err)
	}
	logData := physToLog(bf.sb, physData)

	extData := encodeRegularExtentData(
		logData,
		uint64(len(compressed)), // disk_num_bytes (compressed size on disk)
		uint64(len(payload)),    // ram_bytes (decompressed size)
		0,                       // offset within the decompressed extent
		uint64(len(payload)),    // num_bytes used from the decompressed extent
		bf.sb.generation+1,
		compressionZlib,
	)
	newRoot, err = cowInsert(nil, bf.f, bf.partOffset, bf.sb, bf.sm, bf.fsTreeRoot, key{ino, typeExtentData, 0}, extData)
	if err != nil {
		t.Fatalf("cowInsert compressed extent: %v", err)
	}
	bf.fsTreeRoot = newRoot
	if err := updateFsTreeRoot(bf.f, bf.partOffset, bf.sb, bf.sm, bf.fsTreeRoot); err != nil {
		t.Fatalf("updateFsTreeRoot: %v", err)
	}

	got, err := fs.ReadFile("/zfile.bin")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("compressed read mismatch: got %d bytes (first 32 = %x), want %d bytes (first 32 = %x)",
			len(got), got[:min(32, len(got))], len(payload), payload[:min(32, len(payload))])
	}
}

// TestUnsupportedCompression returns an explicit error rather than
// silently returning garbage when the on-disk extent claims an unsupported
// compression algorithm, and also when LZO / Zstd payloads are malformed.
func TestUnsupportedCompression(t *testing.T) {
	if _, err := decompressExtent([]byte("ignored"), compressionLzo, 16); err == nil {
		t.Fatalf("decompressExtent lzo on garbage: expected error, got nil")
	}
	if _, err := decompressExtent([]byte("ignored"), compressionZstd, 16); err == nil {
		t.Fatalf("decompressExtent zstd on garbage: expected error, got nil")
	}
	if _, err := decompressExtent([]byte("ignored"), 42, 16); err == nil {
		t.Fatalf("decompressExtent unknown code: expected error, got nil")
	}
}

// zstdCompress returns a zstd-compressed frame of payload.
func zstdCompress(t *testing.T, payload []byte) []byte {
	t.Helper()
	zw, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	defer zw.Close()
	return zw.EncodeAll(payload, nil)
}

// btrfsLzoEncodeAllLiterals encodes payload as a btrfs-framed LZO extent
// in which the LZO1X-1 segments contain only literals followed by the
// end-of-stream marker. This is a valid LZO1X-1 stream and a valid btrfs
// extent layout — useful for tests that don't want to depend on a real
// LZO encoder. The function chunks payload into pages of
// btrfsLzoPageSize bytes per segment, mirroring what the kernel encoder
// does.
func btrfsLzoEncodeAllLiterals(payload []byte) []byte {
	// First, encode each page as an LZO1X-1 all-literals frame.
	var segments [][]byte
	for off := 0; off < len(payload); off += btrfsLzoPageSize {
		end := off + btrfsLzoPageSize
		if end > len(payload) {
			end = len(payload)
		}
		segments = append(segments, lzo1xEncodeAllLiterals(payload[off:end]))
	}
	if len(payload) == 0 {
		// Empty extent: a single empty segment still needs the EOS
		// marker so the decoder sees a valid LZO1X stream.
		segments = append(segments, lzo1xEncodeAllLiterals(nil))
	}

	// Now build the btrfs framing. We must respect the rule that a
	// 4-byte segment header may not straddle a 4 KiB on-disk page
	// boundary.
	buf := make([]byte, 4) // reserve room for total_size
	for _, seg := range segments {
		// If the upcoming header would cross a page boundary, pad.
		pos := len(buf)
		if pos/btrfsLzoPageSize != (pos+4-1)/btrfsLzoPageSize {
			pad := ((pos/btrfsLzoPageSize)+1)*btrfsLzoPageSize - pos
			buf = append(buf, make([]byte, pad)...)
		}
		var hdr [4]byte
		binary.LittleEndian.PutUint32(hdr[:], uint32(len(seg)))
		buf = append(buf, hdr[:]...)
		buf = append(buf, seg...)
	}
	binary.LittleEndian.PutUint32(buf[:4], uint32(len(buf)))
	return buf
}

// lzo1xEncodeAllLiterals emits an LZO1X-1 stream that is just literals
// plus the end-of-stream marker. This is always a valid encoding (the
// decoder accepts it) and avoids depending on a real LZO1X encoder.
//
// Encoding strategy:
//
//   - n == 0: just emit the EOS marker (M4 opcode 0x11 followed by LE16
//     0x0000, which yields decoded distance == 16384 — the end-of-stream
//     sentinel).
//   - n in 4..238: use the "first byte" encoding (byte = n + 17, in
//     21..255), which copies n literals and sets state = 4.
//   - n >= 18 and not in 4..238: use the M1-long-literal opcode 0x00 in
//     the regular instruction stream (decoder's first-byte branch falls
//     through because byte 0 < 18, then the main loop reads opcode 0
//     with curState=0 → handleM1LongLiteral). Length is encoded as
//     length = 18 + zeros*255 + nz, where nz in 1..255 is the
//     terminating non-zero byte and zeros is the number of intermediate
//     zero bytes. This path handles arbitrary n >= 18.
//   - n in 1..3: not supported. The btrfs LZO framing chunks payloads
//     into 4 KiB segments so this case never occurs in our tests.
func lzo1xEncodeAllLiterals(payload []byte) []byte {
	out := make([]byte, 0, len(payload)+8)
	n := len(payload)
	switch {
	case n == 0:
		// EOS marker only.
		out = append(out, 0x11, 0x00, 0x00)
		return out
	case n >= 4 && n <= 238:
		// First-byte literal encoding. byte = n + 17 in 21..255.
		out = append(out, byte(n+17))
		out = append(out, payload...)
		out = append(out, 0x11, 0x00, 0x00)
		return out
	case n >= 18:
		// M1 long literal. length = 18 + zeros*255 + nz, nz in 1..255.
		rem := n - 18
		zeros := rem / 255
		nz := rem - zeros*255
		if nz == 0 {
			// nz must be > 0 to terminate the zero-byte run.
			// Subtract one whole 255-block and use nz = 255.
			zeros--
			nz = 255
		}
		out = append(out, 0x00)
		for i := 0; i < zeros; i++ {
			out = append(out, 0x00)
		}
		out = append(out, byte(nz))
		out = append(out, payload...)
		out = append(out, 0x11, 0x00, 0x00)
		return out
	default:
		// n in 1..3. btrfs chunks into 4 KiB pages so this never
		// happens in production; failing loudly avoids silently
		// producing wrong fixtures.
		panic("lzo1xEncodeAllLiterals: payload length 1..3 not supported")
	}
}

// TestLZO_RoundTrip exercises the LZO decoder over a hand-built
// all-literals stream. This confirms both the btrfs framing parser and
// the underlying LZO1X-1 decoder are wired up correctly.
func TestLZO_RoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
	}{
		{"small-text", []byte("Hello, btrfs LZO compression! This payload is short.")},
		{"medium", bytes.Repeat([]byte("ABCDEFGHIJKLMNOP"), 100)},                // 1600 bytes (one page)
		{"two-pages", bytes.Repeat([]byte("0123456789abcdef"), 600)},             // 9600 bytes (3 segments)
		{"exact-page", bytes.Repeat([]byte{'X'}, btrfsLzoPageSize)},              // exactly one page
		{"page-plus-some", bytes.Repeat([]byte{'Y'}, btrfsLzoPageSize+50)},       // forces 2 segments (second segment >= 4 bytes)
		{"large", bytes.Repeat([]byte("the quick brown fox jumps over "), 2000)}, // ~62 KiB
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			framed := btrfsLzoEncodeAllLiterals(tc.payload)
			got, err := decompressExtent(framed, compressionLzo, uint64(len(tc.payload)))
			if err != nil {
				t.Fatalf("decompressExtent lzo: %v", err)
			}
			if !bytes.Equal(got, tc.payload) {
				t.Fatalf("lzo round-trip mismatch: got %d bytes, want %d bytes\nfirst 32 got:  %x\nfirst 32 want: %x",
					len(got), len(tc.payload),
					got[:min(32, len(got))], tc.payload[:min(32, len(tc.payload))])
			}
		})
	}
}

// TestLZO_Malformed exercises the error paths of the btrfs LZO framing
// parser.
func TestLZO_Malformed(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"too-short", []byte{0x01, 0x02}},
		{"total-size-zero", []byte{0x00, 0x00, 0x00, 0x00}},
		{"total-size-too-large", []byte{0xff, 0xff, 0xff, 0xff, 0x00, 0x00, 0x00, 0x00}},
		{"truncated-segment-header", func() []byte {
			b := make([]byte, 6)
			binary.LittleEndian.PutUint32(b[:4], 6) // total_size = 6, but no room for 4-byte seg header
			return b
		}()},
		{"segment-length-too-large", func() []byte {
			b := make([]byte, 12)
			binary.LittleEndian.PutUint32(b[:4], 12)
			binary.LittleEndian.PutUint32(b[4:8], 100) // segLen=100 but only 4 bytes remain
			return b
		}()},
		{"corrupt-lzo-payload", func() []byte {
			// total_size=12, segLen=4, segment data = 4 bytes of garbage
			b := []byte{0x0c, 0x00, 0x00, 0x00, 0x04, 0x00, 0x00, 0x00, 0xff, 0xff, 0xff, 0xff}
			return b
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decompressLzo(tc.in, 4096); err == nil {
				t.Fatalf("decompressLzo: expected error on %q, got nil", tc.name)
			}
		})
	}
}

// TestLZO_PageStraddleSkip exercises the rule that a segment header may
// not straddle a 4 KiB on-disk page boundary: the encoder pads to the
// next page when it would. We build a payload that places a segment
// header near the end of a page so the decoder must skip ahead.
func TestLZO_PageStraddleSkip(t *testing.T) {
	// Two segments where the first one is large enough to push the
	// second segment's header within 3 bytes of the next page
	// boundary. Our encoder handles the padding rule for us.
	payload := append(
		bytes.Repeat([]byte{'a'}, btrfsLzoPageSize),      // segment 1 (one page)
		bytes.Repeat([]byte{'b'}, btrfsLzoPageSize/2)..., // segment 2
	)
	framed := btrfsLzoEncodeAllLiterals(payload)
	got, err := decompressExtent(framed, compressionLzo, uint64(len(payload)))
	if err != nil {
		t.Fatalf("decompressExtent lzo: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("lzo straddle mismatch (got %d bytes, want %d)", len(got), len(payload))
	}
}

// TestLZO_RamBytesCap rejects streams whose decompressed size exceeds
// the declared ram_bytes ceiling.
func TestLZO_RamBytesCap(t *testing.T) {
	payload := bytes.Repeat([]byte("AAAAAAAA"), 200) // 1600 bytes
	framed := btrfsLzoEncodeAllLiterals(payload)
	// Cap ram_bytes at half the real size; expect an error.
	if _, err := decompressLzo(framed, uint64(len(payload)/2)); err == nil {
		t.Fatalf("decompressLzo: expected ramBytes-cap error, got nil")
	}
}

// TestLZO_RamBytesZero falls back to a generous internal cap when
// ramBytes is zero — useful when the caller has no decompressed-size
// hint (e.g. the unsupported-codec defensive tests).
func TestLZO_RamBytesZero(t *testing.T) {
	payload := []byte("hello-zero-rambytes-path")
	framed := btrfsLzoEncodeAllLiterals(payload)
	got, err := decompressLzo(framed, 0)
	if err != nil {
		t.Fatalf("decompressLzo with ramBytes=0: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("ramBytes=0 round-trip mismatch")
	}
}

// TestLZO_ZeroLengthSegment exercises the segLen == 0 path: a segment
// header with length zero is skipped, and the loop continues to the
// next segment.
func TestLZO_ZeroLengthSegment(t *testing.T) {
	// total_size = 4 (header) + 4 (zero-len seg) + segment_for_payload.
	payload := []byte("after-zero-segment-payload")
	seg := lzo1xEncodeAllLiterals(payload)
	buf := make([]byte, 4)                    // total_size placeholder
	buf = append(buf, 0x00, 0x00, 0x00, 0x00) // zero-length segment header
	hdr := make([]byte, 4)
	binary.LittleEndian.PutUint32(hdr, uint32(len(seg)))
	buf = append(buf, hdr...)
	buf = append(buf, seg...)
	binary.LittleEndian.PutUint32(buf[:4], uint32(len(buf)))

	got, err := decompressLzo(buf, uint64(len(payload)))
	if err != nil {
		t.Fatalf("decompressLzo zero-length segment: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("zero-segment payload mismatch: got %q, want %q", got, payload)
	}
}

// TestLZO_PageStraddleAtEnd exercises the page-skip branch where the
// padding lands exactly at total_size, leaving no room for a header.
// The decoder should terminate cleanly via the in >= end check after
// skipping.
func TestLZO_PageStraddleAtEnd(t *testing.T) {
	// Build: total_size = 4096 (one page). A single segment that
	// fills the rest of the first page, then nothing after — but
	// total_size declares the buffer ends exactly at the page
	// boundary, so the post-segment position cannot fit another
	// segment header. The straddle-skip branch should jump to
	// in == 4096 == end and exit.
	payload := []byte("page-fill-payload-aaaaaaaaaaaaaaaaaaaaaa")
	seg := lzo1xEncodeAllLiterals(payload)
	// First-page contents: 4-byte total + 4-byte segLen + seg.
	// We want post-segment position to be near the page boundary
	// such that header+4 would straddle.
	header := 4
	segHdr := 4
	firstSegOffset := header + segHdr
	bodyLen := firstSegOffset + len(seg)
	// Pad up to within 3 bytes of the next page so the next header
	// would straddle.
	padTarget := btrfsLzoPageSize - 3
	if bodyLen >= padTarget {
		t.Skip("segment too large for this test layout")
	}
	pad := padTarget - bodyLen
	buf := make([]byte, 4)
	hdr := make([]byte, 4)
	binary.LittleEndian.PutUint32(hdr, uint32(len(seg)))
	buf = append(buf, hdr...)
	buf = append(buf, seg...)
	buf = append(buf, make([]byte, pad)...)
	// Now len(buf) = 4093. Declared total_size = 4096 (page).
	// The straddle-skip branch will round in up to 4096; then
	// in >= end → break.
	binary.LittleEndian.PutUint32(buf[:4], btrfsLzoPageSize)
	buf = append(buf, make([]byte, btrfsLzoPageSize-len(buf))...) // pad buffer to total_size

	got, err := decompressLzo(buf, uint64(len(payload)))
	if err != nil {
		t.Fatalf("decompressLzo page-straddle-at-end: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("page-straddle-at-end mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}

// TestZstd_RoundTrip checks the zstd decoder against payloads compressed
// with klauspost/compress/zstd.
func TestZstd_RoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
	}{
		{"small", []byte("zstd is fun")},
		{"medium", bytes.Repeat([]byte("zstd-medium-test-"), 200)},
		{"large", bytes.Repeat([]byte("the quick brown fox"), 5000)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compressed := zstdCompress(t, tc.payload)
			got, err := decompressExtent(compressed, compressionZstd, uint64(len(tc.payload)))
			if err != nil {
				t.Fatalf("decompressExtent zstd: %v", err)
			}
			if !bytes.Equal(got, tc.payload) {
				t.Fatalf("zstd round-trip mismatch: got %d bytes, want %d", len(got), len(tc.payload))
			}
		})
	}
}

// TestZstd_Malformed exercises the zstd error path.
func TestZstd_Malformed(t *testing.T) {
	if _, err := decompressZstd([]byte("not a zstd frame at all"), 64); err == nil {
		t.Fatalf("decompressZstd: expected error on garbage, got nil")
	}
}

// TestZstd_ReadCompressedExtent reads a file whose data lives in a
// real zstd-compressed extent via the FS read path.
func TestZstd_ReadCompressedExtent(t *testing.T) {
	testReadCompressedExtent(t, "zstd", compressionZstd, func(t *testing.T, payload []byte) []byte {
		return zstdCompress(t, payload)
	})
}

// TestLZO_ReadCompressedExtent reads a file whose data lives in a
// btrfs-framed LZO-compressed extent via the FS read path.
func TestLZO_ReadCompressedExtent(t *testing.T) {
	testReadCompressedExtent(t, "lzo", compressionLzo, func(t *testing.T, payload []byte) []byte {
		return btrfsLzoEncodeAllLiterals(payload)
	})
}

// testReadCompressedExtent factors out the common machinery: format a
// fresh image, write a placeholder file, replace its single EXTENT_DATA
// item with a hand-crafted compressed one, then read the file back.
func testReadCompressedExtent(t *testing.T, name string, codec uint8, compress func(*testing.T, []byte) []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+"-disk.img")
	fs, err := Format(path, 8*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	payload := bytes.Repeat([]byte(name+"-compresses-this-well-"), 256)
	placeholder := make([]byte, len(payload))
	fname := "/" + name + "file.bin"
	if err := fs.WriteFile(fname, placeholder, 0o644); err != nil {
		t.Fatalf("WriteFile placeholder: %v", err)
	}
	st, err := fs.Stat(fname)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	ino := st.Inode()

	freeInodeExtents(bf.f, bf.partOffset, bf.sb, bf.sm, bf.fsTreeRoot, ino)
	newRoot, err := cowDeletePrefix(nil, bf.f, bf.partOffset, bf.sb, bf.sm, bf.fsTreeRoot, ino, typeExtentData)
	if err != nil {
		t.Fatalf("cowDeletePrefix: %v", err)
	}
	bf.fsTreeRoot = newRoot

	compressed := compress(t, payload)
	physData, _, err := bf.sm.allocDataBytes(uint64(len(compressed)), uint64(bf.sb.sectorSize))
	if err != nil {
		t.Fatalf("allocDataBytes: %v", err)
	}
	if _, err := bf.f.WriteAt(compressed, bf.partOffset+int64(physData)); err != nil {
		t.Fatalf("WriteAt compressed payload: %v", err)
	}
	logData := physToLog(bf.sb, physData)
	extData := encodeRegularExtentData(
		logData,
		uint64(len(compressed)),
		uint64(len(payload)),
		0,
		uint64(len(payload)),
		bf.sb.generation+1,
		codec,
	)
	newRoot, err = cowInsert(nil, bf.f, bf.partOffset, bf.sb, bf.sm, bf.fsTreeRoot, key{ino, typeExtentData, 0}, extData)
	if err != nil {
		t.Fatalf("cowInsert compressed extent: %v", err)
	}
	bf.fsTreeRoot = newRoot
	if err := updateFsTreeRoot(bf.f, bf.partOffset, bf.sb, bf.sm, bf.fsTreeRoot); err != nil {
		t.Fatalf("updateFsTreeRoot: %v", err)
	}

	got, err := fs.ReadFile(fname)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("%s read mismatch: got %d bytes, want %d bytes", name, len(got), len(payload))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
