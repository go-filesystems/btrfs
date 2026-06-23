package filesystem_btrfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"time"
)

func writeFile(rwaAt readerWriterAt, rws readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, fsTreeRoot *uint64,
	p string, data []byte, perm os.FileMode,
) error {
	dir, name := path.Split(path.Clean(p))
	if dir == "" {
		dir = "/"
	}
	dir = path.Clean(dir)
	parentIno, err := pathLookupIno(rwaAt, partOff, sb, *fsTreeRoot, dir)
	if err != nil {
		return fmt.Errorf("btrfs write: parent dir %q: %w", dir, err)
	}
	childObjID, childFtype, existErr := lookupDirEntry(rwaAt, partOff, sb, *fsTreeRoot, parentIno, name)
	if existErr == nil {
		if childFtype == ftDir {
			return fmt.Errorf("btrfs write: %q is a directory", p)
		}
		return overwriteFile(rwaAt, rws, partOff, sb, sm, fsTreeRoot, childObjID, data)
	}
	return createFile(rwaAt, rws, partOff, sb, sm, fsTreeRoot, parentIno, name, data, perm)
}

func isNotFoundErr(err error) bool { return errors.Is(err, ErrNotFound) }

func pathLookupIno(r io.ReaderAt, partOff int64, sb *superblock, fsTreeRoot uint64, p string) (uint64, error) {
	in, err := pathLookup(r, partOff, sb, fsTreeRoot, p)
	if err != nil {
		return 0, err
	}
	return in.num, nil
}

func nextInodeNum(r io.ReaderAt, partOff int64, sb *superblock, fsTreeRoot uint64) (uint64, error) {
	max := rootDirObjID
	_ = walkLeaves(r, partOff, sb, fsTreeRoot, func(buf []byte, items []leafItem) error {
		for _, it := range items {
			if it.k.typ == typeInodeItem && it.k.objID > max {
				max = it.k.objID
			}
		}
		return nil
	})
	return max + 1, nil
}

func nextDirIndexOffset(r io.ReaderAt, partOff int64, sb *superblock, fsTreeRoot uint64, dirIno uint64) uint64 {
	max := uint64(2)
	items, err := collectPrefixItems(r, partOff, sb, fsTreeRoot, dirIno, typeDirIndex)
	if err == nil {
		for _, m := range items {
			if m.k.objID == dirIno && m.k.typ == typeDirIndex && m.k.offset > max {
				max = m.k.offset
			}
		}
	}
	return max + 1
}

// hashDirName computes the btrfs directory-entry name hash used as the offset
// of DIR_ITEM / DIR_INDEX-paired DIR_ITEM keys. It matches the kernel's
// btrfs_name_hash(): the standard reflected CRC32c framing seeded at 1 with a
// final inversion, i.e. crc32c_raw(seed=1, name) ^ 0xFFFFFFFF. (Using a bare
// crc32c update with seed 0xFFFFFFFF/0xFFFFFFFE produces a different value that
// the kernel's tree-checker rejects as "name hash mismatch with key".)
func hashDirName(name string) uint64 {
	return uint64(crc32cSum([]byte(name), 1) ^ 0xFFFFFFFF)
}

func encodeDirItem(locationObjID uint64, locationTyp uint8, ftype uint8, name string) []byte {
	le := binary.LittleEndian
	nameBytes := []byte(name)
	buf := make([]byte, dirItemHdrSize+len(nameBytes))
	le.PutUint64(buf[0x00:], locationObjID)
	buf[0x08] = locationTyp
	le.PutUint64(buf[0x09:], 0)
	le.PutUint64(buf[0x11:], 0)
	le.PutUint16(buf[0x19:], 0)
	le.PutUint16(buf[0x1B:], uint16(len(nameBytes)))
	buf[0x1D] = ftype
	copy(buf[0x1E:], nameBytes)
	return buf
}

