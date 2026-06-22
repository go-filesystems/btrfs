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

	// Well-known object IDs (tree objectids and special keys).
	rootTreeObjID   uint64 = 1
	extentTreeObjID uint64 = 2
	chunkTreeObjID  uint64 = 3
	devTreeObjID    uint64 = 4
	fsTreeObjID     uint64 = 5
	csumTreeObjID   uint64 = 7
	uuidTreeObjID   uint64 = 9
	// dataRelocTreeObjID is BTRFS_DATA_RELOC_TREE_OBJECTID (-9 as a signed
	// objectid). open_ctree's btrfs_read_roots requires this root to exist
	// (btrfs_get_fs_root(DATA_RELOC, true) returns -ENOENT otherwise, which the
	// kernel misreports as "failed to read root (objectid=4)").
	dataRelocTreeObjID  uint64 = 0xFFFFFFFFFFFFFFF7
	devItemsObjID       uint64 = 1   // BTRFS_DEV_ITEMS_OBJECTID
	firstChunkTreeObjID uint64 = 256 // BTRFS_FIRST_CHUNK_TREE_OBJECTID
	devStatsObjID       uint64 = 0   // BTRFS_DEV_STATS_OBJECTID

	rootDirObjID uint64 = 256 // BTRFS_FIRST_FREE_OBJECTID

	// Item type codes.
	typeInodeItem      uint8 = 0x01
	typePersistentItem uint8 = 0xF9 // BTRFS_PERSISTENT_ITEM_KEY (DEV_STATS uses this)
	typeInodeRef       uint8 = 0x0C
	typeXattrItem      uint8 = 0x18
	typeDirItem        uint8 = 0x54
	typeDirIndex       uint8 = 0x60
	typeExtentData     uint8 = 0x6C
	typeRootItem       uint8 = 0x84
	typeExtentItem     uint8 = 0xA8
	typeMetadataItem   uint8 = 0xA9
	typeTreeBlockRef   uint8 = 0xB0
	typeBlockGroupItem uint8 = 0xC0
	typeDevExtent      uint8 = 0xCC
	typeDevItem        uint8 = 0xD8
	typeChunkItem      uint8 = 0xE4

	// extent_item flags.
	extentFlagTreeBlock uint64 = 1 << 1 // BTRFS_EXTENT_FLAG_TREE_BLOCK

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
