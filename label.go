package filesystem_btrfs

import (
	"bytes"
	"encoding/binary"
	"fmt"

	filesystem "github.com/go-filesystems/interface"
)

// MaxLabelLen is the on-disk size of the btrfs volume label (sb_fname /
// sbfLabel, 256 bytes). The kernel imposes an additional limit of 255
// (to leave room for a trailing NUL), so we mirror that.
const MaxLabelLen = 255

// Compile-time assertion: btrfsFS implements filesystem.Labeller.
var _ filesystem.Labeller = (*btrfsFS)(nil)

// Label returns the current volume label, decoded from sb_fname. An empty
// string means no label is set.
func (fs *btrfsFS) Label() string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return string(bytes.TrimRight(fs.sb.label[:], "\x00"))
}

// SetLabel writes a new volume label into the primary superblock and any
// superblock mirror that's actually populated within the image (mkfs.btrfs
// writes mirrors at 64 MiB / 256 GiB on large-enough images; small images
// only have the primary at 64 KiB). Each touched superblock has its CRC
// recomputed.
//
// Concurrency: like ext4's SetLabel this is a direct read-mutate-write
// over each 4 KiB superblock — bypasses the COW write path. Use only on
// a filesystem no other writer is touching.
func (fs *btrfsFS) SetLabel(label string) error {
	b := []byte(label)
	if len(b) > MaxLabelLen {
		return fmt.Errorf("btrfs: label %q is %d bytes, exceeds maximum %d", label, len(b), MaxLabelLen)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Try each canonical mirror offset; the primary at 0x10000 must
	// succeed, mirrors at 64 MiB / 256 GiB are best-effort.
	mirrors := []int64{0x10000, 0x4000000, 0x4000000000}
	writtenPrimary := false
	for i, sbOff := range mirrors {
		buf := make([]byte, sbfSize)
		if _, err := fs.rwa.ReadAt(buf, fs.partOffset+sbOff); err != nil {
			if i == 0 {
				return fmt.Errorf("btrfs SetLabel: read primary superblock: %w", err)
			}
			continue // mirror is past EOF — image too small
		}
		if binary.LittleEndian.Uint64(buf[sbfMagic:]) != sbMagic {
			if i == 0 {
				return fmt.Errorf("btrfs SetLabel: bad magic in primary superblock")
			}
			continue // mirror unpopulated
		}

		// Zero the label slot, then copy. Keeps the trailing bytes clean
		// when the new label is shorter than the previous one.
		for j := 0; j < 256; j++ {
			buf[sbfLabel+j] = 0
		}
		copy(buf[sbfLabel:], b)
		updateSuperblockCRC(buf)

		if _, err := fs.rwa.WriteAt(buf, fs.partOffset+sbOff); err != nil {
			return fmt.Errorf("btrfs SetLabel: write superblock at 0x%x: %w", sbOff, err)
		}
		if i == 0 {
			writtenPrimary = true
		}
	}
	if !writtenPrimary {
		return fmt.Errorf("btrfs SetLabel: primary superblock not updated")
	}
	if err := fs.f.Sync(); err != nil {
		return fmt.Errorf("btrfs SetLabel: sync: %w", err)
	}

	// Keep the in-memory label coherent with on-disk truth.
	for i := 0; i < 256; i++ {
		fs.sb.label[i] = 0
	}
	copy(fs.sb.label[:], b)
	return nil
}
