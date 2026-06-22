package filesystem_btrfs

import (
	"errors"
	"fmt"
	"io"

	"github.com/go-volumes/gpt"
)

const sectorSize = 512

// linuxPartTypeGPT is the GUID for a Linux filesystem partition (LE wire form).
var linuxPartTypeGPT = gpt.LinuxFilesystemGUID

// partitionOffset returns the byte offset to the start of the requested
// partition. Pass -1 to auto-select the first Linux data partition.
//
// Partition-table parsing is delegated to the hardened go-volumes/gpt parser,
// which validates every entry size, count, LBA, and partition extent against
// the device size (rejecting overflowing or out-of-range geometry) before
// returning anything. A bare image with no partition table (gpt.ErrNoTable)
// falls back to offset 0, matching btrfs images written directly onto a whole
// device or a raw file.
func partitionOffset(r io.ReaderAt, partIndex int) (int64, error) {
	dev := deviceSize(r)

	if partIndex >= 0 {
		p, err := gpt.ByIndex(r, dev, partIndex)
		if err != nil {
			if errors.Is(err, gpt.ErrNoTable) {
				// Bare image: index 0 means "the whole device".
				if partIndex == 0 {
					return 0, nil
				}
				return 0, fmt.Errorf("btrfs: partition index %d on bare image: %w", partIndex, err)
			}
			return 0, fmt.Errorf("btrfs: locate partition %d: %w", partIndex, err)
		}
		return p.StartOffset, nil
	}

	// Auto-select: prefer the first Linux-filesystem GPT partition; otherwise
	// fall back to the first MBR Linux (type 0x83) partition; otherwise the
	// first populated partition of any scheme.
	parts, err := gpt.List(r, dev)
	if err != nil {
		if errors.Is(err, gpt.ErrNoTable) {
			// Bare image: the filesystem starts at offset 0.
			return 0, nil
		}
		return 0, fmt.Errorf("btrfs: read partition table: %w", err)
	}
	for _, p := range parts {
		if p.Scheme == gpt.SchemeGPT && p.TypeGUID == linuxPartTypeGPT {
			return p.StartOffset, nil
		}
	}
	for _, p := range parts {
		if p.Scheme == gpt.SchemeMBR && p.MBRType == 0x83 {
			return p.StartOffset, nil
		}
	}
	return 0, fmt.Errorf("btrfs: no Linux data partition found in partition table")
}
