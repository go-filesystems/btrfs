package filesystem_btrfs

import (
	"encoding/binary"
	"fmt"
	"io"
)

// inodeItem mirrors the on-disk btrfs_inode_item (160 bytes).
// We only decode the fields we need.
type inodeItem struct {
	num   uint64 // key objectid (= inode number)
	mode  uint16
	size  uint64
	nlink uint32
}

// btrfs_inode_item flags (subset). Stored at inodeOffFlags as uint64 LE.
const (
	inodeFlagNoDataSum  uint64 = 1 << 0 // data has no checksum in the csum_tree
	inodeFlagNoDataCOW  uint64 = 1 << 1
	inodeFlagReadOnly   uint64 = 1 << 2
	inodeFlagNoCompress uint64 = 1 << 3
)

// INODE_ITEM layout offsets (all LE).
const (
	inodeOffGeneration = 0x00 // uint64
	inodeOffTransID    = 0x08 // uint64
	inodeOffSize       = 0x10 // uint64
	inodeOffNBytes     = 0x18 // uint64
	inodeOffBlockGroup = 0x20 // uint64
	inodeOffNLink      = 0x28 // uint32
	inodeOffUID        = 0x2C // uint32
	inodeOffGID        = 0x30 // uint32
	inodeOffMode       = 0x34 // uint32 (lower 16 bits are mode)
	inodeOffRDev       = 0x38 // uint64
	inodeOffFlags      = 0x40 // uint64
	inodeOffSequence   = 0x48 // uint64
	// 32 reserved bytes between 0x50 and 0x6F.
	inodeOffATime = 0x70 // btrfs_timespec (sec int64 + nsec uint32 = 12 bytes)
	inodeOffCTime = 0x7C
	inodeOffMTime = 0x88
	inodeOffOTime = 0x94
	inodeItemSize = 0xA0 // 160 bytes
)

func (in *inodeItem) isRegular() bool { return in.mode&0xF000 == 0x8000 }
func (in *inodeItem) isDir() bool     { return in.mode&0xF000 == 0x4000 }
func (in *inodeItem) isSymlink() bool { return in.mode&0xF000 == 0xA000 }

// readInode reads the INODE_ITEM for the given inode number from the FS tree.
func readInode(r io.ReaderAt, partOff int64, sb *superblock, fsTreeRoot uint64, ino uint64) (*inodeItem, error) {
	buf, it, err := searchTree(r, partOff, sb, fsTreeRoot, ino, typeInodeItem, 0)
	if err != nil {
		return nil, fmt.Errorf("btrfs: inode %d: %w", ino, err)
	}
	d := it.data(buf)
	if len(d) < inodeItemSize {
		return nil, fmt.Errorf("btrfs: inode %d: INODE_ITEM too short (%d bytes)", ino, len(d))
	}
	le := binary.LittleEndian
	return &inodeItem{
		num:   ino,
		size:  le.Uint64(d[inodeOffSize:]),
		nlink: le.Uint32(d[inodeOffNLink:]),
		mode:  uint16(le.Uint32(d[inodeOffMode:])),
	}, nil
}
