package filesystem_btrfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// errLeafFull is returned by leafInsertItem when the target leaf cannot hold
// another item. cowInsert recognizes this sentinel and falls back to the
// split path.
var errLeafFull = errors.New("btrfs: leaf full")

// ─────────────────────────────────────────────────────────────────────────────
// Copy-on-Write B-tree mutations
//
// Btrfs is a purely COW filesystem: modifying any item requires allocating a
// new node, writing the modified content, and propagating the new block pointer
// up to the root — creating a new root node at the end.
//
// We implement a simplified COW path for leaf-level mutations only:
//   - insertItem: insert a new key/item pair in the FS tree leaf
//   - updateItem: replace the data of an existing item in-place (no key change)
//   - deleteItem: remove a key/item pair from a leaf
//
// For internal nodes we only handle the simple case where a leaf has room.
// If a leaf is full we split it (creating one new leaf) and update the parent.
// ─────────────────────────────────────────────────────────────────────────────

// cowCtx carries the write context for a single mutation.
type cowCtx struct {
	rw      readerWriterAt
	rwa     readerWriterAt
	partOff int64
	sb      *superblock
	sm      *spaceManager
	// generation is bumped on every write transaction.
	generation uint64
}

// readerWriterAt combines io.ReaderAt and io.WriterAt.
type readerWriterAt interface {
	io.ReaderAt
	io.WriterAt
}

// writeNode writes buf to the physical location of logAddr and updates node CRC.
// Returns the same logAddr (node was modified in-place on the COW copy).
func (c *cowCtx) writeNodeAtPhys(physAddr int64, buf []byte) error {
	updateNodeCRC(buf)
	if _, err := c.rwa.WriteAt(buf, physAddr); err != nil {
		return fmt.Errorf("btrfs: write node at phys 0x%X: %w", physAddr, err)
	}
	return nil
}

// cowNode allocates a new physical block, copies the current node into it,
// and returns (newLogAddr, newPhysAddr, newBuf, err).
func (c *cowCtx) cowNode(oldLogAddr uint64) (uint64, int64, []byte, error) {
	// Read the old node.
	oldPhys, err := c.sb.physAddr(c.partOff, oldLogAddr)
	if err != nil {
		return 0, 0, nil, err
	}
	buf := make([]byte, c.sb.nodeSize)
	if _, err := c.rwa.ReadAt(buf, oldPhys); err != nil {
		return 0, 0, nil, fmt.Errorf("btrfs: cow read node: %w", err)
	}

	// Allocate a new physical block.
	newPhys, err := c.sm.allocNodeBlock()
	if err != nil {
		return 0, 0, nil, err
	}

	// For a fresh single-device image the chunk tree maps the entirety of the
	// metadata area. We reuse the same logical address for the COW copy but
	// record the new physical address by updating the chunk map.
	// In practice, for simple file-injection use-cases this means we first
	// allocate within the same chunk. We add the new mapping to the superblock.
	newLog := c.sb.sysChunks[0].logStart + (newPhys - c.sb.sysChunks[0].physStart)

	// Update the node's generation and logical-address fields.
	le := binary.LittleEndian
	le.PutUint64(buf[0x30:], newLog)       // bytenr
	le.PutUint64(buf[0x50:], c.generation) // generation

	newCopy := make([]byte, len(buf))
	copy(newCopy, buf)
	return newLog, int64(newPhys), newCopy, nil
}

// ─── Leaf-level item operations ───────────────────────────────────────────