// encodeInodeRef returns the on-disk bytes of a single INODE_REF tuple
// (idx, name_len, name). One INODE_REF item's payload is the concatenation
// of one or more such tuples — one per directory entry referring to the
// child inode from the same parent.
func encodeInodeRef(idxOff uint64, name string) []byte {
	nameBytes := []byte(name)
	buf := make([]byte, 8+2+len(nameBytes))
	le := binary.LittleEndian
	le.PutUint64(buf[0:], idxOff)
	le.PutUint16(buf[8:], uint16(len(nameBytes)))
	copy(buf[10:], nameBytes)
	return buf
}

// inodeRefEntry is a single back-reference inside an INODE_REF item.
type inodeRefEntry struct {
	idx  uint64
	name string
}

// parseInodeRefEntries decodes the (idx, name_len, name) tuples in an
// INODE_REF item's data payload. Returns an empty slice on malformed input.
func parseInodeRefEntries(d []byte) []inodeRefEntry {
	var out []inodeRefEntry
	le := binary.LittleEndian
	for off := 0; off+10 <= len(d); {
		idx := le.Uint64(d[off:])
		nameLen := int(le.Uint16(d[off+8:]))
		if off+10+nameLen > len(d) {
			break
		}
		out = append(out, inodeRefEntry{
			idx:  idx,
			name: string(d[off+10 : off+10+nameLen]),
		})
		off += 10 + nameLen
	}
	return out
}

// encodeInodeRefEntries re-encodes a list of back-references into a single
// INODE_REF item payload.
func encodeInodeRefEntries(entries []inodeRefEntry) []byte {
	total := 0
	for _, e := range entries {
		total += 10 + len(e.name)
	}
	out := make([]byte, 0, total)
	for _, e := range entries {
		out = append(out, encodeInodeRef(e.idx, e.name)...)
	}
	return out
}

// appendInodeRef adds a (idx, name) back-reference to the INODE_REF item at
// key (childIno, INODE_REF, parentIno), creating the item when needed. If
// the same name already appears, no duplicate entry is added.
func appendInodeRef(rwaAt readerWriterAt, rws readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, fsTreeRoot *uint64,
	childIno, parentIno, idx uint64, name string,
) error {
	refKey := key{childIno, typeInodeRef, parentIno}
	if existingBuf, existingIt, serr := searchTree(rwaAt, partOff, sb, *fsTreeRoot, refKey.objID, refKey.typ, refKey.offset); serr == nil {
		entries := parseInodeRefEntries(existingIt.data(existingBuf))
		for _, e := range entries {
			if e.name == name {
				return nil // already present, idempotent
			}
		}
		entries = append(entries, inodeRefEntry{idx: idx, name: name})
		newRoot, err := cowUpdate(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, refKey, encodeInodeRefEntries(entries))
		if err != nil {
			return fmt.Errorf("append inode ref: %w", err)
		}
		*fsTreeRoot = newRoot
		return nil
	}
	newRoot, err := cowInsert(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, refKey, encodeInodeRef(idx, name))
	if err != nil {
		return fmt.Errorf("insert inode ref: %w", err)
	}
	*fsTreeRoot = newRoot
	return nil
}

// removeInodeRef drops the entry named `name` from the INODE_REF item at
// (childIno, INODE_REF, parentIno). When no entries remain, the whole item
// is cowDeleted; otherwise the trimmed payload is cowUpdated back. Absence
// of the item is tolerated silently (legacy images).
func removeInodeRef(rwaAt readerWriterAt, rws readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, fsTreeRoot *uint64,
	childIno, parentIno uint64, name string,
) error {
	refKey := key{childIno, typeInodeRef, parentIno}
	refBuf, refIt, refErr := searchTree(rwaAt, partOff, sb, *fsTreeRoot, refKey.objID, refKey.typ, refKey.offset)
	if refErr != nil {
		return nil
	}
	entries := parseInodeRefEntries(refIt.data(refBuf))
	kept := entries[:0]
	for _, e := range entries {
		if e.name != name {
			kept = append(kept, e)
		}
	}
	if len(kept) == 0 {
		newRoot, err := cowDelete(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, refKey)
		if err != nil {
			return fmt.Errorf("drop inode ref: %w", err)
		}
		*fsTreeRoot = newRoot
		return nil
	}
	newRoot, err := cowUpdate(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, refKey, encodeInodeRefEntries(kept))
	if err != nil {
		return fmt.Errorf("shrink inode ref: %w", err)
	}
	*fsTreeRoot = newRoot
	return nil
}

