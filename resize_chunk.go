package filesystem_btrfs

// Shrink-time whole-chunk removal.
//
// When a shrink drops the device below the start of the trailing chunk, that
// chunk must be removed wholesale — its BLOCK_GROUP_ITEM, CHUNK_ITEM, DEV_EXTENT
// and (when present) sys_chunk_array entry deleted and the dev-tree / chunk-tree
// kept consistent — mirroring the kernel's btrfs_remove_chunk path. A lower
// chunk must still anchor the device tail after removal.
//
// When the trailing chunk is EMPTY the items are deleted directly. When it is
// NON-empty, its live data extents and metadata blocks are first COW-relocated
// into the lower anchoring chunk's free space (relocateNonEmptyChunkLocked — the
// kernel's relocate_block_group / `btrfs balance` path), the transaction is
// finalized so the block-group `used` moves to the lower chunk, and the now-empty
// chunk is then removed.
//
// Removing the only chunk or the bootstrap SYSTEM chunk, or a non-empty chunk
// whose live footprint does not fit below the new size, stays a precise refusal
// — see Shrink / removeWholeTrailingChunkLocked.

import (
	"encoding/binary"
	"fmt"
)

// removeWholeTrailingChunkLocked validates that the trailing chunk at
// sysChunks[idx] can be removed wholesale by a shrink to newSize, then performs
// the removal — relocating its live contents into the lower anchoring chunk
// first when the chunk is non-empty. The preconditions (each a precise refusal,
// image untouched):
//
//   - newSize must equal the chunk's physical start: the device tail lands
//     exactly at the chunk boundary, not part-way into it (a partial drop of a
//     lower chunk is the in-chunk shrink path, handled elsewhere).
//   - a lower LOCAL chunk must end exactly at newSize, so the device stays
//     gap-free and anchored after the removal (never remove the only/bootstrap
//     chunk and leave the device with no chunk reaching its tail).
//
// When the chunk is empty it is deleted directly (btrfs_remove_chunk for an
// empty block group). When it is non-empty, its live data extents and metadata
// blocks are first COW-relocated into the lower chunk's free space (the kernel's
// relocate_block_group / btrfs balance path) via relocateNonEmptyChunkLocked,
// then the now-empty chunk is removed. A non-empty chunk whose live footprint
// does not fit below newSize is refused (image untouched).
//
// Caller holds fs.mu.
func (fs *btrfsFS) removeWholeTrailingChunkLocked(idx int, newSize uint64) error {
	chunk := fs.sb.sysChunks[idx]
	if newSize != chunk.physStart {
		return fmt.Errorf("new size %d does not land on the trailing chunk boundary 0x%X (partial drop of a lower chunk not supported)",
			newSize, chunk.physStart)
	}
	// Require a lower local chunk whose tail abuts newSize.
	anchored := false
	for i, m := range fs.sb.sysChunks {
		if i == idx || m.localStripeIdx < 0 {
			continue
		}
		if m.physStart+m.size == newSize {
			anchored = true
			break
		}
	}
	if !anchored {
		return fmt.Errorf("removing chunk at phys 0x%X would leave the device with no chunk reaching its tail %d (only/bootstrap-chunk removal not supported)",
			chunk.physStart, newSize)
	}
	if !fs.chunkIsEvacuableLocked(idx) {
		// Non-empty: relocate the chunk's live contents into the lower chunk,
		// then fall through to the empty-chunk removal. relocateNonEmptyChunkLocked
		// leaves the image consistent (rolls the evicted tail back) on failure.
		if err := fs.relocateNonEmptyChunkLocked(idx, newSize); err != nil {
			return err
		}
	}
	return fs.removeTrailingChunkLocked(idx, newSize)
}

