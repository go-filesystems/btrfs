package filesystem_btrfs

import (
	"encoding/binary"
	"path/filepath"
	"testing"
	"time"
)

// readDirMTime returns the on-disk mtime (sec) of the inode for the given path.
func readDirMTime(t *testing.T, fs *btrfsFS, ino uint64) time.Time {
	t.Helper()
	buf, it, err := searchTree(fs.f, fs.partOffset, fs.sb, fs.fsTreeRoot, ino, typeInodeItem, 0)
	if err != nil {
		t.Fatalf("searchTree inode %d: %v", ino, err)
	}
	d := it.data(buf)
	if len(d) < inodeItemSize {
		t.Fatalf("INODE_ITEM too short")
	}
	sec := int64(binary.LittleEndian.Uint64(d[inodeOffMTime:]))
	nsec := int64(binary.LittleEndian.Uint32(d[inodeOffMTime+8:]))
	return time.Unix(sec, nsec).UTC()
}

// expectMTimeAdvances runs op, asserting that the recorded mtime of ino is
// strictly greater after the operation than before. A short sleep between
// reads avoids the 1ns-resolution edge case where two back-to-back
// time.Now() calls could return identical values on fast machines.
func expectMTimeAdvances(t *testing.T, fs *btrfsFS, ino uint64, opLabel string, op func() error) {
	t.Helper()
	before := readDirMTime(t, fs, ino)
	time.Sleep(2 * time.Millisecond)
	if err := op(); err != nil {
		t.Fatalf("%s: %v", opLabel, err)
	}
	after := readDirMTime(t, fs, ino)
	if !after.After(before) {
		t.Fatalf("%s did not advance parent mtime: before=%v after=%v", opLabel, before, after)
	}
}

func TestDirTimes_WriteFileBumpsParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)
	expectMTimeAdvances(t, bf, rootDirObjID, "WriteFile /a.txt", func() error {
		return fs.WriteFile("/a.txt", []byte("x"), 0o644)
	})
}

func TestDirTimes_DeleteFileBumpsParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)
	if err := fs.WriteFile("/doomed", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	expectMTimeAdvances(t, bf, rootDirObjID, "DeleteFile /doomed", func() error {
		return fs.DeleteFile("/doomed")
	})
}

func TestDirTimes_MkDirBumpsParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)
	expectMTimeAdvances(t, bf, rootDirObjID, "MkDir /d", func() error {
		return fs.MkDir("/d", 0o755)
	})
}

func TestDirTimes_DeleteDirBumpsParent(t *testing.T) {
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
	expectMTimeAdvances(t, bf, rootDirObjID, "DeleteDir /d", func() error {
		return fs.DeleteDir("/d")
	})
}

func TestDirTimes_RenameSameParentBumps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)
	if err := fs.WriteFile("/old", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	expectMTimeAdvances(t, bf, rootDirObjID, "Rename /old → /new", func() error {
		return fs.Rename("/old", "/new")
	})
}

func TestDirTimes_RenameCrossParentBumpsBoth(t *testing.T) {
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
	if err := fs.WriteFile("/src/file", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	stSrc, _ := fs.Stat("/src")
	stDst, _ := fs.Stat("/dst")
	srcBefore := readDirMTime(t, bf, stSrc.Inode())
	dstBefore := readDirMTime(t, bf, stDst.Inode())
	time.Sleep(2 * time.Millisecond)

	if err := fs.Rename("/src/file", "/dst/file"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	srcAfter := readDirMTime(t, bf, stSrc.Inode())
	dstAfter := readDirMTime(t, bf, stDst.Inode())
	if !srcAfter.After(srcBefore) {
		t.Errorf("source parent mtime didn't advance: %v → %v", srcBefore, srcAfter)
	}
	if !dstAfter.After(dstBefore) {
		t.Errorf("dst parent mtime didn't advance: %v → %v", dstBefore, dstAfter)
	}
}
