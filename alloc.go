package filesystem_btrfs

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/go-volumes/safeio"
)

// ─────────────────────────────────────────────────────────────────────────────
// Free Space Tree (v1 — block groups tracked via BLOCK_GROUP_ITEM in the
// extent tree; free extents tracked via FREE_SPACE_INFO/FREE_SPACE_BITMAP
// items in the free-space tree, or simply via the extent tree itself).
//
// For a simpler implementation that works on freshly formatted single-device
// Fedora Cloud images we maintain an in-memory free-extent list that is
// bootstrapped by scanning EXTENT_DATA items (to identify used regions)
// and subtracting from the total space reported by block-group items.
//
// This approach is sound for a low-write-amplification use case (cloud-init
// file injection): we only need to allocate a handful of new extents per image.
// ─────────────────────────────────────────────────────────────────────────────

// freeExtent represents a contiguous free physical region.
type freeExtent struct {
	physStart uint64
	size      uint64
}

// spaceManager is the in-memory free-space book-keeper.
type spaceManager struct {
	freeExts []freeExtent
	nodeSize uint32
}

// buildSpaceManager scans the extent tree and block-group items to build the
// in-memory free-space list.  We adopt the conservative approach:
//
//  1. Enumerate all CHUNK_ITEM mappings to know each DATA chunk's physical range.
//  2. Mark the entire chunk range as free.
//  3. Walk the FS_TREE to find all EXTENT_DATA items referencing regular
//     extents, and remove those physical ranges from the free list.
//  4. Also remove the fixed overhead regions (superblock copies, node blocks
//     we have already read).
//
// For simplicity we track space at node-size granularity.
func buildSpaceManager(r io.ReaderAt, partOff int64, sb *superblock, fsTreeRoot uint64) (*spaceManager, error) {
	sm := &spaceManager{nodeSize: sb.nodeSize}

	// Step 1: seed with DATA chunks.
	for _, m := range sb.sysChunks {
		// Only DATA chunks (type & 0x01 == DATA).
		// We include all chunks since SYSTEM/METADATA also supply node space.
		sm.freeExts = append(sm.freeExts, freeExtent{
			physStart: m.physStart,
			size:      m.size,
		})
	}

	// Step 2: remove well-known fixed regions.
	// Address 0 is reserved as a sparse-extent sentinel (diskBytenr==0 means no data).
	sm.remove(0, uint64(sb.nodeSize))
	// Superblock primary at 0x10000 (1 node) + mirrors at 64MiB + 256GiB.
	for _, physSB := range []uint64{0x10000, 0x4000000} {
		sm.remove(physSB, uint64(sb.nodeSize))
	}

	// Step 3: walk the entire B-tree space and reserve node blocks.
	_ = sm.reserveTreeNodes(r, partOff, sb, sb.rootLogAddr)
	_ = sm.reserveTreeNodes(r, partOff, sb, sb.chunkLogAddr)
	_ = sm.reserveTreeNodes(r, partOff, sb, fsTreeRoot)

	// Step 4: remove data extents used by files.
	if err := sm.reserveDataExtents(r, partOff, sb, fsTreeRoot); err != nil {
		return nil, fmt.Errorf("btrfs alloc: reserve data extents: %w", err)
	}

	return sm, nil
}

// reserveTreeNodes walks every node of a tree and marks its physical block as used.
func (sm *spaceManager) reserveTreeNodes(r io.ReaderAt, partOff int64, sb *superblock, rootLogAddr uint64) error {
	return walkNodeAddrs(r, partOff, sb, rootLogAddr, func(logAddr uint64) error {
		phys, err := sb.logToPhys(logAddr)
		if err != nil {
			return nil // ignore unmapped addresses
		}
		sm.remove(uint64(phys), uint64(sb.nodeSize))
		return nil
	})
}

