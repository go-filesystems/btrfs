package filesystem_btrfs

import (
	"encoding/binary"
	"path/filepath"
	"testing"
)

// readDotDotInodeNum returns the inode number that the ".." DIR_INDEX entry
// inside the directory `dirIno` resolves to.
func readDotDotInodeNum(t *testing.T, fs *btrfsFS, dirIno uint64) uint64 {
	t.Helper()
	buf, it, err := searchTree(fs.f, fs.partOffset, fs.sb, fs.fsTreeRoot, dirIno, typeDirIndex, 2)
	if err != nil {
		t.Fatalf("searchTree DIR_INDEX 2 of dir inode %d: %v", dirIno, err)
	}
	d := it.data(buf)
	if len(d) < dirItemHdrSize {
		t.Fatalf("DIR_INDEX 2 data too short: %d", len(d))
	}
	return binary.LittleEndian.Uint64(d[0:])
}

func TestRenameDir_CrossParentUpdatesDotDot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	if err := fs.MkDir("/a", 0o755); err != nil {
		t.Fatalf("MkDir /a: %v", err)
	}
	if err := fs.MkDir("/b", 0o755); err != nil {
		t.Fatalf("MkDir /b: %v", err)
	}
	if err := fs.MkDir("/a/sub", 0o755); err != nil {
		t.Fatalf("MkDir /a/sub: %v", err)
	}
	stA, _ := fs.Stat("/a")
	stB, _ := fs.Stat("/b")
	stSub, _ := fs.Stat("/a/sub")

	// Before rename: /a/sub's ".." should point at /a.
	if got := readDotDotInodeNum(t, bf, stSub.Inode()); got != stA.Inode() {
		t.Fatalf("before rename: /a/sub/.. = %d, want /a = %d", got, stA.Inode())
	}

	if err := fs.Rename("/a/sub", "/b/sub"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	// After rename: /b/sub's ".." must now point at /b.
	if got := readDotDotInodeNum(t, bf, stSub.Inode()); got != stB.Inode() {
		t.Fatalf("after rename: /b/sub/.. = %d, want /b = %d", got, stB.Inode())
	}
}

func TestRenameDir_CrossParentShiftsNlink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	if err := fs.MkDir("/src", 0o755); err != nil {
		t.Fatalf("MkDir /src: %v", err)
	}
	if err := fs.MkDir("/dst", 0o755); err != nil {
		t.Fatalf("MkDir /dst: %v", err)
	}
	if err := fs.MkDir("/src/d", 0o755); err != nil {
		t.Fatalf("MkDir /src/d: %v", err)
	}
	stSrc, _ := fs.Stat("/src")
	stDst, _ := fs.Stat("/dst")
	srcBefore := readDirNlink(t, bf, stSrc.Inode())
	dstBefore := readDirNlink(t, bf, stDst.Inode())

	if err := fs.Rename("/src/d", "/dst/d"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	srcAfter := readDirNlink(t, bf, stSrc.Inode())
	dstAfter := readDirNlink(t, bf, stDst.Inode())
	if srcAfter != srcBefore-1 {
		t.Errorf("src parent nlink: was %d, now %d, want %d", srcBefore, srcAfter, srcBefore-1)
	}
	if dstAfter != dstBefore+1 {
		t.Errorf("dst parent nlink: was %d, now %d, want %d", dstBefore, dstAfter, dstBefore+1)
	}
}

func TestRenameDir_SameParentDoesNotChangeNlink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	if err := fs.MkDir("/p", 0o755); err != nil {
		t.Fatalf("MkDir /p: %v", err)
	}
	if err := fs.MkDir("/p/old-name", 0o755); err != nil {
		t.Fatalf("MkDir /p/old-name: %v", err)
	}
	stP, _ := fs.Stat("/p")
	before := readDirNlink(t, bf, stP.Inode())

	if err := fs.Rename("/p/old-name", "/p/new-name"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	after := readDirNlink(t, bf, stP.Inode())
	if after != before {
		t.Errorf("same-parent dir rename changed parent nlink: %d → %d", before, after)
	}
}

func TestRenameFile_CrossParentLeavesParentNlinkAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	if err := fs.MkDir("/src", 0o755); err != nil {
		t.Fatalf("MkDir /src: %v", err)
	}
	if err := fs.MkDir("/dst", 0o755); err != nil {
		t.Fatalf("MkDir /dst: %v", err)
	}
	if err := fs.WriteFile("/src/file.txt", []byte("payload"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	stSrc, _ := fs.Stat("/src")
	stDst, _ := fs.Stat("/dst")
	srcBefore := readDirNlink(t, bf, stSrc.Inode())
	dstBefore := readDirNlink(t, bf, stDst.Inode())

	if err := fs.Rename("/src/file.txt", "/dst/file.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	srcAfter := readDirNlink(t, bf, stSrc.Inode())
	dstAfter := readDirNlink(t, bf, stDst.Inode())
	if srcAfter != srcBefore || dstAfter != dstBefore {
		t.Errorf("cross-parent file rename changed parent nlinks: src %d→%d, dst %d→%d (both should stay equal)",
			srcBefore, srcAfter, dstBefore, dstAfter)
	}
}