// relocateNonEmptyChunkLocked evacuates the live contents of the non-empty
// trailing chunk at sysChunks[idx] into free space in the lower (anchoring)
// chunk, leaving the trailing chunk empty so removeTrailingChunkLocked can drop
// it. It reuses the shrink-relocation machinery with the relocation window set
// to the WHOLE chunk: data extents are COW-copied below the chunk start and
// every referencing item / backref is rewritten; non-FS metadata blocks are
// path-COW-relocated; the FS_TREE reseats via COW. The transaction is then
// finalized (updateFsTreeRoot → rebuildExtentTree) BEFORE the chunk is removed,
// so the rebuilt block-group `used` moves from this chunk (→ 0) to the lower one
// and the intermediate image is itself btrfs-check-clean.
//
// On entry the chunk's logical range is still in the space manager's free map as
// far as future allocations are concerned; we evict it first so every
// replacement lands strictly below newSize (== the chunk start), i.e. in the
// lower chunk. Any allocation that cannot fit there surfaces as an error and the
// evicted range is restored. Caller holds fs.mu.
func (fs *btrfsFS) relocateNonEmptyChunkLocked(idx int, newSize uint64) error {
	chunk := fs.sb.sysChunks[idx]
	dropStart := chunk.physStart
	dropEnd := chunk.physStart + chunk.size

	// Evict the chunk's range so replacement allocations never land back in it.
	fs.sm.remove(dropStart, dropEnd-dropStart)

	if err := fs.relocateTailExtents(dropStart, dropEnd); err != nil {
		fs.sm.freeRange(dropStart, dropEnd-dropStart)
		return fmt.Errorf("relocate non-empty chunk at phys 0x%X: %w", chunk.physStart, err)
	}

	// Post-condition oracle: no live data extent and no live metadata block may
	// overlap the chunk after relocation.
	if leftover := fs.collectRelocTargets(dropStart, dropEnd); len(leftover) > 0 {
		fs.sm.freeRange(dropStart, dropEnd-dropStart)
		return fmt.Errorf("relocate non-empty chunk at phys 0x%X: %d live data extent(s) remain after relocation",
			chunk.physStart, len(leftover))
	}

	// Finalize the relocation transaction while the (now-empty) chunk is STILL in
	// sb.sysChunks, so rebuildExtentTree recomputes this chunk's block-group
	// `used` to 0 and grows the lower chunk's `used` by the relocated bytes. The
	// chunk's own BLOCK_GROUP_ITEM is deleted afterwards by removeTrailingChunkLocked.
	if err := updateFsTreeRoot(fs.rwa, fs.partOffset, fs.sb, fs.sm, fs.fsTreeRoot); err != nil {
		return fmt.Errorf("relocate non-empty chunk at phys 0x%X: finalize: %w", chunk.physStart, err)
	}
	fs.invalidateCache()

	// Sanity: the chunk must now report a zero block-group `used`.
	if used, ok := fs.blockGroupUsedLocked(chunk.logStart, chunk.size); ok && used != 0 {
		return fmt.Errorf("relocate non-empty chunk at phys 0x%X: block group still reports used=%d after relocation",
			chunk.physStart, used)
	}
	return nil
}

// removeTrailingChunkLocked deletes the empty trailing chunk at sysChunks[idx]
// from the chunk tree, dev tree, extent tree and sys_chunk_array, then refreshes
// the superblock geometry and truncates the device to newSize. It mirrors
// btrfs_remove_chunk for an empty block group. The chunk's logical range must be
// free in the space manager and its block group `used` must be 0 — both verified
// by the caller. No generation bump: every edit is in place at an existing
// tree-block bytenr, so each tree's ROOT_ITEM generation stays matched to its
// node header. Caller holds fs.mu.
func (fs *btrfsFS) removeTrailingChunkLocked(idx int, newSize uint64) error {
	chunk := fs.sb.sysChunks[idx]

	// 1. Delete the CHUNK_ITEM (FIRST_CHUNK_TREE, CHUNK_ITEM, logStart) from the
	//    single-leaf chunk tree, in place.
	if err := fs.deleteChunkTreeItemLocked(firstChunkTreeObjID, typeChunkItem, chunk.logStart); err != nil {
		return fmt.Errorf("delete CHUNK_ITEM: %w", err)
	}

	// 2. Delete the backing DEV_EXTENT (1, DEV_EXTENT, logStart) from the dev tree.
	if err := fs.deleteDevTreeItemLocked(1, typeDevExtent, chunk.logStart); err != nil {
		return fmt.Errorf("delete DEV_EXTENT: %w", err)
	}

	// 3. Delete the BLOCK_GROUP_ITEM (logStart, BLOCK_GROUP_ITEM, size) from the
	//    extent tree.
	if err := fs.deleteBlockGroupItemLocked(chunk.logStart, chunk.size); err != nil {
		return fmt.Errorf("delete BLOCK_GROUP_ITEM: %w", err)
	}

	// 4. Remove the sys_chunk_array entry, if this chunk had one (data chunks
	//    normally do not, but Format mirrors its single chunk there).
	if err := fs.deleteSysChunkArrayEntryLocked(chunk.logStart); err != nil {
		return fmt.Errorf("remove sys_chunk_array entry: %w", err)
	}

	// 5. Drop the chunk from the in-memory map and its range from the allocator.
	fs.sb.sysChunks = append(fs.sb.sysChunks[:idx], fs.sb.sysChunks[idx+1:]...)
	if fs.sm != nil {
		fs.sm.remove(chunk.physStart, chunk.size)
	}

	// 6. Refresh super.total_bytes + dev_item totals/bytes_used (recomputed from
	//    the now-smaller chunk map) and the chunk-tree DEV_ITEM mirror.
	if err := fs.rewriteResizedSuperblockLocked(newSize); err != nil {
		return fmt.Errorf("rewrite superblock: %w", err)
	}
	fs.sb.totalBytes = newSize

	// 7. Truncate the device last; the metadata is already consistent.
	if err := fs.f.Truncate(fs.partOffset + int64(newSize)); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}
	return fs.f.Sync()
}