// leafInsertItem inserts (k, data) into the leaf at leafBuf.
// Returns an error if the leaf has no room.
func leafInsertItem(leafBuf []byte, k key, data []byte) error {
	hdr := parseNodeHeader(leafBuf)
	n := int(hdr.nItems)
	nodeSize := len(leafBuf)

	// Compute where existing items data starts (grows from end backwards).
	dataAreaEnd := nodeSize
	for i := 0; i < n; i++ {
		off := nodeHdrSize + i*itemSize
		dataOff := int(binary.LittleEndian.Uint32(leafBuf[off+17:]))
		dataEnd := nodeHdrSize + dataOff + int(binary.LittleEndian.Uint32(leafBuf[off+21:]))
		_ = dataEnd
		if dataOff < dataAreaEnd-nodeHdrSize {
			dataAreaEnd = nodeHdrSize + dataOff
		}
	}
	// Locate insertion point (sorted by key).
	insertIdx := n
	for i := 0; i < n; i++ {
		off := nodeHdrSize + i*itemSize
		ik := readKey(leafBuf[off:])
		if compareKeys(ik, k.objID, k.typ, k.offset) > 0 {
			insertIdx = i
			break
		}
	}

	// Check space: need itemSize for descriptor + len(data) for data.
	needed := itemSize + len(data)
	// Current used space: header + n*itemSize + data area.
	dataUsed := nodeSize - dataAreaEnd
	headerUsed := nodeHdrSize + n*itemSize
	free := nodeSize - headerUsed - dataUsed
	if free < needed {
		return fmt.Errorf("%w (need %d, have %d free)", errLeafFull, needed, free)
	}

	le := binary.LittleEndian

	// Snapshot the existing items' (key, data) before we overwrite descriptors,
	// so we can rebuild the leaf with its data area tightly packed in descriptor
	// order. The Linux kernel's leaf validator requires
	// item[i].offset + item[i].size == item[i-1].offset (data contiguous and in
	// reverse descriptor order); the previous in-place insert left a hole when
	// inserting in the middle, which the kernel rejects as "unexpected item end".
	type itemRec struct {
		k    key
		data []byte
	}
	recs := make([]itemRec, 0, n+1)
	for i := 0; i < n; i++ {
		off := nodeHdrSize + i*itemSize
		ik := readKey(leafBuf[off:])
		dOff := nodeHdrSize + int(le.Uint32(leafBuf[off+17:]))
		dSize := int(le.Uint32(leafBuf[off+21:]))
		d := make([]byte, dSize)
		copy(d, leafBuf[dOff:dOff+dSize])
		recs = append(recs, itemRec{ik, d})
	}
	// Insert the new record at the sorted position.
	nd := make([]byte, len(data))
	copy(nd, data)
	recs = append(recs, itemRec{})
	copy(recs[insertIdx+1:], recs[insertIdx:])
	recs[insertIdx] = itemRec{k, nd}

	// Rewrite all descriptors and pack data from the end of the node backwards.
	dataCursor := nodeSize
	for i, r := range recs {
		dataCursor -= len(r.data)
		copy(leafBuf[dataCursor:], r.data)
		descOff := nodeHdrSize + i*itemSize
		le.PutUint64(leafBuf[descOff:], r.k.objID)
		leafBuf[descOff+8] = r.k.typ
		le.PutUint64(leafBuf[descOff+9:], r.k.offset)
		le.PutUint32(leafBuf[descOff+17:], uint32(dataCursor-nodeHdrSize))
		le.PutUint32(leafBuf[descOff+21:], uint32(len(r.data)))
	}

	// Bump nritems.
	le.PutUint32(leafBuf[0x60:], uint32(n+1))
	return nil
}

// leafUpdateItem updates the data of the item at idx in-place.
// The new data must be the same size as the old data.
func leafUpdateItemSameSize(leafBuf []byte, idx int, newData []byte) error {
	off := nodeHdrSize + idx*itemSize
	dataOff := int(binary.LittleEndian.Uint32(leafBuf[off+17:]))
	dataSize := int(binary.LittleEndian.Uint32(leafBuf[off+21:]))
	if dataSize != len(newData) {
		return fmt.Errorf("btrfs: leafUpdateItemSameSize: size mismatch %d != %d", dataSize, len(newData))
	}
	copy(leafBuf[nodeHdrSize+dataOff:], newData)
	return nil
}

// leafReplaceItemData replaces the data of the item at idx. The new data may
// be a different size; this rebuilds the data area.
func leafReplaceItemData(leafBuf []byte, idx int, newData []byte) error {
	hdr := parseNodeHeader(leafBuf)
	n := int(hdr.nItems)
	nodeSize := len(leafBuf)

	// Read current item descriptor.
	descOff := nodeHdrSize + idx*itemSize
	le := binary.LittleEndian
	oldDataSize := int(le.Uint32(leafBuf[descOff+21:]))

	delta := len(newData) - oldDataSize
	if delta == 0 {
		return leafUpdateItemSameSize(leafBuf, idx, newData)
	}

	// Compute existing data area end (= the byte right after the last item descriptor).
	dataAreaEnd := nodeSize // starts conservative

	// Check capacity.
	dataUsed := 0
	for i := 0; i < n; i++ {
		off := nodeHdrSize + i*itemSize
		doff := int(le.Uint32(leafBuf[off+17:]))
		dsz := int(le.Uint32(leafBuf[off+21:]))
		end := nodeHdrSize + doff + dsz
		if end < dataAreaEnd {
			dataAreaEnd = end
		}
		dataUsed += dsz
	}
	_ = dataAreaEnd

	free := nodeSize - nodeHdrSize - n*itemSize - dataUsed
	if delta > free {
		return fmt.Errorf("btrfs: not enough space to expand item (%d vs %d free)", delta, free)
	}

	// Rebuild data area: extract all data, replace the target, repack.
	type itemSnap struct {
		k    key
		data []byte
	}
	snaps := make([]itemSnap, n)
	for i := 0; i < n; i++ {
		off := nodeHdrSize + i*itemSize
		k := readKey(leafBuf[off:])
		doff := int(le.Uint32(leafBuf[off+17:]))
		dsz := int(le.Uint32(leafBuf[off+21:]))
		d := make([]byte, dsz)
		copy(d, leafBuf[nodeHdrSize+doff:nodeHdrSize+doff+dsz])
		snaps[i] = itemSnap{k: k, data: d}
	}
	snaps[idx].data = newData

	// Rewrite into buf.
	le.PutUint32(leafBuf[0x60:], 0)
	clear(leafBuf[nodeHdrSize:])
	le.PutUint32(leafBuf[0x60:], uint32(n))
	writePtr := nodeSize
	for i, s := range snaps {
		writePtr -= len(s.data)
		copy(leafBuf[writePtr:], s.data)
		off := nodeHdrSize + i*itemSize
		le.PutUint64(leafBuf[off:], s.k.objID)
		leafBuf[off+8] = s.k.typ
		le.PutUint64(leafBuf[off+9:], s.k.offset)
		le.PutUint32(leafBuf[off+17:], uint32(writePtr-nodeHdrSize))
		le.PutUint32(leafBuf[off+21:], uint32(len(s.data)))
	}
	return nil
}

