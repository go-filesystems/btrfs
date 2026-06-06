package filesystem_btrfs

import (
	"path/filepath"
	"testing"
	"time"
)

func TestChown_UpdatesUIDGID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	if err := fs.WriteFile("/owned.txt", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	before, _ := bf.ExtendedStat("/owned.txt")
	if before.UID != 0 || before.GID != 0 {
		t.Fatalf("freshly created file should be owned by 0/0, got %d/%d", before.UID, before.GID)
	}
	time.Sleep(2 * time.Millisecond)

	const wantUID, wantGID = 1000, 2000
	if err := bf.Chown("/owned.txt", wantUID, wantGID); err != nil {
		t.Fatalf("Chown: %v", err)
	}
	after, err := bf.ExtendedStat("/owned.txt")
	if err != nil {
		t.Fatalf("ExtendedStat after Chown: %v", err)
	}
	if after.UID != wantUID || after.GID != wantGID {
		t.Errorf("Chown didn't stick: uid=%d gid=%d, want %d/%d", after.UID, after.GID, wantUID, wantGID)
	}
	if !after.CTime.After(before.CTime) {
		t.Errorf("ctime didn't advance after Chown: %v → %v", before.CTime, after.CTime)
	}
	if after.MTime != before.MTime {
		t.Errorf("Chown should not touch mtime: %v → %v", before.MTime, after.MTime)
	}
	if after.Sequence <= before.Sequence {
		t.Errorf("sequence didn't advance after Chown: %d → %d", before.Sequence, after.Sequence)
	}
}

func TestChmod_UpdatesPermBitsKeepsType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	if err := fs.WriteFile("/exe", []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	before, _ := bf.ExtendedStat("/exe")
	const wantPerm = 0o755
	if err := bf.Chmod("/exe", wantPerm); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	after, err := bf.ExtendedStat("/exe")
	if err != nil {
		t.Fatalf("ExtendedStat: %v", err)
	}
	if after.Mode&0o7777 != wantPerm {
		t.Errorf("Chmod perm bits = 0o%o, want 0o%o", after.Mode&0o7777, wantPerm)
	}
	if after.Mode&0xF000 != before.Mode&0xF000 {
		t.Errorf("Chmod altered file-type bits: 0x%04x → 0x%04x", before.Mode, after.Mode)
	}
	if !after.IsRegular() {
		t.Errorf("regular file lost its type bit after Chmod (mode = 0x%04x)", after.Mode)
	}
}

func TestChmod_PreservesDirectoryType(t *testing.T) {
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
	if err := bf.Chmod("/d", 0o700); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	st, err := bf.ExtendedStat("/d")
	if err != nil {
		t.Fatalf("ExtendedStat: %v", err)
	}
	if !st.IsDir() {
		t.Errorf("Chmod on directory clobbered the type bit (mode = 0x%04x)", st.Mode)
	}
	if st.Mode&0o7777 != 0o700 {
		t.Errorf("Chmod perm bits = 0o%o, want 0o700", st.Mode&0o7777)
	}
}

func TestChtimes_UpdatesATimeMTime_BumpsCTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	if err := fs.WriteFile("/dated.txt", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	before, _ := bf.ExtendedStat("/dated.txt")
	time.Sleep(2 * time.Millisecond)

	// Set both to an arbitrary timestamp in the past (Y2K).
	want := time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)
	if err := bf.Chtimes("/dated.txt", want, want); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	after, err := bf.ExtendedStat("/dated.txt")
	if err != nil {
		t.Fatalf("ExtendedStat: %v", err)
	}
	if !after.ATime.Equal(want) {
		t.Errorf("atime = %v, want %v", after.ATime, want)
	}
	if !after.MTime.Equal(want) {
		t.Errorf("mtime = %v, want %v", after.MTime, want)
	}
	// ctime tracks "metadata changed" — must be NOW, not the supplied time.
	if !after.CTime.After(before.CTime) {
		t.Errorf("ctime didn't advance after Chtimes: %v → %v", before.CTime, after.CTime)
	}
	// otime (birth) must be unchanged.
	if !after.OTime.Equal(before.OTime) {
		t.Errorf("otime changed: %v → %v", before.OTime, after.OTime)
	}
}

func TestChown_NotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)
	if err := bf.Chown("/no/such/file", 1, 1); err == nil {
		t.Fatal("expected error for nonexistent path")
	}
}

func TestChmod_NotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)
	if err := bf.Chmod("/no/such/file", 0o644); err == nil {
		t.Fatal("expected error for nonexistent path")
	}
}

func TestChtimes_NotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)
	if err := bf.Chtimes("/no/such/file", time.Now(), time.Now()); err == nil {
		t.Fatal("expected error for nonexistent path")
	}
}
