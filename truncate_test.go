package filesystem_btrfs

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"
)

func TestTruncate_GrowSparse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, 8*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	body := []byte("five!")
	if err := fs.WriteFile("/grow", body, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	before, _ := bf.ExtendedStat("/grow")
	freeBefore := totalFreeSpace(bf)

	const newSize = 64 * 1024 // 64 KiB
	if err := bf.Truncate("/grow", newSize); err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	after, _ := bf.ExtendedStat("/grow")
	if after.Size != newSize {
		t.Errorf("Size = %d, want %d", after.Size, newSize)
	}
	// Growing a file via sparse extension must not consume disk sectors for
	// the new region; only metadata COW (a few node blocks) is allowed.
	freeAfter := totalFreeSpace(bf)
	const maxMetadataOverhead = 64 * 1024
	if freeBefore-freeAfter > maxMetadataOverhead {
		t.Errorf("grow consumed too much space: before=%d after=%d delta=%d > %d (sparse extension should be free)",
			freeBefore, freeAfter, freeBefore-freeAfter, maxMetadataOverhead)
	}

	// Read-back: first 5 bytes are the original body, the rest is zeros.
	got, err := fs.ReadFile("/grow")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != newSize {
		t.Fatalf("ReadFile len = %d, want %d", len(got), newSize)
	}
	if !bytes.Equal(got[:5], body) {
		t.Errorf("first 5 bytes corrupted: got %q want %q", got[:5], body)
	}
	wantZeros := make([]byte, newSize-5)
	if !bytes.Equal(got[5:], wantZeros) {
		t.Errorf("grown region not zero-filled: first non-zero at offset %d", firstNonZero(got[5:])+5)
	}
	_ = before
}

func TestTruncate_ShrinkInline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, 8*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	body := []byte("the quick brown fox jumps over the lazy dog")
	if err := fs.WriteFile("/inline", body, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	const newSize = 10
	if err := bf.Truncate("/inline", newSize); err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	st, _ := bf.ExtendedStat("/inline")
	if st.Size != newSize {
		t.Errorf("Size = %d, want %d", st.Size, newSize)
	}
	got, err := fs.ReadFile("/inline")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, body[:newSize]) {
		t.Errorf("ReadFile after shrink: got %q want %q", got, body[:newSize])
	}
}

func TestTruncate_ShrinkRegularExtent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, 8*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	// 8 KiB payload — above inline threshold, fits in a single regular
	// extent.
	body := make([]byte, 8*1024)
	for i := range body {
		body[i] = byte(i)
	}
	if err := fs.WriteFile("/big", body, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	const newSize = 3000
	if err := bf.Truncate("/big", newSize); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	st, _ := bf.ExtendedStat("/big")
	if st.Size != newSize {
		t.Errorf("Size = %d, want %d", st.Size, newSize)
	}
	got, err := fs.ReadFile("/big")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, body[:newSize]) {
		t.Errorf("content mismatch after shrink — first divergence at offset %d", firstDiff(got, body[:newSize]))
	}
}

func TestTruncate_ShrinkToZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, 8*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	if err := fs.WriteFile("/zero", []byte("non-empty"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := bf.Truncate("/zero", 0); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	st, _ := bf.ExtendedStat("/zero")
	if st.Size != 0 {
		t.Errorf("Size = %d, want 0", st.Size)
	}
	got, err := fs.ReadFile("/zero")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ReadFile after Truncate(0): got %d bytes, want 0", len(got))
	}
}

func TestTruncate_NoChangeStillBumpsMTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	body := []byte("hello")
	if err := fs.WriteFile("/x", body, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	before, _ := bf.ExtendedStat("/x")
	sleepShort(t)
	if err := bf.Truncate("/x", int64(len(body))); err != nil {
		t.Fatalf("Truncate (same size): %v", err)
	}
	after, _ := bf.ExtendedStat("/x")
	if !after.CTime.After(before.CTime) {
		t.Errorf("ctime didn't advance on same-size truncate: %v → %v", before.CTime, after.CTime)
	}
}

func TestTruncate_RejectsNegativeAndDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	if err := fs.WriteFile("/file", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := bf.Truncate("/file", -1); err == nil {
		t.Errorf("Truncate(-1) unexpectedly succeeded")
	}
	if err := fs.MkDir("/d", 0o755); err != nil {
		t.Fatalf("MkDir: %v", err)
	}
	if err := bf.Truncate("/d", 0); err == nil {
		t.Errorf("Truncate on directory unexpectedly succeeded")
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────

func totalFreeSpace(fs *btrfsFS) uint64 {
	var total uint64
	for _, fe := range fs.sm.freeExts {
		total += fe.size
	}
	return total
}

func firstNonZero(b []byte) int {
	for i, x := range b {
		if x != 0 {
			return i
		}
	}
	return -1
}

func firstDiff(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return n
	}
	return -1
}

func sleepShort(t *testing.T) {
	t.Helper()
	// Two ms is enough to make Go's time.Now() advance reliably on every
	// platform we run on (typical clock resolution is microseconds).
	time.Sleep(2 * time.Millisecond)
}
