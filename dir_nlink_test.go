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

func TestDirNlink_RootStartsAtOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)
	// btrfs: an empty directory has nlink=1 (the kernel's tree-checker rejects
	// nlink>1 for a dir with no subdirectories).
	if got := readDirNlink(t, bf, rootDirObjID); got != 1 {
		t.Errorf("freshly formatted root nlink = %d, want 1", got)
	}
}

func TestDirNlink_NewSubdirHasOne(t *testing.T) {
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
	// btrfs: a new empty subdirectory has nlink=1.
	if got := readDirNlink(t, bf, st.Inode()); got != 1 {
		t.Errorf("new empty subdir nlink = %d, want 1", got)
	}
}

// btrfs keeps every directory at nlink == 1 regardless of how many
// subdirectories it holds — unlike traditional Unix filesystems, subdirectories
// are NOT counted in the parent's link count, and the kernel's tree-checker
// rejects any directory whose nlink exceeds 1 ("invalid nlink: has 2 expect no
// more than 1 for dir"). These tests pin that invariant so the regression that
// made our images unmountable cannot reappear.
func TestDirNlink_ParentStaysOneOnMkDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	if err := fs.MkDir("/sub", 0o755); err != nil {
		t.Fatalf("MkDir: %v", err)
	}
	if got := readDirNlink(t, bf, rootDirObjID); got != 1 {
		t.Errorf("root nlink after MkDir = %d, want 1 (btrfs dirs never exceed 1)", got)
	}

	// A second sibling subdir still leaves the parent at 1.
	if err := fs.MkDir("/sub2", 0o755); err != nil {
		t.Fatalf("MkDir sub2: %v", err)
	}
	if got := readDirNlink(t, bf, rootDirObjID); got != 1 {
		t.Errorf("root nlink after 2nd subdir = %d, want 1", got)
	}
}

func TestDirNlink_ParentStaysOneOnDeleteDir(t *testing.T) {
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
	if err := fs.DeleteDir("/temp"); err != nil {
		t.Fatalf("DeleteDir: %v", err)
	}
	if got := readDirNlink(t, bf, rootDirObjID); got != 1 {
		t.Errorf("root nlink after DeleteDir = %d, want 1", got)
	}
}

func TestDirNlink_NestedDirsStayOne(t *testing.T) {
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
	if got := readDirNlink(t, bf, outerIno); got != 1 {
		t.Fatalf("outer dir start nlink = %d, want 1", got)
	}

	// Adding subdirs must NOT change /outer's nlink (stays 1).
	for _, name := range []string{"/outer/a", "/outer/b", "/outer/c"} {
		if err := fs.MkDir(name, 0o755); err != nil {
			t.Fatalf("MkDir %q: %v", name, err)
		}
	}
	if got := readDirNlink(t, bf, outerIno); got != 1 {
		t.Errorf("outer nlink after 3 subdirs = %d, want 1", got)
	}

	// And deleting one still leaves it at 1.
	if err := fs.DeleteDir("/outer/b"); err != nil {
		t.Fatalf("DeleteDir: %v", err)
	}
	if got := readDirNlink(t, bf, outerIno); got != 1 {
		t.Errorf("outer nlink after deleting one subdir = %d, want 1", got)
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
