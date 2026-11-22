package filesystem_btrfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Guard tests: each internal path that reads a tree block or a superblock and
// then rewrites it must refuse a corrupt one (a rewrite recomputes the crc and
// would launder the corruption).

func csumOpen(t *testing.T, img string, opts OpenOptions) *btrfsFS {
	t.Helper()
	fsys, err := OpenWithOptions(img, -1, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fsys.Close() })
	return fsys.(*btrfsFS)
}

// flipBlock changes one header byte of the tree block at logAddr, leaving its
// stored checksum untouched.
func flipBlock(t *testing.T, fs *btrfsFS, logAddr uint64) int64 {
	t.Helper()
	phys, err := fs.sb.physAddr(fs.partOffset, logAddr)
	if err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := fs.rwa.ReadAt(b[:], phys+40); err != nil {
		t.Fatal(err)
	}
	b[0] ^= 1
	if _, err := fs.rwa.WriteAt(b[:], phys+40); err != nil {
		t.Fatal(err)
	}
	return phys
}

func TestCsumGuardTreeBlockConsumers(t *testing.T) {
	cases := []struct {
		name  string
		block func(fs *btrfsFS) uint64
		call  func(fs *btrfsFS, phys int64) error
	}{
		{"chunk-size", func(fs *btrfsFS) uint64 { return fs.sb.chunkLogAddr },
			func(fs *btrfsFS, _ int64) error { return fs.rewriteChunkSizeLocked(0, 0) }},
		{"chunk-devitem", func(fs *btrfsFS) uint64 { return fs.sb.chunkLogAddr },
			func(fs *btrfsFS, _ int64) error { return fs.rewriteChunkTreeDevItemLocked(1, 1) }},
		{"delete-leaf-item", func(fs *btrfsFS) uint64 { return fs.sb.chunkLogAddr },
			func(fs *btrfsFS, phys int64) error { return fs.deleteLeafItemInPlace(phys, 0, 0, 0) }},
		{"relocate-leaf", func(fs *btrfsFS) uint64 { return fs.sb.rootLogAddr },
			func(fs *btrfsFS, _ int64) error {
				_, err := fs.relocateLeafBlock(fs.sb.rootLogAddr, 1<<40)
				return err
			}},
		{"repoint-root-item", func(fs *btrfsFS) uint64 { return fs.sb.rootLogAddr },
			func(fs *btrfsFS, _ int64) error { return fs.repointRootItemLocked(extentTreeObjID, 1) }},
		{"repoint-extent-root", func(fs *btrfsFS) uint64 { return fs.sb.rootLogAddr },
			func(fs *btrfsFS, _ int64) error { return repointExtentRoot(fs.rwa, fs.partOffset, fs.sb, 1, 0) }},
		{"extent-root-generation", func(fs *btrfsFS) uint64 { return fs.sb.rootLogAddr },
			func(fs *btrfsFS, _ int64) error {
				return updateExtentRootGeneration(fs.rwa, fs.partOffset, fs.sb, 1)
			}},
		{"cow-node", func(fs *btrfsFS) uint64 { return fs.sb.rootLogAddr },
			func(fs *btrfsFS, _ int64) error {
				c := &cowCtx{rwa: fs.rwa, sb: fs.sb, partOff: fs.partOffset, sm: fs.sm}
				_, _, _, err := c.cowNode(fs.sb.rootLogAddr)
				return err
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := csumOpen(t, csumImage(t), OpenOptions{})
			phys := flipBlock(t, fs, tc.block(fs))
			err := tc.call(fs, phys)
			var ce *ChecksumError
			if !errors.As(err, &ce) {
				t.Fatalf("want *ChecksumError, got %v", err)
			}
		})
	}
}

func TestCsumGuardRebuildExtentTreeRefusesCorruptRoot(t *testing.T) {
	fs := csumOpen(t, csumImage(t), OpenOptions{})
	extRoot, err := extentTreeRoot(fs.rwa, fs.partOffset, fs.sb)
	if err != nil {
		t.Fatal(err)
	}
	flipBlock(t, fs, extRoot)
	if err := rebuildExtentTree(fs.rwa, fs.partOffset, fs.sb, fs.sm); !errors.Is(err, ErrChecksum) {
		t.Fatalf("want a checksum error, got %v", err)
	}
}

