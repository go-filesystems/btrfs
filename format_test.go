package filesystem_btrfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const btrfsTestSize = 2 * 1024 * 1024 // 2 MiB (≥ 1 MiB minimum)

var errBtrfsBoom = errors.New("btrfs format injected error")

// ── Validation errors ─────────────────────────────────────────────────────

func TestBtrfsFmt_NotMultipleOfNodeSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.img")
	if _, err := Format(path, 4097, FormatConfig{}); err == nil {
		t.Error("expected error: size not a multiple of node size")
	}
}

func TestBtrfsFmt_TooSmall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tiny.img")
	if _, err := Format(path, fmtNodeSize, FormatConfig{}); err == nil {
		t.Error("expected error: size too small")
	}
}

// ── Happy-path basics ─────────────────────────────────────────────────────

func TestBtrfsFmt_CreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs.Close()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("image file not created: %v", err)
	}
}

func TestBtrfsFmt_FileSizePreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != btrfsTestSize {
		t.Errorf("size = %d, want %d", info.Size(), btrfsTestSize)
	}
}

func TestBtrfsFmt_TruncatesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.img")
	if err := os.WriteFile(path, make([]byte, 512*1024), 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs.Close()
}

func TestBtrfsFmt_StatRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	st, err := fs.Stat("/")
	if err != nil {
		t.Fatalf("Stat /: %v", err)
	}
	if st.Mode()&0xF000 != 0x4000 {
		t.Errorf("root mode 0x%04X is not a directory", st.Mode())
	}
}

func TestBtrfsFmt_ListDirRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir /: %v", err)
	}
	// parseDirItemsAll filters out "." and ".." — freshly formatted root is empty.
	if len(entries) != 0 {
		t.Errorf("expected empty root dir, got %d entries", len(entries))
	}
}

func TestBtrfsFmt_WriteReadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	const data = "hello from btrfs Format\n"
	if err := fs.WriteFile("/hello.txt", []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := fs.ReadFile("/hello.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != data {
		t.Errorf("got %q, want %q", got, data)
	}
}

func TestBtrfsFmt_CustomLabel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{Label: "myvolume"})
	if err != nil {
		t.Fatalf("Format with label: %v", err)
	}
	fs.Close()
	fs2, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open after Format: %v", err)
	}
	defer fs2.Close()
}

func TestBtrfsFmt_CustomUUID(t *testing.T) {
	var uuid [16]byte
	for i := range uuid {
		uuid[i] = byte(i + 1)
	}
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{UUID: uuid})
	if err != nil {
		t.Fatalf("Format with UUID: %v", err)
	}
	fs.Close()
}

func TestBtrfsFmt_ReOpenAndWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	{
		fs, err := Format(path, btrfsTestSize, FormatConfig{})
		if err != nil {
			t.Fatalf("Format: %v", err)
		}
		if err := fs.WriteFile("/data.bin", []byte("original"), 0o600); err != nil {
			fs.Close()
			t.Fatalf("WriteFile: %v", err)
		}
		fs.Close()
	}
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	got, err := fs.ReadFile("/data.bin")
	if err != nil {
		t.Fatalf("ReadFile after re-open: %v", err)
	}
	if string(got) != "original" {
		t.Errorf("got %q, want %q", got, "original")
	}
}

// ── Error injection ───────────────────────────────────────────────────────

type btrfsCountingFile struct {
	inner     btrfsFormatFile
	writeCall int
	failAt    int
}

func (f *btrfsCountingFile) WriteAt(p []byte, off int64) (int, error) {
	f.writeCall++
	if f.writeCall == f.failAt {
		return 0, errBtrfsBoom
	}
	return f.inner.WriteAt(p, off)
}
func (f *btrfsCountingFile) Truncate(n int64) error { return f.inner.Truncate(n) }
func (f *btrfsCountingFile) Close() error           { return f.inner.Close() }

type btrfsTruncFailFile struct{}

