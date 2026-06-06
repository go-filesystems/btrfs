package filesystem_btrfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// ErrNotFound is returned when a path component cannot be located.
var ErrNotFound = errors.New("not found")

// nodeHeader is the parsed form of the 101-byte on-disk Btrfs node header.
// Layout (all LE):
//
//	0x00 [32]byte  csum
//	0x20 [16]byte  fsid
//	0x30  uint64   bytenr (logical addr of this node)
//	0x38  uint64   flags (bottom bit = 1 for leaf)
//	0x40 [16]byte  chunk_tree_uuid
//	0x50  uint64   generation
//	0x58  uint64   owner       — was missing from this struct pre-2026-05-21,
//	                             causing nritems/level to be read 8 bytes too low
//	0x60  uint32   nritems (number of items / key-ptrs)
//	0x64  uint8    level (0 = leaf)
//	0x65           start of items / key-ptrs
type nodeHeader struct {
	logAddr uint64
	nItems  uint32
	level   uint8
}

const nodeHdrSize = 0x65 // 101 bytes

func parseNodeHeader(buf []byte) nodeHeader {
	le := binary.LittleEndian
	return nodeHeader{
		logAddr: le.Uint64(buf[0x30:]),
		nItems:  le.Uint32(buf[0x60:]),
		level:   buf[0x64],
	}
}

// key is a Btrfs on-disk key (objectid:8 LE + type:1 + offset:8 LE).
type key struct {
	objID  uint64
	typ    uint8
	offset uint64
}

func readKey(buf []byte) key {
	le := binary.LittleEndian
	return key{
		objID:  le.Uint64(buf[0:]),
		typ:    buf[8],
		offset: le.Uint64(buf[9:]),
	}
}

// keyPtrSize is the size of one key-pointer pair in an internal node.
// key(17) + blockptr(8) + generation(8) = 33 bytes.
const keyPtrSize = 33

// itemSize is the size of one item descriptor in a leaf node.
// key(17) + data_offset(4) + data_size(4) = 25 bytes.
const itemSize = 25

// leafItem is a parsed leaf item descriptor.
type leafItem struct {
	k        key
	dataOff  uint32 // relative to end of header (nodeHdrSize)
	dataSize uint32
}

func parseLeafItems(buf []byte, n uint32) []leafItem {
	items := make([]leafItem, 0, n)
	le := binary.LittleEndian
	for i := uint32(0); i < n; i++ {
		off := nodeHdrSize + int(i)*itemSize
		if off+itemSize > len(buf) {
			break
		}
		it := leafItem{
			k:        readKey(buf[off:]),
			dataOff:  le.Uint32(buf[off+17:]),
			dataSize: le.Uint32(buf[off+21:]),
		}
		items = append(items, it)
	}
	return items
}

// itemData returns the data slice for a leaf item within the node buffer.
func (it leafItem) data(buf []byte) []byte {
	start := nodeHdrSize + int(it.dataOff)
	end := start + int(it.dataSize)
	if start < nodeHdrSize || end > len(buf) {
		return nil
	}
	return buf[start:end]
}

// readNode reads the node at the given logical address.
// It uses the chunk resolver in sb to convert logical -> physical.
func readNode(r io.ReaderAt, partOff int64, sb *superblock, logAddr uint64) ([]byte, error) {
	phys, err := sb.physAddr(partOff, logAddr)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, sb.nodeSize)
	if _, err := r.ReadAt(buf, phys); err != nil {
		return nil, fmt.Errorf("btrfs: read node at phys 0x%X: %w", phys, err)
	}
	return buf, nil
}

// searchTree searches a Btrfs B-tree rooted at rootLogAddr for the item
// with exactly (objID, typ, offset). Returns the leaf buffer and the matching
// leafItem, or ErrNotFound.
func searchTree(r io.ReaderAt, partOff int64, sb *superblock,
	rootLogAddr uint64, wantObjID uint64, wantType uint8, wantOffset uint64,
) ([]byte, leafItem, error) {
	logAddr := rootLogAddr
	for {
		buf, err := readNode(r, partOff, sb, logAddr)
		if err != nil {
			return nil, leafItem{}, err
		}
		hdr := parseNodeHeader(buf)
		if hdr.level == 0 {
			// Leaf node: linear scan for the key.
			items := parseLeafItems(buf, hdr.nItems)
			for _, it := range items {
				if it.k.objID == wantObjID && it.k.typ == wantType && it.k.offset == wantOffset {
					return buf, it, nil
				}
			}
			return nil, leafItem{}, fmt.Errorf("btrfs: key (%d %02X %d): %w",
				wantObjID, wantType, wantOffset, ErrNotFound)
		}
		// Internal node: binary search for the rightmost key-ptr <= target.
		le := binary.LittleEndian
		chosen := uint64(0)
		for i := uint32(0); i < hdr.nItems; i++ {
			off := nodeHdrSize + int(i)*keyPtrSize
			if off+keyPtrSize > len(buf) {
				break
			}
			k := readKey(buf[off:])
			if compareKeys(k, wantObjID, wantType, wantOffset) <= 0 {
				chosen = le.Uint64(buf[off+17:])
			} else {
				break
			}
		}
		if chosen == 0 {
			return nil, leafItem{}, fmt.Errorf("btrfs: key (%d %02X %d): %w",
				wantObjID, wantType, wantOffset, ErrNotFound)
		}
		logAddr = chosen
	}
}

