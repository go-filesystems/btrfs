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
//   - Shrink: when the [newSize, oldSize) physical tail is already free,
//     shrink the trailing chunk's `size` and its DEV_EXTENT length, refresh
//     the SB / dev_item / sys_chunk_array, fix the BLOCK_GROUP_ITEM length,
//     and truncate the device. When the tail is inhabited, COW-relocate the
//     live DATA extents AND the non-FS tree-root metadata blocks out of it into
//     free space below the new size (see resize_reloc.go), then proceed. When
//     the shrink drops an entire trailing chunk, remove it wholesale if it is
//     empty and a lower chunk anchors the device tail (see resize_chunk.go).
//     The residual refusals (image left untouched): a tail block this writer
//     cannot relocate (a multi-level interior node), and a non-empty whole-chunk
//     drop that would need cross-chunk content relocation.
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
// strictly smaller than the current size (equal is a no-op). When the
// [newSize, oldSize) physical tail is inhabited, Shrink COW-relocates the
// live DATA extents and the non-FS_TREE metadata blocks out of it into free
// space below the new size (see resize_reloc.go); a whole-chunk drop first
// relocates the chunk's live contents into the lower anchoring chunk (see
// resize_chunk.go). It refuses (image left untouched) only on the residual
// cases the write path cannot handle: a multi-level ROOT_TREE ROOT_ITEM edit,
// the EXTENT_TREE's own nodes when that tree is multi-level, and a non-empty
// whole-chunk drop whose footprint does not fit below the new size (see the
// scope block at the end of this file). Returns an error wrapping a "live data
// extent(s) remain" message when relocation cannot fully evacuate the tail.
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
	// (the tail chunk). If the range is already free it is truncated directly;
	// if it is inhabited the live extents/metadata are COW-relocated below the
	// new size (see the non-empty branch below and resize_reloc.go).
	tailIdx, err := fs.tailChunkIdxLocked(uint64(cur))
	if err != nil {
		return fmt.Errorf("btrfs shrink: %w", err)
	}
	tail := fs.sb.sysChunks[tailIdx]
	if uint64(newSizeBytes) <= tail.physStart {
		// The shrink drops the entire trailing chunk. Remove it wholesale: when it
		// is empty the chunk items are deleted directly; when it is NON-empty its
		// live contents are first relocated into the lower anchoring chunk
		// (cross-chunk relocation, the kernel's `btrfs balance` path) and the now-
		// empty chunk is then dropped. A lower local chunk must anchor the new
		// device tail (mirrors `btrfs filesystem resize`); a partial drop into the
		// chunk below, the only/bootstrap chunk, or a non-empty chunk whose
		// footprint does not fit below the new size — is refused.
		if err := fs.removeWholeTrailingChunkLocked(tailIdx, uint64(newSizeBytes)); err != nil {
			return fmt.Errorf("btrfs shrink: %w", err)
		}
		fs.invalidateCache()
		return nil
	}
	dropStart := uint64(newSizeBytes)
	dropEnd := uint64(cur)
	newChunkSize := tail.size - (dropEnd - dropStart)

	relocated := false
	if !fs.sm.rangeFree(dropStart, dropEnd-dropStart) {
		// The tail is inhabited. Relocate live data extents out of it before
		// truncating (mirrors `btrfs filesystem resize` shrink). Evict the tail
		// from the free list first so every replacement allocation lands below
		// the new size, then COW-relocate the overlapping data extents. If
		// relocation cannot fully evacuate the tail (e.g. an un-movable
		// metadata block of another tree remains), it returns an error and we
		// restore the free list and leave the image untouched.
		fs.sm.remove(dropStart, dropEnd-dropStart)
		if err := fs.relocateTailExtents(dropStart, dropEnd); err != nil {
			fs.sm.freeRange(dropStart, dropEnd-dropStart)
			return fmt.Errorf("btrfs shrink: %w", err)
		}
		// Post-condition oracle: no live data extent and no live metadata block
		// may overlap the tail. We verify against the live trees (not the space
		// manager's free list, which intentionally no longer tracks the evicted
		// tail). relocateTailExtents already refused on leftover metadata; this
		// guards against any data extent the relocation pass missed.
		if leftover := fs.collectRelocTargets(dropStart, dropEnd); len(leftover) > 0 {
			return fmt.Errorf("btrfs shrink: %d live data extent(s) remain in [%d, %d) after relocation",
				len(leftover), dropStart, dropEnd)
		}
		relocated = true
	}

	// Shrink the chunk geometry: CHUNK_ITEM size (+ sys_chunk_array mirror) and
	// the backing DEV_EXTENT length, plus the in-memory chunk mapping. This must
	// happen BEFORE finalizing the extent tree so the rebuilt BLOCK_GROUP_ITEM
	// records the new (smaller) chunk length and every relocated extent maps
	// into it.
	if err := fs.rewriteChunkSizeLocked(tail.logStart, newChunkSize); err != nil {
		return fmt.Errorf("btrfs shrink: rewrite chunk: %w", err)
	}
	fs.sb.sysChunks[tailIdx].size = newChunkSize

	// Finalize the extent-tree accounting so the BLOCK_GROUP_ITEM's length (its
	// key offset) and `used` match the resized chunk; `btrfs check` rejects a
	// block group whose length disagrees with its chunk.
	if relocated {
		// Relocation COW'd the FS_TREE to a new generation. Run the full
		// write-path finalize: rebuild the extent tree against the resized
		// geometry and commit the superblock + generation consistently.
		if err := updateFsTreeRoot(fs.rwa, fs.partOffset, fs.sb, fs.sm, fs.fsTreeRoot); err != nil {
			return fmt.Errorf("btrfs shrink: finalize relocation: %w", err)
		}
	} else {
		// Empty-tail shrink: no tree changed, so we must NOT bump any
		// generation (doing so makes ROOT_ITEM.generation disagree with the
		// unchanged tree-root node and triggers a parent-transid failure).
		// Edit the single BLOCK_GROUP_ITEM's length (its key offset) in place.
		if err := fs.rewriteBlockGroupLengthLocked(tail.logStart, tail.size, newChunkSize); err != nil {
			return fmt.Errorf("btrfs shrink: rewrite block group: %w", err)
		}
	}

	// Refresh total_bytes / dev_item.total_bytes in the superblock (no
	// generation bump — the in-place chunk edit and the relocation commit have
	// already set the correct generation).
	if err := fs.rewriteResizedSuperblockLocked(uint64(newSizeBytes)); err != nil {
		return fmt.Errorf("btrfs shrink: rewrite superblock: %w", err)
	}
	fs.sb.totalBytes = uint64(newSizeBytes)

	// Drop the shrunk tail from the free list (idempotent if relocation already
	// removed it).
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
	if err := fs.rewriteSysChunkArrayEntryLocked(logStart, newSize); err != nil {
		return err
	}

	// (c) DEV_EXTENT length in the dev tree. The chunk's backing dev_extent
	// records the same length; `btrfs check` (and open_ctree on newer kernels)
	// cross-check chunk size against its dev_extent and reject "device extent
	// didn't find the relative chunk" when they disagree. Update it in place.
	return fs.rewriteDevExtentLengthLocked(logStart, newSize)
}

