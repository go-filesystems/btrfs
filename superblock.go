package filesystem_btrfs

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/go-volumes/safeio"
)

// superblock holds the in-memory representation of the Btrfs superblock.
// On-disk the primary copy is at partition_offset + 0x10000 (64 KiB).
// All fields are stored little-endian on disk.
type superblock struct {
	nodeSize     uint32
	sectorSize   uint32
	leafSize     uint32
	stripeSize   uint32
	totalBytes   uint64
	rootLogAddr  uint64 // logical address of root tree root
	chunkLogAddr uint64 // logical address of chunk tree root
	uuid         [16]byte
	label        [256]byte

	// devID is read from the embedded dev_item at sbfDevItem. It lets the
	// chunk-map resolver pick the stripe pointing at THIS device when the
	// image is one leg of a multi-device profile (RAID1 / RAID10 / DUP).
	// Without this, chunk parsing took stripe[0] blindly and a RAID1 leg
	// opened on devid=2 silently read stripe[0]'s offset which targets
	// devid=1's physical layout → wrong bytes for every block.
	devID uint64

	// Derived at parse time from the embedded sys_chunk_array.
	// Maps logical address -> physical offset for SYSTEM chunks.
	generation uint64 // superblock generation (incremented on every write)

	sysChunks []chunkMapping
}

// chunkStripe is one (device, physical-offset) pair within a chunk.
// A SINGLE chunk has one stripe; RAID1 has N copies (each a separate stripe);
// RAID0 / RAID5 / RAID6 / RAID10 stripe data across multiple stripes.
type chunkStripe struct {
	devID  uint64
	offset uint64
}

// btrfs chunk-type bitmask values (from include/uapi/linux/btrfs_tree.h).
const (
	blockGroupData    uint64 = 1 << 0  // 0x01
	blockGroupSystem  uint64 = 1 << 1  // 0x02
	blockGroupMeta    uint64 = 1 << 2  // 0x04
	blockGroupRAID0   uint64 = 1 << 3  // 0x08
	blockGroupRAID1   uint64 = 1 << 4  // 0x10
	blockGroupDup     uint64 = 1 << 5  // 0x20
	blockGroupRAID10  uint64 = 1 << 6  // 0x40
	blockGroupRAID5   uint64 = 1 << 7  // 0x80
	blockGroupRAID6   uint64 = 1 << 8  // 0x100
	blockGroupRAID1C3 uint64 = 1 << 9  // 0x200
	blockGroupRAID1C4 uint64 = 1 << 10 // 0x400

	// Mask covering all profile bits (RAID + DUP + RAID1Cn) — anything in
	// here is a redundancy/striping descriptor; the data/system/metadata
	// bits below it pick the block-group class.
	blockGroupProfileMask = blockGroupRAID0 | blockGroupRAID1 | blockGroupDup |
		blockGroupRAID10 | blockGroupRAID5 | blockGroupRAID6 |
		blockGroupRAID1C3 | blockGroupRAID1C4
)

// chunkMapping describes how a logical address range maps onto one or more
// physical block-device offsets. For SINGLE / DUP this is a single stripe per
// chunk; for RAID1 / RAID1C{3,4} the stripes are mirror copies; for RAID0 /
// RAID10 / RAID5 / RAID6 the stripes are pieces of the data spread across
// devices (with optional parity columns). The on-disk chunk_item carries
// (chunk_type, stripe_len, num_stripes, sub_stripes, stripe[]) which we
// preserve here so the device-pool router can implement the per-profile
// stripe math from fs/btrfs/volumes.c:btrfs_map_block.
//
// localStripeIdx is the index into stripes[] whose devID matches sb.devID
// (the device the FS was opened against). -1 means none of the stripes lives
// on the opened device — only meaningful for multi-device profiles where
// other backends in the device pool carry the data. The single-leg
// logToPhys path uses physStart (set from stripes[localStripeIdx].offset)
// and ignores chunks with localStripeIdx == -1.
type chunkMapping struct {
	logStart       uint64
	size           uint64
	physStart      uint64 // stripes[localStripeIdx].offset, or 0 if no local stripe
	localStripeIdx int    // -1 if none of stripes[] is on sb.devID
	profile        uint64 // chunkType bitmask (block-group type + profile)
	stripeLen      uint64 // bytes per stripe row (for RAID0/5/6/10); 0 otherwise
	subStripes     uint16 // RAID10: mirror legs per stripe pair (always 2 today)
	stripes        []chunkStripe
}