// prefixMatch is a (key, data) pair returned by collectPrefixItems. The data
// slice is a copy owned by the match — independent of any leaf buffer.
type prefixMatch struct {
	k    key
	data []byte
}

// collectPrefixItems walks every leaf in the tree containing items with
// (objID, typ, *) keys and returns the matching items with their data
// extracted. This is the multi-leaf-safe replacement for searchTreePrefix
// and must be used by any caller that needs to enumerate items spread
// across multiple leaves after a leaf split.
func collectPrefixItems(r io.ReaderAt, partOff int64, sb *superblock,
	rootLogAddr uint64, wantObjID uint64, wantType uint8,
) ([]prefixMatch, error) {
	var out []prefixMatch
	if err := walkPrefixLeaves(r, partOff, sb, rootLogAddr, wantObjID, wantType, func(buf []byte, it leafItem) bool {
		d := it.data(buf)
		cpy := make([]byte, len(d))
		copy(cpy, d)
		out = append(out, prefixMatch{k: it.k, data: cpy})
		return true
	}); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("btrfs: no items for objID=%d type=0x%02X: %w", wantObjID, wantType, ErrNotFound)
	}
	return out, nil
}

// walkPrefixLeaves descends the B-tree visiting every leaf whose key range
// can contain (wantObjID, wantType, *) items, invoking visit for each
// matching item. The walk stops early when visit returns false.
func walkPrefixLeaves(r io.ReaderAt, partOff int64, sb *superblock,
	rootLogAddr uint64, wantObjID uint64, wantType uint8,
	visit func(buf []byte, it leafItem) bool,
) error {
	type frame struct {
		logAddr uint64
	}
	// Depth-first left-to-right traversal of all subtrees whose key range
	// overlaps the prefix.
	stack := []frame{{logAddr: rootLogAddr}}
	for len(stack) > 0 {
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		buf, err := readNode(r, partOff, sb, top.logAddr)
		if err != nil {
			return err
		}
		hdr := parseNodeHeader(buf)
		if hdr.level == 0 {
			items := parseLeafItems(buf, hdr.nItems)
			for _, it := range items {
				if it.k.objID == wantObjID && it.k.typ == wantType {
					if !visit(buf, it) {
						return nil
					}
				}
			}
			continue
		}
		le := binary.LittleEndian
		// For internal nodes we need the children whose subtree first-key is
		// ≤ the largest (objID, typ, *) and whose successor's first-key is
		// after (objID, typ, 0). Walk left-to-right but skip subtrees
		// entirely past the prefix. Push children in REVERSE so the LIFO
		// stack pops them left-to-right.
		var children []uint64
		for i := uint32(0); i < hdr.nItems; i++ {
			off := nodeHdrSize + int(i)*keyPtrSize
			if off+keyPtrSize > len(buf) {
				break
			}
			k := readKey(buf[off:])
			// Skip subtrees whose ENTIRE range is already past our prefix.
			if k.objID > wantObjID || (k.objID == wantObjID && k.typ > wantType) {
				break
			}
			childLog := le.Uint64(buf[off+17:])
			children = append(children, childLog)
		}
		for i := len(children) - 1; i >= 0; i-- {
			stack = append(stack, frame{logAddr: children[i]})
		}
	}
	return nil
}

// searchTreePrefix searches for the first leaf item with objID==wantObjID,
// typ==wantType (any offset). Returns all matching items in that leaf.
//
// Legacy single-leaf scan retained for callers that only need any matching
// leaf (e.g. cowDeletePrefix targets one leaf at a time). For callers that
// must observe every matching item across the entire tree, use
// collectPrefixItems instead.
func searchTreePrefix(r io.ReaderAt, partOff int64, sb *superblock,
	rootLogAddr uint64, wantObjID uint64, wantType uint8,
) ([]byte, []leafItem, error) {
	logAddr := rootLogAddr
	for {
		buf, err := readNode(r, partOff, sb, logAddr)
		if err != nil {
			return nil, nil, err
		}
		hdr := parseNodeHeader(buf)
		if hdr.level == 0 {
			items := parseLeafItems(buf, hdr.nItems)
			var matched []leafItem
			for _, it := range items {
				if it.k.objID == wantObjID && it.k.typ == wantType {
					matched = append(matched, it)
				}
			}
			if len(matched) == 0 {
				return nil, nil, fmt.Errorf("btrfs: no items for objID=%d type=0x%02X: %w",
					wantObjID, wantType, ErrNotFound)
			}
			return buf, matched, nil
		}
		le := binary.LittleEndian
		chosen := uint64(0)
		for i := uint32(0); i < hdr.nItems; i++ {
			off := nodeHdrSize + int(i)*keyPtrSize
			if off+keyPtrSize > len(buf) {
				break
			}
			k := readKey(buf[off:])
			if k.objID < wantObjID || (k.objID == wantObjID && k.typ <= wantType) {
				chosen = le.Uint64(buf[off+17:])
			} else {
				break
			}
		}
		if chosen == 0 {
			return nil, nil, fmt.Errorf("btrfs: no items for objID=%d type=0x%02X: %w",
				wantObjID, wantType, ErrNotFound)
		}
		logAddr = chosen
	}
}

// compareKeys returns negative/zero/positive.
func compareKeys(k key, objID uint64, typ uint8, offset uint64) int {
	if k.objID != objID {
		if k.objID < objID {
			return -1
		}
		return 1
	}
	if k.typ != typ {
		if k.typ < typ {
			return -1
		}
		return 1
	}
	if k.offset != offset {
		if k.offset < offset {
			return -1
		}
		return 1
	}
	return 0
}
