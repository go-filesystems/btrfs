package filesystem_btrfs

import (
	"encoding/binary"
	"path/filepath"
	"testing"
)

// readInodeNBytes returns the on-disk `nbytes` field of the inode for the
// given path.
func readInodeNBytes(t *testing.T, fs *btrfsFS, ino uint64) uint64 {
	t.Helper()
	buf, it, err := searchTree(fs.f, fs.partOffset, fs.sb, fs.fsTreeRoot, ino, typeInodeItem, 0)
	if err != nil {
		t.Fatalf("searchTree inode %d: %v", ino, err)
	}
	d := it.data(buf)
	if len(d) < inodeItemSize {
		t.Fatalf("INODE_ITEM too short: %d bytes", len(d))
	}
	return binary.LittleEndian.Uint64(d[inodeOffNBytes:])
}

func TestNBytes_InlineIsRamBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	const payload = "tiny payload"
	if err := fs.WriteFile("/inline.txt", []byte(payload), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	st, err := fs.Stat("/inline.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	// btrfs: an inline file's nbytes equals the inline data's ram_bytes (its
	// uncompressed length), so `btrfs check` does not flag "nbytes wrong".
	if got := readInodeNBytes(t, bf, st.Inode()); got != uint64(len(payload)) {
		t.Fatalf("inline file nbytes = %d, want %d", got, len(payload))
	}
}

func TestNBytes_RegularIsSectorAligned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, 8*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	const payloadSize = 5000 // > inline threshold, not sector-aligned
	body := make([]byte, payloadSize)
	for i := range body {
		body[i] = byte(i)
	}
	if err := fs.WriteFile("/big.bin", body, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	st, err := fs.Stat("/big.bin")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	sec := uint64(bf.sb.sectorSize)
	want := (uint64(payloadSize) + sec - 1) / sec * sec
	if got := readInodeNBytes(t, bf, st.Inode()); got != want {
		t.Fatalf("regular file nbytes = %d, want %d (= ceil(%d / %d) * %d)", got, want, payloadSize, sec, sec)
	}
}

func TestNBytes_SparseIsZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, 8*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	if err := fs.WriteFile("/zeros.bin", make([]byte, 16*1024), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	st, err := fs.Stat("/zeros.bin")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := readInodeNBytes(t, bf, st.Inode()); got != 0 {
		t.Fatalf("sparse (zero-filled) file nbytes = %d, want 0", got)
	}
}

func TestNBytes_RefreshesOnOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, 8*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	// First write: regular extent.
	body1 := make([]byte, 5000)
	for i := range body1 {
		body1[i] = byte(i)
	}
	if err := fs.WriteFile("/morphing.bin", body1, 0o644); err != nil {
		t.Fatalf("WriteFile v1: %v", err)
	}
	st, err := fs.Stat("/morphing.bin")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	ino := st.Inode()
	if got := readInodeNBytes(t, bf, ino); got == 0 {
		t.Fatalf("after regular write: nbytes = 0, expected non-zero")
	}

	// Overwrite with tiny inline payload — nbytes resets to the inline ram_bytes
	// (the uncompressed length), matching what `btrfs check` expects.
	const tiny = "now tiny"
	if err := fs.WriteFile("/morphing.bin", []byte(tiny), 0o644); err != nil {
		t.Fatalf("WriteFile v2: %v", err)
	}
	if got := readInodeNBytes(t, bf, ino); got != uint64(len(tiny)) {
		t.Fatalf("after overwrite to inline: nbytes = %d, want %d", got, len(tiny))
	}
}