func encodeInodeItem(ino uint64, size uint64, mode uint16, generation uint64, nbytes uint64, nlink uint32) []byte {
	buf := make([]byte, inodeItemSize)
	le := binary.LittleEndian
	le.PutUint64(buf[inodeOffGeneration:], generation)
	le.PutUint64(buf[inodeOffTransID:], generation)
	le.PutUint64(buf[inodeOffSize:], size)
	le.PutUint64(buf[inodeOffNBytes:], nbytes)
	le.PutUint32(buf[inodeOffNLink:], nlink)
	le.PutUint32(buf[inodeOffMode:], uint32(mode))
	// We never populate the CSUM_TREE for file data, so set NODATASUM on every
	// inode we create — a real kernel mount would otherwise expect checksum
	// items for each data extent and refuse to read the file.
	le.PutUint64(buf[inodeOffFlags:], inodeFlagNoDataSum)
	now := time.Now().UTC()
	writeBtrfsTimespec(buf[inodeOffATime:], now)
	writeBtrfsTimespec(buf[inodeOffCTime:], now)
	writeBtrfsTimespec(buf[inodeOffMTime:], now)
	writeBtrfsTimespec(buf[inodeOffOTime:], now)
	return buf
}

// classifyExtent reports the on-disk shape that writeExtents will produce
// for the given payload: inline / regular / regular-sparse, and the
// associated nbytes (disk usage in bytes, sector-aligned for regular,
// always 0 for inline and sparse).
func classifyExtent(data []byte, sectorSize uint64) (extType uint8, nbytes uint64) {
	const maxInlineSize = 2048
	if len(data) == 0 {
		return extentDataInline, 0
	}
	if len(data) <= maxInlineSize {
		// btrfs sets an inline file's inode.nbytes to the inline data's
		// ram_bytes (the uncompressed length, NOT sector-aligned). `btrfs check`
		// flags "nbytes wrong" when this is left at 0.
		return extentDataInline, uint64(len(data))
	}
	if isAllZero(data) {
		return extentDataRegular, 0
	}
	n := (uint64(len(data)) + sectorSize - 1) / sectorSize * sectorSize
	return extentDataRegular, n
}

// isAllZero reports whether the byte slice contains only zero bytes. Used by
// writeExtents to detect the "sparse write" fast path.
func isAllZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// writeBtrfsTimespec writes a btrfs_timespec at dst[0:12]: sec (int64 LE) +
// nsec (uint32 LE).
func writeBtrfsTimespec(dst []byte, t time.Time) {
	le := binary.LittleEndian
	le.PutUint64(dst[0:], uint64(t.Unix()))
	le.PutUint32(dst[8:], uint32(t.Nanosecond()))
}

// bumpInodeTransIDSequence refreshes the two on-disk INODE_ITEM fields that
// real btrfs updates on every inode mutation: transid (the tx in which the
// inode was last modified) and sequence (a monotonic per-inode counter).
// The immutable `generation` field — tx of CREATION — is left alone.
func bumpInodeTransIDSequence(inodeBuf []byte, currentGeneration uint64) {
	le := binary.LittleEndian
	le.PutUint64(inodeBuf[inodeOffTransID:], currentGeneration)
	le.PutUint64(inodeBuf[inodeOffSequence:], le.Uint64(inodeBuf[inodeOffSequence:])+1)
}