// deleteChunkTreeItemLocked removes the item keyed (objID, typ, off) from the
// single-leaf chunk tree, editing the leaf in place and refreshing its CRC. The
// chunk tree lives in the SYSTEM chunk and must not be COW-relocated. Caller
// holds fs.mu.
func (fs *btrfsFS) deleteChunkTreeItemLocked(objID uint64, typ uint8, off uint64) error {
	phys, err := fs.sb.physAddr(fs.partOffset, fs.sb.chunkLogAddr)
	if err != nil {
		return fmt.Errorf("locate chunk tree node: %w", err)
	}
	return fs.deleteLeafItemInPlace(phys, objID, typ, off)
}

// deleteDevTreeItemLocked removes (objID, typ, off) from the single-leaf dev
// tree in place. A missing dev tree / item is tolerated (synthetic fixtures).
// Caller holds fs.mu.
func (fs *btrfsFS) deleteDevTreeItemLocked(objID uint64, typ uint8, off uint64) error {
	devRoot, err := fs.devTreeRootLocked()
	if err != nil {
		return nil // no dev tree — nothing to maintain
	}
	// Descend interior nodes if the dev tree is multi-level (e.g. after a
	// multi-level relocation), locating the leaf that holds the item.
	leaf, phys, err := fs.findExtentLeafWithKey(devRoot, objID, typ, off)
	if err != nil {
		return fmt.Errorf("locate dev tree item: %w", err)
	}
	if leaf == nil {
		return nil // tolerate absence
	}
	return fs.deleteLeafItemInPlace(phys, objID, typ, off)
}

// deleteBlockGroupItemLocked removes the BLOCK_GROUP_ITEM (chunkLogStart,
// BLOCK_GROUP_ITEM, size) from the extent tree leaf in place. A missing extent
// tree / item is tolerated. Caller holds fs.mu.
func (fs *btrfsFS) deleteBlockGroupItemLocked(chunkLogStart, size uint64) error {
	extRoot, err := extentTreeRoot(fs.rwa, fs.partOffset, fs.sb)
	if err != nil {
		return nil // no extent tree (synthetic fixture)
	}
	leaf, phys, err := fs.findExtentLeafWithKey(extRoot, chunkLogStart, typeBlockGroupItem, size)
	if err != nil {
		return err
	}
	if leaf == nil {
		return nil // not found — tolerate
	}
	return fs.deleteLeafItemInPlace(phys, chunkLogStart, typeBlockGroupItem, size)
}

// deleteLeafItemInPlace re-reads the leaf at phys, removes the item keyed
// (objID, typ, off) via leafDeleteItem, refreshes the CRC and writes it back to
// the same physical block (transid stays valid). Caller holds fs.mu.
func (fs *btrfsFS) deleteLeafItemInPlace(phys int64, objID uint64, typ uint8, off uint64) error {
	leaf := make([]byte, fs.sb.nodeSize)
	if _, err := fs.rwa.ReadAt(leaf, phys); err != nil {
		return fmt.Errorf("read leaf: %w", err)
	}
	idx := findItemIdx(leaf, objID, typ, off)
	if idx < 0 {
		return fmt.Errorf("item (%d,%#x,%d) not in leaf", objID, typ, off)
	}
	leafDeleteItem(leaf, idx)
	updateNodeCRC(leaf)
	if _, err := fs.rwa.WriteAt(leaf, phys); err != nil {
		return fmt.Errorf("write leaf: %w", err)
	}
	return nil
}

