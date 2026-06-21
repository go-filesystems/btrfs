// Package btrfs provides read/write access to Btrfs filesystem images.
// It targets the common Btrfs on-disk format (single device, CRC32c checksums)
// as used by Fedora Cloud images.
//
// Partition tables (MBR/GPT) are detected automatically; pass partIndex = -1
// to auto-select the first Linux data partition.
//
// Supported operations:
//   - Open, Close, Format (single device)
//   - ReadFile, ListDir, Stat, ReadLink
//   - WriteFile, MkDir, DeleteFile, DeleteDir, Rename
//   - Symlink, Link, Truncate
//   - Chmod, Chown, Chtimes, get/set volume label, get/set xattrs
//   - Grow / Resize (shrink trims free trailing space only)
//
// Multi-device profiles (RAID0/1/10/5/6/DUP) are decoded for reading;
// writes are single-device.
package filesystem_btrfs

import filesystem "github.com/go-filesystems/interface"

// On-disk magic numbers (little-endian).
const (
	// Superblock primary copy offset.
	superblockOffset int64 = 0x10000 // 64 KiB

	// Magic bytes at superblock+0x40.
	sbMagic uint64 = 0x4D5F53665248425F // "_BHRfS_M" LE

	// B-tree node header magic (v2 csum-enabled).
	nodeHeaderMagic uint32 = 0xEB176CBA

	// Well-known object IDs.
	rootTreeObjID  uint64 = 1
	chunkTreeObjID uint64 = 3
	fsTreeObjID    uint64 = 5

	rootDirObjID uint64 = 256 // BTRFS_FIRST_FREE_OBJECTID

	// Item type codes.
	typeInodeItem  uint8 = 0x01
	typeInodeRef   uint8 = 0x0C
	typeXattrItem  uint8 = 0x18
	typeDirItem    uint8 = 0x54
	typeDirIndex   uint8 = 0x60
	typeExtentData uint8 = 0x6C
	typeRootItem   uint8 = 0x84

	// DirItem file type values.
	ftUnknown uint8 = 0
	ftRegFile uint8 = 1
	ftDir     uint8 = 2
	ftSymlink uint8 = 7

	// Extent data type.
	extentDataInline  uint8 = 0
	extentDataRegular uint8 = 1

	// Compression algorithms used in EXTENT_DATA items.
	compressionNone uint8 = 0
	compressionZlib uint8 = 1
	compressionLzo  uint8 = 2
	compressionZstd uint8 = 3
)

// Verify implementation of the common filesystem interface + every
// capability interface btrfsFS supports.
var (
	_ filesystem.Filesystem     = (*btrfsFS)(nil)
	_ filesystem.Symlinker      = (*btrfsFS)(nil)
	_ filesystem.HardLinker     = (*btrfsFS)(nil)
	_ filesystem.Truncater      = (*btrfsFS)(nil)
	_ filesystem.MetadataSetter = (*btrfsFS)(nil)
	_ filesystem.Labeller       = (*btrfsFS)(nil)
	_ filesystem.Grower         = (*btrfsFS)(nil)
)
