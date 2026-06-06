package filesystem_btrfs

import (
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

// unlinkInodeOnly drops one directory entry (parentIno/name) referring to a
// multi-link inode and decrements the inode's nlink count, leaving the inode
// and its data intact. Used by removeInode when the target still has other
// links.
func unlinkInodeOnly(rwaAt readerWriterAt, rws readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, fsTreeRoot *uint64,
	ino uint64, parentIno uint64, name string,
) error {
	// Remove DIR_INDEX for this name in parent (multi-leaf scan).
	items, _ := collectPrefixItems(rwaAt, partOff, sb, *fsTreeRoot, parentIno, typeDirIndex)
	for _, m := range items {
		d := m.data
		if len(d) < dirItemHdrSize {
			continue
		}
		nameLen := int(d[0x1B]) | int(d[0x1C])<<8
		if dirItemHdrSize+nameLen > len(d) {
			continue
		}
		if string(d[dirItemHdrSize:dirItemHdrSize+nameLen]) == name {
			newRoot, derr := cowDelete(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, m.k)
			if derr != nil {
				return fmt.Errorf("btrfs unlink: dir index: %w", derr)
			}
			*fsTreeRoot = newRoot
			break
		}
	}

	// Remove DIR_ITEM for this name by hash. Tolerate absence.
	nameHash := hashDirName(name)
	newRoot, err := cowDelete(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{parentIno, typeDirItem, nameHash})
	if err != nil && !isNotFoundErr(err) {
		return fmt.Errorf("btrfs unlink: dir item: %w", err)
	}
	if err == nil {
		*fsTreeRoot = newRoot
	}

	// Drop this name's back-reference; if other hardlinks under the same
	// parent remain, the trimmed INODE_REF item is kept rather than fully
	// deleted. Absence of the item is tolerated (legacy images).
	if err := removeInodeRef(rwaAt, rws, partOff, sb, sm, fsTreeRoot, ino, parentIno, name); err != nil {
		return fmt.Errorf("btrfs unlink: %w", err)
	}

	// Decrement nlink and bump ctime on the inode item.
	inBuf, it, ierr := searchTree(rwaAt, partOff, sb, *fsTreeRoot, ino, typeInodeItem, 0)
	if ierr != nil {
		return fmt.Errorf("btrfs unlink: refetch inode: %w", ierr)
	}
	d := make([]byte, it.dataSize)
	copy(d, it.data(inBuf))
	le := binary.LittleEndian
	cur := le.Uint32(d[inodeOffNLink:])
	if cur > 0 {
		le.PutUint32(d[inodeOffNLink:], cur-1)
	}
	bumpInodeTransIDSequence(d, sb.generation+1)
	writeBtrfsTimespec(d[inodeOffCTime:], time.Now().UTC())
	newRoot, err = cowUpdate(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{ino, typeInodeItem, 0}, d)
	if err != nil {
		return fmt.Errorf("btrfs unlink: update inode: %w", err)
	}
	*fsTreeRoot = newRoot

	// Parent contents shrank by one entry — bump its mtime/ctime.
	if err := touchDir(rwaAt, rws, partOff, sb, sm, fsTreeRoot, parentIno); err != nil {
		return fmt.Errorf("btrfs unlink: touch parent: %w", err)
	}
	return updateFsTreeRoot(rwaAt, partOff, sb, sm, *fsTreeRoot)
}

func deleteFile(rwaAt readerWriterAt, rws readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, fsTreeRoot *uint64, p string,
) error {
	dir, name := splitPath(p)
	parentIno, err := pathLookupIno(rwaAt, partOff, sb, *fsTreeRoot, dir)
	if err != nil {
		return fmt.Errorf("btrfs delete: parent %q: %w", dir, err)
	}
	childObjID, childType, err := lookupDirEntry(rwaAt, partOff, sb, *fsTreeRoot, parentIno, name)
	if err != nil {
		return fmt.Errorf("btrfs delete: %q: %w", p, err)
	}
	// Regular files and symlinks are removed the same way; directories must
	// go through DeleteDir which recurses into their contents.
	if childType == ftDir {
		return fmt.Errorf("btrfs delete: %q is a directory", p)
	}
	return removeInode(rwaAt, rws, partOff, sb, sm, fsTreeRoot, childObjID, parentIno, name)
}

func deleteDir(rwaAt readerWriterAt, rws readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, fsTreeRoot *uint64, p string,
) error {
	dir, name := splitPath(p)
	parentIno, err := pathLookupIno(rwaAt, partOff, sb, *fsTreeRoot, dir)
	if err != nil {
		return fmt.Errorf("btrfs deletedir: parent %q: %w", dir, err)
	}
	childObjID, childType, err := lookupDirEntry(rwaAt, partOff, sb, *fsTreeRoot, parentIno, name)
	if err != nil {
		return fmt.Errorf("btrfs deletedir: %q: %w", p, err)
	}
	if childType != ftDir {
		return fmt.Errorf("btrfs deletedir: %q is not a directory", p)
	}
	// Recursively remove all contents before removing the directory itself.
	if err := purgeDirContents(rwaAt, rws, partOff, sb, sm, fsTreeRoot, childObjID); err != nil {
		return err
	}
	// Remove "." and ".." DIR_INDEX entries from the child dir itself
	newRoot, err := cowDeletePrefix(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, childObjID, typeDirIndex)
	if err != nil {
		return fmt.Errorf("btrfs deletedir: remove dot entries: %w", err)
	}
	*fsTreeRoot = newRoot
	if err := removeInode(rwaAt, rws, partOff, sb, sm, fsTreeRoot, childObjID, parentIno, name); err != nil {
		return err
	}
	// The disappearing subdirectory's "..": one fewer link to the parent.
	return adjustDirNlink(rwaAt, rws, partOff, sb, sm, fsTreeRoot, parentIno, -1)
}

// purgeDirContents recursively removes all files and subdirectories inside the
// directory at dirIno, freeing their inodes and parent directory entries.
// Dot/dotdot entries of dirIno itself are NOT removed here.
func purgeDirContents(rwaAt readerWriterAt, rws readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, fsTreeRoot *uint64, dirIno uint64,
) error {
	entries, _ := readDir(rwaAt, partOff, sb, *fsTreeRoot, dirIno)
	for _, e := range entries {
		childObjID := e.Inode()
		childType := e.FileType()
		if childType == ftDir {
			if err := purgeDirContents(rwaAt, rws, partOff, sb, sm, fsTreeRoot, childObjID); err != nil {
				return err
			}
			newRoot, err := cowDeletePrefix(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, childObjID, typeDirIndex)
			if err != nil {
				return fmt.Errorf("btrfs purgedir: dot entries in %q: %w", e.Name(), err)
			}
			*fsTreeRoot = newRoot
		}
		if err := removeInode(rwaAt, rws, partOff, sb, sm, fsTreeRoot, childObjID, dirIno, e.Name()); err != nil {
			return err
		}
	}
	return nil
}

// countDirEntries counts non-dot entries in a directory.
func countDirEntries(r io.ReaderAt, partOff int64, sb *superblock, fsTreeRoot uint64, dirIno uint64) int {
	items, err := collectPrefixItems(r, partOff, sb, fsTreeRoot, dirIno, typeDirIndex)
	if err != nil {
		return 0
	}
	count := 0
	for _, m := range items {
		// offset 1 = ".", offset 2 = ".."
		if m.k.offset > 2 {
			count++
		}
	}
	return count
}

// removeInode removes a file or directory inode and its parent directory entries.
func removeInode(rwaAt readerWriterAt, rws readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, fsTreeRoot *uint64,
	ino uint64, parentIno uint64, name string,
) error {
	// Hardlink-aware path: when the target inode still has other links the
	// caller is only unlinking THIS name, not the inode itself. Remove the
	// directory entries + the matching INODE_REF, decrement nlink, leave the
	// data and inode in place.
	if in, ierr := readInode(rwaAt, partOff, sb, *fsTreeRoot, ino); ierr == nil && in.nlink > 1 {
		return unlinkInodeOnly(rwaAt, rws, partOff, sb, sm, fsTreeRoot, ino, parentIno, name)
	}

	// Free data blocks
	freeInodeExtents(rwaAt, partOff, sb, sm, *fsTreeRoot, ino)

	// Remove all EXTENT_DATA items
	newRoot, err := cowDeletePrefix(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, ino, typeExtentData)
	if err != nil {
		return fmt.Errorf("btrfs remove: extent data: %w", err)
	}
	*fsTreeRoot = newRoot

	// Remove any XATTR_ITEM(ino, *). Tolerate "not found" — xattrs are
	// optional and most inodes have none.
	newRoot, err = cowDeletePrefix(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, ino, typeXattrItem)
	if err != nil && !isNotFoundErr(err) {
		return fmt.Errorf("btrfs remove: xattr items: %w", err)
	}
	if err == nil {
		*fsTreeRoot = newRoot
	}

	// Remove INODE_REF(parentIno) for this inode. Tolerate absence — older
	// images written by previous versions of this driver did not record the
	// back-reference.
	newRoot, err = cowDelete(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{ino, typeInodeRef, parentIno})
	if err != nil && !isNotFoundErr(err) {
		return fmt.Errorf("btrfs remove: inode ref: %w", err)
	}
	if err == nil {
		*fsTreeRoot = newRoot
	}

	// Remove INODE_ITEM
	newRoot, err = cowDelete(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{ino, typeInodeItem, 0})
	if err != nil {
		return fmt.Errorf("btrfs remove: inode item: %w", err)
	}
	*fsTreeRoot = newRoot

	// Remove DIR_INDEX from parent: multi-leaf scan for the matching name.
	items, _ := collectPrefixItems(rwaAt, partOff, sb, *fsTreeRoot, parentIno, typeDirIndex)
	for _, m := range items {
		d := m.data
		if len(d) < dirItemHdrSize {
			continue
		}
		nameStart := dirItemHdrSize
		nameLen := int(d[0x1B]) | int(d[0x1C])<<8
		if nameStart+nameLen > len(d) {
			continue
		}
		if string(d[nameStart:nameStart+nameLen]) == name {
			newRoot, err = cowDelete(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, m.k)
			if err != nil {
				return fmt.Errorf("btrfs remove: dir index: %w", err)
			}
			*fsTreeRoot = newRoot
			break
		}
	}

	// Remove DIR_ITEM from parent
	nameHash := hashDirName(name)
	newRoot, err = cowDelete(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{parentIno, typeDirItem, nameHash})
	if err != nil && !isNotFoundErr(err) {
		return fmt.Errorf("btrfs remove: dir item: %w", err)
	}
	if err == nil {
		*fsTreeRoot = newRoot
	}

	// Parent contents shrank — bump its mtime/ctime.
	if err := touchDir(rwaAt, rws, partOff, sb, sm, fsTreeRoot, parentIno); err != nil {
		return fmt.Errorf("btrfs remove: touch parent: %w", err)
	}
	return updateFsTreeRoot(rwaAt, partOff, sb, sm, *fsTreeRoot)
}
