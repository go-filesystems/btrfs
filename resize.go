package filesystem_btrfs

// Filesystem-level resize (grow/shrink) for the in-place btrfs image.
//
// Semantics (mirroring `btrfs filesystem resize`):
//
//   - Grow: extend the trailing chunk's `size`, bump the superblock's
//     `total_bytes` and the embedded dev_item's `total_bytes`, refresh
//     the sys_chunk_array entry covering the resized chunk, and extend
//     the backing device. The newly-mapped range becomes free space
//     for the allocator.
//   - Shrink: validate that the [newSize, oldSize) physical region is
//     fully free (no allocated extents, no node blocks); shrink the
//     trailing chunk's `size` and the SB / dev_item / sys_chunk_array
//     in the same fashion; truncate the device. Inhabited shrink
//     targets are rejected — the kernel relocates extents to lower
//     addresses, but the work needed for full extent relocation is
//     deferred (see TODO at the bottom of this file).
//   - Resize: thin dispatcher that picks Grow vs Shrink (or no-op when
//     the new size equals the current size).
//
// All three methods take the lock and act on a quiescent FS — concurrent
// writes during a resize are not safe (same constraint as SetLabel and
// the kernel's online-resize ioctl).

import (
	"encoding/binary"
	"fmt"
)

// Minimum image size accepted by Resize/Grow/Shrink; mirrors fmtMinSize.
const resizeMinSize int64 = fmtMinSize

// GrowTo satisfies the filesystem.Grower optional interface — it is a
// thin alias for Grow, which carries btrfs-specific semantics
// (Grow/Shrink/Resize live on the FS interface in this package).
func (fs *btrfsFS) GrowTo(newSizeBytes int64) error { return fs.Grow(newSizeBytes) }

// Resize sets the filesystem size to exactly newSizeBytes. Larger than
// the current size dispatches to Grow; smaller dispatches to Shrink;
// equal is a no-op. Negative sizes are rejected.
func (fs *btrfsFS) Resize(newSizeBytes int64) error {
	if newSizeBytes < 0 {
		return fmt.Errorf("btrfs resize: negative size %d", newSizeBytes)
	}
	fs.mu.Lock()
	cur := int64(fs.sb.totalBytes)
	fs.mu.Unlock()

	switch {
	case newSizeBytes == cur:
		return nil
	case newSizeBytes > cur:
		return fs.Grow(newSizeBytes)
	default:
		return fs.Shrink(newSizeBytes)
	}
}

