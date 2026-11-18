package filesystem_btrfs

import (
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
