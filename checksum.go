package filesystem_btrfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Metadata checksum verification.
//
// On-disk format reference: the Btrfs documentation, "On-disk Format" ->
// "Checksum" / "Superblock" / "Node header"
// (https://btrfs.readthedocs.io/en/latest/dev/On-disk-format.html), and the
// kernel UAPI header include/uapi/linux/btrfs_tree.h (BTRFS_CSUM_TYPE_*,
// BTRFS_CSUM_SIZE = 32). Every metadata object starts with a 32-byte csum
// field; the checksum covers every byte AFTER that field: bytes [32, 4096) of a
// superblock, bytes [32, nodesize) of a tree block. For crc32c the 4-byte
// result is stored little-endian at offset 0 and the remaining 28 bytes are
// zero. Superblock copies live at 64 KiB, 64 MiB, 256 GiB and 1 PiB
// (BTRFS_SUPER_INFO_OFFSET and the two mirrors, btrfs_sb_offset() in
// fs/btrfs/disk-io.c, which the kernel tries in turn and validates one by one).

// Superblock csum_type values (btrfs_tree.h enum btrfs_csum_type).
const (
	csumTypeXXHash uint16 = 1
	csumTypeSHA256 uint16 = 2
	csumTypeBlake2 uint16 = 3
)

// Offsets of the superblock mirrors relative to the start of the filesystem:
// 64 MiB and 256 GiB and 1 PiB (the primary is superblockOffset, 64 KiB).
var backupSuperblockOffsets = [...]int64{64 << 20, 256 << 30, 1 << 50}

// csumNodeStart is the size of the csum field: the checksum covers everything
// after it.
const csumNodeStart = 32

// ErrChecksum is matched (errors.Is) by every *ChecksumError.
var ErrChecksum = errors.New("btrfs: checksum mismatch")

// ErrUnsupportedChecksum is matched (errors.Is) by every
// *UnsupportedChecksumError.
var ErrUnsupportedChecksum = errors.New("btrfs: unsupported checksum type")

// ChecksumError reports a superblock or tree block whose stored checksum does
// not match the checksum of its content: the image was corrupted or tampered
// with. Retrieve it with errors.As. Open(WithOptions) with IgnoreChecksums
// skips the check for forensic reads of damaged images.
type ChecksumError struct {
	Kind     string // "superblock" or "tree block"
	Physical int64  // byte offset on the device of the object
	Logical  uint64 // logical address (tree blocks only; 0 for a superblock)
	Stored   uint32 // checksum found in the csum field (crc32c)
	Computed uint32 // checksum of the content
}

func (e *ChecksumError) Error() string {
	if e.Kind == "superblock" {
		return fmt.Sprintf("btrfs: superblock at 0x%X: checksum mismatch (stored 0x%08X, computed 0x%08X)",
			e.Physical, e.Stored, e.Computed)
	}
	return fmt.Sprintf("btrfs: tree block at logical 0x%X (phys 0x%X): checksum mismatch (stored 0x%08X, computed 0x%08X)",
		e.Logical, e.Physical, e.Stored, e.Computed)
}

// Is makes errors.Is(err, ErrChecksum) true.
func (e *ChecksumError) Is(target error) bool { return target == ErrChecksum }

// UnsupportedChecksumError reports a superblock csum_type this package cannot
// verify (and, because writes recompute checksums with crc32c only, cannot
// safely modify). The image is refused rather than served unverified; this is
// also the case under IgnoreChecksums, since a read-modify-write would corrupt
// it. Retrieve it with errors.As.
type UnsupportedChecksumError struct {
	Type uint16
	Name string
}

func (e *UnsupportedChecksumError) Error() string {
	return fmt.Sprintf("btrfs: unsupported checksum type %d (%s): only crc32c is implemented", e.Type, e.Name)
}

// Is makes errors.Is(err, ErrUnsupportedChecksum) true.
func (e *UnsupportedChecksumError) Is(target error) bool { return target == ErrUnsupportedChecksum }

// checkCsumType refuses every csum_type other than crc32c.
func checkCsumType(t uint16) error {
	switch t {
	case csumTypeCRC32C:
		return nil
	case csumTypeXXHash:
		return &UnsupportedChecksumError{Type: t, Name: "xxhash64"}
	case csumTypeSHA256:
		return &UnsupportedChecksumError{Type: t, Name: "sha256"}
	case csumTypeBlake2:
		return &UnsupportedChecksumError{Type: t, Name: "blake2b"}
	}
	return &UnsupportedChecksumError{Type: t, Name: "unknown"}
}

// verifyCRC32C checks buf's csum field against crc32c(buf[32:]). It does not
// allocate.
func verifyCRC32C(buf []byte) (stored, computed uint32, ok bool) {
	if len(buf) <= csumNodeStart {
		return 0, 0, false
	}
	stored = binary.LittleEndian.Uint32(buf[0:])
	computed = crc32cSum(buf[csumNodeStart:], btrfsCsumSeed)
	return stored, computed, stored == computed
}

// verifyTreeBlock checks the checksum of a tree block read from phys (logical
// address logAddr). It is a no-op when sb.ignoreCsum is set.
func verifyTreeBlock(sb *superblock, buf []byte, phys int64, logAddr uint64) error {
	if sb.ignoreCsum {
		return nil
	}
	if err := checkCsumType(sb.csumType); err != nil {
		return err
	}
	stored, computed, ok := verifyCRC32C(buf)
	if !ok {
		return &ChecksumError{Kind: "tree block", Physical: phys, Logical: logAddr, Stored: stored, Computed: computed}
	}
	return nil
}

// verifySuperblockBuf checks the checksum of a 4 KiB superblock image read from
// phys.
func verifySuperblockBuf(buf []byte, phys int64) error {
	if len(buf) < sbfSize {
		return &ChecksumError{Kind: "superblock", Physical: phys}
	}
	stored, computed, ok := verifyCRC32C(buf[:sbfSize])
	if !ok {
		return &ChecksumError{Kind: "superblock", Physical: phys, Stored: stored, Computed: computed}
	}
	return nil
}

// verifySuperblockRMW is used by the read-modify-write paths that re-checksum a
// superblock they just patched: patching a corrupt superblock and recomputing
// its crc would launder the corruption into a "valid" one, so refuse first.
func verifySuperblockRMW(sb *superblock, buf []byte, phys int64) error {
	if sb != nil && sb.ignoreCsum {
		return nil
	}
	return verifySuperblockBuf(buf, phys)
}

// readSuperblockFallback reads the primary superblock and, when its checksum
// does not match, the backup copies (64 MiB, 256 GiB, 1 PiB), returning the
// valid copy with the highest generation (as btrfs_read_dev_super does). The
// primary's *ChecksumError is returned when no backup is valid either. Any
// other failure of the primary (read error, bad magic, unsupported checksum)
// is returned as is: a backup is only a remedy for a checksum mismatch.
func readSuperblockFallback(r io.ReaderAt, partOff int64, ignore bool) (*superblock, error) {
	sb, err := readSuperblockAt(r, partOff, superblockOffset, ignore)
	if err == nil {
		return sb, nil
	}
	var ce *ChecksumError
	if !errors.As(err, &ce) {
		return nil, err
	}
	var best *superblock
	for _, off := range backupSuperblockOffsets {
		b, berr := readSuperblockAt(r, partOff, off, ignore)
		if berr != nil {
			continue
		}
		if best == nil || b.generation > best.generation {
			best = b
		}
	}
	if best == nil {
		return nil, err
	}
	return best, nil
}
