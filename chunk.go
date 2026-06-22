package filesystem_btrfs

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/go-volumes/safeio"
)

// loadChunkTree walks the chunk tree and merges all CHUNK_ITEM mappings into
// sb.sysChunks. Mappings already present in sys_chunk_array (from
// parseSysChunkArray during superblock read) are skipped to avoid duplicate
// entries — the space manager seeds freeExts from sb.sysChunks and a
// duplicate full-range chunk would let the allocator hand out the same
// physical region twice, clobbering writes.
func loadChunkTree(r io.ReaderAt, partOff int64, sb *superblock) error {
	return walkLeaves(r, partOff, sb, sb.chunkLogAddr, func(buf []byte, items []leafItem) error {
		le := binary.LittleEndian
		for _, it := range items {
			if it.k.typ != 0xE4 { // CHUNK_ITEM
				continue
			}
			d := it.data(buf)
			if len(d) < chunkHeaderSize+chunkStripeSize {
				continue
			}
			numStripes := le.Uint16(d[chunkNumStripes:])
			if numStripes == 0 {
				continue
			}
			stripes := parseAllStripes(le, d, numStripes)
			localIdx := -1
			var physOff uint64
			for i, s := range stripes {
				if (sb.devID != 0 && s.devID == sb.devID) || (sb.devID == 0 && i == 0) {
					localIdx = i
					physOff = s.offset
					break
				}
			}
			mapping := chunkMapping{
				logStart:       it.k.offset,
				size:           le.Uint64(d[chunkSize:]),
				physStart:      physOff,
				localStripeIdx: localIdx,
				profile:        le.Uint64(d[chunkType:]),
				stripeLen:      le.Uint64(d[chunkStripeLen:]),
				subStripes:     le.Uint16(d[chunkSubStripes:]),
				stripes:        stripes,
			}
			if !sb.hasChunkMapping(mapping) {
				sb.sysChunks = append(sb.sysChunks, mapping)
			}
		}
		return nil
	})
}

// walkLeaves walks every leaf in the tree rooted at rootLogAddr and calls fn
// for each leaf's items. This runs at mount time (loadChunkTree), so it is
// pre-auth and must stay bounded against malicious chunk-tree geometry.
func walkLeaves(r io.ReaderAt, partOff int64, sb *superblock,
	rootLogAddr uint64, fn func(buf []byte, items []leafItem) error,
) error {
	return walkNode(r, partOff, sb, rootLogAddr, fn)
}

// walkNode walks the subtree rooted at logAddr, calling fn for each leaf's
// items, with its own cycle-detection set and node-count guard so a
// self-referential or fan-out-bomb tree cannot recurse without bound or
// revisit the same node forever.
func walkNode(r io.ReaderAt, partOff int64, sb *superblock,
	logAddr uint64, fn func(buf []byte, items []leafItem) error,
) error {
	w := &treeWalk{
		seen:  &safeio.VisitSet{},
		guard: safeio.NewLoopGuard(maxTreeNodes),
	}
	return w.walkNode(r, partOff, sb, logAddr, 0, fn)
}

// treeWalk carries the cycle-detection set and node-count guard shared across
// a single recursive chunk-tree walk.
type treeWalk struct {
	seen  *safeio.VisitSet
	guard *safeio.LoopGuard
}

func (w *treeWalk) walkNode(r io.ReaderAt, partOff int64, sb *superblock,
	logAddr uint64, depth int, fn func(buf []byte, items []leafItem) error,
) error {
	if depth > maxBtreeDepth {
		return fmt.Errorf("btrfs: chunk-tree depth exceeds %d: %w", maxBtreeDepth, safeio.ErrLoopLimit)
	}
	if err := w.guard.Next(); err != nil {
		return fmt.Errorf("btrfs: walkNode: %w", err)
	}
	if err := w.seen.Check(logAddr); err != nil {
		return fmt.Errorf("btrfs: walkNode: %w", err)
	}
	buf, err := readNode(r, partOff, sb, logAddr)
	if err != nil {
		return err
	}
	hdr := parseNodeHeader(buf)
	if hdr.level == 0 {
		return fn(buf, parseLeafItems(buf, hdr.nItems))
	}
	le := binary.LittleEndian
	for i := uint32(0); i < hdr.nItems; i++ {
		off := nodeHdrSize + int(i)*keyPtrSize
		if off+keyPtrSize > len(buf) {
			break
		}
		child := le.Uint64(buf[off+17:])
		// Logical address 0 is reserved in btrfs and never a real node
		// pointer; a zeroed key-ptr (e.g. from a truncated/over-claimed
		// nItems) must be skipped rather than followed into a bogus node.
		if child == 0 {
			continue
		}
		if err := w.walkNode(r, partOff, sb, child, depth+1, fn); err != nil {
			return err
		}
	}
	return nil
}

// resolveRootTree walks the ROOT_TREE to find the logical address of the
// FS_TREE (objectid = BTRFS_FS_TREE_OBJECTID = 5).
//
// On-disk btrfs_root_item layout:
//
//	0x00  struct btrfs_inode_item inode   (160 bytes)
//	0xA0  __le64 generation               (8)
//	0xA8  __le64 root_dirid               (8)
//	0xB0  __le64 bytenr                   (8) <- logical addr of root node
//	... (further fields ignored)
//
// The bytenr is the logical address of the FS_TREE's root node.
func resolveRootTree(r io.ReaderAt, partOff int64, sb *superblock) (uint64, error) {
	buf, it, err := searchTree(r, partOff, sb, sb.rootLogAddr, fsTreeObjID, typeRootItem, 0)
	if err != nil {
		return 0, fmt.Errorf("btrfs: locate FS_TREE root item: %w", err)
	}
	d := it.data(buf)
	const bytenrOff = 0xB0 // 176 bytes into root_item
	if len(d) < bytenrOff+8 {
		return 0, fmt.Errorf("btrfs: ROOT_ITEM for FS_TREE too short (%d bytes, need at least %d)", len(d), bytenrOff+8)
	}
	return binary.LittleEndian.Uint64(d[bytenrOff:]), nil
}
