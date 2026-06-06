package filesystem_btrfs

import (
	"path/filepath"
	"testing"
)

// findInodeRef walks every leaf in the FS tree looking for an INODE_REF item
// with the given (childIno, parentIno) pair. Returns true on match.
func findInodeRef(t *testing.T, fs *btrfsFS, childIno, parentIno uint64) bool {
	t.Helper()
	_, _, err := searchTree(fs.f, fs.partOffset, fs.sb, fs.fsTreeRoot, childIno, typeInodeRef, parentIno)
	return err == nil
}

func TestInodeRef_PresentAfterCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()

	if err := fs.WriteFile("/hello.txt", []byte("hi"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	st, err := fs.Stat("/hello.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	ino := st.Inode()
	if !findInodeRef(t, fs.(*btrfsFS), ino, rootDirObjID) {
		t.Fatalf("expected INODE_REF for inode %d → parent %d, none found", ino, rootDirObjID)
	}
}

func TestInodeRef_AbsentAfterDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()

	if err := fs.WriteFile("/x.txt", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	st, err := fs.Stat("/x.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	ino := st.Inode()
	if !findInodeRef(t, fs.(*btrfsFS), ino, rootDirObjID) {
		t.Fatalf("INODE_REF missing immediately after create")
	}
	if err := fs.DeleteFile("/x.txt"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if findInodeRef(t, fs.(*btrfsFS), ino, rootDirObjID) {
		t.Fatalf("INODE_REF still present after DeleteFile")
	}
}

func TestInodeRef_FollowsRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()

	if err := fs.MkDir("/a", 0o755); err != nil {
		t.Fatalf("MkDir /a: %v", err)
	}
	if err := fs.MkDir("/b", 0o755); err != nil {
		t.Fatalf("MkDir /b: %v", err)
	}
	if err := fs.WriteFile("/a/file", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	stA, err := fs.Stat("/a")
	if err != nil {
		t.Fatalf("Stat /a: %v", err)
	}
	stB, err := fs.Stat("/b")
	if err != nil {
		t.Fatalf("Stat /b: %v", err)
	}
	stF, err := fs.Stat("/a/file")
	if err != nil {
		t.Fatalf("Stat /a/file: %v", err)
	}

	if !findInodeRef(t, fs.(*btrfsFS), stF.Inode(), stA.Inode()) {
		t.Fatalf("expected INODE_REF child=%d parent=%d (under /a)", stF.Inode(), stA.Inode())
	}

	if err := fs.Rename("/a/file", "/b/file"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	if findInodeRef(t, fs.(*btrfsFS), stF.Inode(), stA.Inode()) {
		t.Fatalf("INODE_REF child=%d still points to old parent %d after rename", stF.Inode(), stA.Inode())
	}
	if !findInodeRef(t, fs.(*btrfsFS), stF.Inode(), stB.Inode()) {
		t.Fatalf("expected INODE_REF child=%d parent=%d after rename into /b", stF.Inode(), stB.Inode())
	}
}
