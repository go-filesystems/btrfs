package filesystem_btrfs

import (
	"encoding/binary"
	"fmt"
	"sort"
)

// Multi-level EXTENT_TREE construction.
//
// rebuildExtentTree recomputes the EXTENT_TREE from the live trees. For small
// filesystems the result fits in a single leaf, written in place. When it does
// not, the extent tree must grow: the records are partitioned across several
// leaves indexed by one or more interior nodes. This is the meta-circular case —
// the extent tree's OWN leaves and interior nodes are themselves live tree
// blocks that must appear in the extent tree as METADATA_ITEMs, and they also
// count toward each block group's `used`. buildMultiLevelExtentTree resolves this
// self-reference with a fixpoint: each iteration ACTUALLY allocates the assumed
// number of extent-tree blocks (from the space manager's lowest free blocks),
// materialises the records (their own METADATA_ITEMs at the real addresses, plus
// the resulting block-group `used`), partitions them into leaves + interior
// nodes, and re-derives the block count. If the derived count differs the
// allocations are freed and the loop retries; on convergence the partition's
// record set is, by construction, exactly the set of blocks allocated, so the
// written tree is self-consistent. This mirrors how the kernel finalises the
// extent tree last during relocation.
//
// Because every extent-tree block is allocated low, a shrink relocation (which
// has already evicted the removed tail from the free list) places the whole
// rebuilt extent tree below the new size.

// itemRec is one extent-tree record: its key and serialized payload.
type itemRec struct {
	k    key
	data []byte
}

// multiLevelExtentInput carries everything buildMultiLevelExtentTree needs that
// is independent of the extent tree's own (yet-unknown) block set.
type multiLevelExtentInput struct {
	// baseRecs are every non-extent-tree-owned, non-block-group record (other
	// trees' METADATA_ITEMs and the data EXTENT_ITEMs).
	baseRecs []itemRec
	// baseBGUsed is per-chunk `used` for everything except the extent tree's own
	// blocks; the extent-tree blocks' contribution is added during the fixpoint.
	baseBGUsed map[uint64]uint64
	// bgLenFlags maps chunk logStart -> {size, flags} for each block group.
	bgLenFlags map[uint64][2]uint64
	// chunkOf maps a logical address to its containing chunk's logStart.
	chunkOf func(uint64) (uint64, bool)
	// oldExtRoot is the extent tree's current single root block.
	oldExtRoot uint64
	// oldExtSafe is true when oldExtRoot is still a reachable, in-bounds block
	// (a normal write or a shrink that kept the extent root's chunk). Then it is
	// freed back to the allocator so its space can be reused low. When false (a
	// shrink dropped the chunk holding it) the block is already gone from the free
	// list and must NOT be freed back, which would re-add removed-tail space.
	oldExtSafe bool
	// hdrTemplate is an extent-tree node buffer used as a header template
	// (fsid, chunk_tree_uuid, owner); only the 0..nodeHdrSize bytes are read.
	hdrTemplate []byte
}

// leafCapacity is the usable item area of a leaf (node minus header).
func leafCapacity(sb *superblock) int { return int(sb.nodeSize) - nodeHdrSize }

// internalCapacity is the maximum number of key-ptrs an interior node holds.
func internalCapacity(sb *superblock) int { return (int(sb.nodeSize) - nodeHdrSize) / keyPtrSize }

// partitionLeaves greedily packs key-sorted records into leaves, returning the
// per-leaf record-index ranges [start,end). Each leaf holds as many records as
// fit in the usable item area (itemSize descriptor + payload per record).
func partitionLeaves(recs []itemRec, capBytes int) [][2]int {
	var ranges [][2]int
	i := 0
	for i < len(recs) {
		used := 0
		start := i
		for i < len(recs) {
			need := itemSize + len(recs[i].data)
			if used+need > capBytes && i > start {
				break
			}
			used += need
			i++
		}
		ranges = append(ranges, [2]int{start, i})
	}
	return ranges
}

// interiorShape returns, for nLeaves children, the per-level node counts from the
// bottom interior level up to (and including) the single root level, the total
// interior-node count, and the root level (1-based height above leaves).
func interiorShape(nLeaves, icap int) (levels []int, total int, rootLevel uint8) {
	children := nLeaves
	for children > 1 {
		nodes := (children + icap - 1) / icap
		levels = append(levels, nodes)
		total += nodes
		children = nodes
		rootLevel++
	}
	return levels, total, rootLevel
}

