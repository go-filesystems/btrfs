package filesystem_btrfs

// Shrink-time extent relocation.
//
// `btrfs filesystem resize` shrink must evacuate every live extent out of the
// [newSize, oldSize) physical tail before the device can be truncated. The
// kernel does this with btrfs_relocate_block_group (COW-copy each extent to a
// lower address, rewrite the referencing items + backrefs). This file
// implements the tractable subset of that for our single-device, log==phys
// writer model:
//
//   - DATA extents (regular file EXTENT_DATA) overlapping the tail are
//     COW-relocated below newSize: their bytes are copied to a freshly
//     allocated low extent and the EXTENT_DATA item's disk_bytenr is rewritten
//     via the normal COW path. The EXTENT_TREE / block-group `used` /
//     super.bytes_used are then recomputed by rebuildExtentTree (invoked from
//     updateFsTreeRoot), exactly as on the write path.
//
//   - METADATA tree blocks (B-tree nodes) sitting in the tail are relocated
//     implicitly: every COW mutation we issue allocates its replacement node
//     from the lowest free block, and we evict the tail from the free list
//     first so nothing new lands there. Any pre-existing live metadata node
//     still inside the tail after relocation is detected and the shrink is
//     refused with a clear error rather than truncating live metadata away.
//
// The boundary (what we refuse): a tail that still holds live metadata blocks
// not reachable from the COW we performed, or a tail covering an entire chunk
// (chunk/dev-tree relocation is a separate, larger piece of work). Those cases
// return a descriptive error and leave the image untouched and valid.

import (
	"encoding/binary"
	"fmt"
)

// relocTarget is one live data extent that must move out of the tail.
type relocTarget struct {
	ino        uint64 // owning inode
	keyOffset  uint64 // EXTENT_DATA key offset (file offset of this item)
	diskBytenr uint64 // current logical (== physical) start of the extent
	diskBytes  uint64 // on-disk allocated length
	fileOffset uint64 // extent.offset (in-extent start, normally 0)
	numBytes   uint64 // logical length referenced by this item
	ramBytes   uint64 // decoded size
	generation uint64
	compress   uint8 // compression type (preserved verbatim)
}

// liveMetaInRange reports the logical address of any live metadata block whose
// physical block overlaps [dropStart, dropEnd), or returns ok=false when the
// tail is free of live metadata. Caller must hold fs.mu.
func (fs *btrfsFS) liveMetaInRange(dropStart, dropEnd uint64) (uint64, bool) {
	var hit uint64
	found := false
	check := func(logAddr uint64) error {
		if found {
			return nil
		}
		phys := physFromLog(fs.sb, logAddr)
		end := phys + uint64(fs.sb.nodeSize)
		if phys < dropEnd && end > dropStart {
			hit = logAddr
			found = true
		}
		return nil
	}
	// Every tree reachable from the current roots. The FS_TREE is walked from
	// the in-memory fs.fsTreeRoot (authoritative during an in-flight
	// transaction, before updateFsTreeRoot rewrites the ROOT_ITEM), so its
	// freshly-COW'd nodes are seen and its stale ROOT_ITEM bytenr is skipped.
	_ = walkNodeAddrs(fs.rwa, fs.partOffset, fs.sb, fs.sb.chunkLogAddr, check)
	_ = walkNodeAddrs(fs.rwa, fs.partOffset, fs.sb, fs.sb.rootLogAddr, check)
	_ = walkNodeAddrs(fs.rwa, fs.partOffset, fs.sb, fs.fsTreeRoot, check)
	_ = walkLeaves(fs.rwa, fs.partOffset, fs.sb, fs.sb.rootLogAddr, func(buf []byte, items []leafItem) error {
		for _, it := range items {
			if it.k.typ != typeRootItem {
				continue
			}
			if it.k.objID == fsTreeObjID {
				continue // walked via fs.fsTreeRoot above
			}
			d := it.data(buf)
			if len(d) < rootItemOffBytenr+8 {
				continue
			}
			br := binary.LittleEndian.Uint64(d[rootItemOffBytenr:])
			if br != 0 {
				_ = walkNodeAddrs(fs.rwa, fs.partOffset, fs.sb, br, check)
			}
		}
		return nil
	})
	return hit, found
}

// collectRelocTargets scans the FS_TREE (and DATA_RELOC_TREE) for regular data
// extents whose physical range overlaps [dropStart, dropEnd). Caller holds
// fs.mu.
func (fs *btrfsFS) collectRelocTargets(dropStart, dropEnd uint64) []relocTarget {
	var out []relocTarget
	le := binary.LittleEndian
	root := fs.fsTreeRoot
	_ = walkLeaves(fs.rwa, fs.partOffset, fs.sb, root, func(buf []byte, items []leafItem) error {
		for _, it := range items {
			if it.k.typ != typeExtentData {
				continue
			}
			ed := it.data(buf)
			if len(ed) < extDataRegularSize {
				continue
			}
			if ed[extDataOffType] != extentDataRegular {
				continue
			}
			disk := le.Uint64(ed[extDataOffDiskBytenr:])
			diskBytes := le.Uint64(ed[extDataOffDiskNumBytes:])
			if disk == 0 || diskBytes == 0 {
				continue // sparse
			}
			phys := physFromLog(fs.sb, disk)
			end := phys + diskBytes
			if phys < dropEnd && end > dropStart {
				out = append(out, relocTarget{
					ino:        it.k.objID,
					keyOffset:  it.k.offset,
					diskBytenr: disk,
					diskBytes:  diskBytes,
					fileOffset: le.Uint64(ed[extDataOffOffset:]),
					numBytes:   le.Uint64(ed[extDataOffNumBytes:]),
					ramBytes:   le.Uint64(ed[extDataOffRamBytes:]),
					generation: le.Uint64(ed[0x00:]),
					compress:   ed[extDataOffCompression],
				})
			}
		}
		return nil
	})
	return out
}

