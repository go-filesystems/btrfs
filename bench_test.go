package filesystem_btrfs_test

// Performance benchmarks for the standard filesystem operations, exercised
// through the public filesystem.Filesystem interface. They establish a
// throughput baseline for the pure-Go Btrfs driver, mirroring the benchmark
// set merged in the ext4 module so the two drivers can be tracked side by
// side.
//
// Run:  GOWORK=off go test -bench=. -benchmem -run=^$
// A file-backed image under b.TempDir() is used so the numbers include real
// block I/O, not just in-memory work.

import (
	"fmt"
	"path/filepath"
	"testing"

	btrfs "github.com/go-filesystems/btrfs"
	filesystem "github.com/go-filesystems/interface"
)

const (
	// 256 MiB — well above the 1 MiB btrfs minimum; room for large files and
	// many inodes. Must be a multiple of the 4 KiB node size (it is).
	benchImageSize = int64(256 << 20)
	benchBigFile   = 8 << 20 // 8 MiB sequential payload
)

// newBenchFS formats a fresh image and returns the mounted filesystem.
func newBenchFS(b *testing.B) filesystem.Filesystem {
	b.Helper()
	path := filepath.Join(b.TempDir(), "bench.img")
	fs, err := btrfs.Format(path, benchImageSize, btrfs.FormatConfig{Label: "bench"})
	if err != nil {
		b.Fatalf("Format: %v", err)
	}
	return fs
}

// BenchmarkFormat measures the cost of laying down a fresh filesystem.
func BenchmarkFormat(b *testing.B) {
	dir := b.TempDir()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		path := filepath.Join(dir, fmt.Sprintf("fmt-%d.img", i))
		fs, err := btrfs.Format(path, benchImageSize, btrfs.FormatConfig{})
		if err != nil {
			b.Fatalf("Format: %v", err)
		}
		fs.Close()
	}
}

// BenchmarkWriteFileSeq measures sequential write throughput (overwriting a
// single large file).
func BenchmarkWriteFileSeq(b *testing.B) {
	fs := newBenchFS(b)
	defer fs.Close()
	data := make([]byte, benchBigFile)
	for i := range data {
		data[i] = byte(i)
	}
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := fs.WriteFile("/big.bin", data, 0o644); err != nil {
			b.Fatalf("WriteFile: %v", err)
		}
	}
}

// BenchmarkReadFileSeq measures sequential read throughput of a large file.
func BenchmarkReadFileSeq(b *testing.B) {
	fs := newBenchFS(b)
	defer fs.Close()
	data := make([]byte, benchBigFile)
	if err := fs.WriteFile("/big.bin", data, 0o644); err != nil {
		b.Fatalf("setup WriteFile: %v", err)
	}
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := fs.ReadFile("/big.bin")
		if err != nil {
			b.Fatalf("ReadFile: %v", err)
		}
		if len(got) != len(data) {
			b.Fatalf("short read: %d", len(got))
		}
	}
}

// BenchmarkStat measures path lookup + inode read latency.
func BenchmarkStat(b *testing.B) {
	fs := newBenchFS(b)
	defer fs.Close()
	for _, d := range []string{"/a", "/a/b", "/a/b/c"} {
		if err := fs.MkDir(d, 0o755); err != nil {
			b.Fatalf("MkDir %s: %v", d, err)
		}
	}
	if err := fs.WriteFile("/a/b/c/file.txt", []byte("x"), 0o644); err != nil {
		b.Fatalf("setup WriteFile: %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := fs.Stat("/a/b/c/file.txt"); err != nil {
			b.Fatalf("Stat: %v", err)
		}
	}
}

// BenchmarkListDir measures directory enumeration for a directory holding
// many entries.
func BenchmarkListDir(b *testing.B) {
	fs := newBenchFS(b)
	defer fs.Close()
	const entries = 200
	if err := fs.MkDir("/d", 0o755); err != nil {
		b.Fatalf("MkDir: %v", err)
	}
	for i := 0; i < entries; i++ {
		if err := fs.WriteFile(fmt.Sprintf("/d/f%04d", i), nil, 0o644); err != nil {
			b.Fatalf("setup file %d: %v", i, err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := fs.ListDir("/d")
		if err != nil {
			b.Fatalf("ListDir: %v", err)
		}
		if len(got) < entries {
			b.Fatalf("ListDir returned %d entries", len(got))
		}
	}
}

// BenchmarkCreateFiles measures small-file creation throughput. Each iteration
// creates a fixed batch on a freshly formatted image (setup excluded from the
// timer) and reports files/op.
func BenchmarkCreateFiles(b *testing.B) {
	const batch = 200
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		fs := newBenchFS(b)
		b.StartTimer()
		for j := 0; j < batch; j++ {
			if err := fs.WriteFile(fmt.Sprintf("/f%05d", j), nil, 0o644); err != nil {
				b.Fatalf("WriteFile %d: %v", j, err)
			}
		}
		b.StopTimer()
		fs.Close()
		b.StartTimer()
	}
	b.ReportMetric(200, "files/op")
}

// BenchmarkDeleteFiles measures unlink throughput.
func BenchmarkDeleteFiles(b *testing.B) {
	const batch = 200
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		fs := newBenchFS(b)
		for j := 0; j < batch; j++ {
			if err := fs.WriteFile(fmt.Sprintf("/f%05d", j), nil, 0o644); err != nil {
				b.Fatalf("setup WriteFile %d: %v", j, err)
			}
		}
		b.StartTimer()
		for j := 0; j < batch; j++ {
			if err := fs.DeleteFile(fmt.Sprintf("/f%05d", j)); err != nil {
				b.Fatalf("DeleteFile %d: %v", j, err)
			}
		}
		b.StopTimer()
		fs.Close()
		b.StartTimer()
	}
	b.ReportMetric(200, "files/op")
}
