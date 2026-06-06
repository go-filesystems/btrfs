package filesystem_btrfs

import (
	"encoding/binary"
	"path/filepath"
	"testing"
	"time"
)

// readInodeTimes reads the four btrfs_timespec fields directly from the
// on-disk INODE_ITEM at fs.fsTreeRoot for the given inode number. Returns
// (atime, ctime, mtime, otime) as time.Time values.
func readInodeTimes(t *testing.T, fs *btrfsFS, ino uint64) (atime, ctime, mtime, otime time.Time) {
	t.Helper()
	buf, it, err := searchTree(fs.f, fs.partOffset, fs.sb, fs.fsTreeRoot, ino, typeInodeItem, 0)
	if err != nil {
		t.Fatalf("searchTree inode %d: %v", ino, err)
	}
	d := it.data(buf)
	if len(d) < inodeItemSize {
		t.Fatalf("INODE_ITEM too short for inode %d: %d", ino, len(d))
	}
	read := func(off int) time.Time {
		le := binary.LittleEndian
		sec := int64(le.Uint64(d[off:]))
		nsec := int64(le.Uint32(d[off+8:]))
		return time.Unix(sec, nsec).UTC()
	}
	return read(inodeOffATime), read(inodeOffCTime), read(inodeOffMTime), read(inodeOffOTime)
}

func TestTimestamps_SetOnCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()

	before := time.Now().UTC().Add(-time.Second)
	if err := fs.WriteFile("/x.txt", []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	after := time.Now().UTC().Add(time.Second)

	st, err := fs.Stat("/x.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	a, c, m, o := readInodeTimes(t, fs.(*btrfsFS), st.Inode())
	for name, ts := range map[string]time.Time{"atime": a, "ctime": c, "mtime": m, "otime": o} {
		if ts.Before(before) || ts.After(after) {
			t.Errorf("%s = %v, not in [%v, %v]", name, ts, before, after)
		}
	}
}

func TestTimestamps_MTimeBumpsOnOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()

	if err := fs.WriteFile("/x.txt", []byte("v1"), 0o644); err != nil {
		t.Fatalf("WriteFile v1: %v", err)
	}
	st, err := fs.Stat("/x.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	_, _, mtime1, otime1 := readInodeTimes(t, fs.(*btrfsFS), st.Inode())

	// Make sure the clock advances enough to be observable.
	time.Sleep(10 * time.Millisecond)

	if err := fs.WriteFile("/x.txt", []byte("v2-longer"), 0o644); err != nil {
		t.Fatalf("WriteFile v2: %v", err)
	}
	_, _, mtime2, otime2 := readInodeTimes(t, fs.(*btrfsFS), st.Inode())

	if !mtime2.After(mtime1) {
		t.Errorf("mtime did not advance: before=%v after=%v", mtime1, mtime2)
	}
	if !otime2.Equal(otime1) {
		t.Errorf("otime should be immutable: was %v, now %v", otime1, otime2)
	}
}

func TestTimestamps_RootDirHasValidTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	before := time.Now().UTC().Add(-time.Second)
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	after := time.Now().UTC().Add(time.Second)
	defer fs.Close()

	_, _, mtime, _ := readInodeTimes(t, fs.(*btrfsFS), rootDirObjID)
	if mtime.Before(before) || mtime.After(after) {
		t.Errorf("root dir mtime %v outside expected [%v, %v]", mtime, before, after)
	}
}
