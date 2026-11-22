package filesystem_btrfs

import (
	"errors"
	"testing"
)

var errBoom = errors.New("boom")

type failReader struct{}

func (failReader) ReadAt([]byte, int64) (int, error) { return 0, errBoom }

// nthCorrupt corrupts every read of the block at phys from the from-th one on,
// so that a second read of a block (the one a rewrite path makes after a
// lookup already read it) can be the tampered one.
type nthCorrupt struct {
	readerWriterAt
	phys      int64
	from, n   int
	delivered bool
}

func (c *nthCorrupt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.readerWriterAt.ReadAt(p, off)
	if off == c.phys && len(p) > 48 {
		c.n++
		if c.n >= c.from {
			p[40] ^= 1
			c.delivered = true
		}
	}
	return n, err
}

func TestCsumGuardSecondReadOfRootLeafIsVerified(t *testing.T) {
	calls := map[string]func(fs *btrfsFS, r readerWriterAt) error{
		"repoint-extent-root": func(fs *btrfsFS, r readerWriterAt) error { return repointExtentRoot(r, fs.partOffset, fs.sb, 1, 0) },
		"extent-root-generation": func(fs *btrfsFS, r readerWriterAt) error {
			return updateExtentRootGeneration(r, fs.partOffset, fs.sb, 1)
		},
		"repoint-root-item": func(fs *btrfsFS, r readerWriterAt) error {
			fs.rwa = r
			return fs.repointRootItemLocked(extentTreeObjID, 1)
		},
	}
	for name, call := range calls {
		for from := 1; from <= 4; from++ {
			fs := csumOpen(t, csumImage(t), OpenOptions{})
			phys, err := fs.sb.physAddr(fs.partOffset, fs.sb.rootLogAddr)
			if err != nil {
				t.Fatal(err)
			}
			real := fs.rwa
			c := &nthCorrupt{readerWriterAt: real, phys: phys, from: from}
			err = call(fs, c)
			if c.delivered && !errors.Is(err, ErrChecksum) {
				t.Fatalf("%s from=%d: corrupt leaf delivered but err = %v", name, from, err)
			}
		}
	}
}

func TestCsumGuardReadErrorsPropagate(t *testing.T) {
	sb := &superblock{nodeSize: 4096}
	if err := readTreeBlockAt(failReader{}, sb, make([]byte, 4096), 0, 0); !errors.Is(err, errBoom) || errors.Is(err, ErrChecksum) {
		t.Fatalf("readTreeBlockAt: %v", err)
	}
	if _, err := readSuperblockAt(failReader{}, 0, superblockOffset, false); !errors.Is(err, errBoom) {
		t.Fatalf("readSuperblockAt: %v", err)
	}
}

func TestCsumGuardUnsupportedTypeRefusedOnTreeBlock(t *testing.T) {
	fs := csumOpen(t, csumImage(t), OpenOptions{})
	buf := make([]byte, fs.sb.nodeSize)
	phys, err := fs.sb.physAddr(fs.partOffset, fs.sb.rootLogAddr)
	if err != nil {
		t.Fatal(err)
	}
	if err := readTreeBlockAt(fs.rwa, fs.sb, buf, phys, fs.sb.rootLogAddr); err != nil {
		t.Fatal(err)
	}
	sb := *fs.sb
	sb.csumType = csumTypeSHA256
	if err := verifyTreeBlock(&sb, buf, phys, fs.sb.rootLogAddr); !errors.Is(err, ErrUnsupportedChecksum) {
		t.Fatalf("a valid block under an unsupported csum_type must be refused: %v", err)
	}
}

func TestCsumGuardCRCBoundaries(t *testing.T) {
	if _, _, ok := verifyCRC32C(make([]byte, csumNodeStart)); ok {
		t.Fatal("a header-only buffer has no payload and must not verify")
	}
	if _, _, ok := verifyCRC32C(make([]byte, csumNodeStart+1)); ok {
		t.Fatal("a zero csum over a non-empty zero payload must not verify")
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("short superblock buffer panicked: %v", r)
			}
		}()
		var ce *ChecksumError
		if err := verifySuperblockBuf(make([]byte, sbfSize-1), 7); !errors.As(err, &ce) || ce.Physical != 7 {
			t.Fatalf("short superblock: %v", err)
		}
	}()
}

func TestCsumGuardFallbackReturnsValidPrimary(t *testing.T) {
	good := goodSuperblockCopy(t)
	sb, err := readSuperblockFallback(sparseDev{superblockOffset: good, backupSuperblockOffsets[0]: withGeneration(good, 99)}, 0, false)
	if err != nil || sb == nil {
		t.Fatalf("valid primary: %v, %v", sb, err)
	}
	if sb.generation == 99 {
		t.Fatal("a valid primary must not be replaced by a backup")
	}
}
