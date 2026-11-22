package filesystem_btrfs

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// zlibExtentData returns 128 KiB of a kind a btrfs extent holds.
func zlibExtentData(kind string) []byte {
	const n = maxDecompressRAM
	r := rand.New(rand.NewPCG(3, 4))
	var b []byte
	switch kind {
	case "synth": // words from a small vocabulary
		vocab := make([][]byte, 2000)
		for i := range vocab {
			w := make([]byte, 2+r.IntN(10))
			for j := range w {
				w[j] = byte('a' + r.IntN(26))
			}
			vocab[i] = w
		}
		for len(b) < n {
			b = append(b, vocab[r.IntN(len(vocab))]...)
			b = append(b, ' ')
		}
	case "gosrc": // the Go sources the toolchain ships
		files, _ := filepath.Glob(filepath.Join(runtime.GOROOT(), "src", "net", "http", "*.go"))
		for _, f := range files {
			if d, err := os.ReadFile(f); err == nil {
				b = append(b, d...)
			}
			if len(b) >= n {
				break
			}
		}
	case "binary": // structured records: small integers, repeated fields
		for len(b) < n {
			rec := make([]byte, 32)
			for i := 0; i < 32; i += 4 {
				rec[i] = byte(r.IntN(8))
				rec[i+1] = byte(r.IntN(256))
			}
			b = append(b, rec...)
		}
	}
	return b[:n]
}

// BenchmarkDecompressExtentZlib decodes one 128 KiB zlib extent the way a read
// does, per data kind and level.
func BenchmarkDecompressExtentZlib(b *testing.B) {
	for _, kind := range []string{"synth", "gosrc", "binary"} {
		for _, level := range []int{3, 9} {
			plain := zlibExtentData(kind)
			var buf bytes.Buffer
			w, _ := zlib.NewWriterLevel(&buf, level)
			w.Write(plain)
			w.Close()
			z := buf.Bytes()
			b.Run(fmt.Sprintf("%s-l%d", kind, level), func(b *testing.B) {
				b.SetBytes(int64(len(plain)))
				b.ReportAllocs()
				for b.Loop() {
					out, err := decompressExtent(z, compressionZlib, uint64(len(plain)))
					if err != nil || len(out) != len(plain) {
						b.Fatal(len(out), err)
					}
				}
			})
		}
	}
}
