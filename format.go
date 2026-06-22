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

// On-disk geometry. We emit a 2-chunk MIXED layout that the Linux kernel's
// open_ctree accepts and that `btrfs check` reports clean. The layout mirrors
// what `mkfs.btrfs --mixed --nodesize 4096 --sectorsize 4096` produces:
//
//   - one SYSTEM chunk    at logical 0x100000, length 4 MiB, physical 0x100000
//   - one DATA|METADATA   at logical 0x500000, length 8 MiB, physical 0x500000
//
// Because each chunk's physical stripe offset equals its logical address, the
// reader's logical==physical assumption (sb.physAddr returns partOff+logical)
// keeps working unchanged. The chunk tree lives in the SYSTEM chunk so it is
// reachable through the superblock's sys_chunk_array; every other tree lives in
// the DATA|METADATA chunk.
const (
	fmtNodeSize      = 4096
	fmtSuperblockOff = 0x10000  // primary superblock at 64 KiB
	fmtMinSize       = 0x100000 // 1 MiB

	// fmtSysChunkLogical is where the SYSTEM chunk starts (logical == physical).
	// mkfs.btrfs places the first chunk at 1 MiB; we follow suit so the chunk
	// tree (which lives in the SYSTEM chunk) is well clear of the superblock.
	fmtSysChunkLogical uint64 = 0x100000 // 1 MiB

	// Number of metadata blocks per block group (used for bytes_used accounting).
	fmtSysBlocks  = 1 // chunk tree
	fmtDataBlocks = 7 // root + fs + extent + dev + csum + uuid + data-reloc

	// fmtStripeLen is BTRFS_STRIPE_LEN (64 KiB), the only stripe length the kernel
	// accepts in a chunk item. Chunk lengths and the DATA-chunk start are aligned
	// to it.
	fmtStripeLen = 64 * 1024

	// headerFlagWritten is BTRFS_HEADER_FLAG_WRITTEN: every on-disk tree block
	// must carry it or the kernel treats the block as corrupt.
	headerFlagWritten uint64 = 1 << 0

	// headerBackrefRevMixed is BTRFS_MIXED_BACKREF_REV, stored in the top byte of
	// the node header flags field. Required when the MIXED_BACKREF incompat bit
	// is set (which it always is on a modern image).
	headerBackrefRevMixed uint64 = 1

	// A skinny METADATA_ITEM payload is a 24-byte extent_item (refs,gen,flags)
	// followed by a 9-byte inline TREE_BLOCK_REF (1-byte type + 8-byte root
	// objectid) = 33 bytes.
	fmtMetadataItemSize = 33
	// BLOCK_GROUP_ITEM: used(8) + chunk_objectid(8) + flags(8) = 24 bytes.
	fmtBlockGroupItemSize = 24
	// DEV_EXTENT: chunk_tree(8) + chunk_objectid(8) + chunk_offset(8) +
	// length(8) + chunk_tree_uuid(16) = 48 bytes.
	fmtDevExtentSize = 48

	// fmtMinSizeLayout is the smallest image the 2-chunk layout can describe:
	// SYSTEM chunk start (1 MiB) + one stripe-len SYSTEM chunk + one stripe-len
	// DATA|METADATA chunk large enough for the 6 metadata blocks.
	fmtMinSizeLayout = fmtSysChunkLogical + 2*fmtStripeLen
)

// fmtLayout describes the on-disk placement chosen for a given image size.
// Every chunk's physical stripe offset equals its logical address, so the
// reader's logical==physical mapping keeps working unchanged.
type fmtLayout struct {
	sysChunkLogical  uint64
	sysChunkLen      uint64
	dataChunkLogical uint64
	dataChunkLen     uint64

	chunkLog     uint64 // chunk tree   (in SYSTEM chunk)
	rootLog      uint64 // root tree    (in DATA chunk)
	fsLog        uint64
	extentLog    uint64
	devLog       uint64
	csumLog      uint64
	uuidLog      uint64
	dataRelocLog uint64

	bytesUsed    uint64 // super.bytes_used = total metadata bytes
	devBytesUsed uint64 // dev_item.bytes_used = sum of chunk lengths
}

