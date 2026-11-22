package filesystem_btrfs

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// zstdExtent returns a 128 KiB compressible buffer and its zstd frame, padded
// with zeros to a 4 KiB sector like a kernel-written extent.
func zstdExtent(tb testing.TB) (raw, frame []byte) {
	tb.Helper()
	rng := rand.New(rand.NewSource(1))
	raw = make([]byte, 128<<10)
	words := []string{"alpha ", "beta ", "gamma ", "delta ", "epsilon ", "zeta "}
	var b bytes.Buffer
	for b.Len() < len(raw) {
		b.WriteString(words[rng.Intn(len(words))])
		if rng.Intn(8) == 0 {
			b.WriteByte(byte(rng.Intn(256)))
		}
	}
	copy(raw, b.Bytes())
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithWindowSize(zstdMaxWindow))
	if err != nil {
		tb.Fatal(err)
	}
	frame = enc.EncodeAll(raw, nil)
	if pad := len(frame) % 4096; pad != 0 {
		frame = append(frame, make([]byte, 4096-pad)...)
	}
	return raw, frame
}

func BenchmarkDecompressZstd(b *testing.B) {
	raw, frame := zstdExtent(b)
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			out, err := decompressZstd(frame, uint64(len(raw)))
			if err != nil || len(out) != len(raw) {
				b.Fatal(err, len(out))
			}
		}
	})
}

func TestDecompressZstdPaddedExtent(t *testing.T) {
	raw, frame := zstdExtent(t)
	out, err := decompressZstd(frame, uint64(len(raw)))
	if err != nil || !bytes.Equal(out, raw) {
		t.Fatalf("padded extent: err=%v equal=%v", err, bytes.Equal(out, raw))
	}
}
