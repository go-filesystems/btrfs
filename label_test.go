package filesystem_btrfs

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openFreshBtrfs(t *testing.T) (FS, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(p, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bf := fs.(*btrfsFS)
	return bf, p
}

func TestBtrfsSetLabel_Roundtrip(t *testing.T) {
	fs, _ := openFreshBtrfs(t)
	defer fs.Close()

	if err := fs.SetLabel("rootfs"); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
	if got := fs.Label(); got != "rootfs" {
		t.Errorf("Label() = %q, want %q", got, "rootfs")
	}
}

func TestBtrfsSetLabel_PersistsAcrossReopen(t *testing.T) {
	fs, img := openFreshBtrfs(t)
	if err := fs.SetLabel("data1"); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
	fs.Close()

	fs2, err := Open(img, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs2.Close()
	if got := fs2.Label(); got != "data1" {
		t.Errorf("after reopen Label() = %q, want %q", got, "data1")
	}
}

func TestBtrfsSetLabel_FormatConfigSeedsLabel(t *testing.T) {
	// Format(...).Label flows into the on-disk sb_fname; verify the
	// driver's Label() reflects what Format wrote.
	p := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(p, btrfsTestSize, FormatConfig{Label: "seeded"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)
	if got := bf.Label(); got != "seeded" {
		t.Errorf("Label() = %q, want %q", got, "seeded")
	}
}

func TestBtrfsSetLabel_RejectsTooLong(t *testing.T) {
	fs, _ := openFreshBtrfs(t)
	defer fs.Close()

	before := fs.Label()
	if err := fs.SetLabel(strings.Repeat("x", MaxLabelLen+1)); err == nil {
		t.Error("SetLabel with oversize input unexpectedly succeeded")
	}
	if after := fs.Label(); after != before {
		t.Errorf("Label() changed after rejected SetLabel: %q -> %q", before, after)
	}
}

func TestBtrfsSetLabel_ShorterClearsTrailingBytes(t *testing.T) {
	fs, img := openFreshBtrfs(t)
	if err := fs.SetLabel("longish-label-name"); err != nil { // 18 bytes
		t.Fatalf("first SetLabel: %v", err)
	}
	if err := fs.SetLabel("hi"); err != nil { // 2 bytes — shorter
		t.Fatalf("second SetLabel: %v", err)
	}
	fs.Close()

	// Read the raw on-disk superblock and check the label slot is
	// null-padded — no trailing garbage from the longer first label.
	f, err := os.Open(img)
	if err != nil {
		t.Fatalf("open img: %v", err)
	}
	defer f.Close()
	buf := make([]byte, sbfSize)
	if _, err := f.ReadAt(buf, 0x10000); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	got := buf[sbfLabel : sbfLabel+256]
	want := append([]byte("hi"), make([]byte, 256-2)...)
	if !bytes.Equal(got, want) {
		// Locate first mismatch for a tight error.
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("label byte %d = 0x%02x, want 0x%02x", i, got[i], want[i])
				break
			}
		}
	}
}

func TestBtrfsSetLabel_UpdatesSuperblockCRC(t *testing.T) {
	fs, img := openFreshBtrfs(t)
	if err := fs.SetLabel("crctest"); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
	fs.Close()

	// Reopen — readSuperblock will fail if the CRC is now wrong (it
	// doesn't strictly verify here, but at least the magic check
	// confirms the field layout isn't torn).
	fs2, err := Open(img, -1)
	if err != nil {
		t.Fatalf("Open after SetLabel: %v", err)
	}
	fs2.Close()

	// And explicitly verify the stored CRC matches what
	// updateSuperblockCRC would write now.
	f, err := os.Open(img)
	if err != nil {
		t.Fatalf("open img: %v", err)
	}
	defer f.Close()
	buf := make([]byte, sbfSize)
	if _, err := f.ReadAt(buf, 0x10000); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	storedCRC := binary.LittleEndian.Uint32(buf[:4])
	wantCRC := crc32cSum(buf[32:sbfSize], btrfsCsumSeed)
	if storedCRC != wantCRC {
		t.Errorf("primary superblock CRC mismatch: stored=0x%08x want=0x%08x", storedCRC, wantCRC)
	}
}