// Superblock field offsets (all LE).
const (
	sbfCsum                = 0x00  // [32]byte
	sbfUUID                = 0x20  // [16]byte
	sbfPhysAddr            = 0x30  // uint64
	sbfFlags               = 0x38  // uint64
	sbfMagic               = 0x40  // uint64
	sbfGeneration          = 0x48  // uint64
	sbfRootLogAddr         = 0x50  // uint64
	sbfChunkLogAddr        = 0x58  // uint64
	sbfLogLogAddr          = 0x60  // uint64
	sbfTotalBytes          = 0x70  // uint64
	sbfBytesUsed           = 0x78  // uint64
	sbfRootDirObjID        = 0x80  // uint64
	sbfNumDevices          = 0x88  // uint64
	sbfSectorSize          = 0x90  // uint32
	sbfNodeSize            = 0x94  // uint32
	sbfLeafSize            = 0x98  // uint32
	sbfStripeSize          = 0x9C  // uint32
	sbfSysChunkArrSz       = 0xA0  // uint32
	sbfChunkRootGeneration = 0xA4  // uint64
	sbfCompatFlags         = 0xAC  // uint64
	sbfCompatROFlags       = 0xB4  // uint64
	sbfIncompatFlags       = 0xBC  // uint64
	sbfCsumType            = 0xC4  // uint16 (0 = CRC32C)
	sbfDevItem             = 0xC9  // dev_item struct (98 bytes); devid is the first uint64 at offset 0xC9
	sbfLabel               = 0x12B // [256]byte
	sbfSysChunkArr         = 0x32B // starts here; length = sbfSysChunkArrSz
)

// Subset of incompat-feature bits we emit. MixedBackref is the baseline
// modern-format bit; mkfs.btrfs has set it on every image since ~2008.
const (
	incompatMixedBackref uint64 = 1 << 0
)

// CRC32C is the only checksum we produce; this maps to csum_type 0.
const (
	csumTypeCRC32C uint16 = 0
)

const sbfSize = 0x1000 // total on-disk superblock size

// Chunk item field offsets (relative to start of chunk item data).
const (
	chunkSize          = 0x00 // uint64
	chunkRootObjID     = 0x08 // uint64
	chunkStripeLen     = 0x10 // uint64
	chunkType          = 0x18 // uint64
	chunkOptIOAlign    = 0x20 // uint32
	chunkOptIOWidth    = 0x24 // uint32
	chunkMinIOSize     = 0x28 // uint32
	chunkNumStripes    = 0x2C // uint16
	chunkSubStripes    = 0x2E // uint16
	chunkStripeDevID   = 0x30 // uint64 (start of first stripe)
	chunkStripeOffset  = 0x38 // uint64
	chunkStripeDevUUID = 0x40 // [16]byte
	chunkStripeSize    = 0x20 // total size per stripe entry: devid(8) + offset(8) + dev_uuid(16) = 32 bytes
	chunkHeaderSize    = 0x30 // up to first stripe
)

// Key size on disk (objectid:8 + type:1 + offset:8 = 17 bytes).
const keySize = 17

