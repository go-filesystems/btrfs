package filesystem_btrfs

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// FormatConfig holds optional parameters for Format.
// All fields are optional; sensible defaults are used when left at their zero value.
type FormatConfig struct {
	// UUID is the filesystem UUID. A random UUID is generated when all bytes are zero.
	UUID [16]byte
	// Label is the volume label (up to 255 bytes, NUL-padded on disk).
	Label string
}

// Minimum image size: superblock (64 KiB) + 3 B-tree nodes + some data space.
// We use 4 KiB nodes and require at least 1 MiB.
const (
	fmtNodeSize      = 4096
	fmtSuperblockOff = 0x10000 // primary superblock at 64 KiB
	fmtChunkPhys     = 0x020000
	fmtRootPhys      = 0x021000
	fmtFSPhys        = 0x022000
	fmtFirstFreePhys = 0x023000
	fmtMinSize       = 0x100000 // 1 MiB
)

type btrfsFormatFile interface {
	WriteAt([]byte, int64) (int, error)
	Truncate(int64) error
	Close() error
}

var btrfsFormatOpenFile = func(path string) (btrfsFormatFile, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
}

var btrfsFormatRandRead = func(p []byte) (int, error) {
	return rand.Read(p)
}

var btrfsFormatOpenFS = Open

