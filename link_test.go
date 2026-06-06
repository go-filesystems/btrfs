package filesystem_btrfs

import (
	"bytes"
	"encoding/binary"
	"path/filepath"
	"testing"
)

// readInodeNlink reads the on-disk nlink field of the INODE_ITEM.
func readInodeNlink(t *testing.T, fs *btrfsFS, ino uint64) uint32 {
	t.Helper()
	buf, it, err := searchTree(fs.f, fs.partOffset, fs.sb, fs.fsTreeRoot, ino, typeInodeItem, 0)
	if err != nil {
		t.Fatalf("searchTree inode %d: %v", ino, err)
	}
	d := it.data(buf)
	if len(d) < inodeItemSize {
		t.Fatalf("INODE_ITEM too short")
	}
	return binary.LittleEndian.Uint32(d[inodeOffNLink:])
}

// TestLink_BothPathsShareData — creating a hardlink makes the two paths
// resolve to the same inode and return the same content.
func TestLink_BothPathsShareData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	body := []byte("shared body")
	if err := fs.WriteFile("/a", body, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := bf.Link("/a", "/b"); err != nil {
		t.Fatalf("Link: %v", err)
	}

	stA, err := fs.Stat("/a")
	if err != nil {
		t.Fatalf("Stat /a: %v", err)
	}
	stB, err := fs.Stat("/b")
	if err != nil {
		t.Fatalf("Stat /b: %v", err)
	}
	if stA.Inode() != stB.Inode() {
		t.Fatalf("hardlinked paths refer to different inodes: a=%d b=%d", stA.Inode(), stB.Inode())
	}
	if got := readInodeNlink(t, bf, stA.Inode()); got != 2 {
		t.Fatalf("nlink after one Link = %d, want 2", got)
	}
	for _, p := range []string{"/a", "/b"} {
		got, err := fs.ReadFile(p)
		if err != nil {
			t.Fatalf("ReadFile %q: %v", p, err)
		}
		if !bytes.Equal(got, body) {
			t.Fatalf("ReadFile %q: got %q want %q", p, got, body)
		}
	}
}

// TestLink_PartialUnlinkKeepsData — deleting one of two hardlinked paths
// must leave the other working with the same content; nlink drops to 1.
func TestLink_PartialUnlinkKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	body := []byte("survives partial unlink")
	if err := fs.WriteFile("/keep", body, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := bf.Link("/keep", "/throw"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	st, _ := fs.Stat("/keep")
	ino := st.Inode()

	if err := fs.DeleteFile("/throw"); err != nil {
		t.Fatalf("DeleteFile /throw: %v", err)
	}

	if got := readInodeNlink(t, bf, ino); got != 1 {
		t.Fatalf("nlink after partial unlink = %d, want 1", got)
	}
	got, err := fs.ReadFile("/keep")
	if err != nil {
		t.Fatalf("ReadFile /keep after partial unlink: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("/keep content after partial unlink: got %q want %q", got, body)
	}
	// /throw should be gone.
	if _, err := fs.Stat("/throw"); err == nil {
		t.Fatalf("/throw still exists after DeleteFile")
	}
}

// TestLink_FullUnlinkFreesInode — deleting the LAST link removes the inode
// and its data extents entirely (existing behavior path).
func TestLink_FullUnlinkFreesInode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	if err := fs.WriteFile("/x", []byte("transient"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := bf.Link("/x", "/y"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	st, _ := fs.Stat("/x")
	ino := st.Inode()

	if err := fs.DeleteFile("/x"); err != nil {
		t.Fatalf("DeleteFile /x: %v", err)
	}
	// One link remains: /y.
	if got := readInodeNlink(t, bf, ino); got != 1 {
		t.Fatalf("nlink after first delete = %d, want 1", got)
	}

	if err := fs.DeleteFile("/y"); err != nil {
		t.Fatalf("DeleteFile /y: %v", err)
	}
	// Inode should be gone now.
	if _, _, err := searchTree(bf.f, bf.partOffset, bf.sb, bf.fsTreeRoot, ino, typeInodeItem, 0); err == nil {
		t.Fatalf("INODE_ITEM for inode %d still present after deleting last link", ino)
	}
}

// TestLink_RejectsDirectory — Link must refuse directories (POSIX).
func TestLink_RejectsDirectory(t *testing.T) {
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
	if err := bf.Link("/d", "/d2"); err == nil {
		t.Fatalf("Link on a directory unexpectedly succeeded")
	}
}

// TestLink_RejectsExistingDestination — Link must refuse to overwrite.
func TestLink_RejectsExistingDestination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	if err := fs.WriteFile("/a", []byte("a"), 0o644); err != nil {
		t.Fatalf("WriteFile a: %v", err)
	}
	if err := fs.WriteFile("/b", []byte("b"), 0o644); err != nil {
		t.Fatalf("WriteFile b: %v", err)
	}
	if err := bf.Link("/a", "/b"); err == nil {
		t.Fatalf("Link over existing destination unexpectedly succeeded")
	}
}