// extentLayout is one fully-resolved candidate tree shape: which blocks are
// leaves vs interior (with their allocated logical addresses), the sorted record
// set whose METADATA_ITEMs exactly cover those blocks, the leaf partition, and
// the per-chunk `used` accounting.
type extentLayout struct {
	leafLogs     []uint64
	interiorLogs [][]uint64
	levels       []int
	rootLevel    uint8
	recs         []itemRec
	ranges       [][2]int
	bgUsed       map[uint64]uint64
	allocated    []uint64 // every block allocated this attempt (for freeLayout)
}

// buildMultiLevelExtentTree materialises a multi-level EXTENT_TREE and repoints
// its ROOT_ITEM. Caller holds the fs lock (this runs inside updateFsTreeRoot).
func buildMultiLevelExtentTree(rwaAt readerWriterAt, partOff int64, sb *superblock, sm *spaceManager, in multiLevelExtentInput) error {
	nodeSize := int(sb.nodeSize)
	icap := internalCapacity(sb)
	if icap < 2 {
		return fmt.Errorf("btrfs: node too small for interior node")
	}

	// Free EVERY block of the old extent tree (root, interior nodes and leaves)
	// so the new tree can reuse that space — otherwise a previously multi-level
	// extent tree would leak all but its root block on each rebuild, exhausting
	// the allocator. Only do this when the old tree is still in-bounds: when a
	// shrink dropped the chunk holding it, those blocks are already outside the
	// free list and must not be re-added (that would hand back removed-tail space).
	if in.oldExtSafe {
		freed := map[uint64]bool{}
		_ = walkNodeAddrs(rwaAt, partOff, sb, in.oldExtRoot, func(a uint64) error {
			if !freed[a] {
				freed[a] = true
				sm.freeRange(physFromLog(sb, a), uint64(nodeSize))
			}
			return nil
		})
	}

	// Fixpoint on the block count, allocating real blocks each iteration so the
	// record set always references genuine addresses; free and retry when the
	// derived count differs from the assumed count.
	blockCount := 1
	var final *extentLayout
	for iter := 0; iter < 64; iter++ {
		lay, err := tryExtentLayout(sb, sm, in, blockCount)
		if err != nil {
			return err
		}
		// Converged when the leaf partition matches the allocated leaf slice (the
		// assumed shape held), i.e. every allocated block is referenced and every
		// referenced block is allocated.
		realLeaves := len(lay.ranges)
		if realLeaves == len(lay.leafLogs) {
			final = lay
			break
		}
		// Free this iteration's allocations and retry with the block count the real
		// leaf partition implies.
		freeLayout(sb, sm, lay)
		_, itot, _ := interiorShape(realLeaves, internalCapacity(sb))
		blockCount = realLeaves + itot
	}
	if final == nil {
		return fmt.Errorf("btrfs: multi-level extent tree sizing did not converge")
	}

	if err := writeMultiLevelTree(rwaAt, partOff, sb, in.hdrTemplate,
		final.recs, final.ranges, final.leafLogs, final.interiorLogs, final.levels); err != nil {
		return err
	}
	rootLog := interiorRootLog(final.leafLogs, final.interiorLogs)
	if err := repointExtentRoot(rwaAt, partOff, sb, rootLog, final.rootLevel); err != nil {
		return err
	}
	var totalUsed uint64
	for _, u := range final.bgUsed {
		totalUsed += u
	}
	return updateSuperBytesUsed(rwaAt, partOff, totalUsed)
}

