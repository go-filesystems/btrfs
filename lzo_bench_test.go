package filesystem_btrfs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
)

// testdata/lzo-extent-128k.bin is a 128 KiB btrfs LZO extent (32 pages, framed
// as the kernel does) whose segments were compressed by liblzo2 2.10
// lzo1x_1_compress, on a text-like corpus (ratio 1.88).
const lzoExtentPlainSHA256 = "3a09c2730b7eff492517a9ef2df00156d37bf59a6018539f73fa577e13e8f215"

func TestLzoExtentFromLiblzo2(t *testing.T) {
	src, err := os.ReadFile("testdata/lzo-extent-128k.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, err := decompressLzo(src, 128<<10)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(got)
	if len(got) != 128<<10 || hex.EncodeToString(sum[:]) != lzoExtentPlainSHA256 {
		t.Fatalf("decoded %d bytes, sha256 %x", len(got), sum)
	}
	// ram_bytes smaller than the page-multiple output is refused, and a
	// ram_bytes that is not a page multiple takes the scratch-buffer tail.
	if _, err := decompressLzo(src, 128<<10-1); err == nil {
		t.Fatal("ram_bytes one short: want an error")
	}
}

// BenchmarkLzoExtent is the decode of one 128 KiB extent: the btrfs side of an
// LZO read (framing, per-segment decoder call, output assembly).
func BenchmarkLzoExtent(b *testing.B) {
	src, err := os.ReadFile("testdata/lzo-extent-128k.bin")
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(128 << 10)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := decompressLzo(src, 128<<10); err != nil {
			b.Fatal(err)
		}
	}
}

// TestLzoExtentInto: decoding straight into the caller's window gives the same
// bytes as decompressLzo, never touches a byte outside the window (the decoder
// scribbles past its output inside the slice it is given, so the window must
// bound it), and refuses a window of the wrong size.
func TestLzoExtentInto(t *testing.T) {
	src, err := os.ReadFile("testdata/lzo-extent-128k.bin")
	if err != nil {
		t.Fatal(err)
	}
	want, err := decompressLzo(src, 128<<10)
	if err != nil {
		t.Fatal(err)
	}
	const guard = 64
	buf := make([]byte, guard+len(want)+guard)
	for i := range buf {
		buf[i] = 0xA5
	}
	win := buf[guard : guard+len(want) : guard+len(want)]
	if err := decompressLzoInto(win, src); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(win, want) {
		t.Fatal("window differs from decompressLzo")
	}
	for i := 0; i < guard; i++ {
		if buf[i] != 0xA5 || buf[guard+len(want)+i] != 0xA5 {
			t.Fatalf("byte outside the window overwritten at guard offset %d", i)
		}
	}
	if err := decompressLzoInto(make([]byte, len(want)-1), src); err == nil {
		t.Fatal("window one byte short: want an error")
	}
	if err := decompressLzoInto(make([]byte, len(want)+1), src); err == nil {
		t.Fatal("window one byte long: want an error")
	}
}

func BenchmarkLzoExtentInto(b *testing.B) {
	src, err := os.ReadFile("testdata/lzo-extent-128k.bin")
	if err != nil {
		b.Fatal(err)
	}
	dst := make([]byte, 128<<10)
	b.SetBytes(128 << 10)
	b.ReportAllocs()
	for b.Loop() {
		if err := decompressLzoInto(dst, src); err != nil {
			b.Fatal(err)
		}
	}
}
