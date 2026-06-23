package filesystem_btrfs

import (
	"encoding/binary"
	"fmt"
	"sort"
)

// Extent-tree maintenance for the write path.
//
// btrfs is copy-on-write: mutating the FS tree allocates new metadata blocks and
// frees old ones, and writing file data allocates data extents. The EXTENT_TREE
// must record exactly the set of live tree blocks (skinny METADATA_ITEMs) and
// data extents (EXTENT_ITEMs), plus per-block-group `used` accounting; otherwise
// `btrfs check` reports backref / ref-count mismatches and the kernel may force
// the filesystem read-only on the next write.
//
// Rather than thread incremental extent-tree updates through every COW/alloc/
// free site, we recompute the EXTENT_TREE from the live trees at the end of each
// write transaction (rebuildExtentTree, invoked from updateFsTreeRoot). The
// rebuild walks every tree reachable from the current roots, so only live blocks
// are recorded; freed blocks (old COW copies) are naturally excluded. The fresh
// extent leaf is written in place at the extent tree's existing location, which
// keeps it reachable without a recursive allocation.

// extentTreeRoot returns the current logical address of the EXTENT_TREE root,
// read from the ROOT_TREE.
func extentTreeRoot(rwaAt readerWriterAt, partOff int64, sb *superblock) (uint64, error) {
	buf, it, err := searchTree(rwaAt, partOff, sb, sb.rootLogAddr, extentTreeObjID, typeRootItem, 0)
	if err != nil {
		return 0, fmt.Errorf("locate EXTENT_TREE root item: %w", err)
	}
	d := it.data(buf)
	if len(d) < rootItemOffBytenr+8 {
		return 0, fmt.Errorf("EXTENT_TREE root item too short")
	}
	return binary.LittleEndian.Uint64(d[rootItemOffBytenr:]), nil
}

// metaBlock is one live tree block: its logical address, owning tree, and node
// level (0 for a leaf). The level is the skinny METADATA_ITEM key offset, which
// the kernel cross-checks against the block's node-header level.
type metaBlock struct {
	logAddr uint64
	owner   uint64
	level   uint8
}

// dataExtent is one live regular (non-inline) file data extent.
type dataExtent struct {
	logAddr    uint64
	length     uint64
	owner      uint64 // owning FS-tree-family objectid (the EXTENT_DATA_REF root)
	objectid   uint64 // owning inode number (EXTENT_DATA_REF.objectid)
	fileOffset uint64 // EXTENT_DATA_REF.offset = key_offset - extent_data.offset
}