// adjustDirNlink reads the INODE_ITEM for dirIno, applies the delta to its
// nlink field (clamping at 0 to avoid wraparound), bumps transid/sequence
// AND mtime+ctime, and cowUpdates it back. Used by MkDir / DeleteDir to
// maintain the "+1 per subdirectory" convention on parent directories.
// Passing delta=0 is the no-nlink-change "touch this directory" variant
// (see touchDir).
func adjustDirNlink(rwaAt readerWriterAt, rws readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, fsTreeRoot *uint64, dirIno uint64, delta int,
) error {
	buf, it, err := searchTree(rwaAt, partOff, sb, *fsTreeRoot, dirIno, typeInodeItem, 0)
	if err != nil {
		return fmt.Errorf("read parent inode %d: %w", dirIno, err)
	}
	d := make([]byte, it.dataSize)
	copy(d, it.data(buf))
	le := binary.LittleEndian
	cur := int64(le.Uint32(d[inodeOffNLink:]))
	cur += int64(delta)
	if cur < 0 {
		cur = 0
	}
	le.PutUint32(d[inodeOffNLink:], uint32(cur))
	bumpInodeTransIDSequence(d, sb.generation+1)
	now := time.Now().UTC()
	writeBtrfsTimespec(d[inodeOffCTime:], now)
	writeBtrfsTimespec(d[inodeOffMTime:], now)
	newRoot, err := cowUpdate(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{dirIno, typeInodeItem, 0}, d)
	if err != nil {
		return fmt.Errorf("update parent inode %d: %w", dirIno, err)
	}
	*fsTreeRoot = newRoot
	return nil
}

// touchDir bumps a directory's mtime/ctime (and transid/sequence) without
// changing nlink — used after operations that change the directory's
// contents (adding/removing children, renaming children within it) but do
// not change the subdirectory count.
func touchDir(rwaAt readerWriterAt, rws readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, fsTreeRoot *uint64, dirIno uint64,
) error {
	return adjustDirNlink(rwaAt, rws, partOff, sb, sm, fsTreeRoot, dirIno, 0)
}

// dirEntrySizeDelta is the amount a directory's i_size changes when an entry of
// the given name is added (positive) — btrfs counts each entry's name length
// twice (once for its DIR_ITEM, once for its DIR_INDEX). `btrfs check` flags
// "dir isize wrong" when this is not maintained.
func dirEntrySizeDelta(name string) int64 { return 2 * int64(len(name)) }

// adjustDirSize applies delta (which may be negative) to a directory inode's
// i_size field, clamping at 0, and cowUpdates it back. Caller passes
// dirEntrySizeDelta(name) when adding an entry and its negation when removing.
func adjustDirSize(rwaAt readerWriterAt, rws readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, fsTreeRoot *uint64, dirIno uint64, delta int64,
) error {
	if delta == 0 {
		return nil
	}
	buf, it, err := searchTree(rwaAt, partOff, sb, *fsTreeRoot, dirIno, typeInodeItem, 0)
	if err != nil {
		return fmt.Errorf("read dir inode %d for size: %w", dirIno, err)
	}
	d := make([]byte, it.dataSize)
	copy(d, it.data(buf))
	le := binary.LittleEndian
	cur := int64(le.Uint64(d[inodeOffSize:])) + delta
	if cur < 0 {
		cur = 0
	}
	le.PutUint64(d[inodeOffSize:], uint64(cur))
	newRoot, err := cowUpdate(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{dirIno, typeInodeItem, 0}, d)
	if err != nil {
		return fmt.Errorf("update dir inode %d size: %w", dirIno, err)
	}
	*fsTreeRoot = newRoot
	return nil
}

