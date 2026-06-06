package filesystem_btrfs

import (
	"encoding/binary"
	"fmt"
	"os"
	"time"
)

// updateInodeMetadata is the shared backbone for chown / chmod / chtimes.
// It reads the target INODE_ITEM, calls mutator on the local copy, bumps
// transid+sequence+ctime, and cowUpdates the result. The mutator is
// responsible for changing only the field(s) it owns; ctime / transid /
// sequence are always refreshed because POSIX considers any metadata
// modification a "change time" event.
func updateInodeMetadata(rwaAt readerWriterAt, rws readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, fsTreeRoot *uint64, ino uint64,
	mutator func(d []byte) error,
) error {
	buf, it, err := searchTree(rwaAt, partOff, sb, *fsTreeRoot, ino, typeInodeItem, 0)
	if err != nil {
		return fmt.Errorf("read inode %d: %w", ino, err)
	}
	d := make([]byte, it.dataSize)
	copy(d, it.data(buf))
	if err := mutator(d); err != nil {
		return err
	}
	bumpInodeTransIDSequence(d, sb.generation+1)
	writeBtrfsTimespec(d[inodeOffCTime:], time.Now().UTC())
	newRoot, err := cowUpdate(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, key{ino, typeInodeItem, 0}, d)
	if err != nil {
		return fmt.Errorf("cowUpdate inode %d: %w", ino, err)
	}
	*fsTreeRoot = newRoot
	return updateFsTreeRoot(rwaAt, partOff, sb, sm, *fsTreeRoot)
}

func chownInode(rwaAt readerWriterAt, rws readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, fsTreeRoot *uint64, path string, uid, gid uint32,
) error {
	in, err := pathLookup(rwaAt, partOff, sb, *fsTreeRoot, path)
	if err != nil {
		return fmt.Errorf("btrfs chown: %q: %w", path, err)
	}
	return updateInodeMetadata(rwaAt, rws, partOff, sb, sm, fsTreeRoot, in.num, func(d []byte) error {
		le := binary.LittleEndian
		le.PutUint32(d[inodeOffUID:], uid)
		le.PutUint32(d[inodeOffGID:], gid)
		return nil
	})
}

func chmodInode(rwaAt readerWriterAt, rws readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, fsTreeRoot *uint64, path string, perm os.FileMode,
) error {
	in, err := pathLookup(rwaAt, partOff, sb, *fsTreeRoot, path)
	if err != nil {
		return fmt.Errorf("btrfs chmod: %q: %w", path, err)
	}
	return updateInodeMetadata(rwaAt, rws, partOff, sb, sm, fsTreeRoot, in.num, func(d []byte) error {
		le := binary.LittleEndian
		cur := le.Uint32(d[inodeOffMode:])
		// Preserve the file-type top nibble; replace only the permission
		// bits (low 12 bits — rwxrwxrwx + setuid/setgid/sticky).
		newMode := (cur &^ 0o7777) | (uint32(perm) & 0o7777)
		le.PutUint32(d[inodeOffMode:], newMode)
		return nil
	})
}

func chtimesInode(rwaAt readerWriterAt, rws readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, fsTreeRoot *uint64, path string, atime, mtime time.Time,
) error {
	in, err := pathLookup(rwaAt, partOff, sb, *fsTreeRoot, path)
	if err != nil {
		return fmt.Errorf("btrfs chtimes: %q: %w", path, err)
	}
	return updateInodeMetadata(rwaAt, rws, partOff, sb, sm, fsTreeRoot, in.num, func(d []byte) error {
		writeBtrfsTimespec(d[inodeOffATime:], atime.UTC())
		writeBtrfsTimespec(d[inodeOffMTime:], mtime.UTC())
		return nil
	})
}