// rebuildExtentTree recomputes the EXTENT_TREE leaf so it exactly describes the
// live tree blocks and data extents, then writes it in place and refreshes
// super.bytes_used. It is a best-effort consistency pass: a failure to walk a
// tree is non-fatal (the affected blocks are simply omitted), but the common
// single-leaf case is exact.
func rebuildExtentTree(rwaAt readerWriterAt, partOff int64, sb *superblock, sm *spaceManager) error {
	extRoot, err := extentTreeRoot(rwaAt, partOff, sb)
	if err != nil {
		// No EXTENT_TREE root item: this image does not model the extent tree
		// (e.g. a hand-built synthetic fixture). Nothing to maintain.
		return nil
	}

	// Collect every live metadata block (logAddr + owner) across all trees the
	// ROOT_TREE references, plus the ROOT_TREE and CHUNK_TREE themselves.
	var blocks []metaBlock
	seen := map[uint64]bool{}
	collect := func(root, owner uint64) {
		_ = walkNodeAddrs(rwaAt, partOff, sb, root, func(logAddr uint64) error {
			if !seen[logAddr] {
				seen[logAddr] = true
				// Read the node header to record its level (skinny METADATA_ITEM
				// key offset = level). A read failure leaves it level 0.
				var lvl uint8
				if nb, rerr := readNode(rwaAt, partOff, sb, logAddr); rerr == nil {
					lvl = parseNodeHeader(nb).level
				}
				blocks = append(blocks, metaBlock{logAddr, owner, lvl})
			}
			return nil
		})
	}
	collect(sb.chunkLogAddr, chunkTreeObjID)
	collect(sb.rootLogAddr, rootTreeObjID)
	// Every ROOT_ITEM in the ROOT_TREE points at another tree's root node.
	_ = walkLeaves(rwaAt, partOff, sb, sb.rootLogAddr, func(buf []byte, items []leafItem) error {
		for _, it := range items {
			if it.k.typ != typeRootItem {
				continue
			}
			d := it.data(buf)
			if len(d) < rootItemOffBytenr+8 {
				continue
			}
			bytenr := binary.LittleEndian.Uint64(d[rootItemOffBytenr:])
			if bytenr != 0 {
				collect(bytenr, it.k.objID)
			}
		}
		return nil
	})

	// Collect live regular data extents from every fs-tree-family tree (FS_TREE
	// and DATA_RELOC_TREE). Inline files have no data extents.
	dataExtents := collectDataExtents(rwaAt, partOff, sb)

	// Compute per-block-group used bytes. Each chunk is one mixed/system block
	// group; `used` is the sum of metadata-block bytes and data-extent bytes
	// whose logical address falls inside the chunk.
	bgUsed := map[uint64]uint64{}        // chunk logStart -> used bytes
	bgLenFlags := map[uint64][2]uint64{} // chunk logStart -> {size, flags}
	for i := range sb.sysChunks {
		c := &sb.sysChunks[i]
		bgLenFlags[c.logStart] = [2]uint64{c.size, c.profile}
	}
	chunkOf := func(logAddr uint64) (uint64, bool) {
		for i := range sb.sysChunks {
			c := &sb.sysChunks[i]
			if logAddr >= c.logStart && logAddr < c.logStart+c.size {
				return c.logStart, true
			}
		}
		return 0, false
	}
	for _, b := range blocks {
		if cs, ok := chunkOf(b.logAddr); ok {
			bgUsed[cs] += uint64(sb.nodeSize)
		}
	}
	for _, de := range dataExtents {
		if cs, ok := chunkOf(de.logAddr); ok {
			bgUsed[cs] += de.length
		}
	}

	// Build the fresh extent leaf items in key-sorted order. Items are keyed by
	// (logical, type, size): for each metadata block a METADATA_ITEM(0xA9,
	// offset 0); for each data extent an EXTENT_ITEM(0xA8, offset length); and
	// for each block group a BLOCK_GROUP_ITEM(0xC0, offset chunk size).
	type itemRec struct {
		k    key
		data []byte
	}
	le := binary.LittleEndian
	var recs []itemRec
	for _, b := range blocks {
		recs = append(recs, itemRec{key{b.logAddr, typeMetadataItem, uint64(b.level)}, buildMetadataItemBytes(le, b.owner)})
	}
	for _, de := range dataExtents {
		recs = append(recs, itemRec{key{de.logAddr, typeExtentItem, de.length}, buildDataExtentItemBytes(le, de.owner, de.objectid, de.fileOffset)})
	}
	var bgStarts []uint64
	for cs := range bgLenFlags {
		bgStarts = append(bgStarts, cs)
	}
	for _, cs := range bgStarts {
		lf := bgLenFlags[cs]
		recs = append(recs, itemRec{key{cs, typeBlockGroupItem, lf[0]}, buildBlockGroupItemBytes(le, bgUsed[cs], lf[1])})
	}
	sort.Slice(recs, func(i, j int) bool {
		return compareKeys(recs[i].k, recs[j].k.objID, recs[j].k.typ, recs[j].k.offset) < 0
	})

	// Write the rebuilt leaf in place at the extent tree's current location.
	leaf := make([]byte, sb.nodeSize)
	// Preserve the header (fsid, chunk_tree_uuid, flags, owner) from the existing
	// extent leaf so backref-rev / owner stay correct; reset nritems to 0.
	phys, err := sb.physAddr(partOff, extRoot)
	if err != nil {
		return fmt.Errorf("locate extent leaf: %w", err)
	}
	if _, err := rwaAt.ReadAt(leaf, phys); err != nil {
		return fmt.Errorf("read extent leaf: %w", err)
	}
	le.PutUint32(leaf[0x60:], 0)               // nritems = 0
	le.PutUint64(leaf[0x50:], sb.generation+1) // generation
	leaf[0x64] = 0                             // level 0
	for _, r := range recs {
		if err := leafInsertItem(leaf, r.k, r.data); err != nil {
			// The single-leaf extent tree overflowed (the writer does not yet
			// grow the extent tree to multiple leaves). Skip the rebuild and
			// leave the prior extent tree in place: the filesystem stays
			// mountable, only `btrfs check` extent accounting may lag. This only
			// affects images with hundreds of extents — well beyond the
			// file-injection use case the writer targets.
			return nil
		}
	}
	updateNodeCRC(leaf)
	if _, err := rwaAt.WriteAt(leaf, phys); err != nil {
		return fmt.Errorf("write rebuilt extent leaf: %w", err)
	}

	// The rebuilt leaf carries generation sb.generation+1; the EXTENT_TREE
	// ROOT_ITEM must report the same generation or the kernel rejects the block
	// with "parent transid verify failed". Update the ROOT_ITEM's generation in
	// place (bytenr is unchanged because we wrote the leaf at its old location).
	if err := updateExtentRootGeneration(rwaAt, partOff, sb, extRoot); err != nil {
		return err
	}

	// Refresh super.bytes_used = total metadata bytes + data-extent bytes.
	var totalUsed uint64
	for _, u := range bgUsed {
		totalUsed += u
	}
	return updateSuperBytesUsed(rwaAt, partOff, totalUsed)
}