// walkNodeAddrs calls fn for every node logical address reachable from root.
// This runs at mount time (buildSpaceManager), so it is pre-auth and bounded
// against cyclic / unbounded-depth tree geometry.
func walkNodeAddrs(r io.ReaderAt, partOff int64, sb *superblock, logAddr uint64, fn func(uint64) error) error {
	w := &addrWalk{seen: &safeio.VisitSet{}, guard: safeio.NewLoopGuard(maxTreeNodes)}
	return w.walk(r, partOff, sb, logAddr, 0, fn)
}

type addrWalk struct {
	seen  *safeio.VisitSet
	guard *safeio.LoopGuard
}

func (w *addrWalk) walk(r io.ReaderAt, partOff int64, sb *superblock, logAddr uint64, depth int, fn func(uint64) error) error {
	if depth > maxBtreeDepth {
		return fmt.Errorf("btrfs: tree depth exceeds %d: %w", maxBtreeDepth, safeio.ErrLoopLimit)
	}
	if err := w.guard.Next(); err != nil {
		return fmt.Errorf("btrfs: walkNodeAddrs: %w", err)
	}
	// A cycle would re-reserve the same nodes endlessly; stop on revisit.
	if !w.seen.Add(logAddr) {
		return nil
	}
	if err := fn(logAddr); err != nil {
		return err
	}
	buf, err := readNode(r, partOff, sb, logAddr)
	if err != nil {
		return nil // tolerate read errors (may be in a different chunk)
	}
	hdr := parseNodeHeader(buf)
	if hdr.level == 0 {
		return nil
	}
	le := binary.LittleEndian
	for i := uint32(0); i < hdr.nItems; i++ {
		off := nodeHdrSize + int(i)*keyPtrSize
		if off+keyPtrSize > len(buf) {
			break
		}
		childLog := le.Uint64(buf[off+17:])
		if childLog == 0 {
			continue // logical 0 is reserved; never a real node pointer
		}
		if err := w.walk(r, partOff, sb, childLog, depth+1, fn); err != nil {
			return err
		}
	}
	return nil
}

// reserveDataExtents marks all non-sparse data extents as used.
func (sm *spaceManager) reserveDataExtents(r io.ReaderAt, partOff int64, sb *superblock, fsTreeRoot uint64) error {
	return walkLeaves(r, partOff, sb, fsTreeRoot, func(buf []byte, items []leafItem) error {
		le := binary.LittleEndian
		for _, it := range items {
			if it.k.typ != typeExtentData {
				continue
			}
			d := it.data(buf)
			if len(d) <= extDataOffType {
				continue
			}
			if d[extDataOffType] != extentDataRegular {
				continue
			}
			if len(d) < extDataRegularSize {
				continue
			}
			diskBytenr := le.Uint64(d[extDataOffDiskBytenr:])
			diskNumBytes := le.Uint64(d[extDataOffDiskNumBytes:])
			if diskBytenr == 0 {
				continue // sparse
			}
			phys, err := sb.logToPhys(diskBytenr)
			if err != nil {
				continue
			}
			sm.remove(uint64(phys), diskNumBytes)
		}
		return nil
	})
}

// remove subtracts [start, start+size) from the free list.
func (sm *spaceManager) remove(start, size uint64) {
	if size == 0 {
		return
	}
	end := start + size
	var next []freeExtent
	for _, fe := range sm.freeExts {
		feEnd := fe.physStart + fe.size
		// No overlap.
		if end <= fe.physStart || start >= feEnd {
			next = append(next, fe)
			continue
		}
		// Left part.
		if fe.physStart < start {
			next = append(next, freeExtent{fe.physStart, start - fe.physStart})
		}
		// Right part.
		if feEnd > end {
			next = append(next, freeExtent{end, feEnd - end})
		}
	}
	sm.freeExts = next
}