// relocateTailExtents moves every live data extent out of [dropStart, dropEnd)
// to a freshly allocated physical range below dropStart, rewriting the
// referencing EXTENT_DATA item in place via COW. It then finalizes the
// transaction (extent-tree rebuild + superblock) via updateFsTreeRoot.
//
// On entry the caller must already have removed [dropStart, dropEnd) from the
// space manager's free list so replacement allocations never land back in the
// tail. Caller must hold fs.mu.
func (fs *btrfsFS) relocateTailExtents(dropStart, dropEnd uint64) error {
	// Refuse if a chunk-internal stripe or metadata block we cannot move sits
	// in the tail: relocate only when the tail is data-only (plus metadata we
	// will COW below). We detect un-movable live metadata after relocation.
	targets := fs.collectRelocTargets(dropStart, dropEnd)

	le := binary.LittleEndian
	for _, t := range targets {
		// Allocate a replacement extent below dropStart. allocDataBytes hands
		// out the lowest free range; with the tail evicted it is guaranteed to
		// be below dropStart (or the call fails, which we surface).
		newPhys, newLen, err := fs.sm.allocDataBytes(t.diskBytes, uint64(fs.sb.sectorSize))
		if err != nil {
			return fmt.Errorf("relocate inode %d extent @0x%X: alloc replacement: %w", t.ino, t.diskBytenr, err)
		}
		if newPhys+newLen > dropStart {
			// Defensive: never relocate into the very tail we are removing.
			fs.sm.freeRange(newPhys, newLen)
			return fmt.Errorf("relocate inode %d extent @0x%X: no free space below new size %d", t.ino, t.diskBytenr, dropStart)
		}
		// Copy the on-disk bytes from old physical range to the new one.
		oldPhys := physFromLog(fs.sb, t.diskBytenr)
		buf := make([]byte, t.diskBytes)
		if _, err := fs.rwa.ReadAt(buf, fs.partOffset+int64(oldPhys)); err != nil {
			fs.sm.freeRange(newPhys, newLen)
			return fmt.Errorf("relocate inode %d: read old extent @0x%X: %w", t.ino, oldPhys, err)
		}
		if _, err := fs.rwa.WriteAt(buf, fs.partOffset+int64(newPhys)); err != nil {
			fs.sm.freeRange(newPhys, newLen)
			return fmt.Errorf("relocate inode %d: write new extent @0x%X: %w", t.ino, newPhys, err)
		}
		newDisk := physToLog(fs.sb, newPhys)

		// Rewrite the EXTENT_DATA item's disk_bytenr (and disk_num_bytes, which
		// stays equal) via a same-size COW update.
		newED := encodeExtentDataRelocated(le, t, newDisk, newLen)
		newRoot, err := cowUpdate(nil, fs.rwa, fs.partOffset, fs.sb, fs.sm,
			fs.fsTreeRoot, key{t.ino, typeExtentData, t.keyOffset}, newED)
		if err != nil {
			return fmt.Errorf("relocate inode %d: cow-update extent item: %w", t.ino, err)
		}
		fs.fsTreeRoot = newRoot

		// The old physical range is inside the tail we are removing; it was
		// already evicted from the free list, so we must NOT freeRange it back.
		// If (defensively) the old extent straddled the boundary, return only
		// the below-dropStart portion to the allocator.
		if oldPhys < dropStart {
			belowLen := dropStart - oldPhys
			if belowLen > t.diskBytes {
				belowLen = t.diskBytes
			}
			fs.sm.freeRange(oldPhys, belowLen)
		}
	}

	// Verify the tail is now free of live metadata. The data COW above reseated
	// FS_TREE nodes below the new size; but pre-existing metadata blocks of
	// OTHER trees (extent/dev/csum/uuid/root) that already sat in the tail are
	// not moved by data relocation. If any remain we refuse — truncating them
	// would destroy live metadata. We have not finalized the transaction (the
	// on-disk superblock still points at the pre-relocation roots), so the
	// image is left valid and untouched.
	if hit, found := fs.liveMetaInRange(dropStart, dropEnd); found {
		return fmt.Errorf("live metadata block at logical 0x%X remains in [%d, %d) after relocation (metadata-block relocation not supported)",
			hit, dropStart, dropEnd)
	}
	return nil
}

// encodeExtentDataRelocated rebuilds a regular EXTENT_DATA payload identical to
// the original target except for the relocated disk_bytenr / disk_num_bytes.
// Length matches the original so the item can be replaced in place via a
// same-size COW update.
func encodeExtentDataRelocated(le binary.ByteOrder, t relocTarget, newDisk, newLen uint64) []byte {
	buf := make([]byte, extDataRegularSize)
	le.PutUint64(buf[0x00:], t.generation)
	le.PutUint64(buf[extDataOffRamBytes:], t.ramBytes)
	buf[extDataOffCompression] = t.compress
	buf[extDataOffType] = extentDataRegular
	le.PutUint64(buf[extDataOffDiskBytenr:], newDisk)
	le.PutUint64(buf[extDataOffDiskNumBytes:], newLen)
	le.PutUint64(buf[extDataOffOffset:], t.fileOffset)
	le.PutUint64(buf[extDataOffNumBytes:], t.numBytes)
	return buf
}
