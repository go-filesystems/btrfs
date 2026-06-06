package filesystem_btrfs

import (
	"path/filepath"
	"testing"
)

func TestSymlink_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	const target = "../usr/lib/x86_64-linux-gnu/libc.so.6"
	if err := bf.Symlink(target, "/libc"); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	got, err := fs.ReadLink("/libc")
	if err != nil {
		t.Fatalf("ReadLink: %v", err)
	}
	if got != target {
		t.Fatalf("symlink round-trip: got %q, want %q", got, target)
	}

	// Stat should report the inode as a symlink (mode S_IFLNK).
	st, err := fs.Stat("/libc")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Mode()&0xF000 != 0xA000 {
		t.Errorf("Stat mode = 0x%04x, expected S_IFLNK (0xA000) in the top nibble", st.Mode())
	}
}

func TestSymlink_RejectsExistingPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	if err := fs.WriteFile("/already", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := bf.Symlink("/elsewhere", "/already"); err == nil {
		t.Fatalf("Symlink over existing path unexpectedly succeeded")
	}
}

func TestSymlink_RejectsEmptyTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	if err := bf.Symlink("", "/empty"); err == nil {
		t.Fatalf("Symlink with empty target unexpectedly succeeded")
	}
}

func TestSymlink_DeleteOnlyUnlinksWhenNLinkOne(t *testing.T) {
	// A symlink's inode has nlink=1, so DeleteFile should fully remove it
	// (no hardlinks possible). This guards against accidentally falling
	// into the nlink-aware unlink path for symlinks.
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
	st, err := fs.Stat("/link")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	ino := st.Inode()
	if err := fs.DeleteFile("/link"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if _, _, err := searchTree(bf.f, bf.partOffset, bf.sb, bf.fsTreeRoot, ino, typeInodeItem, 0); err == nil {
		t.Fatalf("INODE_ITEM for symlink inode %d still present after DeleteFile", ino)
	}
}

func TestSymlink_LongTargetUsesInline(t *testing.T) {
	// Symlinks shorter than the inline threshold (2 KiB) should be stored
	// inline — no separate disk sector allocated. Verifies that the symlink
	// path goes through writeExtents' inline branch.
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	target := "/" + filepath.Join(
		"some", "moderately", "long", "but", "still", "under", "two", "kib",
		"path", "components", "for", "the", "symlink", "target", "test")
	if err := bf.Symlink(target, "/longlink"); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	st, _ := fs.Stat("/longlink")
	buf, it, err := searchTree(bf.f, bf.partOffset, bf.sb, bf.fsTreeRoot, st.Inode(), typeExtentData, 0)
	if err != nil {
		t.Fatalf("searchTree EXTENT_DATA: %v", err)
	}
	d := it.data(buf)
	if d[extDataOffType] != extentDataInline {
		t.Errorf("symlink target stored with extent type %d, expected inline (0)", d[extDataOffType])
	}
}
