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
	// A fresh btrfs directory has nlink=1 (an empty dir links only via its own
	// entry); each child SUBDIR later bumps it by one. btrfs does NOT store
	// "."/".." as DIR_INDEX/DIR_ITEM entries — those are implicit, and the
	// kernel's tree-checker rejects them (name-hash mismatch). The parent link
	// is recorded by the INODE_REF inserted below.
	inodeBuf := encodeInodeItem(ino, 0, mode, generation, 0, 1)
	newRoot, err := cowInsert(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{ino, typeInodeItem, 0}, inodeBuf)
	if err != nil {
		return fmt.Errorf("btrfs mkdir: insert inode: %w", err)
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
	// The new subdir adds an entry to the parent (grow its i_size). Unlike
	// traditional Unix filesystems, btrfs does NOT count subdirectories in a
	// directory's nlink: every directory inode keeps i_nlink == 1, and the
	// kernel's tree-checker rejects any directory with nlink > 1
	// ("invalid nlink: has 2 expect no more than 1 for dir"). So the parent's
	// nlink is left unchanged here.
	if err := adjustDirSize(rwaAt, rws, partOff, sb, sm, fsTreeRoot, parentIno, dirEntrySizeDelta(name)); err != nil {
		return fmt.Errorf("btrfs mkdir: grow parent size: %w", err)
	}
	// Adding a child changes the parent directory: bump its mtime/ctime (without
	// touching its nlink, which btrfs keeps at 1).
	if err := touchDir(rwaAt, rws, partOff, sb, sm, fsTreeRoot, parentIno); err != nil {
		return fmt.Errorf("btrfs mkdir: touch parent: %w", err)
	}
	return updateFsTreeRoot(rwaAt, partOff, sb, sm, *fsTreeRoot)
}