// rewriteDevExtentLengthLocked rewrites the `length` field of the DEV_EXTENT
// item that backs the chunk at logStart, so the dev tree agrees with the
// resized CHUNK_ITEM. The DEV_EXTENT key is (1, DEV_EXTENT, chunkLogical) and
// its length lives at byte offset 24 of the 48-byte item. The dev tree is a
// single leaf for images this writer produces; we edit it in place (same
// bytenr → transid stays valid) and refresh the node CRC. A missing dev tree
// or item is tolerated (hand-built fixtures). Caller must hold fs.mu.
func (fs *btrfsFS) rewriteDevExtentLengthLocked(logStart, newLen uint64) error {
	devRoot, err := fs.devTreeRootLocked()
	if err != nil {
		return nil // no dev tree (synthetic fixture) — nothing to maintain
	}
	// Locate the leaf holding the DEV_EXTENT, descending interior nodes if the
	// dev tree is multi-level (e.g. after a multi-level relocation). The leaf is
	// edited in place at its own bytenr — transid stays valid.
	leafBuf, phys, err := fs.findExtentLeafWithKey(devRoot, 1, typeDevExtent, logStart)
	if err != nil {
		return fmt.Errorf("locate DEV_EXTENT leaf: %w", err)
	}
	if leafBuf == nil {
		return nil // no matching dev extent — tolerate
	}
	idx := findItemIdx(leafBuf, 1, typeDevExtent, logStart)
	if idx < 0 {
		return nil
	}
	items := parseLeafItems(leafBuf, parseNodeHeader(leafBuf).nItems)
	d := items[idx].data(leafBuf)
	const devExtentLengthOff = 24
	if len(d) < devExtentLengthOff+8 {
		return fmt.Errorf("DEV_EXTENT payload too short (%d bytes)", len(d))
	}
	binary.LittleEndian.PutUint64(d[devExtentLengthOff:], newLen)
	updateNodeCRC(leafBuf)
	if _, err := fs.rwa.WriteAt(leafBuf, phys); err != nil {
		return fmt.Errorf("write dev tree node: %w", err)
	}
	return nil
}