func readSuperblock(r io.ReaderAt, partOff int64) (*superblock, error) {
	buf := make([]byte, sbfSize)
	if _, err := r.ReadAt(buf, partOff+superblockOffset); err != nil {
		return nil, fmt.Errorf("btrfs: read superblock: %w", err)
	}
	le := binary.LittleEndian

	magic := le.Uint64(buf[sbfMagic:])
	if magic != sbMagic {
		return nil, fmt.Errorf("btrfs: bad superblock magic 0x%016X at offset 0x%X",
			magic, partOff+superblockOffset)
	}

	sb := &superblock{
		sectorSize:   le.Uint32(buf[sbfSectorSize:]),
		nodeSize:     le.Uint32(buf[sbfNodeSize:]),
		leafSize:     le.Uint32(buf[sbfLeafSize:]),
		stripeSize:   le.Uint32(buf[sbfStripeSize:]),
		totalBytes:   le.Uint64(buf[sbfTotalBytes:]),
		rootLogAddr:  le.Uint64(buf[sbfRootLogAddr:]),
		chunkLogAddr: le.Uint64(buf[sbfChunkLogAddr:]),
		generation:   le.Uint64(buf[sbfGeneration:]),
		devID:        le.Uint64(buf[sbfDevItem:]), // dev_item.devid is first uint64
	}
	copy(sb.uuid[:], buf[sbfUUID:sbfUUID+16])
	copy(sb.label[:], buf[sbfLabel:sbfLabel+256])

	if sb.nodeSize == 0 || sb.sectorSize == 0 {
		return nil, fmt.Errorf("btrfs: invalid superblock geometry nodeSize=%d sectorSize=%d",
			sb.nodeSize, sb.sectorSize)
	}
	// Validate node geometry before any nodeSize-driven allocation in
	// readNode: nodeSize must be at least one header, at most maxNodeSize,
	// and a whole multiple of sectorSize. An attacker-supplied nodeSize of 1
	// (OOB on every node read) or 0xFFFFFFFF (4 GiB alloc) is rejected here.
	if sb.nodeSize < nodeHdrSize || sb.nodeSize > maxNodeSize {
		return nil, fmt.Errorf("btrfs: nodeSize %d out of range [%d,%d]",
			sb.nodeSize, nodeHdrSize, maxNodeSize)
	}
	if sb.nodeSize%sb.sectorSize != 0 {
		return nil, fmt.Errorf("btrfs: nodeSize %d not a multiple of sectorSize %d",
			sb.nodeSize, sb.sectorSize)
	}

	// Parse sys_chunk_array. arrSz is an attacker-controlled uint32; slice it
	// out of the fixed superblock buffer with an overflow-safe bounds check
	// so a value such as 0xFFFFFFFF yields an error instead of a panic.
	arrSz := int(le.Uint32(buf[sbfSysChunkArrSz:]))
	arrBuf, err := safeio.Slice(buf, sbfSysChunkArr, arrSz)
	if err != nil {
		return nil, fmt.Errorf("btrfs: sys_chunk_array size %d out of bounds: %w", arrSz, err)
	}
	chunks, err := parseSysChunkArray(le, arrBuf, sb.devID)
	if err != nil {
		return nil, fmt.Errorf("btrfs: parse sys_chunk_array: %w", err)
	}
	sb.sysChunks = chunks

	return sb, nil
}

// pickStripeForDevID scans `numStripes` chunk_stripe entries (each
// chunkStripeSize bytes, starting at the chunk header end) and returns the
// physical offset of the stripe whose devid matches `wantDev`. For SINGLE-
// profile images numStripes==1 and we get stripe[0]. For RAID1 / RAID10 / DUP
// we get the stripe pointing at the device the caller actually opened — which
// is the only one whose bytes are physically present on that device.
//
// If wantDev is 0 (the superblock's dev_item.devid was not set — common in
// hand-built unit-test fixtures and very old images), the function falls
// back to stripe[0]. mkfs.btrfs always writes dev_item.devid >= 1, so this
// fallback never triggers on a real-world image.
//
// Returns ok=false if no stripe matches wantDev; the caller decides whether
// that is fatal (RAID0 / RAID5 / RAID6 with this leg missing → can't
// reconstruct without parity math, which is not yet implemented).
func pickStripeForDevID(le binary.ByteOrder, chunkBuf []byte, numStripes uint16, wantDev uint64) (uint64, bool) {
	if wantDev == 0 && numStripes >= 1 {
		base := chunkHeaderSize
		if base+chunkStripeSize > len(chunkBuf) {
			return 0, false
		}
		return le.Uint64(chunkBuf[base+8:]), true
	}
	for i := uint16(0); i < numStripes; i++ {
		base := chunkHeaderSize + int(i)*chunkStripeSize
		if base+chunkStripeSize > len(chunkBuf) {
			return 0, false
		}
		devID := le.Uint64(chunkBuf[base:])
		if devID == wantDev {
			return le.Uint64(chunkBuf[base+8:]), true
		}
	}
	return 0, false
}

// parseAllStripes decodes all on-disk stripes of a chunk_item. The slice is
// returned in stripe-index order (mkfs writes them in the order matching the
// btrfs_map_block stripe layout — stripe[i] corresponds to the i-th column in
// the RAID layout). Used by the multi-device router; SINGLE-device callers
// can keep using pickStripeForDevID.
func parseAllStripes(le binary.ByteOrder, chunkBuf []byte, numStripes uint16) []chunkStripe {
	out := make([]chunkStripe, 0, numStripes)
	for i := uint16(0); i < numStripes; i++ {
		base := chunkHeaderSize + int(i)*chunkStripeSize
		if base+chunkStripeSize > len(chunkBuf) {
			break
		}
		out = append(out, chunkStripe{
			devID:  le.Uint64(chunkBuf[base:]),
			offset: le.Uint64(chunkBuf[base+8:]),
		})
	}
	return out
}

