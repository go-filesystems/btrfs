package filesystem_btrfs

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"
)

func TestXattrWrite_SetThenGet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	if err := fs.WriteFile("/labeled", []byte("data"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	const name = "security.selinux"
	want := []byte("system_u:object_r:tmp_t:s0\x00")
	if err := bf.SetXattr("/labeled", name, want); err != nil {
		t.Fatalf("SetXattr: %v", err)
	}
	got, err := bf.GetXattr("/labeled", name)
	if err != nil {
		t.Fatalf("GetXattr: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("GetXattr returned %q, want %q", got, want)
	}
	all, err := bf.Xattrs("/labeled")
	if err != nil {
		t.Fatalf("Xattrs: %v", err)
	}
	if !bytes.Equal(all[name], want) {
		t.Errorf("Xattrs[%q] = %q, want %q", name, all[name], want)
	}
}

func TestXattrWrite_ReplaceValue(t *testing.T) {
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
	const name = "user.note"
	if err := bf.SetXattr("/file", name, []byte("v1")); err != nil {
		t.Fatalf("SetXattr v1: %v", err)
	}
	if err := bf.SetXattr("/file", name, []byte("v2 — quite a bit longer than v1")); err != nil {
		t.Fatalf("SetXattr v2: %v", err)
	}
	got, err := bf.GetXattr("/file", name)
	if err != nil {
		t.Fatalf("GetXattr: %v", err)
	}
	if !bytes.Equal(got, []byte("v2 — quite a bit longer than v1")) {
		t.Errorf("GetXattr returned %q, want updated value", got)
	}
}

func TestXattrWrite_RemoveThenGetFails(t *testing.T) {
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
	const name = "user.tmp"
	if err := bf.SetXattr("/file", name, []byte("v")); err != nil {
		t.Fatalf("SetXattr: %v", err)
	}
	if err := bf.RemoveXattr("/file", name); err != nil {
		t.Fatalf("RemoveXattr: %v", err)
	}
	if _, err := bf.GetXattr("/file", name); err == nil {
		t.Fatal("GetXattr after RemoveXattr unexpectedly succeeded")
	} else if !errors.Is(err, ErrNotFound) {
		t.Errorf("GetXattr error = %v, expected wrapping ErrNotFound", err)
	}
}

func TestXattrWrite_RemoveMissingErrors(t *testing.T) {
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
	if err := bf.RemoveXattr("/file", "user.never-set"); err == nil {
		t.Fatal("RemoveXattr on missing xattr unexpectedly succeeded")
	}
}

func TestXattrWrite_BumpsCTimeAndSequence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)
	if err := fs.WriteFile("/x", []byte("a"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	before, _ := bf.ExtendedStat("/x")
	if err := bf.SetXattr("/x", "user.tag", []byte("v")); err != nil {
		t.Fatalf("SetXattr: %v", err)
	}
	after, _ := bf.ExtendedStat("/x")
	if !after.CTime.After(before.CTime) {
		t.Errorf("ctime didn't advance after SetXattr: %v → %v", before.CTime, after.CTime)
	}
	if after.Sequence <= before.Sequence {
		t.Errorf("sequence didn't advance after SetXattr: %d → %d", before.Sequence, after.Sequence)
	}
}

func TestXattrWrite_MissingPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)
	if err := bf.SetXattr("/no/such/file", "user.k", []byte("v")); err == nil {
		t.Fatal("SetXattr on missing path unexpectedly succeeded")
	}
	if _, err := bf.GetXattr("/no/such/file", "user.k"); err == nil {
		t.Fatal("GetXattr on missing path unexpectedly succeeded")
	}
	if err := bf.RemoveXattr("/no/such/file", "user.k"); err == nil {
		t.Fatal("RemoveXattr on missing path unexpectedly succeeded")
	}
}