// writeExtents writes the file body for inode ino as one or more EXTENT_DATA
// items. Tries first to allocate a single contiguous extent (the common
// case); falls back to greedy multi-extent allocation when no single free
// extent is large enough, so fragmented free space can still satisfy a
// modest-sized write. An empty data slice produces a single inline-empty
// EXTENT_DATA item to preserve the "regular file with size 0" shape.
func writeExtents(rwaAt readerWriterAt, rws readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, fsTreeRoot *uint64,
	ino uint64, data []byte, generation uint64,
) error {
	if len(data) == 0 {
		extData := encodeExtentDataInline(nil, generation)
		newRoot, err := cowInsert(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{ino, typeExtentData, 0}, extData)
		if err != nil {
			return fmt.Errorf("insert empty extent: %w", err)
		}
		*fsTreeRoot = newRoot
		return nil
	}
	// Inline fast path: tiny files live entirely inside the EXTENT_DATA
	// item, avoiding any sector allocation. We mirror the kernel default of
	// max_inline=2048 — beyond that the leaf bloat is not worth the saved
	// sector. Inline is only valid for files that fit in one extent at
	// offset 0, which our writeFile interface always produces anyway.
	const maxInlineSize = 2048
	if len(data) <= maxInlineSize {
		extData := encodeExtentDataInline(data, generation)
		newRoot, err := cowInsert(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{ino, typeExtentData, 0}, extData)
		if err != nil {
			return fmt.Errorf("insert inline extent: %w", err)
		}
		*fsTreeRoot = newRoot
		return nil
	}
	// Sparse fast path: an all-zero payload doesn't need disk storage at
	// all. The reader treats diskBytenr=0 as zero-filled and produces the
	// right bytes without ever touching disk.
	if isAllZero(data) {
		extData := encodeExtentData(0, 0, 0, uint64(len(data)), generation)
		newRoot, err := cowInsert(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{ino, typeExtentData, 0}, extData)
		if err != nil {
			return fmt.Errorf("insert sparse extent: %w", err)
		}
		*fsTreeRoot = newRoot
		return nil
	}
	// Single-extent fast path.
	if physData, physSize, err := sm.allocDataBytes(uint64(len(data)), uint64(sb.sectorSize)); err == nil {
		if _, werr := rwaAt.WriteAt(data, partOff+int64(physData)); werr != nil {
			return fmt.Errorf("write data: %w", werr)
		}
		logData := physToLog(sb, physData)
		extData := encodeExtentData(logData, physSize, 0, uint64(len(data)), generation)
		newRoot, ierr := cowInsert(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{ino, typeExtentData, 0}, extData)
		if ierr != nil {
			return fmt.Errorf("insert extent: %w", ierr)
		}
		*fsTreeRoot = newRoot
		return nil
	}
	// Multi-extent fallback: greedy fill from the largest free extents.
	remaining := uint64(len(data))
	fileOffset := uint64(0)
	for remaining > 0 {
		phys, allocated, err := sm.allocDataBytesUpTo(remaining, uint64(sb.sectorSize))
		if err != nil {
			return fmt.Errorf("alloc data (%d bytes left): %w", remaining, err)
		}
		chunk := allocated
		if chunk > remaining {
			chunk = remaining
		}
		if _, werr := rwaAt.WriteAt(data[fileOffset:fileOffset+chunk], partOff+int64(phys)); werr != nil {
			return fmt.Errorf("write data chunk at file offset %d: %w", fileOffset, werr)
		}
		logData := physToLog(sb, phys)
		extData := encodeExtentData(logData, allocated, 0, chunk, generation)
		newRoot, ierr := cowInsert(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{ino, typeExtentData, fileOffset}, extData)
		if ierr != nil {
			return fmt.Errorf("insert extent at file offset %d: %w", fileOffset, ierr)
		}
		*fsTreeRoot = newRoot
		fileOffset += chunk
		remaining -= chunk
	}
	return nil
}

func createFile(rwaAt readerWriterAt, rws readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, fsTreeRoot *uint64,
	parentIno uint64, name string, data []byte, perm os.FileMode,
) error {
	mode := uint16(0o100000 | (perm & 0o777))
	return createInodeWithDirEntry(rwaAt, rws, partOff, sb, sm, fsTreeRoot, parentIno, name, data, mode, ftRegFile, "create")
}

