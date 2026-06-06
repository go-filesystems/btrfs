package filesystem_btrfs

import (
	"fmt"
	"path"
)

// symlinkInode creates a symlink at linkPath whose target is the given
// string. The target is stored as the file body of a new symlink inode;
// readSymlink decodes it the same way it does for files (via readFileData),
// so no separate read path is needed.
func symlinkInode(rwaAt readerWriterAt, rws readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, fsTreeRoot *uint64,
	target, linkPath string,
) error {
	if target == "" {
		return fmt.Errorf("btrfs symlink: empty target")
	}
	dir, name := path.Split(path.Clean(linkPath))
	if dir == "" {
		dir = "/"
	}
	dir = path.Clean(dir)
	if name == "" {
		return fmt.Errorf("btrfs symlink: invalid link path %q", linkPath)
	}

	parentIno, err := pathLookupIno(rwaAt, partOff, sb, *fsTreeRoot, dir)
	if err != nil {
		return fmt.Errorf("btrfs symlink: parent dir %q: %w", dir, err)
	}
	if _, _, existErr := lookupDirEntry(rwaAt, partOff, sb, *fsTreeRoot, parentIno, name); existErr == nil {
		return fmt.Errorf("btrfs symlink: %q already exists", linkPath)
	}

	// Symlink mode: S_IFLNK | 0777. POSIX traditionally always allows rwx
	// on the symlink itself; the permission check is done on the target.
	mode := uint16(0o120000 | 0o777)
	return createInodeWithDirEntry(rwaAt, rws, partOff, sb, sm, fsTreeRoot,
		parentIno, name, []byte(target), mode, ftSymlink, "symlink")
}