// Grow extends the filesystem to span newSizeBytes. The new size must be
// greater than or equal to the current size (equal is a no-op) and a
// multiple of the filesystem's sector size. The backing device is
// extended via blockBackend.Truncate; the trailing chunk's mapping is
// stretched to cover the new tail; and the superblock + dev_item +
// sys_chunk_array entries are rewritten.
func (fs *btrfsFS) Grow(newSizeBytes int64) error {
	if newSizeBytes < 0 {
		return fmt.Errorf("btrfs grow: negative size %d", newSizeBytes)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	defer fs.invalidateCache()

	cur := int64(fs.sb.totalBytes)
	if newSizeBytes == cur {
		return nil
	}
	if newSizeBytes < cur {
		return fmt.Errorf("btrfs grow: new size %d below current %d (use Shrink)", newSizeBytes, cur)
	}
	if newSizeBytes < resizeMinSize {
		return fmt.Errorf("btrfs grow: size %d below minimum %d", newSizeBytes, resizeMinSize)
	}
	sec := int64(fs.sb.sectorSize)
	if sec <= 0 {
		return fmt.Errorf("btrfs grow: invalid sector size %d", fs.sb.sectorSize)
	}
	if newSizeBytes%sec != 0 {
		return fmt.Errorf("btrfs grow: size %d is not a multiple of sector size %d", newSizeBytes, sec)
	}

	// Locate the chunk whose tail abuts the current device size. For images
	// produced by Format() and for most cloud images there's a single chunk
	// covering [0, totalBytes); for hand-crafted layouts we pick the chunk
	// with the highest (physStart + size) on the local device — that's the
	// one the new tail will extend.
	tailIdx, err := fs.tailChunkIdxLocked(uint64(cur))
	if err != nil {
		return fmt.Errorf("btrfs grow: %w", err)
	}
	added := uint64(newSizeBytes - cur)

	// Extend the backing device first. If the FS update later fails the
	// extra bytes are simply unreferenced — they don't corrupt anything.
	if err := fs.f.Truncate(fs.partOffset + newSizeBytes); err != nil {
		return fmt.Errorf("btrfs grow: truncate: %w", err)
	}

	// Update on-disk state. We do these in an order that leaves the FS
	// readable at every step: chunk tree first (so the new logical range
	// resolves), then sys_chunk_array + SB last (so dump-super sees both
	// the new total_bytes and the matching chunk geometry).
	oldChunk := fs.sb.sysChunks[tailIdx]
	newSize := oldChunk.size + added
	if err := fs.rewriteChunkSizeLocked(oldChunk.logStart, newSize); err != nil {
		return fmt.Errorf("btrfs grow: rewrite chunk: %w", err)
	}

	// Update the in-memory chunk so subsequent reads / writes see the
	// extended mapping.
	fs.sb.sysChunks[tailIdx].size = newSize

	// Update the SB: total_bytes + dev_item.total_bytes + bump generation.
	if err := fs.rewriteResizedSuperblockLocked(uint64(newSizeBytes)); err != nil {
		return fmt.Errorf("btrfs grow: rewrite superblock: %w", err)
	}
	fs.sb.totalBytes = uint64(newSizeBytes)

	// Hand the new tail to the space manager so future writes can use it.
	if fs.sm != nil {
		fs.sm.freeRange(oldChunk.physStart+oldChunk.size, added)
	}

	return fs.f.Sync()
}

// Shrink reduces the filesystem to newSizeBytes. The new size must be
// strictly smaller than the current size (equal is a no-op), and the
// [newSize, oldSize) physical region must be entirely free — Shrink
// does not yet relocate extents (see TODO at end of file). Returns
// an error wrapping a "shrink would discard live data" message when
// the trailing region is not empty.
func (fs *btrfsFS) Shrink(newSizeBytes int64) error {
	if newSizeBytes < 0 {
		return fmt.Errorf("btrfs shrink: negative size %d", newSizeBytes)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	defer fs.invalidateCache()

	cur := int64(fs.sb.totalBytes)
	if newSizeBytes == cur {
		return nil
	}
	if newSizeBytes > cur {
		return fmt.Errorf("btrfs shrink: new size %d above current %d (use Grow)", newSizeBytes, cur)
	}
	if newSizeBytes < resizeMinSize {
		return fmt.Errorf("btrfs shrink: size %d below minimum %d", newSizeBytes, resizeMinSize)
	}
	sec := int64(fs.sb.sectorSize)
	if sec <= 0 {
		return fmt.Errorf("btrfs shrink: invalid sector size %d", fs.sb.sectorSize)
	}
	if newSizeBytes%sec != 0 {
		return fmt.Errorf("btrfs shrink: size %d is not a multiple of sector size %d", newSizeBytes, sec)
	}

	// Read bytes_used from the SB; refuse to drop below the live footprint.
	bytesUsed, err := fs.readBytesUsedLocked()
	if err != nil {
		return fmt.Errorf("btrfs shrink: %w", err)
	}
	if uint64(newSizeBytes) < bytesUsed {
		return fmt.Errorf("btrfs shrink: new size %d below bytes_used %d", newSizeBytes, bytesUsed)
	}

	// The shrink window [newSize, cur) must lie entirely within ONE chunk
	// (the tail chunk) AND that range must be fully free in the space
	// manager. Relocation of live extents is not implemented.
	tailIdx, err := fs.tailChunkIdxLocked(uint64(cur))
	if err != nil {
		return fmt.Errorf("btrfs shrink: %w", err)
	}
	tail := fs.sb.sysChunks[tailIdx]
	if uint64(newSizeBytes) <= tail.physStart {
		return fmt.Errorf("btrfs shrink: new size %d would remove entire trailing chunk at phys 0x%X (relocation not supported)",
			newSizeBytes, tail.physStart)
	}
	dropStart := uint64(newSizeBytes)
	dropEnd := uint64(cur)
	if !fs.sm.rangeFree(dropStart, dropEnd-dropStart) {
		return fmt.Errorf("btrfs shrink: range [%d, %d) is not free (relocation not supported)", dropStart, dropEnd)
	}

	// Update on-disk state. SB last so a torn write still leaves a valid
	// (over-sized) image rather than a too-small one with stale chunks.
	newChunkSize := tail.size - (dropEnd - dropStart)
	if err := fs.rewriteChunkSizeLocked(tail.logStart, newChunkSize); err != nil {
		return fmt.Errorf("btrfs shrink: rewrite chunk: %w", err)
	}
	fs.sb.sysChunks[tailIdx].size = newChunkSize

	if err := fs.rewriteResizedSuperblockLocked(uint64(newSizeBytes)); err != nil {
		return fmt.Errorf("btrfs shrink: rewrite superblock: %w", err)
	}
	fs.sb.totalBytes = uint64(newSizeBytes)

	// Drop the shrunk tail from the free list.
	if fs.sm != nil {
		fs.sm.remove(dropStart, dropEnd-dropStart)
	}

	// Truncate the device last; even if it fails the FS metadata is
	// already consistent at the smaller size.
	if err := fs.f.Truncate(fs.partOffset + newSizeBytes); err != nil {
		return fmt.Errorf("btrfs shrink: truncate: %w", err)
	}
	return fs.f.Sync()
}

// tailChunkIdxLocked returns the index in fs.sb.sysChunks of the chunk
// whose tail (physStart + size) sits at — or, for tolerance against
// hand-built fixtures, closest to — `endPhys`. Only chunks with a local
// stripe (data physically on this device) are considered. Caller must
// hold fs.mu.
func (fs *btrfsFS) tailChunkIdxLocked(endPhys uint64) (int, error) {
	best := -1
	var bestTail uint64
	for i, m := range fs.sb.sysChunks {
		if m.localStripeIdx < 0 {
			continue
		}
		tail := m.physStart + m.size
		if tail > bestTail {
			bestTail = tail
			best = i
		}
	}
	if best < 0 {
		return -1, fmt.Errorf("no local chunk found to anchor resize")
	}
	if bestTail != endPhys {
		// Be strict: extending a chunk that doesn't reach the device tail
		// would leave a gap (grow) or split a live chunk (shrink). Refuse
		// rather than guess.
		return -1, fmt.Errorf("trailing chunk ends at 0x%X but device size is 0x%X (gap or unrelated chunk layout)", bestTail, endPhys)
	}
	return best, nil
}

// rewriteChunkSizeLocked persists a new `size` value for the chunk_item
// keyed by logStart. It updates (a) the chunk tree's CHUNK_ITEM via the
// COW write path, and (b) the embedded sys_chunk_array entry in the
// primary superblock when one is present for that key. Caller must hold
// fs.mu.
func (fs *btrfsFS) rewriteChunkSizeLocked(logStart, newSize uint64) error {
	// (a) Chunk tree update — IN PLACE. The chunk tree lives in the SYSTEM chunk
	// (the only chunk the superblock's sys_chunk_array maps), so it must NOT be
	// COW-relocated: the space manager only allocates from DATA|METADATA chunks,
	// and a relocated chunk-tree node would land in a chunk that is itself only
	// described inside the chunk tree — unreachable at the next mount. We instead
	// rewrite the CHUNK_ITEM's size field in the existing single-leaf chunk tree
	// and refresh its CRC. CHUNK_ITEM key:
	// (FIRST_CHUNK_TREE_OBJECTID, CHUNK_ITEM, logStart).
	phys, err := fs.sb.physAddr(fs.partOffset, fs.sb.chunkLogAddr)
	if err != nil {
		return fmt.Errorf("locate chunk tree node: %w", err)
	}
	leafBuf := make([]byte, fs.sb.nodeSize)
	if _, err := fs.rwa.ReadAt(leafBuf, phys); err != nil {
		return fmt.Errorf("read chunk tree node: %w", err)
	}
	idx := findItemIdx(leafBuf, firstChunkTreeObjID, typeChunkItem, logStart)
	if idx < 0 {
		return fmt.Errorf("locate CHUNK_ITEM %d: not in chunk tree leaf", logStart)
	}
	items := parseLeafItems(leafBuf, parseNodeHeader(leafBuf).nItems)
	d := items[idx].data(leafBuf)
	if len(d) < chunkHeaderSize {
		return fmt.Errorf("CHUNK_ITEM payload too short (%d bytes)", len(d))
	}
	binary.LittleEndian.PutUint64(d[chunkSize:], newSize)
	updateNodeCRC(leafBuf)
	if _, err := fs.rwa.WriteAt(leafBuf, phys); err != nil {
		return fmt.Errorf("write chunk tree node: %w", err)
	}

	// (b) sys_chunk_array embedded copy. Only present for SYSTEM chunks and
	// for chunks Format() writes into the array (its single chunk has
	// type=0 but is still mirrored there). We scan the array for the key
	// (1, 0xE4, logStart) and overwrite the size field in place.
	return fs.rewriteSysChunkArrayEntryLocked(logStart, newSize)
}

// rewriteSysChunkArrayEntryLocked rewrites the chunk_item.size field of
// the sys_chunk_array entry keyed by logStart, if such an entry exists
// in the primary superblock. Best-effort: a missing entry is not an
// error (most data/metadata chunks live only in the chunk tree). Caller
// must hold fs.mu.
func (fs *btrfsFS) rewriteSysChunkArrayEntryLocked(logStart, newSize uint64) error {
	buf := make([]byte, sbfSize)
	if _, err := fs.rwa.ReadAt(buf, fs.partOffset+superblockOffset); err != nil {
		return fmt.Errorf("read superblock: %w", err)
	}
	le := binary.LittleEndian
	arrSz := int(le.Uint32(buf[sbfSysChunkArrSz:]))
	if arrSz <= 0 || sbfSysChunkArr+arrSz > len(buf) {
		return nil // empty / corrupt — leave alone
	}
	arr := buf[sbfSysChunkArr : sbfSysChunkArr+arrSz]
	off := 0
	for off+keySize+chunkHeaderSize+chunkStripeSize <= len(arr) {
		keyType := arr[off+8]
		keyOff := le.Uint64(arr[off+9:])
		chunkBase := off + keySize
		numStripes := le.Uint16(arr[chunkBase+chunkNumStripes:])
		entryLen := keySize + chunkHeaderSize + int(numStripes)*chunkStripeSize
		if off+entryLen > len(arr) {
			break
		}
		if keyType == typeChunkItem && keyOff == logStart {
			le.PutUint64(arr[chunkBase+chunkSize:], newSize)
			// Persist with refreshed CRC.
			updateSuperblockCRC(buf)
			if _, err := fs.rwa.WriteAt(buf, fs.partOffset+superblockOffset); err != nil {
				return fmt.Errorf("write superblock: %w", err)
			}
			return nil
		}
		off += entryLen
	}
	return nil // not in sys_chunk_array — nothing more to do
}

// rewriteResizedSuperblockLocked refreshes the primary superblock to
// carry the new total_bytes, dev_item.total_bytes, chunkLogAddr (in
// case cowUpdate reseated it), and a bumped generation. Caller must
// hold fs.mu.
func (fs *btrfsFS) rewriteResizedSuperblockLocked(newTotalBytes uint64) error {
	buf := make([]byte, sbfSize)
	if _, err := fs.rwa.ReadAt(buf, fs.partOffset+superblockOffset); err != nil {
		return fmt.Errorf("read superblock: %w", err)
	}
	le := binary.LittleEndian

	le.PutUint64(buf[sbfTotalBytes:], newTotalBytes)
	le.PutUint64(buf[sbfChunkLogAddr:], fs.sb.chunkLogAddr)
	// dev_item.total_bytes is at sbfDevItem + 0x08 (devid:8 then total_bytes:8).
	le.PutUint64(buf[sbfDevItem+0x08:], newTotalBytes)
	gen := le.Uint64(buf[sbfGeneration:]) + 1
	le.PutUint64(buf[sbfGeneration:], gen)

	updateSuperblockCRC(buf)
	if _, err := fs.rwa.WriteAt(buf, fs.partOffset+superblockOffset); err != nil {
		return fmt.Errorf("write superblock: %w", err)
	}
	fs.sb.generation = gen
	return nil
}

// readBytesUsedLocked returns the bytes_used value from the primary
// superblock. Caller must hold fs.mu.
func (fs *btrfsFS) readBytesUsedLocked() (uint64, error) {
	buf := make([]byte, 8)
	if _, err := fs.rwa.ReadAt(buf, fs.partOffset+superblockOffset+int64(sbfBytesUsed)); err != nil {
		return 0, fmt.Errorf("read bytes_used: %w", err)
	}
	return binary.LittleEndian.Uint64(buf), nil
}

// rangeFree reports whether [start, start+size) is fully covered by the
// space manager's free list (i.e. no byte inside the range is allocated).
// Used by Shrink to gate the no-relocation path.
func (sm *spaceManager) rangeFree(start, size uint64) bool {
	if size == 0 {
		return true
	}
	end := start + size
	// Walk free extents; cover requires the union of [start,end)-overlapping
	// extents to be contiguous and span the entire window.
	cursor := start
	for _, fe := range sm.freeExts {
		feEnd := fe.physStart + fe.size
		if feEnd <= cursor {
			continue
		}
		if fe.physStart > cursor {
			return false // gap
		}
		if feEnd >= end {
			return true
		}
		cursor = feEnd
	}
	return false
}

// TODO(resize-relocation): the kernel's `btrfs filesystem resize` shrink
// path walks every EXTENT_DATA / EXTENT_ITEM whose physical end exceeds
// newSize and rewrites its diskBytenr to a lower physical address (via
// the regular extent allocator). Implementing that here means:
//
//   1. Scan the FS_TREE for EXTENT_DATA items with regular extents whose
//      diskBytenr+diskNumBytes lies in [newSize, oldSize).
//   2. For each such extent, allocate a new physical range below newSize
//      via sm.allocDataBytes, copy the bytes, COW-update the EXTENT_DATA
//      to point at the new bytenr, and free the old range.
//   3. Repeat for metadata node blocks (read each node, allocate below,
//      rewrite all parent pointers — this is the COW tree-rewrite path).
//   4. Only then proceed to shrink the chunk + SB + truncate.
//
// The current implementation refuses inhabited shrinks rather than open
// that ~600-800-LOC can of worms. The Grow path and the empty-tail
// shrink path together are enough for cloud-image rootfs grow + reclaim
// of unused tail capacity, which is the headline use-case.