// createInodeWithDirEntry is the shared backbone used by createFile and
// createSymlink. It allocates a new inode, writes the inode body via
// writeExtents (inline / sparse / regular), and wires up the parent
// directory entry plus the INODE_REF back-pointer.
func createInodeWithDirEntry(rwaAt readerWriterAt, rws readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, fsTreeRoot *uint64,
	parentIno uint64, name string, data []byte, mode uint16, fileType uint8, opLabel string,
) error {
	ino, _ := nextInodeNum(rwaAt, partOff, sb, *fsTreeRoot)
	generation := sb.generation + 1
	_, nbytes := classifyExtent(data, uint64(sb.sectorSize))
	inodeBuf := encodeInodeItem(ino, uint64(len(data)), mode, generation, nbytes, 1)
	newRoot, err := cowInsert(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{ino, typeInodeItem, 0}, inodeBuf)
	if err != nil {
		return fmt.Errorf("btrfs %s: insert inode: %w", opLabel, err)
	}
	*fsTreeRoot = newRoot
	if err := writeExtents(rwaAt, rws, partOff, sb, sm, fsTreeRoot, ino, data, generation); err != nil {
		return fmt.Errorf("btrfs %s: write extents: %w", opLabel, err)
	}
	idxOff := nextDirIndexOffset(rwaAt, partOff, sb, *fsTreeRoot, parentIno)
	dirItemBuf := encodeDirItem(ino, typeInodeItem, fileType, name)
	newRoot, err = cowInsert(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{parentIno, typeDirIndex, idxOff}, dirItemBuf)
	if err != nil {
		return fmt.Errorf("btrfs %s: insert dir index: %w", opLabel, err)
	}
	*fsTreeRoot = newRoot
	nameHash := hashDirName(name)
	newRoot, err = cowInsert(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{parentIno, typeDirItem, nameHash}, dirItemBuf)
	if err != nil {
		return fmt.Errorf("btrfs %s: insert dir item: %w", opLabel, err)
	}
	*fsTreeRoot = newRoot
	refBuf := encodeInodeRef(idxOff, name)
	newRoot, err = cowInsert(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{ino, typeInodeRef, parentIno}, refBuf)
	if err != nil {
		return fmt.Errorf("btrfs %s: insert inode ref: %w", opLabel, err)
	}
	*fsTreeRoot = newRoot
	// Adding a child changes the parent's contents — grow its i_size by the
	// btrfs per-entry amount and bump its mtime/ctime.
	if err := adjustDirSize(rwaAt, rws, partOff, sb, sm, fsTreeRoot, parentIno, dirEntrySizeDelta(name)); err != nil {
		return fmt.Errorf("btrfs %s: grow parent size: %w", opLabel, err)
	}
	if err := touchDir(rwaAt, rws, partOff, sb, sm, fsTreeRoot, parentIno); err != nil {
		return fmt.Errorf("btrfs %s: touch parent: %w", opLabel, err)
	}
	return updateFsTreeRoot(rwaAt, partOff, sb, sm, *fsTreeRoot)
}