// computeLayout picks chunk sizes for imageSize. The SYSTEM chunk uses the
// canonical 4 MiB mkfs size (shrunk to one stripe-len for tiny images, where it
// only needs to hold the single chunk-tree block). The DATA|METADATA chunk then
// spans the rest of the device (rounded down to a stripe-len multiple). Unlike
// `mkfs.btrfs`, which starts with an 8 MiB data chunk and adds chunks on demand,
// our single-transaction writer cannot allocate new chunks after Format, so it
// maps all remaining space up front into one large DATA|METADATA chunk. A single
// large chunk is still a valid layout that the kernel mounts and `btrfs check`
// accepts.
func computeLayout(imageSize uint64) fmtLayout {
	const refSysLen = 4 << 20 // 4 MiB
	sysLen := uint64(refSysLen)
	dataStart := fmtSysChunkLogical + sysLen

	// Shrink the SYSTEM chunk for images too small to spare 4 MiB for it while
	// still leaving room for the metadata blocks in the DATA chunk.
	if imageSize < dataStart+fmtDataBlocks*fmtNodeSize+fmtStripeLen {
		sysLen = fmtStripeLen
		dataStart = fmtSysChunkLogical + sysLen
	}
	dataLen := (imageSize - dataStart) / fmtStripeLen * fmtStripeLen

	l := fmtLayout{
		sysChunkLogical:  fmtSysChunkLogical,
		sysChunkLen:      sysLen,
		dataChunkLogical: dataStart,
		dataChunkLen:     dataLen,
	}
	l.chunkLog = l.sysChunkLogical
	l.rootLog = l.dataChunkLogical
	l.fsLog = l.dataChunkLogical + 1*fmtNodeSize
	l.extentLog = l.dataChunkLogical + 2*fmtNodeSize
	l.devLog = l.dataChunkLogical + 3*fmtNodeSize
	l.csumLog = l.dataChunkLogical + 4*fmtNodeSize
	l.uuidLog = l.dataChunkLogical + 5*fmtNodeSize
	l.dataRelocLog = l.dataChunkLogical + 6*fmtNodeSize
	l.bytesUsed = (fmtSysBlocks + fmtDataBlocks) * fmtNodeSize
	l.devBytesUsed = sysLen + dataLen
	return l
}

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
	// The 2-chunk layout requires room for the SYSTEM chunk plus a DATA|METADATA
	// chunk large enough for the 6 metadata blocks.
	if uint64(sizeBytes) < fmtMinSizeLayout {
		return nil, fmt.Errorf("btrfs: format: size %d too small (minimum %d bytes for chunk layout)",
			sizeBytes, fmtMinSizeLayout)
	}
	lay := computeLayout(uint64(sizeBytes))
	if lay.dataChunkLen < fmtDataBlocks*fmtNodeSize {
		return nil, fmt.Errorf("btrfs: format: size %d too small for metadata (data chunk %d bytes < %d)",
			sizeBytes, lay.dataChunkLen, fmtDataBlocks*fmtNodeSize)
	}

	f, err := btrfsFormatOpenFile(path)
	if err != nil {
		return nil, fmt.Errorf("btrfs: format: %w", err)
	}
	if err := f.Truncate(sizeBytes); err != nil {
		f.Close()
		return nil, fmt.Errorf("btrfs: format: truncate: %w", err)
	}

	// Generate the filesystem UUID (fsid) if not provided.
	uuid := cfg.UUID
	if uuid == [16]byte{} {
		if _, err := btrfsFormatRandRead(uuid[:]); err != nil {
			f.Close()
			return nil, fmt.Errorf("btrfs: format: generate UUID: %w", err)
		}
	}

	// Generate the device UUID (dev_item.uuid). The kernel's open_ctree verifies
	// that the chunk stripe's dev_uuid equals dev_item.uuid, so the same value
	// must be written into both the superblock dev_item and every chunk stripe.
	var devUUID [16]byte
	if _, err := btrfsFormatRandRead(devUUID[:]); err != nil {
		f.Close()
		return nil, fmt.Errorf("btrfs: format: generate device UUID: %w", err)
	}

	// chunk_tree_uuid is written into every node header and into DEV_EXTENT items.
	var chunkUUID [16]byte
	if _, err := btrfsFormatRandRead(chunkUUID[:]); err != nil {
		f.Close()
		return nil, fmt.Errorf("btrfs: format: generate chunk UUID: %w", err)
	}

	imageSize := uint64(sizeBytes)
	le := binary.LittleEndian
	now := time.Now().UTC()

	// ── Helper: build an empty leaf node ────────────────────────────────────
	buildLeaf := func(logAddr, generation, owner uint64) []byte {
		buf := make([]byte, fmtNodeSize)
		copy(buf[32:48], uuid[:])
		le.PutUint64(buf[0x30:], logAddr)
		// header.flags is a __le64 whose low 56 bits are flag bits and whose top
		// byte (offset 0x3F) is the backref revision. With MIXED_BACKREF set we
		// must use BTRFS_MIXED_BACKREF_REV (1); a 0 here makes the kernel reject
		// every tree block ("backref revision 0").
		le.PutUint64(buf[0x38:], headerFlagWritten|(headerBackrefRevMixed<<56))
		copy(buf[0x40:0x50], chunkUUID[:]) // chunk_tree_uuid
		le.PutUint64(buf[0x50:], generation)
		le.PutUint64(buf[0x58:], owner)
		le.PutUint32(buf[0x60:], 0)
		buf[0x64] = 0 // level = 0 (leaf)
		return buf
	}

	// ── Chunk tree leaf (in SYSTEM chunk) ───────────────────────────────────
	// item0 DEV_ITEM, item1 SYSTEM CHUNK_ITEM, item2 DATA|METADATA CHUNK_ITEM.
	chunkLeaf := buildLeaf(lay.chunkLog, 1, chunkTreeObjID)
	devItem := buildDevItemBytes(le, imageSize, lay.devBytesUsed, devUUID, uuid)
	_ = leafInsertItem(chunkLeaf, key{devItemsObjID, typeDevItem, 1}, devItem)
	sysChunkItem := buildSysChunkItemBytes(le, lay, devUUID)
	_ = leafInsertItem(chunkLeaf, key{firstChunkTreeObjID, typeChunkItem, lay.sysChunkLogical}, sysChunkItem)
	dataChunkItem := buildDataChunkItemBytes(le, lay, devUUID)
	_ = leafInsertItem(chunkLeaf, key{firstChunkTreeObjID, typeChunkItem, lay.dataChunkLogical}, dataChunkItem)
	updateNodeCRC(chunkLeaf)

	// ── Root tree leaf ──────────────────────────────────────────────────────
	// ROOT_ITEMs for EXTENT(2), DEV(4), FS(5), CSUM(7), UUID(9).
	rootLeaf := buildLeaf(lay.rootLog, 1, rootTreeObjID)
	insertRootItem := func(objID, dirID, bytenr uint64) {
		ri := buildRootItemBytes(le, dirID, bytenr, now)
		_ = leafInsertItem(rootLeaf, key{objID, typeRootItem, 0}, ri)
	}
	insertRootItem(extentTreeObjID, 0, lay.extentLog)
	insertRootItem(devTreeObjID, 0, lay.devLog)
	insertRootItem(fsTreeObjID, rootDirObjID, lay.fsLog)
	insertRootItem(csumTreeObjID, 0, lay.csumLog)
	insertRootItem(uuidTreeObjID, 0, lay.uuidLog)
	// DATA_RELOC_TREE: open_ctree's btrfs_read_roots requires this root. Its key
	// objectid is -9 (the largest unsigned value among our root items) so it
	// sorts last in the root leaf.
	insertRootItem(dataRelocTreeObjID, rootDirObjID, lay.dataRelocLog)
	updateNodeCRC(rootLeaf)

	// ── FS tree leaf ────────────────────────────────────────────────────────
	// Root directory inode (ino 256) + "." / ".." DIR_INDEX entries. This is
	// the layout our own reader expects (searchTree on INODE_ITEM/DIR_INDEX).
	buildDirTreeLeaf := func(logAddr, owner uint64) []byte {
		leaf := buildLeaf(logAddr, 1, owner)
		rinode := make([]byte, inodeItemSize)
		le.PutUint64(rinode[inodeOffGeneration:], 1)
		// btrfs directories carry nlink=1 for an empty dir; each child subdir
		// bumps it by one. (Unlike traditional Unix where "."/".." make an empty
		// dir nlink=2 — the kernel's tree-checker rejects nlink>1 here.)
		le.PutUint32(rinode[inodeOffNLink:], 1)
		le.PutUint32(rinode[inodeOffMode:], 0x41ED) // directory + rwxr-xr-x
		le.PutUint64(rinode[inodeOffFlags:], inodeFlagNoDataSum)
		writeBtrfsTimespec(rinode[inodeOffATime:], now)
		writeBtrfsTimespec(rinode[inodeOffCTime:], now)
		writeBtrfsTimespec(rinode[inodeOffMTime:], now)
		writeBtrfsTimespec(rinode[inodeOffOTime:], now)
		_ = leafInsertItem(leaf, key{rootDirObjID, typeInodeItem, 0}, rinode)
		// INODE_REF (256 -> 256, ".."): the self-referential parent ref that mkfs
		// writes for a tree's root directory. We do NOT emit "."/".." DIR_INDEX
		// entries: btrfs never stores those as directory items (they are implicit)
		// and the kernel's tree-checker rejects them as name-hash mismatches. Our
		// own readDir already filters "."/".." and tolerates their absence.
		ref := make([]byte, 8+2+2)
		le.PutUint64(ref[0:], 0) // index
		le.PutUint16(ref[8:], 2) // name_len
		copy(ref[10:], "..")
		_ = leafInsertItem(leaf, key{rootDirObjID, typeInodeRef, rootDirObjID}, ref)
		updateNodeCRC(leaf)
		return leaf
	}
	fsLeaf := buildDirTreeLeaf(lay.fsLog, fsTreeObjID)
	// DATA_RELOC tree: the kernel requires this root to exist (see
	// dataRelocTreeObjID). It only needs a valid root-dir inode; our reader never
	// traverses it.
	dataRelocLeaf := buildDirTreeLeaf(lay.dataRelocLog, dataRelocTreeObjID)

	// ── Extent tree leaf ────────────────────────────────────────────────────
	// One skinny METADATA_ITEM per metadata block + one BLOCK_GROUP_ITEM per
	// block group. Items must be key-sorted: leafInsertItem sorts on insert.
	extentLeaf := buildLeaf(lay.extentLog, 1, extentTreeObjID)
	addMeta := func(logAddr, owner uint64) {
		mi := buildMetadataItemBytes(le, owner)
		_ = leafInsertItem(extentLeaf, key{logAddr, typeMetadataItem, 0}, mi)
	}
	addBG := func(logAddr, length, used, flags uint64) {
		bg := buildBlockGroupItemBytes(le, used, flags)
		_ = leafInsertItem(extentLeaf, key{logAddr, typeBlockGroupItem, length}, bg)
	}
	// SYSTEM block group + its single metadata block (chunk tree).
	addMeta(lay.chunkLog, chunkTreeObjID)
	addBG(lay.sysChunkLogical, lay.sysChunkLen, fmtSysBlocks*fmtNodeSize, blockGroupSystem)
	// DATA|METADATA block group + its metadata blocks.
	addMeta(lay.rootLog, rootTreeObjID)
	addMeta(lay.fsLog, fsTreeObjID)
	addMeta(lay.extentLog, extentTreeObjID)
	addMeta(lay.devLog, devTreeObjID)
	addMeta(lay.csumLog, csumTreeObjID)
	addMeta(lay.uuidLog, uuidTreeObjID)
	addMeta(lay.dataRelocLog, dataRelocTreeObjID)
	addBG(lay.dataChunkLogical, lay.dataChunkLen, fmtDataBlocks*fmtNodeSize, blockGroupData|blockGroupMeta)
	updateNodeCRC(extentLeaf)

	// ── Dev tree leaf ───────────────────────────────────────────────────────
	// DEV_STATS PERSISTENT_ITEM (key (0, PERSISTENT_ITEM=0xF9, 1), a 40-byte
	// btrfs_dev_stats_item of five zeroed error counters) + one DEV_EXTENT per
	// chunk, mirroring exactly what mkfs.btrfs writes.
	devLeaf := buildLeaf(lay.devLog, 1, devTreeObjID)
	_ = leafInsertItem(devLeaf, key{devStatsObjID, typePersistentItem, 1}, make([]byte, 40))
	sysDevExt := buildDevExtentBytes(le, lay.sysChunkLogical, lay.sysChunkLen, chunkUUID)
	_ = leafInsertItem(devLeaf, key{1, typeDevExtent, lay.sysChunkLogical}, sysDevExt)
	dataDevExt := buildDevExtentBytes(le, lay.dataChunkLogical, lay.dataChunkLen, chunkUUID)
	_ = leafInsertItem(devLeaf, key{1, typeDevExtent, lay.dataChunkLogical}, dataDevExt)
	updateNodeCRC(devLeaf)

	// ── Csum tree + Uuid tree: empty leaves ─────────────────────────────────
	csumLeaf := buildLeaf(lay.csumLog, 1, csumTreeObjID)
	updateNodeCRC(csumLeaf)
	uuidLeaf := buildLeaf(lay.uuidLog, 1, uuidTreeObjID)
	updateNodeCRC(uuidLeaf)

	// ── Superblock ───────────────────────────────────────────────────────────
	sb := buildSuperblockBytes(le, uuid, devUUID, cfg.Label, imageSize, lay)
	updateSuperblockCRC(sb)

	// ── Write all structures at their physical (== logical) offsets ──────────
	writes := []struct {
		off  int64
		data []byte
	}{
		{fmtSuperblockOff, sb},
		{int64(lay.chunkLog), chunkLeaf},
		{int64(lay.rootLog), rootLeaf},
		{int64(lay.fsLog), fsLeaf},
		{int64(lay.extentLog), extentLeaf},
		{int64(lay.devLog), devLeaf},
		{int64(lay.csumLog), csumLeaf},
		{int64(lay.uuidLog), uuidLeaf},
		{int64(lay.dataRelocLog), dataRelocLeaf},
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
func buildSuperblockBytes(le binary.ByteOrder, uuid, devUUID [16]byte, label string, imageSize uint64, lay fmtLayout) []byte {
	buf := make([]byte, sbfSize)
	copy(buf[sbfUUID:sbfUUID+16], uuid[:])
	le.PutUint64(buf[sbfPhysAddr:], uint64(fmtSuperblockOff))
	le.PutUint64(buf[sbfMagic:], sbMagic)
	le.PutUint64(buf[sbfGeneration:], 1)
	// super_block.flags carries BTRFS_HEADER_FLAG_WRITTEN on every mkfs image.
	le.PutUint64(buf[sbfFlags:], headerFlagWritten)
	le.PutUint64(buf[sbfRootLogAddr:], lay.rootLog)
	le.PutUint64(buf[sbfChunkLogAddr:], lay.chunkLog)
	le.PutUint64(buf[sbfTotalBytes:], imageSize)
	le.PutUint64(buf[sbfBytesUsed:], lay.bytesUsed)
	le.PutUint64(buf[sbfRootDirObjID:], rootDirObjID)
	le.PutUint64(buf[sbfNumDevices:], 1)
	le.PutUint32(buf[sbfSectorSize:], fmtNodeSize)
	le.PutUint32(buf[sbfNodeSize:], fmtNodeSize)
	le.PutUint32(buf[sbfLeafSize:], fmtNodeSize)
	le.PutUint32(buf[sbfStripeSize:], fmtNodeSize)

	// dev_item (98 bytes) embedded at sbfDevItem — identical to the DEV_ITEM in
	// the chunk tree (the kernel cross-checks the two).
	copy(buf[sbfDevItem:sbfDevItem+98], buildDevItemBytes(le, imageSize, lay.devBytesUsed, devUUID, uuid))

	// Feature flags. MIXED layout: 0x345.
	le.PutUint64(buf[sbfChunkRootGeneration:], 1)
	le.PutUint64(buf[sbfCompatFlags:], 0)
	le.PutUint64(buf[sbfCompatROFlags:], 0)
	le.PutUint64(buf[sbfIncompatFlags:], incompatMixedFlags)
	le.PutUint16(buf[sbfCsumType:], csumTypeCRC32C)

	// Volume label.
	if len(label) > 255 {
		label = label[:255]
	}
	copy(buf[sbfLabel:sbfLabel+256], label)

	// sys_chunk_array: only the SYSTEM chunk (so the kernel can reach the chunk
	// tree, which itself lives in the SYSTEM chunk).
	sysEntry := buildSysChunkItemKey(le, lay, devUUID)
	copy(buf[sbfSysChunkArr:], sysEntry)
	le.PutUint32(buf[sbfSysChunkArrSz:], uint32(len(sysEntry)))

	// root_backups[0]: the kernel's init_tree_roots reads the backup-roots ring
	// (super_roots) to recover from a torn primary commit. mkfs writes a valid
	// backup[0]; leaving the ring zeroed makes the kernel's backup-root fallback
	// dereference a bytenr of 0. We mirror the current (only) commit into slot 0.
	bk := buf[sbfRootBackups : sbfRootBackups+rootBackupSize]
	le.PutUint64(bk[0x00:], lay.rootLog)   // tree_root
	le.PutUint64(bk[0x08:], 1)             // tree_root_gen
	le.PutUint64(bk[0x10:], lay.chunkLog)  // chunk_root
	le.PutUint64(bk[0x18:], 1)             // chunk_root_gen
	le.PutUint64(bk[0x20:], lay.extentLog) // extent_root
	le.PutUint64(bk[0x28:], 1)             // extent_root_gen
	le.PutUint64(bk[0x30:], lay.fsLog)     // fs_root
	le.PutUint64(bk[0x38:], 1)             // fs_root_gen
	le.PutUint64(bk[0x40:], lay.devLog)    // dev_root
	le.PutUint64(bk[0x48:], 1)             // dev_root_gen
	le.PutUint64(bk[0x50:], lay.csumLog)   // csum_root
	le.PutUint64(bk[0x58:], 1)             // csum_root_gen
	le.PutUint64(bk[0x60:], imageSize)     // total_bytes
	le.PutUint64(bk[0x68:], lay.bytesUsed) // bytes_used
	le.PutUint64(bk[0x70:], 1)             // num_devices
	// levels (all leaves, level 0) at 0x98..: tree/chunk/extent/fs/dev/csum.

	return buf
}

// buildDevItemBytes builds the 98-byte btrfs_dev_item. dev_item.fsid must equal
// the superblock fsid and dev_item.uuid must equal every chunk stripe's
// dev_uuid, or open_ctree rejects the image.
func buildDevItemBytes(le binary.ByteOrder, imageSize, devBytesUsed uint64, devUUID, fsid [16]byte) []byte {
	d := make([]byte, 98)
	le.PutUint64(d[0x00:], 1)            // devid
	le.PutUint64(d[0x08:], imageSize)    // total_bytes
	le.PutUint64(d[0x10:], devBytesUsed) // bytes_used
	le.PutUint32(d[0x18:], fmtNodeSize)  // io_align
	le.PutUint32(d[0x1C:], fmtNodeSize)  // io_width
	le.PutUint32(d[0x20:], fmtNodeSize)  // sector_size
	// type(0x24)=0, generation(0x28)=0, start_offset(0x30)=0, dev_group(0x38)=0,
	// seek_speed(0x3C)=0, bandwidth(0x3D)=0 — all zero like mkfs.
	copy(d[0x42:0x52], devUUID[:]) // uuid
	copy(d[0x52:0x62], fsid[:])    // fsid
	return d
}

// buildSysChunkItemKey returns the key+chunk_item bytes for the SYSTEM chunk in
// the sys_chunk_array (key objectid = FIRST_CHUNK_TREE, offset = logical addr).
func buildSysChunkItemKey(le binary.ByteOrder, lay fmtLayout, devUUID [16]byte) []byte {
	k := make([]byte, keySize)
	le.PutUint64(k[0:], firstChunkTreeObjID)
	k[8] = typeChunkItem
	le.PutUint64(k[9:], lay.sysChunkLogical)
	return append(k, buildSysChunkItemBytes(le, lay, devUUID)...)
}

// buildSysChunkItemBytes returns the chunk_item bytes for the SYSTEM chunk.
func buildSysChunkItemBytes(le binary.ByteOrder, lay fmtLayout, devUUID [16]byte) []byte {
	return buildChunkItemBytes(le, lay.sysChunkLen, blockGroupSystem,
		lay.sysChunkLogical, fmtNodeSize, fmtNodeSize, 0, devUUID)
}

// buildDataChunkItemBytes returns the chunk_item bytes for the DATA|METADATA
// chunk. mkfs uses io_align/io_width = stripe_len (64 KiB) for this chunk and
// sub_stripes = 1.
func buildDataChunkItemBytes(le binary.ByteOrder, lay fmtLayout, devUUID [16]byte) []byte {
	return buildChunkItemBytes(le, lay.dataChunkLen, blockGroupData|blockGroupMeta,
		lay.dataChunkLogical, fmtStripeLen, fmtStripeLen, 1, devUUID)
}

// buildChunkItemBytes builds a single-stripe chunk_item. The stripe's dev_uuid
// must equal dev_item.uuid.
func buildChunkItemBytes(le binary.ByteOrder, length, typ, stripeOffset uint64,
	ioAlign, ioWidth uint32, subStripes uint16, devUUID [16]byte) []byte {
	item := make([]byte, chunkHeaderSize+chunkStripeSize)
	le.PutUint64(item[chunkSize:], length)
	le.PutUint64(item[chunkRootObjID:], extentTreeObjID) // owner = EXTENT_TREE (2)
	le.PutUint64(item[chunkStripeLen:], fmtStripeLen)
	le.PutUint64(item[chunkType:], typ)
	le.PutUint32(item[chunkOptIOAlign:], ioAlign)
	le.PutUint32(item[chunkOptIOWidth:], ioWidth)
	le.PutUint32(item[chunkMinIOSize:], fmtNodeSize) // sector_size
	le.PutUint16(item[chunkNumStripes:], 1)
	le.PutUint16(item[chunkSubStripes:], subStripes)
	le.PutUint64(item[chunkStripeDevID:], 1)
	le.PutUint64(item[chunkStripeOffset:], stripeOffset) // phys == logical
	copy(item[chunkStripeDevUUID:], devUUID[:])
	return item
}

// buildRootItemBytes builds a 439-byte btrfs_root_item. The reader reads bytenr
// at 0xB0; the kernel additionally reads generation (0xA0), root_dirid (0xA8),
// bytes_used (0xB8), refs (0xC8), and level (0xC9). We fill the fields mkfs
// sets so `btrfs check` accepts the root.
func buildRootItemBytes(le binary.ByteOrder, dirID, bytenr uint64, now time.Time) []byte {
	ri := make([]byte, 439)
	// Embedded inode_item (root_item.inode) at offset 0: set nbytes and a sane
	// mode so check doesn't complain (mkfs leaves most of it zero except nbytes).
	le.PutUint64(ri[inodeOffNBytes:], fmtNodeSize)
	le.PutUint64(ri[0xA0:], 1)           // generation
	le.PutUint64(ri[0xA8:], dirID)       // root_dirid
	le.PutUint64(ri[0xB0:], bytenr)      // bytenr (logical addr of tree root node)
	le.PutUint64(ri[0xB8:], 0)           // byte_limit
	le.PutUint64(ri[0xC0:], fmtNodeSize) // bytes_used
	le.PutUint64(ri[0xC8:], 0)           // last_snapshot
	le.PutUint64(ri[0xD0:], 0)           // flags
	le.PutUint32(ri[0xD8:], 1)           // refs
	// drop_progress key (0xDC, 17 bytes) + drop_level (0xED) left zero.
	ri[0xEE] = 0                                  // level = 0
	le.PutUint64(ri[rootItemOffGenerationV2:], 1) // generation_v2 (= generation)
	return ri
}

// buildMetadataItemBytes builds a skinny METADATA_ITEM payload: a 24-byte
// extent_item (refs=1, generation=1, flags=TREE_BLOCK) followed by an inline
// 9-byte TREE_BLOCK_REF (type + owning-root objectid).
func buildMetadataItemBytes(le binary.ByteOrder, owner uint64) []byte {
	d := make([]byte, fmtMetadataItemSize)
	le.PutUint64(d[0:], 1)                    // refs
	le.PutUint64(d[8:], 1)                    // generation
	le.PutUint64(d[16:], extentFlagTreeBlock) // flags
	d[24] = typeTreeBlockRef                  // inline ref type
	le.PutUint64(d[25:], owner)               // owning root objectid
	return d
}

// buildBlockGroupItemBytes builds a 24-byte BLOCK_GROUP_ITEM:
// used(8) + chunk_objectid(8) + flags(8).
func buildBlockGroupItemBytes(le binary.ByteOrder, used, flags uint64) []byte {
	d := make([]byte, fmtBlockGroupItemSize)
	le.PutUint64(d[0:], used)
	le.PutUint64(d[8:], firstChunkTreeObjID) // chunk_objectid = 256
	le.PutUint64(d[16:], flags)
	return d
}

// buildDevExtentBytes builds a 48-byte DEV_EXTENT:
// chunk_tree(8) + chunk_objectid(8) + chunk_offset(8) + length(8) + chunk_tree_uuid(16).
func buildDevExtentBytes(le binary.ByteOrder, chunkOffset, length uint64, chunkUUID [16]byte) []byte {
	d := make([]byte, fmtDevExtentSize)
	le.PutUint64(d[0:], chunkTreeObjID)      // chunk_tree = 3
	le.PutUint64(d[8:], firstChunkTreeObjID) // chunk_objectid = 256
	le.PutUint64(d[16:], chunkOffset)        // chunk_offset (logical)
	le.PutUint64(d[24:], length)             // length
	copy(d[32:48], chunkUUID[:])
	return d
}