// leafDeleteItem removes the item at idx from the leaf.
//
// btrfs leaf layout: descriptors grow forward from the header, item data grows
// backward from the end of the node. Items added LATER have LOWER dataOff
// (closer to the descriptors), items added FIRST have HIGHER dataOff (at the
// very end of the node). To keep the data area tight at the END of the node
// (so leafInsertItem can correctly compute free space via the smallest
// dataOff), we compact the data area by moving the items with LOWER dataOff
// (later inserts, at lower addresses in the buffer) UPWARD by delDataSize
// bytes — closing the gap from above instead of leaving wasted bytes at the
// tail of the node. Without this, repeated delete-and-reinsert cycles leak
// space because newly inserted data keeps landing below the prior data area.
func leafDeleteItem(leafBuf []byte, idx int) {
	hdr := parseNodeHeader(leafBuf)
	n := int(hdr.nItems)
	le := binary.LittleEndian

	descOff := nodeHdrSize + idx*itemSize
	delDataOff := int(le.Uint32(leafBuf[descOff+17:]))
	delDataSize := int(le.Uint32(leafBuf[descOff+21:]))

	// Find the lowest dataOff among all items — this is the upper boundary of
	// the free space (data area starts here, growing toward the end).
	minDataOff := delDataOff
	for i := 0; i < n; i++ {
		if i == idx {
			continue
		}
		off := nodeHdrSize + i*itemSize
		doff := int(le.Uint32(leafBuf[off+17:]))
		if doff < minDataOff {
			minDataOff = doff
		}
	}

	// Shift items at LOWER addresses (higher in the buffer toward the start of
	// the data area, i.e., dataOff < delDataOff) UPWARD by delDataSize bytes,
	// closing the gap above them. Their dataOff values increase by delDataSize.
	srcStart := nodeHdrSize + minDataOff
	srcEnd := nodeHdrSize + delDataOff // exclusive: bytes [srcStart, srcEnd) move up
	dstStart := srcStart + delDataSize
	if srcEnd > srcStart {
		copy(leafBuf[dstStart:dstStart+(srcEnd-srcStart)], leafBuf[srcStart:srcEnd])
	}
	// Clear the now-vacated low end of the data area.
	clear(leafBuf[srcStart : srcStart+delDataSize])

	// Update dataOff for every surviving item that was at a lower address than
	// the deleted item. Its new dataOff is increased by delDataSize.
	for i := 0; i < n; i++ {
		if i == idx {
			continue
		}
		off := nodeHdrSize + i*itemSize
		doff := int(le.Uint32(leafBuf[off+17:]))
		if doff < delDataOff {
			le.PutUint32(leafBuf[off+17:], uint32(doff+delDataSize))
		}
	}

	// Remove the item descriptor.
	copy(leafBuf[descOff:], leafBuf[descOff+itemSize:nodeHdrSize+n*itemSize])
	clear(leafBuf[nodeHdrSize+(n-1)*itemSize : nodeHdrSize+n*itemSize])

	le.PutUint32(leafBuf[0x60:], uint32(n-1))
}

// findItemIdx returns the index of the first item matching (objID, typ, offset)
// in the given leaf, or -1 if not found.
func findItemIdx(leafBuf []byte, objID uint64, typ uint8, offset uint64) int {
	hdr := parseNodeHeader(leafBuf)
	for i := uint32(0); i < hdr.nItems; i++ {
		off := nodeHdrSize + int(i)*itemSize
		k := readKey(leafBuf[off:])
		if k.objID == objID && k.typ == typ && k.offset == offset {
			return int(i)
		}
	}
	return -1
}

// findItemIdxPrefix returns the indexes of all items matching (objID, typ)
// (any offset) in the given leaf.
func findItemIdxPrefix(leafBuf []byte, objID uint64, typ uint8) []int {
	hdr := parseNodeHeader(leafBuf)
	var out []int
	for i := uint32(0); i < hdr.nItems; i++ {
		off := nodeHdrSize + int(i)*itemSize
		k := readKey(leafBuf[off:])
		if k.objID == objID && k.typ == typ {
			out = append(out, int(i))
		}
	}
	return out
}
