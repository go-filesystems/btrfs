package filesystem_btrfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/go-volumes/safeio"
)

// ─────────────────────────────────────────────────────────────────────────────
// COW path-copy logic for btrfs B-trees
//
// Every mutation allocates a NEW copy of the leaf and all internal nodes on
// the path from the root to the leaf. Old blocks are returned to the space
// manager so long-running workloads do not burn space monotonically.
//
// When a leaf is too full to absorb a new item, cowInsert splits the leaf in
// two and propagates the resulting key-ptr up the path, splitting internal
// nodes as needed and growing the tree by a level when the root itself
// splits. This is what allows the driver to scale past a single leaf.
// ─────────────────────────────────────────────────────────────────────────────

// cowInsert inserts (k, data) into the FS-tree rooted at rootLogAddr.
// Returns the new root logical address. If the target leaf cannot absorb the
// new item, the leaf is split and the resulting structural change propagates
// up the path, growing the tree by a level when necessary.
func cowInsert(rwa readerWriterAt, rwaAt readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, rootLogAddr uint64,
	k key, data []byte,
) (uint64, error) {
	newRoot, err := cowMutate(rwaAt, partOff, sb, sm, rootLogAddr, k, func(leafBuf []byte) error {
		return leafInsertItem(leafBuf, k, data)
	})
	if err == nil {
		return newRoot, nil
	}
	if !errors.Is(err, errLeafFull) {
		return 0, err
	}
	// Leaf full — split path.
	return cowInsertSplit(rwaAt, partOff, sb, sm, rootLogAddr, k, data)
}

// cowUpdate replaces the data of the item with key k.
// newData must be the same size as the old data. For variable-size updates use
// cowReplace.
func cowUpdate(rwa readerWriterAt, rwaAt readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, rootLogAddr uint64,
	k key, newData []byte,
) (uint64, error) {
	return cowMutate(rwaAt, partOff, sb, sm, rootLogAddr, k, func(leafBuf []byte) error {
		idx := findItemIdx(leafBuf, k.objID, k.typ, k.offset)
		if idx < 0 {
			return fmt.Errorf("btrfs cow update: key (%d 0x%02X %d) not found", k.objID, k.typ, k.offset)
		}
		return leafReplaceItemData(leafBuf, idx, newData)
	})
}

// cowDelete removes the item with key k from the FS-tree. Returns an error
// wrapping ErrNotFound when no such key exists, so callers can selectively
// tolerate absence (e.g. legacy images without INODE_REF backrefs).
func cowDelete(rwa readerWriterAt, rwaAt readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, rootLogAddr uint64,
	k key,
) (uint64, error) {
	return cowMutate(rwaAt, partOff, sb, sm, rootLogAddr, k, func(leafBuf []byte) error {
		idx := findItemIdx(leafBuf, k.objID, k.typ, k.offset)
		if idx < 0 {
			return fmt.Errorf("btrfs cow delete: key (%d 0x%02X %d): %w", k.objID, k.typ, k.offset, ErrNotFound)
		}
		leafDeleteItem(leafBuf, idx)
		return nil
	})
}

