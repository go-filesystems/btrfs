package filesystem_btrfs

import (
	"path/filepath"
	"testing"
	"time"
)

func TestExtendedStat_FreshFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	before := time.Now().UTC().Add(-time.Second)
	if err := fs.WriteFile("/hello.txt", []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	after := time.Now().UTC().Add(time.Second)

	st, err := bf.ExtendedStat("/hello.txt")
	if err != nil {
		t.Fatalf("ExtendedStat: %v", err)
	}
	if !st.IsRegular() {
		t.Errorf("IsRegular() false; mode = 0x%04x", st.Mode)
	}
	if st.Size != 5 {
		t.Errorf("Size = %d, want 5", st.Size)
	}
	if st.NLink != 1 {
		t.Errorf("NLink = %d, want 1", st.NLink)
	}
	if st.NBytes != 5 {
		t.Errorf("NBytes = %d, want 5 (inline ram_bytes)", st.NBytes)
	}
	if st.Mode&0o777 != 0o644 {
		t.Errorf("Mode perm bits = 0o%o, want 0o644", st.Mode&0o777)
	}
	if st.Generation == 0 {
		t.Errorf("Generation = 0, want >0")
	}
	if st.TransID != st.Generation {
		t.Errorf("TransID (%d) != Generation (%d) at creation", st.TransID, st.Generation)
	}
	if st.Sequence != 0 {
		t.Errorf("Sequence = %d, want 0 at creation", st.Sequence)
	}
	if st.Flags&inodeFlagNoDataSum == 0 {
		t.Errorf("Flags = 0x%x, NODATASUM missing", st.Flags)
	}
	for _, ts := range []struct {
		name string
		t    time.Time
	}{{"atime", st.ATime}, {"ctime", st.CTime}, {"mtime", st.MTime}, {"otime", st.OTime}} {
		if ts.t.Before(before) || ts.t.After(after) {
			t.Errorf("%s = %v, not in [%v, %v]", ts.name, ts.t, before, after)
		}
	}
}

func TestExtendedStat_DirHasNlinkOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)
	if err := fs.MkDir("/d", 0o755); err != nil {
		t.Fatalf("MkDir: %v", err)
	}
	st, err := bf.ExtendedStat("/d")
	if err != nil {
		t.Fatalf("ExtendedStat: %v", err)
	}
	if !st.IsDir() {
		t.Errorf("IsDir() false; mode = 0x%04x", st.Mode)
	}
	if st.NLink != 1 {
		t.Errorf("NLink = %d, want 1 for empty dir (btrfs convention)", st.NLink)
	}
}

func TestExtendedStat_TracksLinkAndOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	if err := fs.WriteFile("/a", []byte("v1"), 0o644); err != nil {
		t.Fatalf("WriteFile v1: %v", err)
	}
	st1, _ := bf.ExtendedStat("/a")
	if st1.NLink != 1 || st1.Sequence != 0 {
		t.Fatalf("after create: NLink=%d Sequence=%d, want 1/0", st1.NLink, st1.Sequence)
	}

	// Hardlink → NLink bumps, Sequence bumps.
	if err := bf.Link("/a", "/b"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	st2, _ := bf.ExtendedStat("/a")
	if st2.NLink != 2 {
		t.Errorf("after Link: NLink = %d, want 2", st2.NLink)
	}
	if st2.Sequence <= st1.Sequence {
		t.Errorf("after Link: Sequence not advanced (%d → %d)", st1.Sequence, st2.Sequence)
	}
	if st2.Generation != st1.Generation {
		t.Errorf("Generation must be immutable: %d → %d", st1.Generation, st2.Generation)
	}

	// Overwrite → TransID and Sequence advance again.
	if err := fs.WriteFile("/a", []byte("v2 — longer payload"), 0o644); err != nil {
		t.Fatalf("WriteFile v2: %v", err)
	}
	st3, _ := bf.ExtendedStat("/a")
	if st3.Sequence <= st2.Sequence {
		t.Errorf("after Overwrite: Sequence not advanced (%d → %d)", st2.Sequence, st3.Sequence)
	}
	if st3.TransID <= st1.TransID {
		t.Errorf("after Overwrite: TransID not advanced from %d, got %d", st1.TransID, st3.TransID)
	}
	if st3.Size != uint64(len("v2 — longer payload")) {
		t.Errorf("after Overwrite: Size = %d, want %d", st3.Size, len("v2 — longer payload"))
	}
}

func TestExtendedStat_SymlinkMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)
	if err := bf.Symlink("/somewhere", "/link"); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	st, err := bf.ExtendedStat("/link")
	if err != nil {
		t.Fatalf("ExtendedStat: %v", err)
	}
	if !st.IsSymlink() {
		t.Errorf("IsSymlink() false; mode = 0x%04x", st.Mode)
	}
}

func TestExtendedStat_NotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)
	if _, err := bf.ExtendedStat("/no/such/file"); err == nil {
		t.Fatal("expected error for nonexistent path")
	}
}