// deleteSysChunkArrayEntryLocked removes the sys_chunk_array entry keyed by
// logStart from the primary superblock, compacting the array and shrinking
// sys_chunk_array_size. A missing entry is not an error. Caller holds fs.mu.
func (fs *btrfsFS) deleteSysChunkArrayEntryLocked(logStart uint64) error {
	buf := make([]byte, sbfSize)
	if _, err := fs.rwa.ReadAt(buf, fs.partOffset+superblockOffset); err != nil {
		return fmt.Errorf("read superblock: %w", err)
	}
	le := binary.LittleEndian
	arrSz := int(le.Uint32(buf[sbfSysChunkArrSz:]))
	if arrSz <= 0 || sbfSysChunkArr+arrSz > len(buf) {
		return nil
	}
	arr := buf[sbfSysChunkArr : sbfSysChunkArr+arrSz]
	off := 0
	for off+keySize+chunkHeaderSize+chunkStripeSize <= len(arr) {
		keyType := arr[off+8]
		keyOff := le.Uint64(arr[off+9:])
		numStripes := le.Uint16(arr[off+keySize+chunkNumStripes:])
		entryLen := keySize + chunkHeaderSize + int(numStripes)*chunkStripeSize
		if off+entryLen > len(arr) {
			break
		}
		if keyType == typeChunkItem && keyOff == logStart {
			// Compact: shift the remaining entries left over this one and shrink
			// the recorded array size.
			copy(arr[off:], arr[off+entryLen:])
			newArrSz := arrSz - entryLen
			le.PutUint32(buf[sbfSysChunkArrSz:], uint32(newArrSz))
			// Zero the vacated tail so no stale entry bytes linger.
			clear(buf[sbfSysChunkArr+newArrSz : sbfSysChunkArr+arrSz])
			updateSuperblockCRC(buf)
			if _, err := fs.rwa.WriteAt(buf, fs.partOffset+superblockOffset); err != nil {
				return fmt.Errorf("write superblock: %w", err)
			}
			return nil
		}
		off += entryLen
	}
	return nil // not in sys_chunk_array
}

// blockGroupUsedLocked returns the `used` byte count recorded in the
// BLOCK_GROUP_ITEM for the chunk at logStart (size = its length), or (0, false)
// when no extent tree / block group is present. Caller holds fs.mu.
func (fs *btrfsFS) blockGroupUsedLocked(logStart, size uint64) (uint64, bool) {
	extRoot, err := extentTreeRoot(fs.rwa, fs.partOffset, fs.sb)
	if err != nil {
		return 0, false
	}
	leaf, _, err := fs.findExtentLeafWithKey(extRoot, logStart, typeBlockGroupItem, size)
	if err != nil || leaf == nil {
		return 0, false
	}
	idx := findItemIdx(leaf, logStart, typeBlockGroupItem, size)
	if idx < 0 {
		return 0, false
	}
	items := parseLeafItems(leaf, parseNodeHeader(leaf).nItems)
	d := items[idx].data(leaf)
	if len(d) < 8 {
		return 0, false
	}
	return binary.LittleEndian.Uint64(d[0:]), true // used is the first u64
}

// chunkIsEvacuableLocked reports whether the chunk at sysChunks[idx] can be
// removed wholesale: its logical range carries no live data extent or metadata
// block, AND (when an extent tree exists) its block group records used == 0.
// Caller holds fs.mu.
func (fs *btrfsFS) chunkIsEvacuableLocked(idx int) bool {
	c := fs.sb.sysChunks[idx]
	// No live data extent may overlap the chunk's physical range.
	if len(fs.collectRelocTargets(c.physStart, c.physStart+c.size)) > 0 {
		return false
	}
	// No live metadata block may sit inside the chunk.
	if _, found := fs.liveMetaInRange(c.physStart, c.physStart+c.size); found {
		return false
	}
	// If the extent tree records a non-zero `used` for this block group, refuse
	// (defensive: the live-tree scan above should already have caught it).
	if used, ok := fs.blockGroupUsedLocked(c.logStart, c.size); ok && used != 0 {
		return false
	}
	return true
}