func (f *btrfsTruncFailFile) WriteAt([]byte, int64) (int, error) { return 0, nil }
func (f *btrfsTruncFailFile) Truncate(int64) error               { return errBtrfsBoom }
func (f *btrfsTruncFailFile) Close() error                       { return nil }

type btrfsCloseFailFile struct{ inner btrfsFormatFile }

func (f *btrfsCloseFailFile) WriteAt(p []byte, off int64) (int, error) {
	return f.inner.WriteAt(p, off)
}
func (f *btrfsCloseFailFile) Truncate(n int64) error { return f.inner.Truncate(n) }
func (f *btrfsCloseFailFile) Close() error           { return errBtrfsBoom }

func injectBtrfsCounting(t *testing.T, failAt int) {
	t.Helper()
	old := btrfsFormatOpenFile
	btrfsFormatOpenFile = func(path string) (btrfsFormatFile, error) {
		inner, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return nil, err
		}
		return &btrfsCountingFile{inner: inner, failAt: failAt}, nil
	}
	t.Cleanup(func() { btrfsFormatOpenFile = old })
}

func btrfsExpectBoom(t *testing.T) {
	t.Helper()
	if _, err := Format(filepath.Join(t.TempDir(), "x.img"), btrfsTestSize, FormatConfig{}); !errors.Is(err, errBtrfsBoom) {
		t.Fatalf("expected errBtrfsBoom, got %v", err)
	}
}

func TestBtrfsFmt_OpenFileFails(t *testing.T) {
	old := btrfsFormatOpenFile
	btrfsFormatOpenFile = func(string) (btrfsFormatFile, error) { return nil, errBtrfsBoom }
	t.Cleanup(func() { btrfsFormatOpenFile = old })
	btrfsExpectBoom(t)
}

func TestBtrfsFmt_TruncateFails(t *testing.T) {
	old := btrfsFormatOpenFile
	btrfsFormatOpenFile = func(string) (btrfsFormatFile, error) { return &btrfsTruncFailFile{}, nil }
	t.Cleanup(func() { btrfsFormatOpenFile = old })
	btrfsExpectBoom(t)
}

func TestBtrfsFmt_RandReadFails(t *testing.T) {
	old := btrfsFormatRandRead
	btrfsFormatRandRead = func([]byte) (int, error) { return 0, errBtrfsBoom }
	t.Cleanup(func() { btrfsFormatRandRead = old })
	if _, err := Format(filepath.Join(t.TempDir(), "x.img"), btrfsTestSize, FormatConfig{}); !errors.Is(err, errBtrfsBoom) {
		t.Fatalf("expected errBtrfsBoom, got %v", err)
	}
}

// Writes: 1=SB, 2=chunk leaf, 3=root leaf, 4=fs leaf
func TestBtrfsFmt_WriteSBFails(t *testing.T)        { injectBtrfsCounting(t, 1); btrfsExpectBoom(t) }
func TestBtrfsFmt_WriteChunkLeafFails(t *testing.T) { injectBtrfsCounting(t, 2); btrfsExpectBoom(t) }
func TestBtrfsFmt_WriteRootLeafFails(t *testing.T)  { injectBtrfsCounting(t, 3); btrfsExpectBoom(t) }
func TestBtrfsFmt_WriteFSLeafFails(t *testing.T)    { injectBtrfsCounting(t, 4); btrfsExpectBoom(t) }

func TestBtrfsFmt_CloseFails(t *testing.T) {
	old := btrfsFormatOpenFile
	btrfsFormatOpenFile = func(path string) (btrfsFormatFile, error) {
		inner, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return nil, err
		}
		return &btrfsCloseFailFile{inner: inner}, nil
	}
	t.Cleanup(func() { btrfsFormatOpenFile = old })
	btrfsExpectBoom(t)
}

func TestBtrfsFmt_OpenFSFails(t *testing.T) {
	old := btrfsFormatOpenFS
	btrfsFormatOpenFS = func(string, int) (FS, error) { return nil, errBtrfsBoom }
	t.Cleanup(func() { btrfsFormatOpenFS = old })
	btrfsExpectBoom(t)
}
