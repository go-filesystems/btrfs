package filesystem_btrfs

import (
	"encoding/binary"
	"fmt"
	"time"
)

// linkInode adds a new directory entry newPath that refers to the same inode
// as oldPath. The inode's nlink count is bumped and its ctime is refreshed.
// Directories cannot be hardlinked (the POSIX rule), and the destination must
// not already exist.
func linkInode(rwaAt readerWriterAt, rws readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, fsTreeRoot *uint64,
	oldPath, newPath string,
) error {
	// Resolve source inode.
	src, err := pathLookup(rwaAt, partOff, sb, *fsTreeRoot, oldPath)
	if err != nil {
		return fmt.Errorf("btrfs link: source %q: %w", oldPath, err)
	}
	if src.isDir() {
		return fmt.Errorf("btrfs link: %q is a directory; hardlinks to directories are not allowed", oldPath)
	}

	// Resolve destination parent + name.
	newDir, newName := splitPath(newPath)
	if newName == "" {
		return fmt.Errorf("btrfs link: invalid destination %q", newPath)
	}
	newParentIno, err := pathLookupIno(rwaAt, partOff, sb, *fsTreeRoot, newDir)
	if err != nil {
		return fmt.Errorf("btrfs link: dst parent %q: %w", newDir, err)
	}

	// Refuse to overwrite an existing entry.
	if _, _, ferr := lookupDirEntry(rwaAt, partOff, sb, *fsTreeRoot, newParentIno, newName); ferr == nil {
		return fmt.Errorf("btrfs link: destination %q already exists", newPath)
	}

	fileType := uint8(ftRegFile)
	if src.isSymlink() {
		fileType = ftSymlink
	}

	// Insert DIR_INDEX at the destination parent.
	idxOff := nextDirIndexOffset(rwaAt, partOff, sb, *fsTreeRoot, newParentIno)
	dirItemBuf := encodeDirItem(src.num, typeInodeItem, fileType, newName)
	newRoot, err := cowInsert(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{newParentIno, typeDirIndex, idxOff}, dirItemBuf)
	if err != nil {
		return fmt.Errorf("btrfs link: insert dir index: %w", err)
	}
	*fsTreeRoot = newRoot

	// Insert DIR_ITEM at the destination parent.
	nameHash := hashDirName(newName)
	newRoot, err = cowInsert(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{newParentIno, typeDirItem, nameHash}, dirItemBuf)
	if err != nil {
		return fmt.Errorf("btrfs link: insert dir item: %w", err)
	}
	*fsTreeRoot = newRoot

	// Add a back-reference entry to the (src, INODE_REF, newParent) item.
	// appendInodeRef handles the create/update split internally so multiple
	// hardlinks under the same parent stay in a single INODE_REF item.
	if err := appendInodeRef(rwaAt, rws, partOff, sb, sm, fsTreeRoot, src.num, newParentIno, idxOff, newName); err != nil {
		return fmt.Errorf("btrfs link: %w", err)
	}

	// Bump nlink + ctime on the inode item.
	inBuf, it, ierr := searchTree(rwaAt, partOff, sb, *fsTreeRoot, src.num, typeInodeItem, 0)
	if ierr != nil {
		return fmt.Errorf("btrfs link: refetch inode: %w", ierr)
	}
	d := make([]byte, it.dataSize)
	copy(d, it.data(inBuf))
	le := binary.LittleEndian
	le.PutUint32(d[inodeOffNLink:], le.Uint32(d[inodeOffNLink:])+1)
	bumpInodeTransIDSequence(d, sb.generation+1)
	writeBtrfsTimespec(d[inodeOffCTime:], time.Now().UTC())
	newRoot, err = cowUpdate(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{src.num, typeInodeItem, 0}, d)
	if err != nil {
		return fmt.Errorf("btrfs link: update inode: %w", err)
	}
	*fsTreeRoot = newRoot

	return updateFsTreeRoot(rwaAt, partOff, sb, sm, *fsTreeRoot)
}
