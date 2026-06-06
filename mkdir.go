package filesystem_btrfs

import (
	"fmt"
	"os"
	"path"
)

func makeDir(rwaAt readerWriterAt, rws readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, fsTreeRoot *uint64,
	p string, perm os.FileMode,
) error {
	dir, name := path.Split(path.Clean(p))
	if dir == "" {
		dir = "/"
	}
	dir = path.Clean(dir)
	parentIno, err := pathLookupIno(rwaAt, partOff, sb, *fsTreeRoot, dir)
	if err != nil {
		return fmt.Errorf("btrfs mkdir: parent %q: %w", dir, err)
	}
	_, _, existErr := lookupDirEntry(rwaAt, partOff, sb, *fsTreeRoot, parentIno, name)
	if existErr == nil {
		return fmt.Errorf("btrfs mkdir: %q already exists", p)
	}
	ino, _ := nextInodeNum(rwaAt, partOff, sb, *fsTreeRoot)
	generation := sb.generation + 1
	mode := uint16(0o040000 | (perm & 0o777))
	// Fresh dir has nlink=2: the entry in the parent + the "." self-reference.
	inodeBuf := encodeInodeItem(ino, 0, mode, generation, 0, 2)
	newRoot, err := cowInsert(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{ino, typeInodeItem, 0}, inodeBuf)
	if err != nil {
		return fmt.Errorf("btrfs mkdir: insert inode: %w", err)
	}
	*fsTreeRoot = newRoot
	dotBuf := encodeDirItem(ino, typeInodeItem, ftDir, ".")
	dotdotBuf := encodeDirItem(parentIno, typeInodeItem, ftDir, "..")
	newRoot, err = cowInsert(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{ino, typeDirIndex, 1}, dotBuf)
	if err != nil {
		return fmt.Errorf("btrfs mkdir: insert '.': %w", err)
	}
	*fsTreeRoot = newRoot
	newRoot, err = cowInsert(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{ino, typeDirIndex, 2}, dotdotBuf)
	if err != nil {
		return fmt.Errorf("btrfs mkdir: insert '..': %w", err)
	}
	*fsTreeRoot = newRoot
	idxOff := nextDirIndexOffset(rwaAt, partOff, sb, *fsTreeRoot, parentIno)
	dirItemBuf := encodeDirItem(ino, typeInodeItem, ftDir, name)
	newRoot, err = cowInsert(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{parentIno, typeDirIndex, idxOff}, dirItemBuf)
	if err != nil {
		return fmt.Errorf("btrfs mkdir: insert parent dir index: %w", err)
	}
	*fsTreeRoot = newRoot
	nameHash := hashDirName(name)
	newRoot, err = cowInsert(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{parentIno, typeDirItem, nameHash}, dirItemBuf)
	if err != nil {
		return fmt.Errorf("btrfs mkdir: insert parent dir item: %w", err)
	}
	*fsTreeRoot = newRoot
	// INODE_REF back-pointer from the new directory inode to its parent dir entry.
	refBuf := encodeInodeRef(idxOff, name)
	newRoot, err = cowInsert(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{ino, typeInodeRef, parentIno}, refBuf)
	if err != nil {
		return fmt.Errorf("btrfs mkdir: insert inode ref: %w", err)
	}
	*fsTreeRoot = newRoot
	// The new subdir's ".." entry counts as an additional link to the parent.
	if err := adjustDirNlink(rwaAt, rws, partOff, sb, sm, fsTreeRoot, parentIno, +1); err != nil {
		return fmt.Errorf("btrfs mkdir: bump parent nlink: %w", err)
	}
	return updateFsTreeRoot(rwaAt, partOff, sb, sm, *fsTreeRoot)
}
