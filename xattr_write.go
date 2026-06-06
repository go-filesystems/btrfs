package filesystem_btrfs

import (
	"encoding/binary"
	"fmt"
	"io"
)

// encodeXattrItemPayload constructs the on-disk bytes of an XATTR_ITEM,
// which reuses the btrfs_dir_item layout: 30-byte header + name + value.
// The header fields not relevant to xattrs (location_objid/type/transid,
// dir-item file-type byte) are left zero.
func encodeXattrItemPayload(name string, value []byte) []byte {
	nameBytes := []byte(name)
	buf := make([]byte, dirItemHdrSize+len(nameBytes)+len(value))
	le := binary.LittleEndian
	le.PutUint16(buf[0x19:], uint16(len(value)))     // data_len
	le.PutUint16(buf[0x1B:], uint16(len(nameBytes))) // name_len
	copy(buf[dirItemHdrSize:], nameBytes)
	copy(buf[dirItemHdrSize+len(nameBytes):], value)
	return buf
}

// getXattrInode returns the value of the xattr `name` attached to the inode
// at path, or an error wrapping ErrNotFound when no such xattr exists.
func getXattrInode(r io.ReaderAt, partOff int64, sb *superblock, fsTreeRoot uint64,
	path, name string,
) ([]byte, error) {
	in, err := pathLookup(r, partOff, sb, fsTreeRoot, path)
	if err != nil {
		return nil, err
	}
	hash := hashDirName(name)
	buf, it, err := searchTree(r, partOff, sb, fsTreeRoot, in.num, typeXattrItem, hash)
	if err != nil {
		return nil, fmt.Errorf("btrfs getxattr %q on %q: %w", name, path, err)
	}
	d := it.data(buf)
	gotName, gotValue, ok := parseXattrItem(d)
	if !ok {
		return nil, fmt.Errorf("btrfs getxattr %q on %q: malformed item", name, path)
	}
	// Hash collisions are rare but possible; verify the name actually matches.
	if gotName != name {
		return nil, fmt.Errorf("btrfs getxattr %q on %q: hash collision (got %q): %w", name, path, gotName, ErrNotFound)
	}
	return gotValue, nil
}

// setXattrInode creates or replaces the xattr `name` with `value` on the
// inode at path. Returns an error when the path does not exist.
func setXattrInode(rwaAt readerWriterAt, rws readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, fsTreeRoot *uint64, path, name string, value []byte,
) error {
	in, err := pathLookup(rwaAt, partOff, sb, *fsTreeRoot, path)
	if err != nil {
		return fmt.Errorf("btrfs setxattr: %q: %w", path, err)
	}
	hash := hashDirName(name)
	payload := encodeXattrItemPayload(name, value)
	xattrKey := key{in.num, typeXattrItem, hash}

	// If the item already exists, replace it (cowUpdate handles the
	// size-changing case). Otherwise insert a fresh one.
	if _, _, serr := searchTree(rwaAt, partOff, sb, *fsTreeRoot, in.num, typeXattrItem, hash); serr == nil {
		newRoot, uerr := cowUpdate(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, xattrKey, payload)
		if uerr != nil {
			return fmt.Errorf("btrfs setxattr: update %q on %q: %w", name, path, uerr)
		}
		*fsTreeRoot = newRoot
	} else {
		newRoot, ierr := cowInsert(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, xattrKey, payload)
		if ierr != nil {
			return fmt.Errorf("btrfs setxattr: insert %q on %q: %w", name, path, ierr)
		}
		*fsTreeRoot = newRoot
	}

	// ctime + transid + sequence reflect the metadata change.
	if err := updateInodeMetadata(rwaAt, rws, partOff, sb, sm, fsTreeRoot, in.num, func(d []byte) error {
		return nil
	}); err != nil {
		return fmt.Errorf("btrfs setxattr: refresh inode metadata: %w", err)
	}
	return nil
}

// removeXattrInode deletes the xattr `name` from the inode at path.
// Removing a non-existent xattr returns an error wrapping ErrNotFound.
func removeXattrInode(rwaAt readerWriterAt, rws readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, fsTreeRoot *uint64, path, name string,
) error {
	in, err := pathLookup(rwaAt, partOff, sb, *fsTreeRoot, path)
	if err != nil {
		return fmt.Errorf("btrfs removexattr: %q: %w", path, err)
	}
	hash := hashDirName(name)
	newRoot, err := cowDelete(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{in.num, typeXattrItem, hash})
	if err != nil {
		return fmt.Errorf("btrfs removexattr: %q on %q: %w", name, path, err)
	}
	*fsTreeRoot = newRoot
	return updateInodeMetadata(rwaAt, rws, partOff, sb, sm, fsTreeRoot, in.num, func(d []byte) error {
		return nil
	})
}