// tryExtentLayout allocates `blockCount` node blocks (apportioned into leaves and
// interior nodes for that count's interior shape), builds the definitive record
// set referencing those exact addresses, partitions it, and returns the
// resolved layout. The caller compares the realised block count (which may
// differ when the leaf partition needs a different number of leaves than
// predicted from blockCount) against blockCount to decide convergence.
func tryExtentLayout(sb *superblock, sm *spaceManager, in multiLevelExtentInput, blockCount int) (*extentLayout, error) {
	le := binary.LittleEndian
	nodeSize := int(sb.nodeSize)
	lcap := leafCapacity(sb)
	icap := internalCapacity(sb)

	// For blockCount total blocks, the leaf count is blockCount minus the interior
	// nodes those leaves require. Solve the small relation L + interior(L) =
	// blockCount by scanning L downward from blockCount.
	nLeaves := blockCount
	var levels []int
	var rootLevel uint8
	for l := blockCount; l >= 1; l-- {
		lv, itot, rl := interiorShape(l, icap)
		if l+itot == blockCount {
			nLeaves, levels, rootLevel = l, lv, rl
			break
		}
		if l == 1 {
			nLeaves, levels, rootLevel = l, lv, rl
		}
	}

	var allocated []uint64
	leafLogs := make([]uint64, nLeaves)
	for i := range leafLogs {
		phys, err := sm.allocNodeBlock()
		if err != nil {
			return nil, fmt.Errorf("btrfs: alloc extent leaf %d: %w", i, err)
		}
		leafLogs[i] = physToLog(sb, phys)
		allocated = append(allocated, leafLogs[i])
	}
	interiorLogs := make([][]uint64, len(levels))
	for lvl, n := range levels {
		interiorLogs[lvl] = make([]uint64, n)
		for i := 0; i < n; i++ {
			phys, err := sm.allocNodeBlock()
			if err != nil {
				return nil, fmt.Errorf("btrfs: alloc extent interior L%d #%d: %w", lvl+1, i, err)
			}
			interiorLogs[lvl][i] = physToLog(sb, phys)
			allocated = append(allocated, interiorLogs[lvl][i])
		}
	}

	type blk struct {
		log uint64
		lvl uint8
	}
	var allBlocks []blk
	for _, l := range leafLogs {
		allBlocks = append(allBlocks, blk{l, 0})
	}
	for lvl := range interiorLogs {
		for _, l := range interiorLogs[lvl] {
			allBlocks = append(allBlocks, blk{l, uint8(lvl + 1)})
		}
	}

	bgUsed := map[uint64]uint64{}
	for cs, u := range in.baseBGUsed {
		bgUsed[cs] = u
	}
	for _, b := range allBlocks {
		if cs, ok := in.chunkOf(b.log); ok {
			bgUsed[cs] += uint64(nodeSize)
		}
	}

	recs := make([]itemRec, 0, len(in.baseRecs)+len(allBlocks)+len(in.bgLenFlags))
	recs = append(recs, in.baseRecs...)
	for _, b := range allBlocks {
		recs = append(recs, itemRec{key{b.log, typeMetadataItem, uint64(b.lvl)}, buildMetadataItemBytes(le, extentTreeObjID)})
	}
	for cs, lf := range in.bgLenFlags {
		recs = append(recs, itemRec{key{cs, typeBlockGroupItem, lf[0]}, buildBlockGroupItemBytes(le, bgUsed[cs], lf[1])})
	}
	sort.Slice(recs, func(a, b int) bool {
		return compareKeys(recs[a].k, recs[b].k.objID, recs[b].k.typ, recs[b].k.offset) < 0
	})

	ranges := partitionLeaves(recs, lcap)
	// realLeaves drives convergence. When it disagrees with the assumed nLeaves the
	// caller frees this attempt and retries with the corrected block count; the
	// (now inconsistent) leafLogs/levels are only used on the converged attempt,
	// where len(ranges) == nLeaves holds by construction.
	_ = icap

	return &extentLayout{
		leafLogs:     leafLogs,
		interiorLogs: interiorLogs,
		levels:       levels,
		rootLevel:    rootLevel,
		recs:         recs,
		ranges:       ranges,
		bgUsed:       bgUsed,
		allocated:    allocated,
	}, nil
}

// freeLayout returns every block a candidate layout allocated to the space
// manager, restoring the pre-attempt free state for the next fixpoint iteration.
func freeLayout(sb *superblock, sm *spaceManager, lay *extentLayout) {
	ns := uint64(sb.nodeSize)
	for _, l := range lay.allocated {
		sm.freeRange(physFromLog(sb, l), ns)
	}
}

// interiorRootLog returns the logical address of the tree root: the sole node of
// the top interior level, or the lone leaf when the tree is single-leaf.
func interiorRootLog(leafLogs []uint64, interiorLogs [][]uint64) uint64 {
	if len(interiorLogs) == 0 {
		return leafLogs[0]
	}
	top := interiorLogs[len(interiorLogs)-1]
	return top[0]
}

