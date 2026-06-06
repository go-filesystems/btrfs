package filesystem_btrfs

import (
	"bytes"
	"path/filepath"
	"testing"
)

// inspectFirstExtentType returns the type of the (ino, EXTENT_DATA, 0)
// item — 0 for inline, 1 for regular (with diskBytenr=0 indicating sparse).
// Also returns the regular-extent diskBytenr/diskNumBytes if applicable.
func inspectFirstExtent(t *testing.T, fs *btrfsFS, ino uint64) (extType uint8, diskBytenr, diskNumBytes uint64) {
	t.Helper()
	buf, it, err := searchTree(fs.f, fs.partOffset, fs.sb, fs.fsTreeRoot, ino, typeExtentData, 0)
	if err != nil {
		t.Fatalf("searchTree EXTENT_DATA for inode %d: %v", ino, err)
	}
	d := it.data(buf)
	if len(d) <= extDataOffType {
		t.Fatalf("EXTENT_DATA too short: %d bytes", len(d))
	}
	extType = d[extDataOffType]
	if extType == extentDataRegular && len(d) >= extDataRegularSize {
		diskBytenr = uint64(d[extDataOffDiskBytenr]) | uint64(d[extDataOffDiskBytenr+1])<<8 |
			uint64(d[extDataOffDiskBytenr+2])<<16 | uint64(d[extDataOffDiskBytenr+3])<<24 |
			uint64(d[extDataOffDiskBytenr+4])<<32 | uint64(d[extDataOffDiskBytenr+5])<<40 |
			uint64(d[extDataOffDiskBytenr+6])<<48 | uint64(d[extDataOffDiskBytenr+7])<<56
		diskNumBytes = uint64(d[extDataOffDiskNumBytes]) | uint64(d[extDataOffDiskNumBytes+1])<<8 |
			uint64(d[extDataOffDiskNumBytes+2])<<16 | uint64(d[extDataOffDiskNumBytes+3])<<24 |
			uint64(d[extDataOffDiskNumBytes+4])<<32 | uint64(d[extDataOffDiskNumBytes+5])<<40 |
			uint64(d[extDataOffDiskNumBytes+6])<<48 | uint64(d[extDataOffDiskNumBytes+7])<<56
	}
	return
}

func TestInlineExtent_SmallFileStoredInline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	body := []byte("a small file that should live inline")
	if err := fs.WriteFile("/tiny.txt", body, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	st, err := fs.Stat("/tiny.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	extType, _, _ := inspectFirstExtent(t, bf, st.Inode())
	if extType != extentDataInline {
		t.Fatalf("small file should use inline extent (type=0), got type=%d", extType)
	}
	got, err := fs.ReadFile("/tiny.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("inline read mismatch: got %q want %q", got, body)
	}
}

func TestInlineExtent_LargeFileStillRegular(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, 8*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	body := bytes.Repeat([]byte("not-inline-"), 300) // ~3.3 KiB > 2 KiB threshold
	if err := fs.WriteFile("/big.txt", body, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	st, err := fs.Stat("/big.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	extType, diskBytenr, _ := inspectFirstExtent(t, bf, st.Inode())
	if extType != extentDataRegular {
		t.Fatalf("file above inline threshold should use regular extent (type=1), got type=%d", extType)
	}
	if diskBytenr == 0 {
		t.Fatalf("non-zero payload should not be sparse, but diskBytenr=0")
	}
}

func TestSparseExtent_AllZeroAvoidsAllocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, 8*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	// Snapshot free-space-in-bytes before the write, then again after.
	beforeFree := uint64(0)
	for _, fe := range bf.sm.freeExts {
		beforeFree += fe.size
	}

	// 16 KiB of zeros — well above inline threshold and large enough that a
	// regular allocation would consume several sectors of disk space.
	body := make([]byte, 16*1024)
	if err := fs.WriteFile("/zeros.bin", body, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	st, err := fs.Stat("/zeros.bin")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	extType, diskBytenr, diskNumBytes := inspectFirstExtent(t, bf, st.Inode())
	if extType != extentDataRegular {
		t.Fatalf("zero-filled payload should still be a regular extent (type=1), got type=%d", extType)
	}
	if diskBytenr != 0 || diskNumBytes != 0 {
		t.Fatalf("zero-filled payload should be sparse (diskBytenr=diskNumBytes=0), got diskBytenr=%d diskNumBytes=%d", diskBytenr, diskNumBytes)
	}

	afterFree := uint64(0)
	for _, fe := range bf.sm.freeExts {
		afterFree += fe.size
	}
	// Sparse writes must not consume DATA sectors for the file body. Some
	// metadata (the new EXTENT_DATA / INODE / DIR items) still gets COWed
	// through node-sized allocations, but the >= 16 KiB of payload sectors
	// must not be subtracted from free space. Allow a modest overhead for
	// COWed metadata nodes but assert the body itself was never allocated.
	const maxMetadataOverhead = 64 * 1024 // generous: ~16 node blocks
	if beforeFree-afterFree > maxMetadataOverhead {
		t.Fatalf("sparse write consumed too much space: before=%d after=%d delta=%d > %d (looks like the zero payload was allocated)",
			beforeFree, afterFree, beforeFree-afterFree, maxMetadataOverhead)
	}

	// Read-back must produce the original (all-zero) bytes.
	got, err := fs.ReadFile("/zeros.bin")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("sparse read mismatch: got %d bytes, want %d zeros", len(got), len(body))
	}
}