// cowDeletePrefix removes ALL items with (objID, typ) from any leaf where they
// reside. Matching items may span multiple leaves; the function visits each
// affected leaf in turn until no more matches exist anywhere in the tree.
func cowDeletePrefix(rwa readerWriterAt, rwaAt readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, rootLogAddr uint64,
	objID uint64, typ uint8,
) (uint64, error) {
	// Repeatedly find a leaf containing at least one matching item and delete
	// every match in that leaf. The b-tree structure has changed between
	// iterations (new root, new leaves) so we restart from the root each pass
	// using a key targeted at the smallest possible match.
	curRoot := rootLogAddr
	prefixKey := key{objID: objID, typ: typ, offset: 0}
	for {
		leafBuf, leafAddr, err := findLeafContainingPrefix(rwaAt, partOff, sb, curRoot, objID, typ)
		if err != nil {
			return 0, err
		}
		if leafBuf == nil {
			// No more matching items anywhere in the tree.
			return curRoot, nil
		}
		_ = leafAddr
		newRoot, err := cowMutate(rwaAt, partOff, sb, sm, curRoot, prefixKey, func(leafBuf []byte) error {
			idx := findItemIdxPrefix(leafBuf, objID, typ)
			for i := len(idx) - 1; i >= 0; i-- {
				leafDeleteItem(leafBuf, idx[i])
			}
			if len(idx) == 0 {
				return fmt.Errorf("btrfs cow delete prefix: routed to leaf with no (%d 0x%02X) items", objID, typ)
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
		curRoot = newRoot
	}
}

// findLeafContainingPrefix walks the tree from rootLogAddr looking for a leaf
// that holds at least one item matching (objID, typ). Returns the leaf bytes
// and its logical address, or (nil, 0, nil) when no such leaf exists.
func findLeafContainingPrefix(r io.ReaderAt, partOff int64, sb *superblock,
	rootLogAddr uint64, objID uint64, typ uint8,
) ([]byte, uint64, error) {
	type frame struct {
		logAddr  uint64
		startIdx int
		depth    int
	}
	var seen safeio.VisitSet
	nodeGuard := safeio.NewLoopGuard(maxTreeNodes)
	stack := []frame{{logAddr: rootLogAddr, startIdx: 0, depth: 0}}
	for len(stack) > 0 {
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if top.depth > maxBtreeDepth {
			return nil, 0, fmt.Errorf("btrfs: btree depth exceeds %d: %w", maxBtreeDepth, safeio.ErrLoopLimit)
		}
		if err := nodeGuard.Next(); err != nil {
			return nil, 0, fmt.Errorf("btrfs: findLeafContainingPrefix: %w", err)
		}
		if err := seen.Check(top.logAddr); err != nil {
			return nil, 0, fmt.Errorf("btrfs: findLeafContainingPrefix: %w", err)
		}
		buf, err := readNode(r, partOff, sb, top.logAddr)
		if err != nil {
			return nil, 0, err
		}
		hdr := parseNodeHeader(buf)
		if hdr.level == 0 {
			items := parseLeafItems(buf, hdr.nItems)
			for _, it := range items {
				if it.k.objID == objID && it.k.typ == typ {
					return buf, top.logAddr, nil
				}
			}
			continue
		}
		le := binary.LittleEndian
		// Push children in REVERSE so we visit left-to-right when popping.
		for i := int(hdr.nItems) - 1; i >= top.startIdx; i-- {
			off := nodeHdrSize + i*keyPtrSize
			if off+keyPtrSize > len(buf) {
				continue
			}
			k := readKey(buf[off:])
			// Skip subtrees whose entire range is past our prefix.
			if k.objID > objID || (k.objID == objID && k.typ > typ) {
				continue
			}
			// Skip subtrees whose entire range is before our prefix only if
			// the NEXT child's first key is still before our prefix — i.e. all
			// items in this subtree are strictly less than (objID, typ, 0).
			// We can only skip if i+1 is in range AND its key is still < our
			// target; the leftmost child whose first key precedes the target
			// may still contain matching items, so include it.
			childLog := le.Uint64(buf[off+17:])
			stack = append(stack, frame{logAddr: childLog, depth: top.depth + 1})
		}
	}
	return nil, 0, nil
}

// ── path-copy engine ──────────────────────────────────────────────────────

// pathEntry records one node on the root→leaf path.
type pathEntry struct {
	logAddr uint64 // original logical address
	buf     []byte // node contents
	level   uint8
	// For internal nodes: the index of the key-pointer that was followed.
	childIdx int
}

// cowMutate performs a copy-on-write mutation on the B-tree.
// fn receives the (mutable copy of) the target leaf and must apply the mutation.
// routeKey selects which leaf to traverse to in a multi-leaf tree (the leaf
// holding the rightmost key-ptr whose key is ≤ routeKey at each internal
// node, matching the search routing used by searchTree). Old leaf and
// internal node blocks are returned to the space manager once a successful
// new root has been computed.
func cowMutate(rwaAt readerWriterAt, partOff int64, sb *superblock, sm *spaceManager,
	rootLogAddr uint64, routeKey key, fn func(leafBuf []byte) error,
) (uint64, error) {
	// 1. Trace the path from root to the leaf where routeKey would land.
	path, err := tracePath(rwaAt, partOff, sb, rootLogAddr, routeKey)
	if err != nil {
		return 0, err
	}

	// 2. Apply fn to a mutable copy of the leaf.
	leafEntry := &path[len(path)-1]
	leafCopy := make([]byte, len(leafEntry.buf))
	copy(leafCopy, leafEntry.buf)
	if err := fn(leafCopy); err != nil {
		return 0, err
	}

	// 3. Walk up the path. The leaf may have been emptied by fn (delete
	// case); when it has, and it is NOT the only leaf in the tree, drop it
	// rather than writing a new empty copy — the parent removes its key-ptr
	// to the dropped child, and the empty-collapse propagates upward.
	hdr := parseNodeHeader(leafCopy)
	freedOld := []uint64{leafEntry.logAddr}
	var newChildLog uint64
	deleted := hdr.nItems == 0 && len(path) > 1
	if !deleted {
		newLeafLog, err := writeCowNode(rwaAt, partOff, sb, sm, leafCopy)
		if err != nil {
			return 0, fmt.Errorf("btrfs cow: write leaf: %w", err)
		}
		newChildLog = newLeafLog
	}

	le := binary.LittleEndian
	for i := len(path) - 2; i >= 0; i-- {
		pe := &path[i]
		nodeCopy := make([]byte, len(pe.buf))
		copy(nodeCopy, pe.buf)

		if deleted {
			internalRemoveKeyPtr(nodeCopy, pe.childIdx)
			nh := parseNodeHeader(nodeCopy)
			if nh.nItems == 0 && i > 0 {
				// Internal node became empty: drop it, keep propagating up.
				freedOld = append(freedOld, pe.logAddr)
				continue
			}
			if i == 0 && nh.nItems == 1 {
				// Root collapses: its single remaining child becomes the new
				// root, shrinking the tree by one level.
				childLog := le.Uint64(nodeCopy[nodeHdrSize+17:])
				freedOld = append(freedOld, pe.logAddr)
				for _, oldLog := range freedOld {
					sm.freeRange(physFromLog(sb, oldLog), uint64(sb.nodeSize))
				}
				return childLog, nil
			}
			// Node still has children; fall through to a normal COW write.
			deleted = false
		} else {
			off := nodeHdrSize + pe.childIdx*keyPtrSize + 17
			le.PutUint64(nodeCopy[off:], newChildLog)
			le.PutUint64(nodeCopy[off+8:], sb.generation+1)
		}

		newNodeLog, err := writeCowNode(rwaAt, partOff, sb, sm, nodeCopy)
		if err != nil {
			return 0, fmt.Errorf("btrfs cow: write internal node: %w", err)
		}
		freedOld = append(freedOld, pe.logAddr)
		newChildLog = newNodeLog
	}

	for _, oldLog := range freedOld {
		sm.freeRange(physFromLog(sb, oldLog), uint64(sb.nodeSize))
	}

	if deleted {
		// We propagated empty-collapse all the way to (and past) the root —
		// the original tree was a single leaf and it is now empty. Write a
		// fresh empty leaf to anchor the FS_TREE.
		emptyLeaf := make([]byte, sb.nodeSize)
		copy(emptyLeaf[:nodeHdrSize], leafEntry.buf[:nodeHdrSize])
		le.PutUint32(emptyLeaf[0x60:], 0)
		emptyLeaf[0x64] = 0
		newLeafLog, err := writeCowNode(rwaAt, partOff, sb, sm, emptyLeaf)
		if err != nil {
			return 0, fmt.Errorf("btrfs cow: write replacement empty leaf: %w", err)
		}
		return newLeafLog, nil
	}

	// Opportunistic post-shrink: if the root is now a thin internal node
	// over two leaves whose contents would fit in one, collapse them.
	if collapsed, ok := tryCollapseTwoLeafRoot(rwaAt, partOff, sb, sm, newChildLog); ok {
		return collapsed, nil
	}
	return newChildLog, nil // new root
}

// internalRemoveKeyPtr removes the key-ptr at idx from an internal node and
// shifts subsequent entries left to close the gap.
func internalRemoveKeyPtr(nodeBuf []byte, idx int) {
	hdr := parseNodeHeader(nodeBuf)
	n := int(hdr.nItems)
	if idx < 0 || idx >= n {
		return
	}
	src := nodeBuf[nodeHdrSize+(idx+1)*keyPtrSize : nodeHdrSize+n*keyPtrSize]
	dst := nodeBuf[nodeHdrSize+idx*keyPtrSize:]
	copy(dst[:len(src)], src)
	// Zero the now-trailing slot.
	tail := nodeHdrSize + (n-1)*keyPtrSize
	for i := tail; i < tail+keyPtrSize && i < len(nodeBuf); i++ {
		nodeBuf[i] = 0
	}
	binary.LittleEndian.PutUint32(nodeBuf[0x60:], uint32(n-1))
}

// tryCollapseTwoLeafRoot is a post-cowMutate compaction step: when the root
// is an internal node with exactly two leaf children whose combined items
// would fit in a single leaf, merge them into one new leaf and return it as
// the new root, shrinking the tree by a level. Returns (newRoot, true) on
// success or (rootLogAddr, false) when no such compaction is possible.
func tryCollapseTwoLeafRoot(rwaAt readerWriterAt, partOff int64, sb *superblock, sm *spaceManager,
	rootLogAddr uint64,
) (uint64, bool) {
	rootBuf, err := readNode(rwaAt, partOff, sb, rootLogAddr)
	if err != nil {
		return rootLogAddr, false
	}
	rootHdr := parseNodeHeader(rootBuf)
	if rootHdr.level != 1 || rootHdr.nItems != 2 {
		return rootLogAddr, false
	}
	le := binary.LittleEndian
	leftLog := le.Uint64(rootBuf[nodeHdrSize+17:])
	rightLog := le.Uint64(rootBuf[nodeHdrSize+keyPtrSize+17:])

	leftBuf, err := readNode(rwaAt, partOff, sb, leftLog)
	if err != nil {
		return rootLogAddr, false
	}
	rightBuf, err := readNode(rwaAt, partOff, sb, rightLog)
	if err != nil {
		return rootLogAddr, false
	}
	leftHdr := parseNodeHeader(leftBuf)
	rightHdr := parseNodeHeader(rightBuf)
	if leftHdr.level != 0 || rightHdr.level != 0 {
		return rootLogAddr, false
	}

	// Build the combined item list (preserving order) and check if it fits.
	leftItems := parseLeafItems(leftBuf, leftHdr.nItems)
	rightItems := parseLeafItems(rightBuf, rightHdr.nItems)
	combined := make([]splitItem, 0, len(leftItems)+len(rightItems))
	for _, it := range leftItems {
		d := it.data(leftBuf)
		cpy := make([]byte, len(d))
		copy(cpy, d)
		combined = append(combined, splitItem{k: it.k, data: cpy})
	}
	for _, it := range rightItems {
		d := it.data(rightBuf)
		cpy := make([]byte, len(d))
		copy(cpy, d)
		combined = append(combined, splitItem{k: it.k, data: cpy})
	}
	dataSum := 0
	for _, s := range combined {
		dataSum += len(s.data)
	}
	headerBytes := nodeHdrSize + len(combined)*itemSize
	if headerBytes+dataSum > int(sb.nodeSize) {
		return rootLogAddr, false
	}

	mergedBuf, err := buildLeafFromItems(leftBuf, combined, int(sb.nodeSize))
	if err != nil {
		return rootLogAddr, false
	}
	mergedLog, err := writeCowNode(rwaAt, partOff, sb, sm, mergedBuf)
	if err != nil {
		return rootLogAddr, false
	}
	// Recycle the now-orphaned internal root and the two source leaves.
	sm.freeRange(physFromLog(sb, rootLogAddr), uint64(sb.nodeSize))
	sm.freeRange(physFromLog(sb, leftLog), uint64(sb.nodeSize))
	sm.freeRange(physFromLog(sb, rightLog), uint64(sb.nodeSize))
	return mergedLog, true
}

// writeCowNode allocates a fresh physical block, writes nodeBuf into it with
// updated header fields (bytenr, generation, CRC) and returns the new logical
// address.
func writeCowNode(rwaAt readerWriterAt, partOff int64, sb *superblock, sm *spaceManager, nodeBuf []byte) (uint64, error) {
	newPhys, err := sm.allocNodeBlock()
	if err != nil {
		return 0, fmt.Errorf("alloc node block: %w", err)
	}
	newLog := physToLog(sb, newPhys)
	le := binary.LittleEndian
	le.PutUint64(nodeBuf[0x30:], newLog)
	le.PutUint64(nodeBuf[0x50:], sb.generation+1)
	updateNodeCRC(nodeBuf)
	if _, err := rwaAt.WriteAt(nodeBuf, partOff+int64(physFromLog(sb, newLog))); err != nil {
		return 0, err
	}
	return newLog, nil
}

// cowInsertSplit handles the leaf-full case by splitting the target leaf into
// two leaves and propagating the resulting (splitKey, rightLog) structural
// addition up the path. Internal nodes are split when they cannot absorb a
// new key-ptr; if the propagation reaches the root, a new root is created at
// one level above.
func cowInsertSplit(rwaAt readerWriterAt, partOff int64, sb *superblock, sm *spaceManager,
	rootLogAddr uint64, k key, data []byte,
) (uint64, error) {
	path, err := tracePath(rwaAt, partOff, sb, rootLogAddr, k)
	if err != nil {
		return 0, err
	}
	leafEntry := &path[len(path)-1]

	// Split the leaf around (k, data).
	leftBuf, rightBuf, splitKey, err := splitAndInsertLeaf(leafEntry.buf, k, data, int(sb.nodeSize))
	if err != nil {
		return 0, fmt.Errorf("btrfs cow split: %w", err)
	}

	leftLog, err := writeCowNode(rwaAt, partOff, sb, sm, leftBuf)
	if err != nil {
		return 0, fmt.Errorf("btrfs cow split: write left leaf: %w", err)
	}
	rightLog, err := writeCowNode(rwaAt, partOff, sb, sm, rightBuf)
	if err != nil {
		return 0, fmt.Errorf("btrfs cow split: write right leaf: %w", err)
	}
	freedOld := []uint64{leafEntry.logAddr}

	// Propagate (leftLog replacing the old child slot, plus a new key-ptr
	// (splitKey, rightLog) inserted right after) up the path. If an internal
	// node cannot absorb the new key-ptr it is split too, producing yet
	// another structural change to propagate.
	newChildLog := leftLog
	pendingKey := splitKey
	pendingChild := rightLog
	pending := true

	le := binary.LittleEndian
	for i := len(path) - 2; i >= 0; i-- {
		pe := &path[i]
		nodeCopy := make([]byte, len(pe.buf))
		copy(nodeCopy, pe.buf)
		// Update the followed child pointer to newChildLog.
		off := nodeHdrSize + pe.childIdx*keyPtrSize + 17
		le.PutUint64(nodeCopy[off:], newChildLog)
		le.PutUint64(nodeCopy[off+8:], sb.generation+1)

		if pending {
			if internalCanFitOneMore(nodeCopy) {
				internalInsertKeyPtr(nodeCopy, pe.childIdx+1, pendingKey, pendingChild, sb.generation+1)
				pending = false
			} else {
				leftI, rightI, splitK, ierr := splitAndInsertInternal(nodeCopy, pe.childIdx+1, pendingKey, pendingChild, sb.generation+1, int(sb.nodeSize))
				if ierr != nil {
					return 0, fmt.Errorf("btrfs cow split internal: %w", ierr)
				}
				leftIL, werr := writeCowNode(rwaAt, partOff, sb, sm, leftI)
				if werr != nil {
					return 0, fmt.Errorf("btrfs cow split: write left internal: %w", werr)
				}
				rightIL, werr := writeCowNode(rwaAt, partOff, sb, sm, rightI)
				if werr != nil {
					return 0, fmt.Errorf("btrfs cow split: write right internal: %w", werr)
				}
				freedOld = append(freedOld, pe.logAddr)
				newChildLog = leftIL
				pendingKey = splitK
				pendingChild = rightIL
				pending = true
				continue
			}
		}

		newNodeLog, werr := writeCowNode(rwaAt, partOff, sb, sm, nodeCopy)
		if werr != nil {
			return 0, fmt.Errorf("btrfs cow: write internal node: %w", werr)
		}
		freedOld = append(freedOld, pe.logAddr)
		newChildLog = newNodeLog
	}

	// If after walking up there is still a pending structural change, the
	// root has split — grow the tree by one level with a fresh root.
	var rootLog uint64
	if pending {
		newRootBuf, leftFirstKey, err := buildNewRoot(rwaAt, partOff, sb, newChildLog, pendingKey, pendingChild, uint8(len(path)))
		_ = leftFirstKey
		if err != nil {
			return 0, fmt.Errorf("btrfs cow split: build new root: %w", err)
		}
		newRootLog, werr := writeCowNode(rwaAt, partOff, sb, sm, newRootBuf)
		if werr != nil {
			return 0, fmt.Errorf("btrfs cow split: write new root: %w", werr)
		}
		rootLog = newRootLog
	} else {
		rootLog = newChildLog
	}

	for _, oldLog := range freedOld {
		sm.freeRange(physFromLog(sb, oldLog), uint64(sb.nodeSize))
	}
	return rootLog, nil
}

// tracePath follows the B-tree from root to the leaf where routeKey would
// land, recording each visited node along with the child index that was
// followed at each internal node. Routing matches searchTree: descend through
// the rightmost key-ptr whose key is ≤ routeKey.
func tracePath(r io.ReaderAt, partOff int64, sb *superblock, rootLogAddr uint64, routeKey key) ([]pathEntry, error) {
	var path []pathEntry
	logAddr := rootLogAddr
	var seen safeio.VisitSet
	for depth := 0; ; depth++ {
		if depth > maxBtreeDepth {
			return nil, fmt.Errorf("btrfs: btree depth exceeds %d: %w", maxBtreeDepth, safeio.ErrLoopLimit)
		}
		if err := seen.Check(logAddr); err != nil {
			return nil, fmt.Errorf("btrfs: tracePath: %w", err)
		}
		buf, err := readNode(r, partOff, sb, logAddr)
		if err != nil {
			return nil, err
		}
		hdr := parseNodeHeader(buf)
		if hdr.level == 0 {
			path = append(path, pathEntry{logAddr: logAddr, buf: buf, level: 0})
			return path, nil
		}
		le := binary.LittleEndian
		chosenIdx := -1
		var chosenLog uint64
		for i := uint32(0); i < hdr.nItems; i++ {
			off := nodeHdrSize + int(i)*keyPtrSize
			if off+keyPtrSize > len(buf) {
				break
			}
			k := readKey(buf[off:])
			if compareKeys(k, routeKey.objID, routeKey.typ, routeKey.offset) <= 0 {
				chosenIdx = int(i)
				chosenLog = le.Uint64(buf[off+17:])
			} else {
				break
			}
		}
		// If routeKey is smaller than every key in this node (e.g. a fresh
		// insert into an empty range, or a prefix probe with offset=0), fall
		// back to the leftmost child so the leaf where routeKey *would* land
		// is still reachable for insertion.
		if chosenIdx < 0 {
			chosenIdx = 0
			chosenLog = le.Uint64(buf[nodeHdrSize+17:])
		}
		path = append(path, pathEntry{logAddr: logAddr, buf: buf, level: hdr.level, childIdx: chosenIdx})
		logAddr = chosenLog
	}
}

// ── Logical↔Physical helpers ─────────────────────────────────────────────

// physToLog converts a physical offset to a logical address using the first
// matching chunk in sb.  On a single-device image this is a straightforward
// inversion of logToPhys.
func physToLog(sb *superblock, physOff uint64) uint64 {
	for _, m := range sb.sysChunks {
		if physOff >= m.physStart && physOff < m.physStart+m.size {
			return m.logStart + (physOff - m.physStart)
		}
	}
	// Fallback: treat physical as logical (should not normally happen).
	return physOff
}

// physFromLog converts a logical address back to raw physical (no partOff).
func physFromLog(sb *superblock, logAddr uint64) uint64 {
	for _, m := range sb.sysChunks {
		if logAddr >= m.logStart && logAddr < m.logStart+m.size {
			return m.physStart + (logAddr - m.logStart)
		}
	}
	return logAddr
}

// ── Leaf split ───────────────────────────────────────────────────────────

// splitItem is one (key, data) pair used to lay out a leaf during a split.
type splitItem struct {
	k    key
	data []byte
}

// splitAndInsertLeaf produces two fresh leaves that together hold the items
// of leafBuf plus the new (k, data) item. The split point is chosen so the
// two halves are approximately balanced by data bytes. Returns
// (leftBuf, rightBuf, firstKeyOfRight, error).
//
// Both returned leaves carry placeholder header bytes — writeCowNode is
// expected to overwrite bytenr / generation / CRC before persisting them.
func splitAndInsertLeaf(leafBuf []byte, k key, data []byte, nodeSize int) ([]byte, []byte, key, error) {
	hdr := parseNodeHeader(leafBuf)
	oldItems := parseLeafItems(leafBuf, hdr.nItems)

	all := make([]splitItem, 0, len(oldItems)+1)
	for _, it := range oldItems {
		d := it.data(leafBuf)
		cpy := make([]byte, len(d))
		copy(cpy, d)
		all = append(all, splitItem{k: it.k, data: cpy})
	}
	// Locate sorted position for the new item.
	insertIdx := len(all)
	for i, s := range all {
		if compareKeys(s.k, k.objID, k.typ, k.offset) > 0 {
			insertIdx = i
			break
		}
	}
	all = append(all, splitItem{})
	copy(all[insertIdx+1:], all[insertIdx:])
	all[insertIdx] = splitItem{k: k, data: append([]byte(nil), data...)}

	// Choose split: roughly half the data bytes go to each side, and each side
	// must hold at least one item.
	totalData := 0
	for _, s := range all {
		totalData += len(s.data)
	}
	target := totalData / 2
	cum := 0
	split := 1
	for i := 0; i < len(all)-1; i++ {
		cum += len(all[i].data)
		if cum >= target {
			split = i + 1
			break
		}
	}
	if split < 1 {
		split = 1
	}
	if split > len(all)-1 {
		split = len(all) - 1
	}

	leftItems := all[:split]
	rightItems := all[split:]
	splitKey := rightItems[0].k

	leftBuf, err := buildLeafFromItems(leafBuf, leftItems, nodeSize)
	if err != nil {
		return nil, nil, key{}, err
	}
	rightBuf, err := buildLeafFromItems(leafBuf, rightItems, nodeSize)
	if err != nil {
		return nil, nil, key{}, err
	}
	// Verify the new item still fits in its destination half. If the single
	// new item is so large that even an empty leaf cannot hold it, return an
	// error — the caller cannot work around it without growing the node size.
	if int(itemSize+len(data)) > nodeSize-nodeHdrSize {
		return nil, nil, key{}, fmt.Errorf("btrfs split: item too large for empty leaf (size=%d, max=%d)", len(data), nodeSize-nodeHdrSize-itemSize)
	}
	return leftBuf, rightBuf, splitKey, nil
}

// buildLeafFromItems constructs a fresh leaf node buffer of size nodeSize
// containing the given items, packing item data from the end of the node
// backward (btrfs convention). headerSrc is used as a template — only the
// node header layout (uuid, csum tail) is copied; bytenr/generation/CRC and
// the item area are overwritten.
func buildLeafFromItems(headerSrc []byte, items []splitItem, nodeSize int) ([]byte, error) {
	buf := make([]byte, nodeSize)
	// Copy header fields (uuid, etc.) from the source so chunk_tree_uuid and
	// fs_uuid remain intact. bytenr/generation/CRC are overwritten on write.
	copy(buf[:nodeHdrSize], headerSrc[:nodeHdrSize])
	le := binary.LittleEndian
	le.PutUint32(buf[0x60:], uint32(len(items)))
	buf[0x64] = 0 // level = 0

	writePtr := nodeSize
	for i, s := range items {
		needed := itemSize + len(s.data)
		used := nodeHdrSize + (i+1)*itemSize + (nodeSize - writePtr)
		if used+len(s.data) > nodeSize {
			return nil, fmt.Errorf("btrfs split: leaf cannot hold item %d (need %d, avail %d)", i, needed, nodeSize-used-len(s.data))
		}
		writePtr -= len(s.data)
		copy(buf[writePtr:], s.data)
		off := nodeHdrSize + i*itemSize
		le.PutUint64(buf[off:], s.k.objID)
		buf[off+8] = s.k.typ
		le.PutUint64(buf[off+9:], s.k.offset)
		le.PutUint32(buf[off+17:], uint32(writePtr-nodeHdrSize))
		le.PutUint32(buf[off+21:], uint32(len(s.data)))
	}
	return buf, nil
}

// ── Internal node split ───────────────────────────────────────────────────

// internalCanFitOneMore reports whether a single additional key-ptr can be
// appended to the internal node.
func internalCanFitOneMore(nodeBuf []byte) bool {
	hdr := parseNodeHeader(nodeBuf)
	used := nodeHdrSize + int(hdr.nItems)*keyPtrSize
	return used+keyPtrSize <= len(nodeBuf)
}

// internalInsertKeyPtr inserts (k, childLog) at insertIdx in an internal
// node. Caller must have checked there is room (internalCanFitOneMore).
func internalInsertKeyPtr(nodeBuf []byte, insertIdx int, k key, childLog uint64, generation uint64) {
	hdr := parseNodeHeader(nodeBuf)
	n := int(hdr.nItems)
	le := binary.LittleEndian
	// Shift existing entries right.
	src := nodeBuf[nodeHdrSize+insertIdx*keyPtrSize : nodeHdrSize+n*keyPtrSize]
	dst := nodeBuf[nodeHdrSize+(insertIdx+1)*keyPtrSize:]
	copy(dst[:len(src)], src)
	// Write new entry.
	off := nodeHdrSize + insertIdx*keyPtrSize
	le.PutUint64(nodeBuf[off:], k.objID)
	nodeBuf[off+8] = k.typ
	le.PutUint64(nodeBuf[off+9:], k.offset)
	le.PutUint64(nodeBuf[off+17:], childLog)
	le.PutUint64(nodeBuf[off+25:], generation)
	le.PutUint32(nodeBuf[0x60:], uint32(n+1))
}

// splitAndInsertInternal splits a full internal node, inserting (k, childLog)
// at insertIdx in the merged sequence. Returns (leftBuf, rightBuf,
// firstKeyOfRight, error). Both buffers carry placeholder bytenr/CRC fields
// that writeCowNode overwrites.
func splitAndInsertInternal(nodeBuf []byte, insertIdx int, k key, childLog uint64, generation uint64, nodeSize int) ([]byte, []byte, key, error) {
	hdr := parseNodeHeader(nodeBuf)
	n := int(hdr.nItems)
	le := binary.LittleEndian
	type kp struct {
		k        key
		childLog uint64
		gen      uint64
	}
	all := make([]kp, 0, n+1)
	for i := 0; i < n; i++ {
		off := nodeHdrSize + i*keyPtrSize
		all = append(all, kp{
			k:        readKey(nodeBuf[off:]),
			childLog: le.Uint64(nodeBuf[off+17:]),
			gen:      le.Uint64(nodeBuf[off+25:]),
		})
	}
	all = append(all, kp{})
	copy(all[insertIdx+1:], all[insertIdx:])
	all[insertIdx] = kp{k: k, childLog: childLog, gen: generation}

	split := len(all) / 2
	if split < 1 {
		split = 1
	}
	leftEntries := all[:split]
	rightEntries := all[split:]
	splitKey := rightEntries[0].k

	build := func(entries []kp) []byte {
		out := make([]byte, nodeSize)
		copy(out[:nodeHdrSize], nodeBuf[:nodeHdrSize])
		le.PutUint32(out[0x60:], uint32(len(entries)))
		out[0x64] = hdr.level // same level as source
		for i, e := range entries {
			off := nodeHdrSize + i*keyPtrSize
			le.PutUint64(out[off:], e.k.objID)
			out[off+8] = e.k.typ
			le.PutUint64(out[off+9:], e.k.offset)
			le.PutUint64(out[off+17:], e.childLog)
			le.PutUint64(out[off+25:], e.gen)
		}
		return out
	}
	return build(leftEntries), build(rightEntries), splitKey, nil
}

// buildNewRoot constructs a new internal root one level above leftChildLog
// and rightChildLog. The first key of the left subtree is determined by
// reading the existing node at leftChildLog (whose first key already
// represents that subtree's range). depth is the previous root depth (level
// of the old root); the new root sits at depth+1.
func buildNewRoot(rwaAt readerWriterAt, partOff int64, sb *superblock,
	leftChildLog uint64, rightFirstKey key, rightChildLog uint64, depth uint8,
) ([]byte, key, error) {
	// Read the left child to derive its first key (used as the left key-ptr
	// in the new root). For an internal node that's the first key in its
	// header; for a leaf that's the first item's key.
	leftBuf, err := readNode(rwaAt, partOff, sb, leftChildLog)
	if err != nil {
		return nil, key{}, fmt.Errorf("read left child: %w", err)
	}
	leftHdr := parseNodeHeader(leftBuf)
	var leftFirstKey key
	if leftHdr.level == 0 {
		items := parseLeafItems(leftBuf, leftHdr.nItems)
		if len(items) == 0 {
			return nil, key{}, fmt.Errorf("left leaf is empty")
		}
		leftFirstKey = items[0].k
	} else {
		leftFirstKey = readKey(leftBuf[nodeHdrSize:])
	}

	out := make([]byte, sb.nodeSize)
	copy(out[:nodeHdrSize], leftBuf[:nodeHdrSize])
	le := binary.LittleEndian
	le.PutUint32(out[0x60:], 2)
	out[0x64] = depth // new root level

	writePair := func(idx int, k key, childLog uint64) {
		off := nodeHdrSize + idx*keyPtrSize
		le.PutUint64(out[off:], k.objID)
		out[off+8] = k.typ
		le.PutUint64(out[off+9:], k.offset)
		le.PutUint64(out[off+17:], childLog)
		le.PutUint64(out[off+25:], sb.generation+1)
	}
	writePair(0, leftFirstKey, leftChildLog)
	writePair(1, rightFirstKey, rightChildLog)
	return out, leftFirstKey, nil
}
