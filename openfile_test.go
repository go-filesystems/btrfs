package filesystem_btrfs

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

func offsetPattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i>>12)
	}
	return b
}

func bigBtrfs(t testing.TB, fileSize int) (filesystem.Filesystem, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "big.img")
	fs, err := Format(path, int64(fileSize)*2+64<<20, FormatConfig{Label: "big"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	data := offsetPattern(fileSize)
	if err := fs.WriteFile("/big", data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.WriteFile("/small", []byte("hello world"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Cleanup(func() { fs.Close() })
	return fs, data
}

// A READ at an offset past 64 MiB must not need the file whole.
func TestOpenFileReadsARangePast64MiB(t *testing.T) {
	const fileSize = 72 << 20
	fs, want := bigBtrfs(t, fileSize)
	o, ok := fs.(filesystem.Opener)
	if !ok {
		t.Fatal("the driver is not a filesystem.Opener")
	}
	f, err := o.OpenFile("/big")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if f.Size() != fileSize {
		t.Fatalf("Size = %d", f.Size())
	}
	for _, off := range []int64{0, 4095, 64 << 20, 66<<20 + 123, fileSize - 10} {
		buf := make([]byte, 4096)
		n, err := f.ReadAt(buf, off)
		wantN := int64(len(buf))
		if fileSize-off < wantN {
			wantN = fileSize - off
		}
		if int64(n) != wantN || (n < len(buf)) != errors.Is(err, io.EOF) || (n == len(buf) && err != nil) {
			t.Fatalf("ReadAt(%d) = %d, %v; want %d", off, n, err, wantN)
		}
		if !bytes.Equal(buf[:n], want[off:off+int64(n)]) {
			t.Fatalf("ReadAt(%d): wrong bytes", off)
		}
	}
	// A window that spans several extents, and the whole file in one read.
	span := make([]byte, 3<<20)
	if n, err := f.ReadAt(span, 10<<20+17); n != len(span) || err != nil || !bytes.Equal(span, want[10<<20+17:10<<20+17+len(span)]) {
		t.Fatalf("a spanning read = %d, %v", n, err)
	}
	if n, err := f.ReadAt(make([]byte, 8), fileSize); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt at the end = %d, %v", n, err)
	}
	if _, err := f.ReadAt(make([]byte, 8), -1); err == nil {
		t.Error("a negative offset was read")
	}
	if err := f.Close(); err != nil {
		t.Error(err)
	}
}

func TestOpenFileInlineEmptyAndRefusals(t *testing.T) {
	fs, _ := bigBtrfs(t, 1<<20)
	o := fs.(filesystem.Opener)
	f, err := o.OpenFile("/small")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if n, err := f.ReadAt(buf, 6); n != 5 || err != nil || string(buf) != "world" {
		t.Errorf("inline ReadAt = %d %v %q", n, err, buf)
	}
	if n, err := f.ReadAt(make([]byte, 20), 0); n != 11 || !errors.Is(err, io.EOF) {
		t.Errorf("inline ReadAt past the end = %d %v", n, err)
	}
	if err := fs.WriteFile("/empty", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	e, err := o.OpenFile("/empty")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := e.ReadAt(make([]byte, 4), 0); n != 0 || !errors.Is(err, io.EOF) || e.Size() != 0 {
		t.Errorf("an empty file: %d %v size %d", n, err, e.Size())
	}
	if _, err := o.OpenFile("/nope"); err == nil {
		t.Error("a missing file was opened")
	}
	if _, err := o.OpenFile("/"); err == nil {
		t.Error("a directory was opened")
	}
}

func TestCopyWindow(t *testing.T) {
	dst := bytes.Repeat([]byte{'.'}, 8)        // the window [10,18)
	copyWindow(dst, 10, 6, []byte("abcdefgh")) // src [6,14): "efgh" lands at the start
	copyWindow(dst, 10, 16, []byte("wxyz"))    // src [16,20): "wx" lands at the end
	copyWindow(dst, 10, 30, []byte("never"))   // past the window
	copyWindow(dst, 10, 0, []byte("beforeit")) // before the window
	if string(dst) != "efgh..wx" {
		t.Fatalf("window = %q", dst)
	}
}

// BenchmarkReadRangeOfBigFile: a 1 MiB read at 66 MiB of a 72 MiB file, through
// OpenFile/ReadAt and through ReadFile. Allocations are exact; times are not
// under load.
func BenchmarkReadRangeOfBigFile(b *testing.B) {
	fs, _ := bigBtrfs(b, 72<<20)
	b.Run("OpenFile-ReadAt", func(b *testing.B) {
		f, err := fs.(filesystem.Opener).OpenFile("/big")
		if err != nil {
			b.Fatal(err)
		}
		buf := make([]byte, 1<<20)
		b.SetBytes(int64(len(buf)))
		b.ReportAllocs()
		for b.Loop() {
			if _, err := f.ReadAt(buf, 66<<20); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("ReadFile-whole", func(b *testing.B) {
		b.SetBytes(1 << 20)
		b.ReportAllocs()
		for b.Loop() {
			data, err := fs.ReadFile("/big")
			if err != nil {
				b.Fatal(err)
			}
			_ = data[66<<20 : 67<<20]
		}
	})
}

// A compressed extent is decompressed whole and the window is copied out of it,
// from the extent's own offset: the same crafted file as TestZlib_ReadCompressedExtent,
// with the item using only [100, 100+n) of the decompressed extent.
func TestOpenFileCompressedExtent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, 8*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)
	plain := bytes.Repeat([]byte("zlib-compresses-this-well-"), 256)
	const extOff = 100
	used := plain[extOff : extOff+5000]
	if err := fs.WriteFile("/zfile.bin", make([]byte, len(used)), 0o644); err != nil {
		t.Fatalf("WriteFile placeholder: %v", err)
	}
	st, err := fs.Stat("/zfile.bin")
	if err != nil {
		t.Fatal(err)
	}
	ino := st.Inode()
	freeInodeExtents(bf.f, bf.partOffset, bf.sb, bf.sm, bf.fsTreeRoot, ino)
	newRoot, err := cowDeletePrefix(nil, bf.f, bf.partOffset, bf.sb, bf.sm, bf.fsTreeRoot, ino, typeExtentData)
	if err != nil {
		t.Fatal(err)
	}
	bf.fsTreeRoot = newRoot
	compressed := zlibCompress(t, plain)
	physData, _, err := bf.sm.allocDataBytes(uint64(len(compressed)), uint64(bf.sb.sectorSize))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bf.f.WriteAt(compressed, bf.partOffset+int64(physData)); err != nil {
		t.Fatal(err)
	}
	extData := encodeRegularExtentData(physToLog(bf.sb, physData), uint64(len(compressed)), uint64(len(plain)), extOff, uint64(len(used)), bf.sb.generation+1, compressionZlib)
	if newRoot, err = cowInsert(nil, bf.f, bf.partOffset, bf.sb, bf.sm, bf.fsTreeRoot, key{ino, typeExtentData, 0}, extData); err != nil {
		t.Fatal(err)
	}
	bf.fsTreeRoot = newRoot
	if err := updateFsTreeRoot(bf.f, bf.partOffset, bf.sb, bf.sm, bf.fsTreeRoot); err != nil {
		t.Fatal(err)
	}

	f, err := bf.OpenFile("/zfile.bin")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ off, n int }{{0, 100}, {1234, 700}, {4900, 100}} {
		buf := make([]byte, tc.n)
		if n, err := f.ReadAt(buf, int64(tc.off)); n != tc.n || err != nil || !bytes.Equal(buf, used[tc.off:tc.off+tc.n]) {
			t.Fatalf("compressed ReadAt(%d, %d) = %d, %v", tc.off, tc.n, n, err)
		}
	}
	whole := make([]byte, len(used)+10)
	if n, err := f.ReadAt(whole, 0); n != len(used) || !errors.Is(err, io.EOF) || !bytes.Equal(whole[:n], used) {
		t.Fatalf("compressed whole ReadAt = %d, %v", n, err)
	}
}

// FuzzOpenFileForgedExtents: whatever an EXTENT_DATA item says (offsets, sizes,
// compression, disk addresses), a ReadAt answers or fails; it does not panic,
// wrap an offset or allocate past what the window and the device allow.
func FuzzOpenFileForgedExtents(f *testing.F) {
	path := filepath.Join(f.TempDir(), "fz.img")
	fsys, err := Format(path, 4*1024*1024, FormatConfig{})
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { fsys.Close() })
	bf := fsys.(*btrfsFS)
	f.Add(encodeRegularExtentData(1<<20, 4096, 4096, 0, 4096, 1, 0), uint64(0), uint16(100), uint64(10))
	f.Add(encodeRegularExtentData(1<<20, 512, 4096, 100, 2000, 1, compressionZlib), uint64(0), uint16(64), uint64(0))
	f.Add(encodeRegularExtentData(^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0), 1, compressionZstd), uint64(1<<63), uint16(8), uint64(1<<62))
	f.Fuzz(func(t *testing.T, data []byte, itemOff uint64, n uint16, readOff uint64) {
		file := &btrfsFile{fs: bf, num: 99, size: 1 << 20,
			items: []prefixMatch{{k: key{99, typeExtentData, itemOff}, data: data}}}
		buf := make([]byte, int(n))
		_, _ = file.ReadAt(buf, int64(readOff%(2<<20)))
	})
}
