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
//   - METADATA tree blocks (B-tree nodes) sitting in the tail are relocated two
//     ways. FS_TREE nodes are reseated implicitly: every COW mutation allocates
//     its replacement node from the lowest free block, with the tail evicted
//     first so nothing new lands there. The OTHER trees (EXTENT / DEV / CSUM /
//     UUID / ROOT / DATA_RELOC), which data relocation does not touch, are
//     COW-relocated explicitly by relocateTailMetadata — single-leaf roots AND
//     multi-level trees: relocateSubtreePath rewrites each root→leaf path that
//     reaches the tail bottom-up (move the deepest tail node, repoint the
//     parent's child key-ptr blockptr + generation, propagate the gen bump up to
//     the ROOT_ITEM), then repoints the ROOT_ITEM (or superblock for the root
//     tree) and lets rebuildExtentTree re-account. Validated against the real
//     kernel (TestRelocMeta_*).
//
// A MULTI-LEVEL ROOT_TREE whose ROOT_ITEM leaf sits in the tail is now handled:
// relocateSubtreePath COW-moves that leaf (and its interior parents) low and the
// in-place ROOT_ITEM editors descend the moved tree via tracePath. A MULTI-LEVEL
// EXTENT_TREE is also handled: it is not relocated block-by-block but rebuilt low
// and multi-level by buildMultiLevelExtentTree in the finalize (a fixpoint over
// the extent tree's own self-referential block records), so any extent-tree node
// in the tail is evacuated automatically.
//
// The boundary (what we still refuse, with a descriptive error and the image left
// consistent): the chunk tree itself (SYSTEM chunk, never in the DATA tail).
// Whole-chunk removal — including relocating a NON-empty chunk's contents into a
// lower chunk (cross-chunk content relocation) — lives in resize_chunk.go.

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
	return fs.liveMetaInRangeOpt(dropStart, dropEnd, false)
}