// rewriteBlockGroupLengthLocked rewrites the BLOCK_GROUP_ITEM key offset
// (the block group's length) from oldLen to newLen for the block group keyed
// by chunkLogStart, editing the extent-tree leaf in place. The block group key
// is (chunkLogStart, BLOCK_GROUP_ITEM, length); only the offset (length)
// changes and the objectid (sort-dominant) is unchanged, so the leaf stays
// key-sorted. No generation bump — the extent tree is otherwise unchanged on
// the empty-tail shrink path, so an in-place edit at the same bytenr keeps the
// transid valid. A missing extent tree / block group is tolerated (synthetic
// fixtures). Caller must hold fs.mu.
func (fs *btrfsFS) rewriteBlockGroupLengthLocked(chunkLogStart, oldLen, newLen uint64) error {
	if oldLen == newLen {
		return nil
	}
	extRoot, err := extentTreeRoot(fs.rwa, fs.partOffset, fs.sb)
	if err != nil {
		return nil // no extent tree (synthetic fixture)
	}
	leafBuf, leafPhys, err := fs.findExtentLeafWithKey(extRoot, chunkLogStart, typeBlockGroupItem, oldLen)
	if err != nil {
		return err
	}
	if leafBuf == nil {
		return nil // block group not found — tolerate
	}
	idx := findItemIdx(leafBuf, chunkLogStart, typeBlockGroupItem, oldLen)
	if idx < 0 {
		return nil
	}
	descOff := nodeHdrSize + idx*itemSize
	binary.LittleEndian.PutUint64(leafBuf[descOff+9:], newLen) // key.offset = length
	updateNodeCRC(leafBuf)
	if _, err := fs.rwa.WriteAt(leafBuf, leafPhys); err != nil {
		return fmt.Errorf("write extent leaf: %w", err)
	}
	return nil
}

// findExtentLeafWithKey walks the extent tree from root and returns the leaf
// buffer + its physical address that contains the item keyed (objID, typ,
// off), or (nil, 0, nil) when absent. Caller must hold fs.mu.
func (fs *btrfsFS) findExtentLeafWithKey(root, objID uint64, typ uint8, off uint64) ([]byte, int64, error) {
	var foundBuf []byte
	var foundPhys int64
	err := walkNodeAddrs(fs.rwa, fs.partOffset, fs.sb, root, func(logAddr uint64) error {
		if foundBuf != nil {
			return nil
		}
		buf, rerr := readNode(fs.rwa, fs.partOffset, fs.sb, logAddr)
		if rerr != nil {
			return nil
		}
		if parseNodeHeader(buf).level != 0 {
			return nil
		}
		if findItemIdx(buf, objID, typ, off) >= 0 {
			phys, perr := fs.sb.physAddr(fs.partOffset, logAddr)
			if perr != nil {
				return nil
			}
			foundBuf = buf
			foundPhys = phys
		}
		return nil
	})
	return foundBuf, foundPhys, err
}

// devTreeRootLocked returns the current logical address of the DEV_TREE root
// node, read from the ROOT_TREE. Caller must hold fs.mu.
func (fs *btrfsFS) devTreeRootLocked() (uint64, error) {
	buf, it, err := searchTree(fs.rwa, fs.partOffset, fs.sb, fs.sb.rootLogAddr, devTreeObjID, typeRootItem, 0)
	if err != nil {
		return 0, err
	}
	d := it.data(buf)
	if len(d) < rootItemOffBytenr+8 {
		return 0, fmt.Errorf("DEV_TREE root item too short")
	}
	return binary.LittleEndian.Uint64(d[rootItemOffBytenr:]), nil
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

// rewriteResizedSuperblockLocked refreshes the primary superblock to carry the
// new total_bytes, dev_item.total_bytes and chunkLogAddr (in case cowUpdate
// reseated it). It does NOT bump super.generation: total_bytes/dev_item are
// edited in place and the chunk tree is edited in place at its existing bytenr,
// so super.generation must keep matching the chunk-root / tree-root node
// generations — bumping it past them yields a transid mismatch the kernel and
// `btrfs check` reject. The relocation transaction (when one ran) already
// committed the correct generation via updateFsTreeRoot. Caller must hold
// fs.mu.
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
	// dev_item.bytes_used (sbfDevItem + 0x10) = sum of every DEV_EXTENT length on
	// this device, i.e. the sum of all local chunk sizes. Recompute from the
	// in-memory chunk map (already updated to the resized geometry) so it agrees
	// with the chunk-tree DEV_ITEM and the dev tree's DEV_EXTENTs.
	var devBytesUsed uint64
	for _, m := range fs.sb.sysChunks {
		if m.localStripeIdx >= 0 {
			devBytesUsed += m.size
		}
	}
	le.PutUint64(buf[sbfDevItem+0x10:], devBytesUsed)

	updateSuperblockCRC(buf)
	if _, err := fs.rwa.WriteAt(buf, fs.partOffset+superblockOffset); err != nil {
		return fmt.Errorf("write superblock: %w", err)
	}

	// The chunk tree carries its own copy of the DEV_ITEM (devItemsObjID,
	// DEV_ITEM, 1); the kernel and `btrfs check` cross-check it against the
	// superblock's embedded dev_item. Mirror total_bytes + bytes_used into it.
	return fs.rewriteChunkTreeDevItemLocked(newTotalBytes, devBytesUsed)
}