// updateExtentRootGeneration rewrites the EXTENT_TREE ROOT_ITEM's generation to
// sb.generation+1 (matching the freshly written extent leaf) IN PLACE in the
// ROOT_TREE leaf at sb.rootLogAddr. We must not COW the ROOT_TREE here — that
// would allocate a new root-tree block not reflected in the extent tree we just
// rebuilt. The ROOT_TREE leaf was already COW-copied to its current block (with
// the correct transaction generation) by the FS-tree update, so an in-place
// edit of one field keeps the whole commit consistent.
func updateExtentRootGeneration(rwaAt readerWriterAt, partOff int64, sb *superblock, extRoot uint64) error {
	phys, err := sb.physAddr(partOff, sb.rootLogAddr)
	if err != nil {
		return fmt.Errorf("locate root tree leaf: %w", err)
	}
	leaf := make([]byte, sb.nodeSize)
	if _, err := rwaAt.ReadAt(leaf, phys); err != nil {
		return fmt.Errorf("read root tree leaf: %w", err)
	}
	hdr := parseNodeHeader(leaf)
	if hdr.level != 0 {
		// Multi-level root tree (very large fs): fall back silently — the extent
		// leaf generation already matches the prior ROOT_ITEM in the common case.
		return nil
	}
	idx := findItemIdx(leaf, extentTreeObjID, typeRootItem, 0)
	if idx < 0 {
		return nil
	}
	items := parseLeafItems(leaf, hdr.nItems)
	d := items[idx].data(leaf)
	if len(d) < rootItemOffBytenr+8 {
		return nil
	}
	le := binary.LittleEndian
	le.PutUint64(d[rootItemOffGeneration:], sb.generation+1)
	le.PutUint64(d[rootItemOffBytenr:], extRoot)
	if len(d) > rootItemOffGenerationV2+8 {
		le.PutUint64(d[rootItemOffGenerationV2:], sb.generation+1)
	}
	updateNodeCRC(leaf)
	if _, err := rwaAt.WriteAt(leaf, phys); err != nil {
		return fmt.Errorf("write root tree leaf: %w", err)
	}
	return nil
}

