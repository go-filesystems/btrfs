package filesystem_btrfs

import (
	"encoding/binary"
	"fmt"
	"io"
	"path"
	"strings"

	filesystem "github.com/go-filesystems/interface"
)

// DIR_ITEM / DIR_INDEX on-disk layout (btrfs_dir_item):
//
//	0x00 [17]byte  location key (objectid:8 + type:1 + offset:8)
//	0x11  uint64   transid
//	0x19  uint16   data_len
//	0x1B  uint16   name_len
//	0x1D  uint8    type (file type)
//	0x1E  name[name_len] (no NUL)
const dirItemHdrSize = 0x1E // 30 bytes before the name

// parseDirItems parses one or more btrfs_dir_item structs packed in data.
// Returns (childObjID, fileType) for the entry whose name matches.
func parseDirItems(data []byte, name string) (uint64, uint8, error) {
	le := binary.LittleEndian
	off := 0
	for off+dirItemHdrSize <= len(data) {
		childObjID := le.Uint64(data[off:])
		nameLen := int(le.Uint16(data[off+0x1B:]))
		ftype := data[off+0x1D]
		nameEnd := off + dirItemHdrSize + nameLen
		if nameEnd > len(data) {
			break
		}
		entName := string(data[off+dirItemHdrSize : nameEnd])
		if entName == name {
			return childObjID, ftype, nil
		}
		// Advance past this entry (header + name + data_len).
		dataLen := int(le.Uint16(data[off+0x19:]))
		off = nameEnd + dataLen
	}
	return 0, 0, ErrNotFound
}

// parseDirItemsAll parses all btrfs_dir_item entries from data.
func parseDirItemsAll(data []byte) []filesystem.DirEntry {
	le := binary.LittleEndian
	off := 0
	var entries []filesystem.DirEntry
	for off+dirItemHdrSize <= len(data) {
		childObjID := le.Uint64(data[off:])
		nameLen := int(le.Uint16(data[off+0x1B:]))
		ftype := data[off+0x1D]
		nameEnd := off + dirItemHdrSize + nameLen
		if nameEnd > len(data) {
			break
		}
		name := string(data[off+dirItemHdrSize : nameEnd])
		if name != "." && name != ".." {
			entries = append(entries, filesystem.NewDirEntry(childObjID, name, ftype))
		}
		dataLen := int(le.Uint16(data[off+0x19:]))
		off = nameEnd + dataLen
	}
	return entries
}

// lookupDirEntry searches a directory inode for name. First tries an exact
// DIR_ITEM lookup by name hash (single B-tree descent); falls back to a
// multi-leaf scan of DIR_INDEX items when the hash collides or the dir item
// is missing.
func lookupDirEntry(r io.ReaderAt, partOff int64, sb *superblock, fsTreeRoot uint64, dirIno uint64, name string) (uint64, uint8, error) {
	// Fast path: exact DIR_ITEM lookup by name hash.
	nameHash := hashDirName(name)
	leafBuf, it, err := searchTree(r, partOff, sb, fsTreeRoot, dirIno, typeDirItem, nameHash)
	if err == nil {
		d := it.data(leafBuf)
		if objID, ftype, e := parseDirItems(d, name); e == nil {
			return objID, ftype, nil
		}
	}
	// Multi-leaf scan over DIR_INDEX items (covers hash collisions and the
	// case where DIR_ITEM is absent — e.g. before a directory rehash).
	items, err := collectPrefixItems(r, partOff, sb, fsTreeRoot, dirIno, typeDirIndex)
	if err == nil {
		for _, m := range items {
			if objID, ftype, e := parseDirItems(m.data, name); e == nil {
				return objID, ftype, nil
			}
		}
	}
	return 0, 0, fmt.Errorf("btrfs: %q not found in inode %d: %w", name, dirIno, ErrNotFound)
}

// pathLookup resolves an absolute path and returns the inode number.
func pathLookup(r io.ReaderAt, partOff int64, sb *superblock, fsTreeRoot uint64, p string) (*inodeItem, error) {
	p = path.Clean(p)
	curIno := rootDirObjID
	if p == "/" || p == "." {
		return readInode(r, partOff, sb, fsTreeRoot, curIno)
	}
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for _, name := range parts {
		childObjID, _, err := lookupDirEntry(r, partOff, sb, fsTreeRoot, curIno, name)
		if err != nil {
			return nil, fmt.Errorf("btrfs: %q: %w", p, err)
		}
		curIno = childObjID
	}
	in, err := readInode(r, partOff, sb, fsTreeRoot, curIno)
	if err != nil {
		return nil, fmt.Errorf("btrfs: stat inode for %q: %w", p, err)
	}
	return in, nil
}

// readDir returns all non-dot entries of the directory at dirIno.
func readDir(r io.ReaderAt, partOff int64, sb *superblock, fsTreeRoot uint64, dirIno uint64) ([]filesystem.DirEntry, error) {
	items, err := collectPrefixItems(r, partOff, sb, fsTreeRoot, dirIno, typeDirIndex)
	if err != nil {
		// Empty dir or no DIR_INDEX items.
		return nil, nil
	}
	var entries []filesystem.DirEntry
	for _, m := range items {
		entries = append(entries, parseDirItemsAll(m.data)...)
	}
	return entries, nil
}
