package filesystem_btrfs

import (
	"encoding/binary"
	"path/filepath"
	"testing"
)

// readDirNlink returns the on-disk nlink field of a directory inode.
func readDirNlink(t *testing.T, fs *btrfsFS, ino uint64) uint32 {
	t.Helper()
	buf, it, err := searchTree(fs.f, fs.partOffset, fs.sb, fs.fsTreeRoot, ino, typeInodeItem, 0)
	if err != nil {
		t.Fatalf("searchTree inode %d: %v", ino, err)
	}
	d := it.data(buf)
	return binary.LittleEndian.Uint32(d[inodeOffNLink:])
}

func TestDirNlink_RootStartsAtTwo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)
	if got := readDirNlink(t, bf, rootDirObjID); got != 2 {
		t.Errorf("freshly formatted root nlink = %d, want 2", got)
	}
}

func TestDirNlink_NewSubdirHasTwo(t *testing.T) {
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
	st, err := fs.Stat("/d")
	if err != nil {
		t.Fatalf("Stat /d: %v", err)
	}
	if got := readDirNlink(t, bf, st.Inode()); got != 2 {
		t.Errorf("new empty subdir nlink = %d, want 2", got)
	}
}

func TestDirNlink_ParentBumpsOnMkDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	before := readDirNlink(t, bf, rootDirObjID)
	if err := fs.MkDir("/sub", 0o755); err != nil {
		t.Fatalf("MkDir: %v", err)
	}
	after := readDirNlink(t, bf, rootDirObjID)
	if after != before+1 {
		t.Errorf("root nlink: before=%d after=%d, want +1", before, after)
	}

	// A second sibling subdir adds one more.
	if err := fs.MkDir("/sub2", 0o755); err != nil {
		t.Fatalf("MkDir sub2: %v", err)
	}
	after2 := readDirNlink(t, bf, rootDirObjID)
	if after2 != after+1 {
		t.Errorf("root nlink after 2nd sub: before=%d after=%d, want +1", after, after2)
	}
}

func TestDirNlink_ParentDecrementsOnDeleteDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	if err := fs.MkDir("/temp", 0o755); err != nil {
		t.Fatalf("MkDir: %v", err)
	}
	withSub := readDirNlink(t, bf, rootDirObjID)
	if err := fs.DeleteDir("/temp"); err != nil {
		t.Fatalf("DeleteDir: %v", err)
	}
	after := readDirNlink(t, bf, rootDirObjID)
	if after != withSub-1 {
		t.Errorf("root nlink: before delete=%d after=%d, want -1", withSub, after)
	}
}

func TestDirNlink_NestedDirs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	if err := fs.MkDir("/outer", 0o755); err != nil {
		t.Fatalf("MkDir outer: %v", err)
	}
	stOuter, _ := fs.Stat("/outer")
	outerIno := stOuter.Inode()
	beforeInner := readDirNlink(t, bf, outerIno)
	if beforeInner != 2 {
		t.Fatalf("outer dir start nlink = %d, want 2", beforeInner)
	}

	// Add three subdirs to /outer. Each must bump /outer's nlink by 1.
	for _, name := range []string{"/outer/a", "/outer/b", "/outer/c"} {
		if err := fs.MkDir(name, 0o755); err != nil {
			t.Fatalf("MkDir %q: %v", name, err)
		}
	}
	got := readDirNlink(t, bf, outerIno)
	if got != beforeInner+3 {
		t.Errorf("outer nlink after 3 subdirs = %d, want %d", got, beforeInner+3)
	}

	// And deleting one drops it back by 1.
	if err := fs.DeleteDir("/outer/b"); err != nil {
		t.Fatalf("DeleteDir: %v", err)
	}
	got2 := readDirNlink(t, bf, outerIno)
	if got2 != got-1 {
		t.Errorf("outer nlink after deleting one subdir = %d, want %d", got2, got-1)
	}
}

// File operations must NOT touch parent nlink — only subdirectory ops do.
func TestDirNlink_FilesDontAffectParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	before := readDirNlink(t, bf, rootDirObjID)
	if err := fs.WriteFile("/a.txt", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := readDirNlink(t, bf, rootDirObjID); got != before {
		t.Errorf("root nlink changed by WriteFile: %d → %d", before, got)
	}
	if err := fs.DeleteFile("/a.txt"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if got := readDirNlink(t, bf, rootDirObjID); got != before {
		t.Errorf("root nlink changed by DeleteFile: %d → %d", before, got)
	}
}