func overwriteFile(rwaAt readerWriterAt, rws readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, fsTreeRoot *uint64,
	ino uint64, newData []byte,
) error {
	generation := sb.generation + 1
	freeInodeExtents(rwaAt, partOff, sb, sm, *fsTreeRoot, ino)
	newRoot, err := cowDeletePrefix(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, ino, typeExtentData)
	if err != nil {
		return fmt.Errorf("btrfs overwrite: remove extents: %w", err)
	}
	*fsTreeRoot = newRoot
	if err := writeExtents(rwaAt, rws, partOff, sb, sm, fsTreeRoot, ino, newData, generation); err != nil {
		return fmt.Errorf("btrfs overwrite: write extents: %w", err)
	}
	inBuf, it, ierr := searchTree(rwaAt, partOff, sb, *fsTreeRoot, ino, typeInodeItem, 0)
	if ierr == nil {
		d := make([]byte, it.dataSize)
		copy(d, it.data(inBuf))
		binary.LittleEndian.PutUint64(d[inodeOffSize:], uint64(len(newData)))
		// Refresh nbytes so external tools (du, btrfs check) see the right
		// on-disk usage after a resize / sparsification of the same inode.
		_, nbytes := classifyExtent(newData, uint64(sb.sectorSize))
		binary.LittleEndian.PutUint64(d[inodeOffNBytes:], nbytes)
		bumpInodeTransIDSequence(d, sb.generation+1)
		// Bump mtime and ctime; leave atime/otime alone (otime is birth time;
		// atime is access — not updated on write).
		now := time.Now().UTC()
		writeBtrfsTimespec(d[inodeOffCTime:], now)
		writeBtrfsTimespec(d[inodeOffMTime:], now)
		newRoot, err = cowUpdate(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{ino, typeInodeItem, 0}, d)
		if err == nil {
			*fsTreeRoot = newRoot
		}
	}
	return updateFsTreeRoot(rwaAt, partOff, sb, sm, *fsTreeRoot)
}

func freeInodeExtents(r io.ReaderAt, partOff int64, sb *superblock, sm *spaceManager, fsTreeRoot uint64, ino uint64) {
	items, err := collectPrefixItems(r, partOff, sb, fsTreeRoot, ino, typeExtentData)
	if err != nil {
		return
	}
	le := binary.LittleEndian
	for _, m := range items {
		d := m.data
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
			continue
		}
		phys, err := sb.logToPhys(diskBytenr)
		if err != nil {
			continue
		}
		sm.freeRange(uint64(phys), diskNumBytes)
	}
}

func encodeExtentData(diskBytenr, diskNumBytes, fileOffset, numBytes, generation uint64) []byte {
	buf := make([]byte, extDataRegularSize)
	le := binary.LittleEndian
	le.PutUint64(buf[0x00:], generation)
	le.PutUint64(buf[0x08:], numBytes)
	buf[0x10] = compressionNone
	buf[0x14] = extentDataRegular
	le.PutUint64(buf[0x15:], diskBytenr)
	le.PutUint64(buf[0x1D:], diskNumBytes)
	le.PutUint64(buf[0x25:], fileOffset)
	le.PutUint64(buf[0x2D:], numBytes)
	return buf
}

func encodeExtentDataInline(inlineData []byte, generation uint64) []byte {
	buf := make([]byte, extDataHdrSize+len(inlineData))
	le := binary.LittleEndian
	le.PutUint64(buf[0x00:], generation)
	le.PutUint64(buf[0x08:], uint64(len(inlineData)))
	buf[0x14] = extentDataInline
	copy(buf[extDataHdrSize:], inlineData)
	return buf
}