// writeMultiLevelTree writes the leaves (from recs/ranges) and the interior
// levels (bottom-up) to their pre-allocated blocks, all at generation+1.
func writeMultiLevelTree(rwaAt readerWriterAt, partOff int64, sb *superblock, hdrTemplate []byte,
	recs []itemRec, ranges [][2]int, leafLogs []uint64, interiorLogs [][]uint64, levels []int,
) error {
	le := binary.LittleEndian
	nodeSize := int(sb.nodeSize)
	if len(ranges) != len(leafLogs) {
		return fmt.Errorf("btrfs: extent tree leaf/range mismatch (%d vs %d)", len(ranges), len(leafLogs))
	}

	childFirstKeys := make([]key, len(ranges))
	for i, rng := range ranges {
		leaf := make([]byte, nodeSize)
		copy(leaf[:nodeHdrSize], hdrTemplate[:nodeHdrSize])
		le.PutUint32(leaf[0x60:], 0)
		leaf[0x64] = 0
		le.PutUint64(leaf[0x30:], leafLogs[i])
		le.PutUint64(leaf[0x50:], sb.generation+1)
		for _, r := range recs[rng[0]:rng[1]] {
			if err := leafInsertItem(leaf, r.k, r.data); err != nil {
				return fmt.Errorf("btrfs: insert into extent leaf %d: %w", i, err)
			}
		}
		updateNodeCRC(leaf)
		phys := physFromLog(sb, leafLogs[i])
		if _, err := rwaAt.WriteAt(leaf, partOff+int64(phys)); err != nil {
			return fmt.Errorf("btrfs: write extent leaf %d: %w", i, err)
		}
		childFirstKeys[i] = recs[rng[0]].k
	}

	childLogs := leafLogs
	for lvl, nNodes := range levels {
		nodeLogs := interiorLogs[lvl]
		if len(nodeLogs) != nNodes {
			return fmt.Errorf("btrfs: extent interior L%d node-log mismatch (%d vs %d)", lvl+1, len(nodeLogs), nNodes)
		}
		newChildLogs := make([]uint64, 0, nNodes)
		newChildKeys := make([]key, 0, nNodes)
		ci := 0
		for ni := 0; ni < nNodes; ni++ {
			remaining := len(childLogs) - ci
			nodesLeft := nNodes - ni
			perNode := (remaining + nodesLeft - 1) / nodesLeft
			node := make([]byte, nodeSize)
			copy(node[:nodeHdrSize], hdrTemplate[:nodeHdrSize])
			node[0x64] = byte(lvl + 1)
			le.PutUint64(node[0x30:], nodeLogs[ni])
			le.PutUint64(node[0x50:], sb.generation+1)
			cnt := 0
			firstKey := childFirstKeys[ci]
			for c := 0; c < perNode && ci < len(childLogs); c++ {
				off := nodeHdrSize + cnt*keyPtrSize
				k := childFirstKeys[ci]
				le.PutUint64(node[off:], k.objID)
				node[off+8] = k.typ
				le.PutUint64(node[off+9:], k.offset)
				le.PutUint64(node[off+17:], childLogs[ci])
				le.PutUint64(node[off+25:], sb.generation+1)
				cnt++
				ci++
			}
			le.PutUint32(node[0x60:], uint32(cnt))
			updateNodeCRC(node)
			phys := physFromLog(sb, nodeLogs[ni])
			if _, err := rwaAt.WriteAt(node, partOff+int64(phys)); err != nil {
				return fmt.Errorf("btrfs: write extent interior L%d #%d: %w", lvl+1, ni, err)
			}
			newChildLogs = append(newChildLogs, nodeLogs[ni])
			newChildKeys = append(newChildKeys, firstKey)
		}
		childLogs = newChildLogs
		childFirstKeys = newChildKeys
	}
	return nil
}

// repointExtentRoot rewrites the EXTENT_TREE ROOT_ITEM's bytenr, generation and
// level in place in the ROOT_TREE leaf that carries it (descending a multi-level
// root tree via rootItemLeafLogAddr). The leaf keeps its logical address, so the
// root tree's parent key-ptrs stay valid.
func repointExtentRoot(rwaAt readerWriterAt, partOff int64, sb *superblock, newRoot uint64, level uint8) error {
	leafLog, err := rootItemLeafLogAddr(rwaAt, partOff, sb, extentTreeObjID)
	if err != nil {
		return err
	}
	phys, err := sb.physAddr(partOff, leafLog)
	if err != nil {
		return fmt.Errorf("locate root tree leaf: %w", err)
	}
	leaf := make([]byte, sb.nodeSize)
	if _, err := rwaAt.ReadAt(leaf, phys); err != nil {
		return fmt.Errorf("read root tree leaf: %w", err)
	}
	idx := findItemIdx(leaf, extentTreeObjID, typeRootItem, 0)
	if idx < 0 {
		return fmt.Errorf("btrfs: EXTENT_TREE ROOT_ITEM not found")
	}
	items := parseLeafItems(leaf, parseNodeHeader(leaf).nItems)
	d := items[idx].data(leaf)
	if len(d) <= rootItemOffLevel {
		return fmt.Errorf("btrfs: EXTENT_TREE ROOT_ITEM too short")
	}
	le := binary.LittleEndian
	le.PutUint64(d[rootItemOffBytenr:], newRoot)
	le.PutUint64(d[rootItemOffGeneration:], sb.generation+1)
	d[rootItemOffLevel] = level
	if len(d) > rootItemOffGenerationV2+8 {
		le.PutUint64(d[rootItemOffGenerationV2:], sb.generation+1)
	}
	updateNodeCRC(leaf)
	if _, err := rwaAt.WriteAt(leaf, phys); err != nil {
		return fmt.Errorf("write root tree leaf: %w", err)
	}
	return nil
}
