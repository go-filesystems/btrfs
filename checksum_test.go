package filesystem_btrfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Regression tests for metadata checksum verification (see checksum.go for the
// on-disk format references).

const csumTestSecret = "ORIGINAL-CONTENT-0123456789"

// csumImage formats an image with one small (inline) file and returns its path.
func csumImage(t *testing.T) string {
	t.Helper()
	img := filepath.Join(t.TempDir(), "img")
	// Format reopens the image it wrote, so a defect in the open path would
	// panic here. Report it as a failure of this test: a panic ends the whole
	// test binary and hides every test after it.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("formatting the fixture panicked: %v", r)
		}
	}()
	fsys, err := Format(img, 128<<20, FormatConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/a.txt", []byte(csumTestSecret), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Close(); err != nil {
		t.Fatal(err)
	}
	return img
}

func tamperLeaf(t *testing.T, img string) {
	t.Helper()
	raw, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(raw, []byte(csumTestSecret)) == 0 {
		t.Fatal("inline data not found in the image")
	}
	raw = bytes.ReplaceAll(raw, []byte(csumTestSecret), []byte("TAMPERED-CONTENT-0123456789"))
	if err := os.WriteFile(img, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The audit reproducer: bytes flipped inside a leaf (and the superblock label)
// without fixing the checksums must be refused with a typed error, never
// served.
func TestAuditOpusTreeBlockChecksumNotVerified(t *testing.T) {
	img := csumImage(t)
	tamperLeaf(t, img)
	raw, _ := os.ReadFile(img)
	copy(raw[0x10000+sbfLabel:], "forged")
	if err := os.WriteFile(img, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	f2, err := Open(img, -1)
	if err == nil {
		defer f2.Close()
		var got []byte
		if got, err = f2.ReadFile("/a.txt"); err == nil {
			t.Fatalf("checksum not verified: ReadFile = %q, label = %q", got, f2.Label())
		}
	}
	var ce *ChecksumError
	if !errors.As(err, &ce) || !errors.Is(err, ErrChecksum) {
		t.Fatalf("want *ChecksumError, got %T: %v", err, err)
	}
}

func TestTreeBlockChecksumTypedErrorOnRead(t *testing.T) {
	img := csumImage(t)
	tamperLeaf(t, img) // superblock untouched: Open may succeed, the read must fail
	fsys, err := Open(img, -1)
	if err != nil {
		var ce *ChecksumError
		if !errors.As(err, &ce) {
			t.Fatalf("Open: want *ChecksumError, got %v", err)
		}
		return
	}
	defer fsys.Close()
	_, err = fsys.ReadFile("/a.txt")
	var ce *ChecksumError
	if !errors.As(err, &ce) || ce.Kind != "tree block" || ce.Stored == ce.Computed {
		t.Fatalf("want tree-block *ChecksumError, got %v", err)
	}
}

func TestIgnoreChecksumsForensicOptOut(t *testing.T) {
	img := csumImage(t)
	tamperLeaf(t, img)
	fsys, err := OpenWithOptions(img, -1, OpenOptions{IgnoreChecksums: true})
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()
	got, err := fsys.ReadFile("/a.txt")
	if err != nil || string(got) != "TAMPERED-CONTENT-0123456789" {
		t.Fatalf("forensic read = %q, %v", got, err)
	}
}

func TestWriteDoesNotLaunderTamperedBlocks(t *testing.T) {
	img := csumImage(t)
	tamperLeaf(t, img)
	fsys, err := Open(img, -1)
	if err != nil {
		return // refused at open: nothing to launder
	}
	defer fsys.Close()
	err = fsys.WriteFile("/b.txt", []byte("x"), 0o644)
	if err == nil {
		t.Fatal("a write over a tampered leaf succeeded (checksums recomputed over tampered data)")
	}
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("want a checksum error, got %v", err)
	}
}

func TestSuperblockChecksumBackupFallback(t *testing.T) {
	img := csumImage(t)
	raw, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}
	prim := raw[0x10000 : 0x10000+sbfSize]
	// Install a valid mirror at 64 MiB, then break the primary's content.
	copy(raw[64<<20:], prim)
	copy(raw[0x10000+sbfLabel:], "forged")
	if err := os.WriteFile(img, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	fsys, err := Open(img, -1)
	if err != nil {
		t.Fatalf("Open with a valid backup superblock: %v", err)
	}
	if got := fsys.Label(); got == "forged" {
		t.Fatalf("label %q comes from the corrupt primary", got)
	}
	// A write must not re-checksum the corrupt primary.
	if err := fsys.WriteFile("/c.txt", []byte("x"), 0o644); !errors.Is(err, ErrChecksum) {
		t.Fatalf("write over a corrupt primary superblock: want checksum error, got %v", err)
	}
	fsys.Close()

	// All copies corrupt: typed error.
	raw[64<<20+sbfLabel] ^= 0xFF
	if err := os.WriteFile(img, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Open(img, -1)
	var ce *ChecksumError
	if !errors.As(err, &ce) || ce.Kind != "superblock" {
		t.Fatalf("want superblock *ChecksumError, got %v", err)
	}
}

func TestUnsupportedChecksumTypeRefused(t *testing.T) {
	for _, typ := range []uint16{1, 2, 3, 9} {
		img := csumImage(t)
		raw, err := os.ReadFile(img)
		if err != nil {
			t.Fatal(err)
		}
		sb := raw[0x10000 : 0x10000+sbfSize]
		binary.LittleEndian.PutUint16(sb[sbfCsumType:], typ)
		updateSuperblockCRC(sb)
		if err := os.WriteFile(img, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		for _, opts := range []OpenOptions{{}, {IgnoreChecksums: true}} {
			_, err := OpenWithOptions(img, -1, opts)
			var ue *UnsupportedChecksumError
			if !errors.As(err, &ue) || ue.Type != typ || !errors.Is(err, ErrUnsupportedChecksum) {
				t.Fatalf("csum_type %d opts %+v: want *UnsupportedChecksumError, got %v", typ, opts, err)
			}
		}
	}
}

// FuzzSuperblockChecksum feeds arbitrary 4 KiB superblock images: parsing never
// panics, and an image accepted without the opt-out has a matching checksum.
func FuzzSuperblockChecksum(f *testing.F) {
	img := filepath.Join(f.TempDir(), "img")
	fsys, err := Format(img, 64<<20, FormatConfig{})
	if err != nil {
		f.Fatal(err)
	}
	fsys.Close()
	raw, err := os.ReadFile(img)
	if err != nil {
		f.Fatal(err)
	}
	good := append([]byte(nil), raw[0x10000:0x10000+sbfSize]...)
	f.Add(good)
	bad := append([]byte(nil), good...)
	bad[sbfLabel] ^= 1 // content changed, checksum not
	f.Add(bad)
	f.Fuzz(func(t *testing.T, data []byte) {
		dev := make([]byte, 0x10000+sbfSize)
		copy(dev[0x10000:], data)
		sb, err := readSuperblockAt(bytes.NewReader(dev), 0, superblockOffset, false)
		if err != nil {
			return
		}
		if _, _, ok := verifyCRC32C(dev[0x10000:]); !ok {
			t.Fatalf("accepted a superblock whose checksum does not match (csum_type %d)", sb.csumType)
		}
	})
}