// liveMetaInRangeOpt is liveMetaInRange with an option to skip the EXTENT_TREE's
// own blocks. The shrink-relocation post-condition skips them because the extent
// tree is always rebuilt low by the finalize (rebuildExtentTree), which evacuates
// any extent-tree node that still sits in the tail at relocation time; the final
// (post-finalize) assertions check WITHOUT the skip. Caller holds fs.mu.
func (fs *btrfsFS) liveMetaInRangeOpt(dropStart, dropEnd uint64, skipExtentTree bool) (uint64, bool) {
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
			if skipExtentTree && it.k.objID == extentTreeObjID {
				continue // evacuated by the finalize rebuild
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

	// The data COW above reseated FS_TREE nodes below the new size; but
	// pre-existing metadata blocks of OTHER trees (extent/dev/csum/uuid/root/
	// data-reloc) that already sat in the tail are not moved by data relocation.
	// COW-relocate those tree blocks too (allocate a low block, copy + rewrite
	// the node header, repoint the ROOT_ITEM / superblock, bump generation), so
	// nothing live remains in the tail.
	if err := fs.relocateTailMetadata(dropStart, dropEnd); err != nil {
		return err
	}

	// Post-condition: the tail must now be free of every live metadata block,
	// EXCEPT the EXTENT_TREE's own nodes — those are evacuated low by the finalize
	// rebuild (updateFsTreeRoot → rebuildExtentTree → buildMultiLevelExtentTree),
	// which always reconstructs the extent tree from the lowest free blocks.
	// Anything else left is a block this writer cannot relocate (the chunk tree
	// itself); refuse rather than truncate it.
	if hit, found := fs.liveMetaInRangeOpt(dropStart, dropEnd, true); found {
		return fmt.Errorf("live metadata block at logical 0x%X remains in [%d, %d) after relocation (metadata-block relocation not supported for this block)",
			hit, dropStart, dropEnd)
	}
	return nil
}

// relocateTailMetadata COW-relocates every live tree block that overlaps
// [dropStart, dropEnd) out of the tail — the EXTENT / DEV / CSUM / UUID / ROOT /
// DATA_RELOC trees, single-leaf OR multi-level. The FS_TREE is handled by data
// relocation's COW path and the CHUNK_TREE lives in the SYSTEM chunk (never in
// the DATA tail), so neither is touched here.
//
// Each tree with any descendant in the tail is rewritten bottom-up via
// relocateSubtreePath: the deepest tail node is moved to a fresh low block
// first, the parent's child key-pointer (blockptr + generation) is repointed,
// and that mutation forces the parent to be rewritten too — propagating up the
// whole path to the root, exactly as the kernel's btrfs_cow_block does. Every
// node touched along the path is written at the committing transaction's
// generation (sb.generation+1) so child header.generation, parent key-ptr
// generation and the ROOT_ITEM generation all stay mutually equal (the kernel's
// parent-transid check). The referencing pointer is then fixed:
//
//   - ROOT_TREE root: fs.sb.rootLogAddr is updated (the superblock's
//     sbfRootLogAddr is persisted later by the finalize) and every subsequent
//     in-place ROOT_ITEM edit targets the new root-tree node.
//   - any other tree: its ROOT_ITEM.bytenr (+ .generation/.generation_v2) in the
//     ROOT_TREE leaf is rewritten in place by repointRootItemLocked.
//
// The EXTENT_TREE accounting (METADATA_ITEM addresses, block-group `used`) is
// recomputed afterwards by rebuildExtentTree (run from updateFsTreeRoot in the
// Shrink finalize) — it walks interior nodes too and, when the records overflow a
// single leaf, emits a multi-level extent tree from the lowest free blocks
// (buildMultiLevelExtentTree). So the EXTENT_TREE is not relocated here at all:
// its tail nodes are evacuated by being rebuilt low. The ROOT_TREE, including a
// multi-level root tree whose ROOT_ITEM leaf lands in the tail, IS relocated here.
// The chunk tree is never relocated (SYSTEM chunk, outside the DATA tail). Caller
// holds fs.mu.
func (fs *btrfsFS) relocateTailMetadata(dropStart, dropEnd uint64) error {
	overlaps := func(logAddr uint64) bool {
		phys := physFromLog(fs.sb, logAddr)
		return phys < dropEnd && phys+uint64(fs.sb.nodeSize) > dropStart
	}
	// subtreeTouchesTail reports whether logAddr's tree has ANY node (root,
	// interior or leaf) physically in the tail.
	subtreeTouchesTail := func(root uint64) bool {
		hit := false
		_ = walkNodeAddrs(fs.rwa, fs.partOffset, fs.sb, root, func(a uint64) error {
			if overlaps(a) {
				hit = true
			}
			return nil
		})
		return hit
	}

	// Relocate the ROOT_TREE first so later ROOT_ITEM edits land in the moved
	// tree. relocateSubtreePath descends a multi-level root tree bottom-up, so a
	// ROOT_ITEM leaf sitting in the tail is COW-moved out and its interior parent
	// repointed; the superblock `root` pointer is then re-seated below. The
	// in-place ROOT_ITEM editors (repointRootItemLocked / updateExtentRootGeneration)
	// descend the moved tree via tracePath, so a multi-level root tree is handled
	// end to end.
	if subtreeTouchesTail(fs.sb.rootLogAddr) {
		newLog, _, err := fs.relocateSubtreePath(fs.sb.rootLogAddr, dropStart, dropEnd, 0)
		if err != nil {
			return fmt.Errorf("relocate root tree: %w", err)
		}
		fs.sb.rootLogAddr = newLog
		fs.invalidateCache()
	}

	// Relocate every other tree referenced by a ROOT_ITEM whose tree has a node
	// in the tail. Snapshot the (objID -> bytenr) set from the (possibly moved)
	// root-tree leaf, then relocate and repoint each in turn.
	type rootRef struct {
		objID  uint64
		bytenr uint64
	}
	var refs []rootRef
	if err := walkLeaves(fs.rwa, fs.partOffset, fs.sb, fs.sb.rootLogAddr, func(buf []byte, items []leafItem) error {
		for _, it := range items {
			if it.k.typ != typeRootItem {
				continue
			}
			// FS_TREE is reseated by the data-COW path; its ROOT_ITEM bytenr is
			// fixed by updateFsTreeRoot in the finalize. Skip it here.
			if it.k.objID == fsTreeObjID {
				continue
			}
			d := it.data(buf)
			if len(d) < rootItemOffBytenr+8 {
				continue
			}
			br := binary.LittleEndian.Uint64(d[rootItemOffBytenr:])
			if br != 0 && subtreeTouchesTail(br) {
				refs = append(refs, rootRef{it.k.objID, br})
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("scan root items: %w", err)
	}

	for _, r := range refs {
		// The EXTENT_TREE is NOT relocated here: the finalize's rebuild
		// (rebuildExtentTree → buildMultiLevelExtentTree) reconstructs it from the
		// lowest free blocks, which evacuates any extent-tree node — single-leaf or
		// multi-level — out of the tail. Relocating it here as well would be
		// redundant and circular (its own node allocations are themselves extent
		// records). Skip it; the post-condition check ignores extent-tree blocks
		// for the same reason.
		if r.objID == extentTreeObjID {
			continue
		}
		newLog, _, err := fs.relocateSubtreePath(r.bytenr, dropStart, dropEnd, 0)
		if err != nil {
			return fmt.Errorf("relocate tree %d: %w", r.objID, err)
		}
		if err := fs.repointRootItemLocked(r.objID, newLog); err != nil {
			return fmt.Errorf("relocate tree %d: repoint ROOT_ITEM: %w", r.objID, err)
		}
	}
	return nil
}

// relocateSubtreePath COW-relocates, bottom-up, every node on each root→leaf
// path of the (sub)tree rooted at logAddr that has at least one node physically
// in [dropStart, dropEnd). It returns the (possibly new) logical address of this
// node and whether it changed.
//
// For an interior node it first recurses into every child; if a child moved, the
// child's key-pointer (blockptr at +17, generation at +25) is rewritten in this
// node's buffer to the new bytenr / committing generation. This node is then
// itself rewritten to a fresh low block (via relocateNodeBlock) whenever it
// changed OR it overlaps the tail — so the entire path from any relocated node
// up to the root lands at sb.generation+1, keeping every parent key-ptr
// generation equal to the child header generation it points at (the kernel's
// parent-transid invariant). Nodes that neither changed nor overlap are left
// untouched (returned verbatim).
//
// depth/guard mirror walkNodeAddrs so a corrupt/looping tree cannot recurse
// without bound. Caller holds fs.mu.
func (fs *btrfsFS) relocateSubtreePath(logAddr, dropStart, dropEnd uint64, depth int) (uint64, bool, error) {
	if depth > maxBtreeDepth {
		return 0, false, fmt.Errorf("relocate subtree: depth exceeds %d", maxBtreeDepth)
	}
	buf, err := readNode(fs.rwa, fs.partOffset, fs.sb, logAddr)
	if err != nil {
		return 0, false, fmt.Errorf("read node 0x%X: %w", logAddr, err)
	}
	// Copy out of any shared node-cache buffer: we may mutate child pointers.
	node := make([]byte, len(buf))
	copy(node, buf)

	hdr := parseNodeHeader(node)
	phys := physFromLog(fs.sb, logAddr)
	overlaps := phys < dropEnd && phys+uint64(fs.sb.nodeSize) > dropStart

	le := binary.LittleEndian
	changed := false
	if hdr.level > 0 {
		for i := uint32(0); i < hdr.nItems; i++ {
			off := nodeHdrSize + int(i)*keyPtrSize
			if off+keyPtrSize > len(node) {
				break
			}
			childLog := le.Uint64(node[off+17:])
			if childLog == 0 {
				continue
			}
			newChild, childChanged, err := fs.relocateSubtreePath(childLog, dropStart, dropEnd, depth+1)
			if err != nil {
				return 0, false, err
			}
			if childChanged {
				le.PutUint64(node[off+17:], newChild)           // child blockptr
				le.PutUint64(node[off+25:], fs.sb.generation+1) // child key-ptr generation
				changed = true
			}
		}
	}

	if !changed && !overlaps {
		return logAddr, false, nil
	}

	// This node must land at a fresh low block at the committing generation,
	// because either it sits in the tail or a child pointer it carries moved (so
	// its content and required generation changed).
	newLog, err := fs.relocateNodeBuffer(node, dropStart)
	if err != nil {
		return 0, false, err
	}
	return newLog, true, nil
}

// relocateNodeBuffer writes the (already-mutated) node buffer to a freshly
// allocated metadata block strictly below limit, rewriting the copy's header
// bytenr (0x30) and generation (0x50 = committing transaction) and refreshing the
// CRC, then returns the new logical address. Works for leaves and interior
// nodes alike. The old physical block is inside the tail being removed (already
// evicted from the free list), so it is not returned to the allocator. Caller
// holds fs.mu.
func (fs *btrfsFS) relocateNodeBuffer(node []byte, limit uint64) (uint64, error) {
	newPhys, err := fs.sm.allocNodeBlock()
	if err != nil {
		return 0, fmt.Errorf("alloc replacement block: %w", err)
	}
	if newPhys+uint64(fs.sb.nodeSize) > limit {
		fs.sm.freeRange(newPhys, uint64(fs.sb.nodeSize))
		return 0, fmt.Errorf("no free metadata block below new size %d", limit)
	}
	newLog := physToLog(fs.sb, newPhys)
	le := binary.LittleEndian
	le.PutUint64(node[0x30:], newLog)             // header.bytenr
	le.PutUint64(node[0x50:], fs.sb.generation+1) // header.generation (committing txn)
	updateNodeCRC(node)
	if _, err := fs.rwa.WriteAt(node, fs.partOffset+int64(newPhys)); err != nil {
		fs.sm.freeRange(newPhys, uint64(fs.sb.nodeSize))
		return 0, fmt.Errorf("write relocated block at 0x%X: %w", newPhys, err)
	}
	return newLog, nil
}

// relocateLeafBlock copies the single tree-block at oldLog to a freshly
// allocated node block strictly below limit (via relocateNodeBuffer), returning
// the new logical address. The old physical block is inside the tail being
// removed (already evicted from the free list), so it is not returned to the
// allocator. A thin wrapper over relocateNodeBuffer for a verbatim move (no
// child-pointer rewrite); retained for the single-block relocation paths and
// their fault-injection tests. Caller holds fs.mu.
func (fs *btrfsFS) relocateLeafBlock(oldLog, limit uint64) (uint64, error) {
	oldPhys, err := fs.sb.physAddr(fs.partOffset, oldLog)
	if err != nil {
		return 0, fmt.Errorf("locate block 0x%X: %w", oldLog, err)
	}
	buf := make([]byte, fs.sb.nodeSize)
	if _, err := fs.rwa.ReadAt(buf, oldPhys); err != nil {
		return 0, fmt.Errorf("read block 0x%X: %w", oldLog, err)
	}
	return fs.relocateNodeBuffer(buf, limit)
}

// rootItemLeafLog returns the logical address of the ROOT_TREE leaf that holds
// the ROOT_ITEM (objID, ROOT_ITEM, 0). For a single-leaf root tree this is just
// fs.sb.rootLogAddr; for a multi-level root tree it descends via tracePath to the
// leaf where that key lives. Caller holds fs.mu.
func (fs *btrfsFS) rootItemLeafLog(objID uint64) (uint64, error) {
	return rootItemLeafLogAddr(fs.rwa, fs.partOffset, fs.sb, objID)
}

// repointRootItemLocked rewrites the ROOT_ITEM (objID, ROOT_ITEM, 0) so its
// bytenr points at newBytenr and its generation / generation_v2 match the
// committing transaction. Edited in place in the (possibly relocated) ROOT_TREE
// leaf that carries it — located via rootItemLeafLog so it works for a
// single-leaf AND a multi-level root tree. An in-place edit leaves the leaf at
// its current logical address, so every parent key-ptr (blockptr + generation)
// stays valid: the leaf header generation was already bumped when the subtree was
// relocated, or will be bumped by the finalize when it was not moved. Caller
// holds fs.mu.
func (fs *btrfsFS) repointRootItemLocked(objID, newBytenr uint64) error {
	leafLog, err := fs.rootItemLeafLog(objID)
	if err != nil {
		return err
	}
	phys, err := fs.sb.physAddr(fs.partOffset, leafLog)
	if err != nil {
		return fmt.Errorf("locate root tree leaf: %w", err)
	}
	leaf := make([]byte, fs.sb.nodeSize)
	if _, err := fs.rwa.ReadAt(leaf, phys); err != nil {
		return fmt.Errorf("read root tree leaf: %w", err)
	}
	if parseNodeHeader(leaf).level != 0 {
		return fmt.Errorf("ROOT_ITEM %d leaf 0x%X is not a leaf", objID, leafLog)
	}
	idx := findItemIdx(leaf, objID, typeRootItem, 0)
	if idx < 0 {
		return fmt.Errorf("ROOT_ITEM %d not found", objID)
	}
	items := parseLeafItems(leaf, parseNodeHeader(leaf).nItems)
	d := items[idx].data(leaf)
	if len(d) < rootItemOffBytenr+8 {
		return fmt.Errorf("ROOT_ITEM %d too short", objID)
	}
	le := binary.LittleEndian
	le.PutUint64(d[rootItemOffBytenr:], newBytenr)
	le.PutUint64(d[rootItemOffGeneration:], fs.sb.generation+1)
	if len(d) > rootItemOffGenerationV2+8 {
		le.PutUint64(d[rootItemOffGenerationV2:], fs.sb.generation+1)
	}
	updateNodeCRC(leaf)
	if _, err := fs.rwa.WriteAt(leaf, phys); err != nil {
		return fmt.Errorf("write root tree leaf: %w", err)
	}
	fs.invalidateCache()
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