// parseSysChunkArray parses the (KEY, CHUNK_ITEM)* array in the superblock.
// Stripes pointing at devices other than ourDevID are skipped; the chunk is
// only mapped if at least one stripe lives on this device.
func parseSysChunkArray(le binary.ByteOrder, data []byte, ourDevID uint64) ([]chunkMapping, error) {
	var mappings []chunkMapping
	off := 0
	for off+keySize <= len(data) {
		// Key: objectid(8) + type(1) + offset(8)
		keyType := data[off+8]
		logAddr := le.Uint64(data[off+9:])
		off += keySize

		if keyType != 0xE4 { // CHUNK_ITEM
			return nil, fmt.Errorf("btrfs: unexpected key type 0x%02X in sys_chunk_array", keyType)
		}
		if off+chunkHeaderSize+chunkStripeSize > len(data) {
			return nil, fmt.Errorf("btrfs: sys_chunk_array truncated")
		}
		chunk := data[off:]
		chunkSizeBytes := le.Uint64(chunk[chunkSize:])
		numStripes := le.Uint16(chunk[chunkNumStripes:])
		if numStripes == 0 {
			return nil, fmt.Errorf("btrfs: chunk with 0 stripes")
		}
		profile := le.Uint64(chunk[chunkType:])
		stripeLen := le.Uint64(chunk[chunkStripeLen:])
		subStripes := le.Uint16(chunk[chunkSubStripes:])
		stripes := parseAllStripes(le, chunk, numStripes)
		localIdx := -1
		var physOff uint64
		for i, s := range stripes {
			// Legacy compat: dev_item.devid=0 means the test fixture didn't
			// set it; treat stripe[0] as local so single-device single-stripe
			// chunks load.
			if (ourDevID != 0 && s.devID == ourDevID) || (ourDevID == 0 && i == 0) {
				localIdx = i
				physOff = s.offset
				break
			}
		}
		mappings = append(mappings, chunkMapping{
			logStart:       logAddr,
			size:           chunkSizeBytes,
			physStart:      physOff,
			localStripeIdx: localIdx,
			profile:        profile,
			stripeLen:      stripeLen,
			subStripes:     subStripes,
			stripes:        stripes,
		})
		// Advance past chunk item (header + stripes).
		off += chunkHeaderSize + int(numStripes)*chunkStripeSize
	}
	return mappings, nil
}

// hasChunkMapping reports whether sb.sysChunks already contains m (same
// logical start, size, and physical start). Used by loadChunkTree to avoid
// double-registering the chunk that sys_chunk_array also carries.
func (sb *superblock) hasChunkMapping(m chunkMapping) bool {
	for _, existing := range sb.sysChunks {
		if existing.logStart == m.logStart && existing.size == m.size && existing.physStart == m.physStart {
			return true
		}
	}
	return false
}

// lookupChunk returns the chunkMapping covering logAddr, or nil + an error
// if no chunk contains it.
func (sb *superblock) lookupChunk(logAddr uint64) (*chunkMapping, error) {
	for i := range sb.sysChunks {
		m := &sb.sysChunks[i]
		if logAddr >= m.logStart && logAddr < m.logStart+m.size {
			return m, nil
		}
	}
	return nil, fmt.Errorf("btrfs: logical address 0x%X not in any known chunk", logAddr)
}

// logToPhys converts a logical address to a physical byte offset on the
// device opened by this FS handle (the one whose dev_item.devid matches the
// stripe selected by sb.devID). Returns an error if the chunk lives entirely
// on other devices (RAID0/5/6 strip on another leg) — callers that need to
// reach those bytes must go through the multi-device router instead.
func (sb *superblock) logToPhys(logAddr uint64) (int64, error) {
	m, err := sb.lookupChunk(logAddr)
	if err != nil {
		return 0, err
	}
	if m.localStripeIdx < 0 {
		return 0, fmt.Errorf("btrfs: logical address 0x%X lives on another device (chunk profile 0x%X, no stripe on local devid)", logAddr, m.profile&blockGroupProfileMask)
	}
	return int64(m.physStart + (logAddr - m.logStart)), nil
}

// physAddr returns the address that callers feed to fs.f.ReadAt. With the
// device pool router (multidev.go) the read API takes (partOff + logical
// address), and the pool resolves chunk + stripe per profile internally.
// For chunks where logStart == physStart this is identical to the old
// chunk-translated form, so single-device images keep working unchanged.
//
// logToPhys is preserved for write-path callers that still need a raw
// per-device offset (the lib's COW allocator), and for the single-leg
// SINGLE-profile fast-path before the pool is wired.
func (sb *superblock) physAddr(partOff int64, logAddr uint64) (int64, error) {
	if _, err := sb.lookupChunk(logAddr); err != nil {
		return 0, err
	}
	return partOff + int64(logAddr), nil
}