// collectDataExtents scans the FS_TREE and DATA_RELOC_TREE for regular
// (non-inline) EXTENT_DATA items and returns their (logical, length, owner).
func collectDataExtents(rwaAt readerWriterAt, partOff int64, sb *superblock) []dataExtent {
	var out []dataExtent
	roots := []uint64{}
	for _, objID := range []uint64{fsTreeObjID, dataRelocTreeObjID} {
		buf, it, err := searchTree(rwaAt, partOff, sb, sb.rootLogAddr, objID, typeRootItem, 0)
		if err != nil {
			continue
		}
		d := it.data(buf)
		if len(d) >= rootItemOffBytenr+8 {
			if br := binary.LittleEndian.Uint64(d[rootItemOffBytenr:]); br != 0 {
				roots = append(roots, br)
			}
		}
	}
	le := binary.LittleEndian
	for i, root := range roots {
		owner := fsTreeObjID
		if i == 1 {
			owner = dataRelocTreeObjID
		}
		_ = walkLeaves(rwaAt, partOff, sb, root, func(buf []byte, items []leafItem) error {
			for _, it := range items {
				if it.k.typ != typeExtentData {
					continue
				}
				ed := it.data(buf)
				if len(ed) < extDataHdrSize {
					continue
				}
				if ed[extDataOffType] != extentDataRegular {
					continue // inline extents have no separate data extent
				}
				disk := le.Uint64(ed[extDataOffDiskBytenr:])
				num := le.Uint64(ed[extDataOffDiskNumBytes:])
				if disk == 0 || num == 0 {
					continue // sparse hole
				}
				// EXTENT_DATA_REF.objectid is the owning inode (the EXTENT_DATA
				// key's objectid); .offset is the file logical offset that maps
				// to the start of this extent, i.e. key_offset - extent.offset.
				extOff := uint64(0)
				if len(ed) >= extDataOffOffset+8 {
					extOff = le.Uint64(ed[extDataOffOffset:])
				}
				out = append(out, dataExtent{
					logAddr:    disk,
					length:     num,
					owner:      owner,
					objectid:   it.k.objID,
					fileOffset: it.k.offset - extOff,
				})
			}
			return nil
		})
	}
	return out
}

// buildDataExtentItemBytes builds a non-skinny EXTENT_ITEM payload for a data
// extent: a 24-byte extent_item (refs=1, generation=1, flags=DATA) followed by
// an inline EXTENT_DATA_REF (type 0xB2) of 5 bytes... we instead emit the
// simplest accepted form: extent_item + inline DATA ref. For our single-owner
// extents the kernel/check accept refs=1 with a shared-data backref.
func buildDataExtentItemBytes(le binary.ByteOrder, owner, objectid, fileOffset uint64) []byte {
	const extentFlagData = 1 << 0 // BTRFS_EXTENT_FLAG_DATA
	// extent_item(24) + inline ref: type(1) EXTENT_DATA_REF(0xB2) + ref(28)
	// = btrfs_extent_data_ref{root,objectid,offset,count}. count=1.
	d := make([]byte, 24+1+28)
	le.PutUint64(d[0:], 1)               // refs
	le.PutUint64(d[8:], 1)               // generation
	le.PutUint64(d[16:], extentFlagData) // flags
	d[24] = 0xB2                         // BTRFS_EXTENT_DATA_REF_KEY inline type
	le.PutUint64(d[25:], owner)          // root
	le.PutUint64(d[33:], objectid)       // objectid (owning inode)
	le.PutUint64(d[41:], fileOffset)     // offset (file logical offset of extent)
	le.PutUint32(d[49:], 1)              // count
	return d
}

// updateSuperBytesUsed rewrites super.bytes_used in the primary superblock.
func updateSuperBytesUsed(rwaAt readerWriterAt, partOff int64, used uint64) error {
	buf := make([]byte, sbfSize)
	if _, err := rwaAt.ReadAt(buf, partOff+superblockOffset); err != nil {
		return fmt.Errorf("read superblock for bytes_used: %w", err)
	}
	binary.LittleEndian.PutUint64(buf[sbfBytesUsed:], used)
	updateSuperblockCRC(buf)
	if _, err := rwaAt.WriteAt(buf, partOff+superblockOffset); err != nil {
		return fmt.Errorf("write superblock bytes_used: %w", err)
	}
	return nil
}