// rewriteChunkTreeDevItemLocked patches the chunk-tree DEV_ITEM's total_bytes
// (+0x08) and bytes_used (+0x10) in place, keeping it consistent with the
// superblock's embedded dev_item after a resize. Edited in place at the chunk
// tree's existing bytenr (transid stays valid). Caller must hold fs.mu.
func (fs *btrfsFS) rewriteChunkTreeDevItemLocked(totalBytes, bytesUsed uint64) error {
	phys, err := fs.sb.physAddr(fs.partOffset, fs.sb.chunkLogAddr)
	if err != nil {
		return fmt.Errorf("locate chunk tree node: %w", err)
	}
	leafBuf := make([]byte, fs.sb.nodeSize)
	if _, err := fs.rwa.ReadAt(leafBuf, phys); err != nil {
		return fmt.Errorf("read chunk tree node: %w", err)
	}
	if parseNodeHeader(leafBuf).level != 0 {
		return nil
	}
	idx := findItemIdx(leafBuf, devItemsObjID, typeDevItem, 1)
	if idx < 0 {
		return nil // no DEV_ITEM (synthetic fixture) — tolerate
	}
	items := parseLeafItems(leafBuf, parseNodeHeader(leafBuf).nItems)
	dd := items[idx].data(leafBuf)
	if len(dd) < 0x10+8 {
		return fmt.Errorf("DEV_ITEM payload too short (%d bytes)", len(dd))
	}
	le := binary.LittleEndian
	le.PutUint64(dd[0x08:], totalBytes)
	le.PutUint64(dd[0x10:], bytesUsed)
	updateNodeCRC(leafBuf)
	if _, err := fs.rwa.WriteAt(leafBuf, phys); err != nil {
		return fmt.Errorf("write chunk tree node: %w", err)
	}
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

// Shrink-relocation scope (implemented in resize_reloc.go):
//
//   - DATA extents whose physical range overlaps [newSize, oldSize) are
//     COW-relocated below newSize (allocate, copy bytes, COW-rewrite the
//     EXTENT_DATA disk_bytenr; the extent tree + block-group `used` +
//     bytes_used are then recomputed by rebuildExtentTree). Validated against
//     the real kernel: `btrfs check` reports the result clean and the kernel
//     loop-mounts it with every file byte-identical (see
//     TestReloc_KernelOracle).
//
// Also implemented:
//
//   - METADATA tree blocks of trees other than the FS_TREE sitting in the tail
//     (EXTENT / DEV / CSUM / UUID / ROOT / DATA_RELOC roots) are COW-relocated
//     below the new size by relocateTailMetadata (resize_reloc.go); the
//     EXTENT_TREE is then rebuilt to account them. btrfs-check / kernel-mount
//     validated (TestRelocMeta_KernelOracle).
//   - A multi-level non-FS tree (an interior root node, or interior/leaf
//     descendants) sitting in the tail is path-COW-relocated bottom-up by
//     relocateSubtreePath (resize_reloc.go): the deepest tail node moves first,
//     parents' child key-pointers (blockptr + generation) are repointed, and the
//     gen bump propagates up the whole path to the ROOT_ITEM — keeping the
//     kernel's parent-transid invariant. btrfs-check / kernel-mount validated.
//   - A shrink that removes an entire trailing chunk deletes its CHUNK_ITEM /
//     DEV_EXTENT / BLOCK_GROUP_ITEM / sys_chunk_array entry (resize_chunk.go),
//     mirroring btrfs_remove_chunk. When the chunk is NON-empty its live data
//     extents and metadata blocks are first relocated into the lower anchoring
//     chunk (relocateNonEmptyChunkLocked, the kernel's relocate_block_group
//     path) and the transaction finalized before removal. Validated by
//     TestRemoveChunk_KernelOracle.
//
// Out of scope (refused with a clear error, image left untouched/consistent):
//
//   - A MULTI-LEVEL ROOT_TREE whose ROOT_ITEM leaf would move (the in-place
//     ROOT_ITEM edit cannot descend a multi-level root tree), and the
//     EXTENT_TREE's own nodes when the extent tree is multi-level (the extent
//     leaf is rebuilt in place under a single-leaf assumption). The chunk tree
//     (SYSTEM chunk, never in the DATA tail) is never relocated.
//   - A non-empty whole-chunk drop whose live footprint does not fit in the
//     lower chunk's free space below the new size.