// allocNodeBlock allocates a single node-sized block and returns its physical offset.
func (sm *spaceManager) allocNodeBlock() (uint64, error) {
	ns := uint64(sm.nodeSize)
	for i, fe := range sm.freeExts {
		if fe.size >= ns {
			phys := fe.physStart
			if fe.size == ns {
				sm.freeExts = append(sm.freeExts[:i], sm.freeExts[i+1:]...)
			} else {
				sm.freeExts[i].physStart += ns
				sm.freeExts[i].size -= ns
			}
			return phys, nil
		}
	}
	return 0, fmt.Errorf("btrfs: no free node-sized block available")
}

// allocDataBytes allocates at least size bytes of data space (rounded up to
// sectorSize granularity) in ONE contiguous extent and returns the physical
// offset. Returns an error when no single free extent is large enough; for
// fragmented free space the caller should fall back to allocDataBytesUpTo
// to obtain a smaller extent and stitch multiple extents together.
func (sm *spaceManager) allocDataBytes(size, sectorSize uint64) (uint64, uint64, error) {
	need := (size + sectorSize - 1) / sectorSize * sectorSize
	for i, fe := range sm.freeExts {
		if fe.size >= need {
			phys := fe.physStart
			if fe.size == need {
				sm.freeExts = append(sm.freeExts[:i], sm.freeExts[i+1:]...)
			} else {
				sm.freeExts[i].physStart += need
				sm.freeExts[i].size -= need
			}
			return phys, need, nil
		}
	}
	return 0, 0, fmt.Errorf("btrfs: no free space for %d bytes", size)
}

// allocDataBytesUpTo allocates one contiguous extent of at most maxSize bytes
// (rounded down to sectorSize), greedily picking the largest available free
// extent. Returns (phys, allocatedBytes, nil) when at least one sector is
// available, or an error when no free space remains at all. Multi-extent
// writes call this repeatedly to stitch together a fragmented allocation.
func (sm *spaceManager) allocDataBytesUpTo(maxSize, sectorSize uint64) (uint64, uint64, error) {
	if maxSize < sectorSize {
		maxSize = sectorSize
	}
	// Find the largest free extent.
	bestIdx := -1
	var bestSize uint64
	for i, fe := range sm.freeExts {
		if fe.size >= sectorSize && fe.size > bestSize {
			bestIdx = i
			bestSize = fe.size
		}
	}
	if bestIdx < 0 {
		return 0, 0, fmt.Errorf("btrfs: no free space remaining")
	}
	take := bestSize
	if take > maxSize {
		take = (maxSize / sectorSize) * sectorSize
	}
	fe := sm.freeExts[bestIdx]
	phys := fe.physStart
	if fe.size == take {
		sm.freeExts = append(sm.freeExts[:bestIdx], sm.freeExts[bestIdx+1:]...)
	} else {
		sm.freeExts[bestIdx].physStart += take
		sm.freeExts[bestIdx].size -= take
	}
	return phys, take, nil
}

// freeRange returns a physical range back to the free list.
func (sm *spaceManager) freeRange(physStart, size uint64) {
	sm.freeExts = append(sm.freeExts, freeExtent{physStart, size})
	// Simple coalesce: sort + merge.
	sm.coalesce()
}

func (sm *spaceManager) coalesce() {
	if len(sm.freeExts) < 2 {
		return
	}
	// Insertion sort by physStart (list is small).
	for i := 1; i < len(sm.freeExts); i++ {
		for j := i; j > 0 && sm.freeExts[j].physStart < sm.freeExts[j-1].physStart; j-- {
			sm.freeExts[j], sm.freeExts[j-1] = sm.freeExts[j-1], sm.freeExts[j]
		}
	}
	merged := sm.freeExts[:1]
	for _, fe := range sm.freeExts[1:] {
		last := &merged[len(merged)-1]
		if last.physStart+last.size == fe.physStart {
			last.size += fe.size
		} else {
			merged = append(merged, fe)
		}
	}
	sm.freeExts = merged
}
