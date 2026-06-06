package filesystem_btrfs

import (
	"encoding/binary"
	"path/filepath"
	"testing"
)

// readInodeFlags returns the on-disk i_flags field of the inode item for
// the given inode number.
func readInodeFlags(t *testing.T, fs *btrfsFS, ino uint64) uint64 {
	t.Helper()
	buf, it, err := searchTree(fs.f, fs.partOffset, fs.sb, fs.fsTreeRoot, ino, typeInodeItem, 0)
	if err != nil {
		t.Fatalf("searchTree inode %d: %v", ino, err)
	}
	d := it.data(buf)
	if len(d) < inodeItemSize {
		t.Fatalf("INODE_ITEM too short: %d bytes", len(d))
	}
	return binary.LittleEndian.Uint64(d[inodeOffFlags:])
}

// TestInodeFlags_NoDataSumOnRoot — the format-time root dir must carry the
// NODATASUM flag, since we never populate the csum tree.
func TestInodeFlags_NoDataSumOnRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)
	flags := readInodeFlags(t, bf, rootDirObjID)
	if flags&inodeFlagNoDataSum == 0 {
		t.Fatalf("root dir inode flags=0x%x, NODATASUM not set", flags)
	}
}

// TestInodeFlags_NoDataSumOnFiles — every file we WriteFile must carry
// NODATASUM. A kernel mount would refuse to read these files otherwise,
// since our driver never inserts items into the CSUM_TREE.
func TestInodeFlags_NoDataSumOnFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	if err := fs.WriteFile("/a.txt", []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	st, err := fs.Stat("/a.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	flags := readInodeFlags(t, bf, st.Inode())
	if flags&inodeFlagNoDataSum == 0 {
		t.Fatalf("created file inode flags=0x%x, NODATASUM not set", flags)
	}
}

// TestInodeFlags_NoDataSumOnDirs — MkDir-created directories must also
// carry the flag.
func TestInodeFlags_NoDataSumOnDirs(t *testing.T) {
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
		t.Fatalf("Stat: %v", err)
	}
	flags := readInodeFlags(t, bf, st.Inode())
	if flags&inodeFlagNoDataSum == 0 {
		t.Fatalf("created dir inode flags=0x%x, NODATASUM not set", flags)
	}
}
