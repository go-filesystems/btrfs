package filesystem_btrfs

import (
	"bytes"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// zstdFrameOnly is the frame of zstdExtent without its sector padding.
func zstdFrameOnly(t testing.TB) (raw, frame []byte) {
	t.Helper()
	raw, padded := zstdExtent(t)
	n, err := zstdFrameLen(padded)
	if err != nil {
		t.Fatal(err)
	}
	return raw, padded[:n:n]
}

func TestZstdFrameLenFindsTheFrameEnd(t *testing.T) {
	raw, frame := zstdFrameOnly(t)
	_, padded := zstdExtent(t)
	if len(frame) >= len(padded) && len(padded)%4096 != 0 {
		t.Fatal("frame not shorter than padded extent")
	}
	for name, tail := range map[string][]byte{
		"none":    nil,
		"zeros":   make([]byte, 4096),
		"garbage": bytes.Repeat([]byte{0xA5, 0x5A, 0xFF}, 1000),
	} {
		src := append(append([]byte{}, frame...), tail...)
		out, err := decompressZstd(src, uint64(len(raw)))
		if err != nil || !bytes.Equal(out, raw) {
			t.Fatalf("%s tail: err=%v equal=%v", name, err, bytes.Equal(out, raw))
		}
	}
}

func TestZstdSecondFrameIsNotDecoded(t *testing.T) {
	raw, frame := zstdFrameOnly(t)
	second := zstdFrame(17) // a valid empty frame
	src := append(append(append([]byte{}, frame...), second...), make([]byte, 100)...)
	out, err := decompressZstd(src, uint64(len(raw)))
	if err != nil || !bytes.Equal(out, raw) {
		t.Fatalf("err=%v equal=%v", err, bytes.Equal(out, raw))
	}
	// Two full frames: still only the first.
	src = append(append([]byte{}, frame...), frame...)
	out, err = decompressZstd(src, uint64(len(raw)))
	if err != nil || !bytes.Equal(out, raw) {
		t.Fatalf("two full frames: err=%v equal=%v", err, bytes.Equal(out, raw))
	}
}

func TestZstdShortFrameRefused(t *testing.T) {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	frame := enc.EncodeAll(bytes.Repeat([]byte("short "), 20), nil)
	if out, err := decompressZstd(frame, 120); err != nil || len(out) != 120 {
		t.Fatalf("exact frame: err=%v len=%d", err, len(out))
	}
	if _, err := decompressZstd(frame, 121); err == nil {
		t.Fatal("a frame that yields fewer bytes than ram_bytes was accepted")
	}
}

func TestZstdTruncationRefusedEverywhere(t *testing.T) {
	raw, frame := zstdFrameOnly(t)
	for n := 0; n < len(frame); n++ {
		if _, err := decompressZstd(frame[:n], uint64(len(raw))); err == nil {
			t.Fatalf("truncation at %d of %d was accepted", n, len(frame))
		}
	}
}

func TestZstdSkippableFirstRefused(t *testing.T) {
	raw, frame := zstdFrameOnly(t)
	skip := []byte{0x50, 0x2A, 0x4D, 0x18, 4, 0, 0, 0, 1, 2, 3, 4}
	if _, err := decompressZstd(append(skip, frame...), uint64(len(raw))); err == nil {
		t.Fatal("a skippable frame first was accepted")
	}
}

func TestZstdReservedBlockRefused(t *testing.T) {
	raw := make([]byte, 100)
	// magic, descriptor 0 (window follows), window byte, block header type 3 last.
	src := []byte{0x28, 0xB5, 0x2F, 0xFD, 0x00, 0x00, 0x07, 0x00, 0x00}
	if _, err := decompressZstd(src, uint64(len(raw))); err == nil {
		t.Fatal("a reserved block type was accepted")
	}
}

// A frame with no content size (streamed) and several blocks, and one whose
// content size exceeds ram_bytes, allocate in proportion to ram_bytes.
func TestZstdFrameWithoutSizeAndOversizeStayBounded(t *testing.T) {
	raw := bytes.Repeat([]byte("btrfs zstd extent "), 8000)[:zstdMaxWindow]
	var b bytes.Buffer
	w, err := zstd.NewWriter(&b, zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(zstdMaxWindow))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(raw); i += 4096 {
		if _, err := w.Write(raw[i : i+4096]); err != nil {
			t.Fatal(err)
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var h zstd.Header
	if err := h.Decode(b.Bytes()); err != nil || h.HasFCS {
		t.Fatalf("expected a frame without content size: err=%v hasFCS=%v", err, h.HasFCS)
	}
	var out []byte
	got := allocDelta(func() { out, err = decompressZstd(b.Bytes(), uint64(len(raw))) })
	if err != nil || !bytes.Equal(out, raw) {
		t.Fatalf("streamed frame: err=%v equal=%v", err, bytes.Equal(out, raw))
	}
	if got > bombAllocLimit {
		t.Fatalf("allocated %d bytes for a streamed extent", got)
	}
	_, frame := zstdFrameOnly(t)
	got = allocDelta(func() { out, err = decompressZstd(frame, 100) })
	if err != nil || len(out) != 100 {
		t.Fatalf("oversize content: err=%v len=%d", err, len(out))
	}
	if got > bombAllocLimit {
		t.Fatalf("allocated %d bytes with ram_bytes 100", got)
	}
}

func FuzzZstdFrameLen(f *testing.F) {
	raw, frame := zstdFrameOnly(f)
	_ = raw
	f.Add(frame)
	f.Add(append(append([]byte{}, frame...), make([]byte, 4096-len(frame)%4096)...))
	f.Add(append(append([]byte{}, frame...), bytes.Repeat([]byte{0xA5}, 300)...))
	f.Add(append(append([]byte{}, frame...), frame...))
	f.Add(zstdFrame(17))
	f.Fuzz(func(t *testing.T, src []byte) {
		n, err := zstdFrameLen(src)
		if err != nil {
			return
		}
		if n < 0 || n > len(src) {
			t.Fatalf("n=%d outside [0,%d]", n, len(src))
		}
		zd, err := getZstdDecoder()
		if err != nil {
			t.Fatal(err)
		}
		defer putZstdDecoder(zd)
		first, err1 := zd.DecodeAll(src[:n], nil)
		if err1 != nil {
			return // a well-formed layout may still be a corrupt block
		}
		again, err2 := zd.DecodeAll(src[:n], nil)
		if err2 != nil || !bytes.Equal(first, again) {
			t.Fatal("decoding the frame twice differs")
		}
	})
}

func TestZstdFrameLenRefusals(t *testing.T) {
	_, frame := zstdFrameOnly(t)
	for name, src := range map[string][]byte{
		"empty":              nil,
		"zeros":              make([]byte, 4096),
		"bad magic":          append([]byte{1, 2, 3, 4}, frame[4:]...),
		"skippable, a block": {0x50, 0x2A, 0x4D, 0x18, 3, 0, 0, 0, 1, 0, 0},
		"reserved bit":       {0x28, 0xB5, 0x2F, 0xFD, 0x08, 0x00, 0x01, 0x00, 0x00},
	} {
		if n, err := zstdFrameLen(src); err == nil {
			t.Errorf("%s: accepted, n=%d", name, n)
		}
	}
	// An empty last raw block with the checksum flag but no checksum bytes.
	if n, err := zstdFrameLen([]byte{0x28, 0xB5, 0x2F, 0xFD, 0x04, 0x00, 0x01, 0x00, 0x00}); err == nil {
		t.Errorf("missing checksum accepted, n=%d", n)
	}
	if n, err := zstdFrameLen([]byte{0x28, 0xB5, 0x2F, 0xFD, 0x04, 0x00, 0x01, 0x00, 0x00, 1, 2, 3, 4, 9}); err != nil || n != 13 {
		t.Errorf("checksummed frame: n=%d err=%v", n, err)
	}
}

// Both sides of the block guards, asserted on the layout itself.
func TestZstdFrameLenBlockGuards(t *testing.T) {
	// single-segment frame, 1-byte content size: 6 header bytes.
	hdr := []byte{0x28, 0xB5, 0x2F, 0xFD, 0x20, 0x05}
	with := func(block ...byte) []byte { return append(append([]byte{}, hdr...), block...) }
	// last, Raw, size 5 = 5<<3 | 0<<1 | 1 = 0x29.
	if n, err := zstdFrameLen(with(0x29, 0, 0, 1, 2, 3, 4, 5)); err != nil || n != 14 {
		t.Errorf("exact raw block: n=%d err=%v", n, err)
	}
	if _, err := zstdFrameLen(with(0x29, 0, 0, 1, 2, 3, 4)); err == nil {
		t.Error("raw block one byte short accepted")
	}
	// last, RLE, size 5 = 0x2B: ONE content byte.
	if n, err := zstdFrameLen(with(0x2B, 0, 0, 7)); err != nil || n != 10 {
		t.Errorf("RLE block: n=%d err=%v", n, err)
	}
	// last, Reserved (3), size 0 = 0x07.
	if _, err := zstdFrameLen(with(0x07, 0, 0)); err == nil {
		t.Error("reserved block type accepted")
	}
	// not last, empty raw block, then last empty raw block: two blocks.
	if n, err := zstdFrameLen(with(0x00, 0, 0, 0x01, 0, 0)); err != nil || n != 12 {
		t.Errorf("two blocks: n=%d err=%v", n, err)
	}
	if _, err := zstdFrameLen(with(0x00, 0, 0)); err == nil {
		t.Error("frame ending on a non-last block accepted")
	}
	if _, err := zstdFrameLen(with(0x01, 0)); err == nil {
		t.Error("two-byte block header accepted")
	}
	// A skippable frame whose payload looks like a valid block.
	if _, err := zstdFrameLen([]byte{0x5F, 0x2A, 0x4D, 0x18, 3, 0, 0, 0, 0x01, 0, 0}); err == nil {
		t.Error("skippable frame accepted")
	}
	// Garbage in place of the magic: decode error, not a layout walk.
	// (with a valid last empty raw block right after the four bad bytes).
	if _, err := zstdFrameLen([]byte{0, 0, 0, 0, 0x01, 0, 0}); err == nil {
		t.Error("bad magic accepted")
	}
}
