package filesystem_btrfs

import (
	"bytes"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// kernelShapedZstdExtent returns what fs/btrfs/zstd.c stores for one 128 KiB
// input: a single zstd frame followed by zero padding up to the sector size.
func kernelShapedZstdExtent(t *testing.T, plain []byte) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	frame := enc.EncodeAll(plain, nil)
	padded := make([]byte, (len(frame)+4095)/4096*4096)
	copy(padded, frame)
	if len(padded) == len(frame) {
		padded = append(padded, make([]byte, 4096)...)
	}
	return padded
}

// A kernel extent holds the frame and then zero padding. Probing one byte past
// ram_bytes made the decoder parse the padding as a second frame
// ("magic number mismatch"). This must fail without the exact-ramBytes read.
func TestDecompressZstd_KernelSectorPadding(t *testing.T) {
	plain := make([]byte, 128<<10)
	for i := range plain {
		plain[i] = byte('A' + (i/4096)%26)
	}
	src := kernelShapedZstdExtent(t, plain)
	got, err := decompressZstd(src, uint64(len(plain)))
	if err != nil {
		t.Fatalf("decompressZstd on a padded kernel extent: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("padded extent decoded to %d bytes, want %d identical", len(got), len(plain))
	}
}

// ram_bytes clamped to a window shorter than the frame: stop there.
func TestDecompressZstd_PaddedPartialWindow(t *testing.T) {
	plain := make([]byte, 128<<10)
	for i := range plain {
		plain[i] = byte(i * 7)
	}
	src := kernelShapedZstdExtent(t, plain)
	got, err := decompressZstd(src, 4096)
	if err != nil {
		t.Fatalf("partial window: %v", err)
	}
	if !bytes.Equal(got, plain[:4096]) {
		t.Fatalf("partial window mismatch (%d bytes)", len(got))
	}
}

// The output stays bounded by ram_bytes even when the frame holds more.
func TestDecompressZstd_BoundedByRamBytes(t *testing.T) {
	plain := make([]byte, 128<<10)
	src := kernelShapedZstdExtent(t, plain)
	got, err := decompressZstd(src, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1000 {
		t.Fatalf("got %d bytes, want exactly the 1000 asked for", len(got))
	}
}