// Format creates a new Btrfs filesystem in the file at path.
// The file is created (or truncated) and formatted. sizeBytes must be at least
// 1 MiB and a multiple of 4096.
//
// On success the newly formatted filesystem is opened and returned; the
// caller must Close it when done.
func Format(path string, sizeBytes int64, cfg FormatConfig) (filesystem.Filesystem, error) {
	if sizeBytes%fmtNodeSize != 0 {
		return nil, fmt.Errorf("btrfs: format: size %d is not a multiple of %d", sizeBytes, fmtNodeSize)
	}
	if sizeBytes < fmtMinSize {
		return nil, fmt.Errorf("btrfs: format: size %d too small (minimum %d bytes)", sizeBytes, fmtMinSize)
	}

	f, err := btrfsFormatOpenFile(path)
	if err != nil {
		return nil, fmt.Errorf("btrfs: format: %w", err)
	}
	if err := f.Truncate(sizeBytes); err != nil {
		f.Close()
		return nil, fmt.Errorf("btrfs: format: truncate: %w", err)
	}

	// Generate UUID if not provided.
	uuid := cfg.UUID
	if uuid == [16]byte{} {
		if _, err := btrfsFormatRandRead(uuid[:]); err != nil {
			f.Close()
			return nil, fmt.Errorf("btrfs: format: generate UUID: %w", err)
		}
	}

	imageSize := uint64(sizeBytes)
	le := binary.LittleEndian

	// ── Helper: build an empty leaf node ────────────────────────────────────
	// Node header layout (little-endian):
	//   [0:32]   checksum (filled by updateNodeCRC)
	//   [32:48]  fs UUID
	//   [0x30]   bytenr  (uint64)
	//   [0x38]   flags   (uint64)
	//   [0x40:56] chunk_tree_uuid
	//   [0x50]   generation (uint64)
	//   [0x58]   owner    (uint64)
	//   [0x60]   nritems  (uint32)
	//   [0x64]   level    (uint8)
	buildEmptyLeafNode := func(physAddr uint64, generation uint64) []byte {
		buf := make([]byte, fmtNodeSize)
		copy(buf[32:48], uuid[:])
		le.PutUint64(buf[0x30:], physAddr)
		le.PutUint64(buf[0x50:], generation)
		le.PutUint32(buf[0x60:], 0)
		buf[0x64] = 0 // level = 0 (leaf)
		updateNodeCRC(buf)
		return buf
	}

	// ── Chunk tree leaf (at fmtChunkPhys) ───────────────────────────────────
	// Contains the CHUNK_ITEM describing the single SYSTEM chunk [0, imageSize).
	chunkLeaf := buildEmptyLeafNode(fmtChunkPhys, 1)
	sysChunkItem := buildSysChunkItemBytes(le, imageSize)
	_ = leafInsertItem(chunkLeaf, key{1, 0xE4, 0}, sysChunkItem)
	le.PutUint64(chunkLeaf[0x50:], 1)
	updateNodeCRC(chunkLeaf)

	// ── Root tree leaf (at fmtRootPhys) ─────────────────────────────────────
	// Contains a ROOT_ITEM pointing to the FS tree at fmtFSPhys.
	rootLeaf := buildEmptyLeafNode(fmtRootPhys, 1)
	rootItemData := make([]byte, 439)
	// btrfs_root_item layout: inode(160) + generation(8) + root_dirid(8) +
	// bytenr(8) at offset 0xB0. resolveRootTree reads bytenr at 0xB0 so we
	// must put it there.
	le.PutUint64(rootItemData[0xA0:], 1)        // generation
	le.PutUint64(rootItemData[0xA8:], 256)      // root_dirid (root inode obj ID)
	le.PutUint64(rootItemData[0xB0:], fmtFSPhys) // root bytenr
	_ = leafInsertItem(rootLeaf, key{fsTreeObjID, typeRootItem, 0}, rootItemData)
	le.PutUint64(rootLeaf[0x50:], 1)
	updateNodeCRC(rootLeaf)

	// ── FS tree leaf (at fmtFSPhys) ─────────────────────────────────────────
	// Contains the root directory inode (ino 256).
	fsLeaf := buildEmptyLeafNode(fmtFSPhys, 1)
	rinode := make([]byte, inodeItemSize)
	le.PutUint64(rinode[inodeOffGeneration:], 1)
	// Empty root dir: "." + ".." (both pointing back at the root) → nlink=2.
	// Each subdir created later bumps this by 1 via adjustDirNlink.
	le.PutUint32(rinode[inodeOffNLink:], 2)
	le.PutUint32(rinode[inodeOffMode:], 0x41ED) // directory + rwxr-xr-x
	le.PutUint64(rinode[inodeOffFlags:], inodeFlagNoDataSum)
	now := time.Now().UTC()
	writeBtrfsTimespec(rinode[inodeOffATime:], now)
	writeBtrfsTimespec(rinode[inodeOffCTime:], now)
	writeBtrfsTimespec(rinode[inodeOffMTime:], now)
	writeBtrfsTimespec(rinode[inodeOffOTime:], now)
	_ = leafInsertItem(fsLeaf, key{rootDirObjID, typeInodeItem, 0}, rinode)
	dotDI := encodeDirItem(rootDirObjID, typeInodeItem, ftDir, ".")
	_ = leafInsertItem(fsLeaf, key{rootDirObjID, typeDirIndex, 1}, dotDI)
	dotdotDI := encodeDirItem(rootDirObjID, typeInodeItem, ftDir, "..")
	_ = leafInsertItem(fsLeaf, key{rootDirObjID, typeDirIndex, 2}, dotdotDI)
	le.PutUint64(fsLeaf[0x50:], 1)
	updateNodeCRC(fsLeaf)

	// ── Superblock ───────────────────────────────────────────────────────────
	sb := buildSuperblockBytes(le, uuid, cfg.Label, imageSize)
	updateSuperblockCRC(sb)

	// ── Write all structures ─────────────────────────────────────────────────
	writes := []struct {
		off  int64
		data []byte
	}{
		{fmtSuperblockOff, sb},
		{fmtChunkPhys, chunkLeaf},
		{fmtRootPhys, rootLeaf},
		{fmtFSPhys, fsLeaf},
	}
	for _, w := range writes {
		if _, err := f.WriteAt(w.data, w.off); err != nil {
			f.Close()
			return nil, fmt.Errorf("btrfs: format: write at 0x%X: %w", w.off, err)
		}
	}

	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("btrfs: format: close: %w", err)
	}

	return btrfsFormatOpenFS(path, -1)
}