// corruptPrimaryWithBackup returns an image whose primary superblock fails its
// checksum and whose 64 MiB mirror is a valid copy of the original.
func corruptPrimaryWithBackup(t *testing.T) string {
	t.Helper()
	img := csumImage(t)
	raw, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}
	copy(raw[64<<20:], raw[0x10000:0x10000+sbfSize])
	raw[0x10000+sbfLabel] ^= 0x55
	if err := os.WriteFile(img, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return img
}

func TestCsumGuardSuperblockReadModifyWrite(t *testing.T) {
	cases := map[string]func(fs *btrfsFS) error{
		"bytes-used":      func(fs *btrfsFS) error { return updateSuperBytesUsed(fs.rwa, fs.partOffset, fs.sb, 1) },
		"write-super":     func(fs *btrfsFS) error { return writeSuperblock(fs.rwa, fs.partOffset, fs.sb, fs.fsTreeRoot) },
		"sys-chunk-set":   func(fs *btrfsFS) error { return fs.rewriteSysChunkArrayEntryLocked(1, 1) },
		"sys-chunk-del":   func(fs *btrfsFS) error { return fs.deleteSysChunkArrayEntryLocked(1) },
		"resized-super":   func(fs *btrfsFS) error { return fs.rewriteResizedSuperblockLocked(1 << 20) },
		"set-label":       func(fs *btrfsFS) error { return fs.SetLabel("x") },
		"extent-rebuild":  func(fs *btrfsFS) error { return updateSuperBytesUsed(fs.rwa, fs.partOffset, nil, 1) },
		"write-file-path": func(fs *btrfsFS) error { return fs.WriteFile("/z.txt", []byte("z"), 0o644) },
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			fs := csumOpen(t, corruptPrimaryWithBackup(t), OpenOptions{})
			var ce *ChecksumError
			if err := call(fs); !errors.As(err, &ce) || ce.Kind != "superblock" {
				t.Fatalf("want superblock *ChecksumError, got %v", err)
			}
		})
	}
}

func TestCsumGuardIgnoreChecksumsAllowsSuperblockRewrite(t *testing.T) {
	img := csumImage(t)
	raw, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}
	raw[0x10000+sbfLabel+200] ^= 0x55 // inside the label, checksum stale
	if err := os.WriteFile(img, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	fs := csumOpen(t, img, OpenOptions{IgnoreChecksums: true})
	if err := fs.SetLabel("repaired"); err != nil {
		t.Fatalf("forensic SetLabel: %v", err)
	}
	if err := verifySuperblockBuf(readAt(t, fs, 0x10000, sbfSize), 0x10000); err != nil {
		t.Fatalf("SetLabel left a bad checksum: %v", err)
	}
}

func readAt(t *testing.T, fs *btrfsFS, off int64, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := fs.rwa.ReadAt(b, fs.partOffset+off); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCsumGuardSetLabelLeavesCorruptMirrorAlone(t *testing.T) {
	img := csumImage(t)
	raw, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}
	copy(raw[64<<20:], raw[0x10000:0x10000+sbfSize])
	raw[64<<20+sbfLabel+200] ^= 0x55 // corrupt mirror, primary intact
	before := append([]byte(nil), raw[64<<20:64<<20+sbfSize]...)
	if err := os.WriteFile(img, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	fs := csumOpen(t, img, OpenOptions{})
	if err := fs.SetLabel("new"); err != nil {
		t.Fatal(err)
	}
	if got := readAt(t, fs, 64<<20, sbfSize); !bytes.Equal(got, before) {
		t.Fatal("SetLabel re-checksummed a corrupt mirror")
	}
}

// sparseDev serves 4 KiB superblock copies at chosen offsets and zeros
// elsewhere.
type sparseDev map[int64][]byte

func (d sparseDev) ReadAt(p []byte, off int64) (int, error) {
	clear(p)
	if b, ok := d[off]; ok {
		copy(p, b)
	}
	return len(p), nil
}

func goodSuperblockCopy(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(csumImage(t))
	if err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), raw[0x10000:0x10000+sbfSize]...)
}

