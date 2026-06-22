package filesystem_btrfs

import (
	"fmt"
	"path"
)

func renameEntry(rwaAt readerWriterAt, rws readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, fsTreeRoot *uint64,
	oldPath, newPath string,
) error {
	oldDir, oldName := splitPath(oldPath)
	newDir, newName := splitPath(newPath)

	oldParentIno, err := pathLookupIno(rwaAt, partOff, sb, *fsTreeRoot, oldDir)
	if err != nil {
		return fmt.Errorf("btrfs rename: src parent %q: %w", oldDir, err)
	}
	srcObjID, srcFtype, err := lookupDirEntry(rwaAt, partOff, sb, *fsTreeRoot, oldParentIno, oldName)
	if err != nil {
		return fmt.Errorf("btrfs rename: src %q: %w", oldPath, err)
	}

	newParentIno, err := pathLookupIno(rwaAt, partOff, sb, *fsTreeRoot, newDir)
	if err != nil {
		return fmt.Errorf("btrfs rename: dst parent %q: %w", newDir, err)
	}

	// If destination already exists, remove it first (only regular files supported)
	dstObjID, dstFtype, existErr := lookupDirEntry(rwaAt, partOff, sb, *fsTreeRoot, newParentIno, newName)
	if existErr == nil {
		if dstFtype == ftDir {
			return fmt.Errorf("btrfs rename: destination %q is a directory", newPath)
		}
		if err := removeInode(rwaAt, rws, partOff, sb, sm, fsTreeRoot, dstObjID, newParentIno, newName); err != nil {
			return fmt.Errorf("btrfs rename: remove dst: %w", err)
		}
	}

	// Remove old DIR_INDEX from source parent. Multi-leaf scan so we still
	// find the entry when the parent's dir entries span several leaves.
	items, _ := collectPrefixItems(rwaAt, partOff, sb, *fsTreeRoot, oldParentIno, typeDirIndex)
	for _, m := range items {
		d := m.data
		if len(d) < dirItemHdrSize {
			continue
		}
		nameLen := int(d[0x1B]) | int(d[0x1C])<<8
		if dirItemHdrSize+nameLen > len(d) {
			continue
		}
		if string(d[dirItemHdrSize:dirItemHdrSize+nameLen]) == oldName {
			newRoot, derr := cowDelete(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, m.k)
			if derr != nil {
				return fmt.Errorf("btrfs rename: remove old dir index: %w", derr)
			}
			*fsTreeRoot = newRoot
			break
		}
	}

	// Remove old DIR_ITEM from source parent.
	oldHash := hashDirName(oldName)
	newRoot, err := cowDelete(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{oldParentIno, typeDirItem, oldHash})
	if err != nil && !isNotFoundErr(err) {
		return fmt.Errorf("btrfs rename: remove old dir item: %w", err)
	}
	if err == nil {
		*fsTreeRoot = newRoot
	}

	// Drop the OLD name's back-reference from (src, INODE_REF, oldParent).
	// When this is the only entry the whole item is removed; otherwise the
	// remaining hardlinks under oldParent stay intact.
	if err := removeInodeRef(rwaAt, rws, partOff, sb, sm, fsTreeRoot, srcObjID, oldParentIno, oldName); err != nil {
		return fmt.Errorf("btrfs rename: %w", err)
	}

	// Insert new DIR_INDEX at destination parent
	idxOff := nextDirIndexOffset(rwaAt, partOff, sb, *fsTreeRoot, newParentIno)
	dirItemBuf := encodeDirItem(srcObjID, typeInodeItem, srcFtype, newName)
	newRoot, err = cowInsert(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{newParentIno, typeDirIndex, idxOff}, dirItemBuf)
	if err != nil {
		return fmt.Errorf("btrfs rename: insert new dir index: %w", err)
	}
	*fsTreeRoot = newRoot

	// Insert new DIR_ITEM at destination parent
	newHash := hashDirName(newName)
	newRoot, err = cowInsert(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{newParentIno, typeDirItem, newHash}, dirItemBuf)
	if err != nil {
		return fmt.Errorf("btrfs rename: insert new dir item: %w", err)
	}
	*fsTreeRoot = newRoot

	// Add the NEW name's back-reference to (src, INODE_REF, newParent),
	// merging into the existing item when newParent already has other
	// hardlinks to the same inode.
	if err := appendInodeRef(rwaAt, rws, partOff, sb, sm, fsTreeRoot, srcObjID, newParentIno, idxOff, newName); err != nil {
		return fmt.Errorf("btrfs rename: %w", err)
	}

	// Maintain parent directory i_size: oldParent lost oldName, newParent gained
	// newName (these are the same inode when the rename stays in one directory).
	if err := adjustDirSize(rwaAt, rws, partOff, sb, sm, fsTreeRoot, oldParentIno, -dirEntrySizeDelta(oldName)); err != nil {
		return fmt.Errorf("btrfs rename: shrink src parent size: %w", err)
	}
	if err := adjustDirSize(rwaAt, rws, partOff, sb, sm, fsTreeRoot, newParentIno, dirEntrySizeDelta(newName)); err != nil {
		return fmt.Errorf("btrfs rename: grow dst parent size: %w", err)
	}

	// btrfs keeps every directory at i_nlink == 1: subdirectories are not counted
	// in a parent's link count (and the kernel's tree-checker rejects nlink > 1 on
	// a directory), so even a cross-parent directory move leaves both parents'
	// nlink unchanged. The moved dir's parent link is recorded solely by its
	// INODE_REF (already repointed at newParent above). We only touch the parents'
	// timestamps to reflect the entry add/remove.
	{
		if err := touchDir(rwaAt, rws, partOff, sb, sm, fsTreeRoot, oldParentIno); err != nil {
			return fmt.Errorf("btrfs rename: touch src parent: %w", err)
		}
		if oldParentIno != newParentIno {
			if err := touchDir(rwaAt, rws, partOff, sb, sm, fsTreeRoot, newParentIno); err != nil {
				return fmt.Errorf("btrfs rename: touch dst parent: %w", err)
			}
		}
	}

	return updateFsTreeRoot(rwaAt, partOff, sb, sm, *fsTreeRoot)
}

// splitPath splits a cleaned path into (dir, name).
// "/" returns ("/", "").
func splitPath(p string) (dir string, name string) {
	p = path.Clean(p)
	if p == "/" {
		return "/", ""
	}
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			d := p[:i]
			if d == "" {
				d = "/"
			}
			return d, p[i+1:]
		}
	}
	return "/", p
}