func updateFsTreeRoot(rwaAt readerWriterAt, partOff int64, sb *superblock, sm *spaceManager, newFsRoot uint64) error {
	leafBuf, it, err := searchTree(rwaAt, partOff, sb, sb.rootLogAddr, fsTreeObjID, typeRootItem, 0)
	if err == nil {
		d := make([]byte, it.dataSize)
		copy(d, it.data(leafBuf))
		// btrfs_root_item: bytenr at 0xB0, generation at 0xA0. generation_v2
		// (0xEF) must track generation or the kernel warns "mismatching
		// generation and generation_v2" and resets the new fields on mount.
		//
		// The ROOT_ITEM.generation must equal the FS-root NODE's header
		// generation (the kernel's parent_transid check compares them). In the
		// normal write path the FS root is COW-reseated to sb.generation+1, so
		// that is the value; but a metadata-only shrink (resize_reloc.go) does not
		// touch the FS tree, leaving its root node at an older generation. Read the
		// node's actual header generation rather than assuming sb.generation+1, so
		// the two stay coherent in both cases.
		fsRootGen := fsRootNodeGeneration(rwaAt, partOff, sb, newFsRoot, sb.generation+1)
		binary.LittleEndian.PutUint64(d[0xB0:], newFsRoot)
		binary.LittleEndian.PutUint64(d[rootItemOffGeneration:], fsRootGen)
		if len(d) > rootItemOffGenerationV2+8 {
			binary.LittleEndian.PutUint64(d[rootItemOffGenerationV2:], fsRootGen)
		}
		// ROOT_ITEM.level must equal the FS-root NODE's header level; the kernel
		// cross-checks them ("root [N 0] level X does not match Y") and rejects the
		// filesystem otherwise. The FS tree grows to multiple levels once it
		// outgrows a single leaf, so read the live level rather than assuming 0.
		if len(d) > rootItemOffLevel {
			d[rootItemOffLevel] = fsRootNodeLevel(rwaAt, partOff, sb, newFsRoot)
		}
		newRootRoot, rerr := cowUpdate(nil, rwaAt, partOff, sb, sm, sb.rootLogAddr, key{fsTreeObjID, typeRootItem, 0}, d)
		if rerr == nil {
			sb.rootLogAddr = newRootRoot
		}
	}
	// Recompute the EXTENT_TREE so it exactly describes the live tree blocks and
	// data extents after this transaction's COW churn. Done before writing the
	// superblock so bytes_used and the extent tree commit together. Best-effort:
	// a rebuild error leaves the prior extent tree in place (still mountable),
	// so we surface it but do not abort the user's already-applied write.
	if rebErr := rebuildExtentTree(rwaAt, partOff, sb, sm); rebErr != nil {
		return fmt.Errorf("btrfs: rebuild extent tree: %w", rebErr)
	}
	return writeSuperblock(rwaAt, partOff, sb, newFsRoot)
}

// fsRootNodeGeneration returns the header generation (0x50) of the FS-tree root
// node at logAddr, or fallback when the node cannot be read. The FS ROOT_ITEM
// generation must match this value for the kernel's parent_transid check.
func fsRootNodeGeneration(rwaAt readerWriterAt, partOff int64, sb *superblock, logAddr, fallback uint64) uint64 {
	buf, err := readNode(rwaAt, partOff, sb, logAddr)
	if err != nil {
		return fallback
	}
	return binary.LittleEndian.Uint64(buf[0x50:])
}

// fsRootNodeLevel returns the header level (0x64) of the node at logAddr, or 0
// when it cannot be read. The FS ROOT_ITEM level must match this value.
func fsRootNodeLevel(rwaAt readerWriterAt, partOff int64, sb *superblock, logAddr uint64) uint8 {
	buf, err := readNode(rwaAt, partOff, sb, logAddr)
	if err != nil {
		return 0
	}
	return parseNodeHeader(buf).level
}

func writeSuperblock(rwaAt readerWriterAt, partOff int64, sb *superblock, newFsRoot uint64) error {
	buf := make([]byte, sbfSize)
	if _, err := rwaAt.ReadAt(buf, partOff+superblockOffset); err != nil {
		return fmt.Errorf("btrfs: read superblock for update: %w", err)
	}
	le := binary.LittleEndian
	le.PutUint64(buf[sbfRootLogAddr:], sb.rootLogAddr)
	gen := le.Uint64(buf[sbfGeneration:])
	le.PutUint64(buf[sbfGeneration:], gen+1)
	updateSuperblockCRC(buf)
	if _, err := rwaAt.WriteAt(buf, partOff+superblockOffset); err != nil {
		return fmt.Errorf("btrfs: write superblock: %w", err)
	}
	sb.generation = gen + 1
	return nil
}

type sortedByOffset []leafItem

func (s sortedByOffset) Len() int           { return len(s) }
func (s sortedByOffset) Less(i, j int) bool { return s[i].k.offset < s[j].k.offset }
func (s sortedByOffset) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
func sortExtentItems(items []leafItem)      { sort.Sort(sortedByOffset(items)) }