func withGeneration(sb []byte, gen uint64) []byte {
	out := append([]byte(nil), sb...)
	binary.LittleEndian.PutUint64(out[sbfGeneration:], gen)
	updateSuperblockCRC(out)
	return out
}

func TestCsumGuardFallbackPicksHighestGeneration(t *testing.T) {
	good := goodSuperblockCopy(t)
	bad := append([]byte(nil), good...)
	bad[sbfLabel] ^= 1
	dev := sparseDev{
		superblockOffset:           bad,
		backupSuperblockOffsets[0]: withGeneration(good, 5),
		backupSuperblockOffsets[1]: withGeneration(good, 9),
		backupSuperblockOffsets[2]: withGeneration(good, 7),
	}
	sb, err := readSuperblockFallback(dev, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if sb.generation != 9 {
		t.Fatalf("picked generation %d, want 9", sb.generation)
	}
	// A backup whose own checksum is wrong is not a candidate.
	dev[backupSuperblockOffsets[1]] = append([]byte(nil), bad...)
	if sb, err = readSuperblockFallback(dev, 0, false); err != nil || sb.generation != 7 {
		t.Fatalf("got %v, %v; want generation 7", sb, err)
	}
	// The 1 PiB copy is read too.
	dev[backupSuperblockOffsets[0]] = bad
	dev[backupSuperblockOffsets[2]] = withGeneration(good, 11)
	if sb, err = readSuperblockFallback(dev, 0, false); err != nil || sb.generation != 11 {
		t.Fatalf("got %v, %v; want generation 11", sb, err)
	}
}

func TestCsumGuardFallbackOnlyForChecksumMismatch(t *testing.T) {
	good := goodSuperblockCopy(t)
	badMagic := append([]byte(nil), good...)
	badMagic[sbfMagic] ^= 1
	updateSuperblockCRC(badMagic)
	dev := sparseDev{superblockOffset: badMagic, backupSuperblockOffsets[0]: withGeneration(good, 5)}
	_, err := readSuperblockFallback(dev, 0, false)
	var ce *ChecksumError
	if err == nil || errors.As(err, &ce) {
		t.Fatalf("a bad magic must not fall back to a backup: %v", err)
	}
	// No valid backup: the primary's own checksum error comes back.
	bad := append([]byte(nil), good...)
	bad[sbfLabel] ^= 1
	if _, err = readSuperblockFallback(sparseDev{superblockOffset: bad}, 0, false); !errors.As(err, &ce) || ce.Physical != superblockOffset {
		t.Fatalf("want the primary's *ChecksumError, got %v", err)
	}
}

func TestCsumGuardChecksumErrorText(t *testing.T) {
	sbErr := (&ChecksumError{Kind: "superblock", Physical: 0x10000, Stored: 1, Computed: 2}).Error()
	tbErr := (&ChecksumError{Kind: "tree block", Physical: 0x20000, Logical: 0x30000, Stored: 1, Computed: 2}).Error()
	if !bytes.Contains([]byte(sbErr), []byte("superblock")) || !bytes.Contains([]byte(tbErr), []byte("logical 0x30000")) {
		t.Fatalf("messages lack the location: %q, %q", sbErr, tbErr)
	}
	if err := checkCsumType(csumTypeCRC32C); err != nil {
		t.Fatal(err)
	}
	if err := verifySuperblockBuf(make([]byte, 10), 0); !errors.Is(err, ErrChecksum) {
		t.Fatalf("short superblock: %v", err)
	}
	if _, _, ok := verifyCRC32C(make([]byte, 10)); ok {
		t.Fatal("short buffer verified")
	}
}

var _ = filepath.Join