// buildSuperblockBytes constructs the raw on-disk superblock buffer.
func buildSuperblockBytes(le binary.ByteOrder, uuid [16]byte, label string, imageSize uint64) []byte {
	buf := make([]byte, sbfSize)
	copy(buf[sbfUUID:sbfUUID+16], uuid[:])
	le.PutUint64(buf[sbfPhysAddr:], uint64(fmtSuperblockOff))
	le.PutUint64(buf[sbfMagic:], sbMagic)
	le.PutUint64(buf[sbfGeneration:], 1)
	le.PutUint64(buf[sbfRootLogAddr:], fmtRootPhys)
	le.PutUint64(buf[sbfChunkLogAddr:], fmtChunkPhys)
	le.PutUint64(buf[sbfTotalBytes:], imageSize)
	le.PutUint64(buf[sbfBytesUsed:], uint64(3*fmtNodeSize))
	le.PutUint64(buf[sbfRootDirObjID:], rootDirObjID)
	le.PutUint64(buf[sbfNumDevices:], 1)
	le.PutUint32(buf[sbfSectorSize:], fmtNodeSize)
	le.PutUint32(buf[sbfNodeSize:], fmtNodeSize)
	le.PutUint32(buf[sbfLeafSize:], fmtNodeSize)
	le.PutUint32(buf[sbfStripeSize:], fmtNodeSize)

	// dev_item.devid = 1 — must match the chunk-stripe devid below
	// (buildSysChunkItemKey writes stripe[0].devid = 1). Setting this here
	// lets readSuperblock pick the correct stripe in multi-stripe profiles.
	le.PutUint64(buf[sbfDevItem:], 1)

	// Feature flags. We don't expose any compat or compat_ro features (we have
	// no free-space-tree, no log-tree, etc.), but every modern btrfs image
	// carries the MIXED_BACKREF incompat bit — without it external tools
	// flag the image as "from a pre-2008 kernel".
	le.PutUint64(buf[sbfChunkRootGeneration:], 1)
	le.PutUint64(buf[sbfCompatFlags:], 0)
	le.PutUint64(buf[sbfCompatROFlags:], 0)
	le.PutUint64(buf[sbfIncompatFlags:], incompatMixedBackref)
	le.PutUint16(buf[sbfCsumType:], csumTypeCRC32C)

	// Volume label.
	if len(label) > 255 {
		label = label[:255]
	}
	copy(buf[sbfLabel:sbfLabel+256], label)

	// sys_chunk_array.
	sysEntry := buildSysChunkItemKey(le, imageSize)
	copy(buf[sbfSysChunkArr:], sysEntry)
	le.PutUint32(buf[sbfSysChunkArrSz:], uint32(len(sysEntry)))

	return buf
}

// buildSysChunkItemKey returns the key+chunk_item bytes suitable for the
// sys_chunk_array in the superblock and/or an item in the chunk tree.
func buildSysChunkItemKey(le binary.ByteOrder, imageSize uint64) []byte {
	k := make([]byte, keySize)
	le.PutUint64(k[0:], 1)
	k[8] = 0xE4
	le.PutUint64(k[9:], 0)
	item := make([]byte, chunkHeaderSize+chunkStripeSize)
	le.PutUint64(item[chunkSize:], imageSize)
	le.PutUint16(item[chunkNumStripes:], 1)
	le.PutUint16(item[chunkSubStripes:], 1)
	le.PutUint64(item[chunkHeaderSize+0:], 1)
	le.PutUint64(item[chunkHeaderSize+8:], 0)
	return append(k, item...)
}

// buildSysChunkItemBytes returns just the chunk_item bytes (without the key prefix)
// used as the item value in the chunk tree leaf.
func buildSysChunkItemBytes(le binary.ByteOrder, imageSize uint64) []byte {
	item := make([]byte, chunkHeaderSize+chunkStripeSize)
	le.PutUint64(item[chunkSize:], imageSize)
	le.PutUint16(item[chunkNumStripes:], 1)
	le.PutUint16(item[chunkSubStripes:], 1)
	le.PutUint64(item[chunkHeaderSize+0:], 1)
	le.PutUint64(item[chunkHeaderSize+8:], 0)
	return item
}
