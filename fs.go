// Package btrfs provides read/write access to Btrfs filesystem images
// without requiring root privileges or external tools.
// It targets the Btrfs on-disk format as used by Fedora Cloud images
// (single device, CRC32c checksums).
//
// Partition tables (MBR/GPT) are detected automatically; pass partIndex = -1
// to auto-select the first Linux data partition.
//
// All read and write operations are fully implemented.
package filesystem_btrfs

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// blockBackend is the interface backing btrfsFS. Read paths
// already speak io.ReaderAt internally; this interface adds
// the lifecycle methods (Sync / Size / Truncate / Close) and
// io.WriterAt for write paths. Any layered block source
// (LUKS Device, qcow2 wrapper, in-memory fixture) can back a
// btrfs filesystem by satisfying this interface.
type blockBackend interface {
	io.ReaderAt
	io.WriterAt
	Sync() error
	Size() (int64, error)
	Truncate(size int64) error
	io.Closer
}

// BlockBackend is the exported alias of blockBackend — uniform
// across go-filesystems/* so a single adapter (e.g. cloud-boot
// init's luksAsBlock) serves every FS.
type BlockBackend = blockBackend

// osFileBackend wraps *os.File so plain-disk-image opens go
// through the same blockBackend path as layered ones.
type osFileBackend struct{ f *os.File }

func (o *osFileBackend) ReadAt(p []byte, off int64) (int, error)  { return o.f.ReadAt(p, off) }
func (o *osFileBackend) WriteAt(p []byte, off int64) (int, error) { return o.f.WriteAt(p, off) }
func (o *osFileBackend) Sync() error                              { return o.f.Sync() }
func (o *osFileBackend) Truncate(size int64) error                { return o.f.Truncate(size) }
func (o *osFileBackend) Close() error                             { return o.f.Close() }
func (o *osFileBackend) Size() (int64, error) {
	fi, err := o.f.Stat()
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// FS represents an open Btrfs filesystem image.
type btrfsFS struct {
	f          blockBackend
	rwa        readerWriterAt // alias to f satisfying the legacy read/write interface
	partOffset int64
	sb         *superblock
	fsTreeRoot uint64 // logical address of the FS_TREE root node
	sm         *spaceManager
	mu         sync.Mutex
}

// osFileRWA adapts *os.File to readerWriterAt.
// Kept for any test code that still constructs one directly.
type osFileRWA struct{ f *os.File }

func (o *osFileRWA) ReadAt(p []byte, off int64) (int, error)  { return o.f.ReadAt(p, off) }
func (o *osFileRWA) WriteAt(p []byte, off int64) (int, error) { return o.f.WriteAt(p, off) }

// FS is the public interface returned by Open. It extends the common
// filesystem.Filesystem with btrfs-specific operations — hardlinks /
// symlinks creation, xattrs read+write, rich inode stat, ownership /
// permission / timestamp setters, truncate.
//
// Callers that want to reach the btrfs-only operations should consume
// this interface type (or a type-assert from the common Filesystem)
// rather than the concrete *btrfsFS.
type FS interface {
	filesystem.Filesystem

	Link(oldPath, newPath string) error
	Symlink(target, linkPath string) error

	Xattrs(path string) (map[string][]byte, error)
	GetXattr(path, name string) ([]byte, error)
	SetXattr(path, name string, value []byte) error
	RemoveXattr(path, name string) error

	ExtendedStat(path string) (*InodeStat, error)
	Chown(path string, uid, gid uint32) error
	Chmod(path string, perm os.FileMode) error
	Chtimes(path string, atime, mtime time.Time) error
	Truncate(path string, newSize int64) error

	Label() string
	SetLabel(label string) error

	// Filesystem-level resize. Grow extends; Shrink reduces (refuses to
	// discard live data); Resize dispatches to whichever direction the
	// new size implies (no-op when equal). All three require an idle
	// FS — concurrent writers during a resize aren't safe.
	Grow(newSizeBytes int64) error
	Shrink(newSizeBytes int64) error
	Resize(newSizeBytes int64) error
}

var _ FS = (*btrfsFS)(nil)

// Open opens a Btrfs filesystem image at imagePath, auto-detecting the
// partition table (MBR/GPT) and selecting the first Linux partition.
// Pass partIndex = -1 for auto-detection.
func Open(imagePath string, partIndex int) (FS, error) {
	f, err := os.OpenFile(imagePath, os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("btrfs: open %s: %w", imagePath, err)
	}
	return OpenFromDevice(&osFileBackend{f: f}, partIndex)
}

// OpenFromDevice opens a Btrfs filesystem backed by an arbitrary
// blockBackend. Layered callers (LUKS / qcow2 / in-memory) feed
// the FS without an *os.File-backed image. Same shape as the ext4
// / xfs / zfs equivalents.
func OpenFromDevice(dev BlockBackend, partIndex int) (FS, error) {
	return OpenFromDevices([]BlockBackend{dev}, partIndex)
}

// OpenFromDevices opens a multi-device Btrfs filesystem. The first
// element of `devs` is treated as the "primary" — its superblock is
// read, its dev_item.devid drives the local-stripe selection used by
// the legacy single-leg fast path, and its partition table (if any)
// determines partOffset. Additional devices in the slice are added to
// the pool by their dev_item.devid (read from each device's superblock)
// so that RAID0 / RAID10 / RAID5 / RAID6 chunks can be served.
//
// For SINGLE / DUP / RAID1 / RAID1Cn filesystems passing just the
// primary works (single-leg open via dev_item.devid matching). RAID0
// and the parity profiles need at least the data-bearing devices
// present in the slice; missing data devices surface as a clear error
// at read time.
//
// btrfs uses the on-disk fsid to bind a multi-device pool together;
// callers are responsible for grouping legs that share the same fsid
// (cloud-boot-init scans /sys/block for matching fsids).
func OpenFromDevices(devs []BlockBackend, partIndex int) (FS, error) {
	if len(devs) == 0 {
		return nil, fmt.Errorf("btrfs: OpenFromDevices: empty device list")
	}
	primary := devs[0]
	off, err := partitionOffset(primary, partIndex)
	if err != nil {
		closeAll(devs)
		return nil, err
	}

	sb, err := readSuperblock(primary, off)
	if err != nil {
		closeAll(devs)
		return nil, err
	}

	// Build the device pool: primary first, then secondaries keyed by their
	// own dev_item.devid. Secondaries are probed with the SAME partition
	// offset as the primary, which matches mkfs.btrfs's behaviour: every
	// leg of a multi-device FS has the superblock at the same byte offset
	// within its partition.
	pool := newDevicePool(primary, off, sb)
	for i, secondary := range devs[1:] {
		ssb, serr := readSuperblock(secondary, off)
		if serr != nil {
			pool.Close()
			return nil, fmt.Errorf("btrfs: read superblock on device %d: %w", i+1, serr)
		}
		if ssb.devID == 0 {
			pool.Close()
			return nil, fmt.Errorf("btrfs: device %d has dev_item.devid=0 (corrupt or test fixture); cannot pool", i+1)
		}
		if ssb.devID == sb.devID {
			pool.Close()
			return nil, fmt.Errorf("btrfs: device %d has duplicate devid %d (already the primary)", i+1, ssb.devID)
		}
		pool.addDevice(ssb.devID, secondary)
	}

	// Load all chunk mappings so we can resolve logical -> physical.
	if err := loadChunkTree(pool, off, sb); err != nil {
		pool.Close()
		return nil, fmt.Errorf("btrfs: load chunk tree: %w", err)
	}

	fsRoot, err := resolveRootTree(pool, off, sb)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("btrfs: resolve FS_TREE: %w", err)
	}

	sm, err := buildSpaceManager(pool, off, sb, fsRoot)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("btrfs: build space manager: %w", err)
	}

	return &btrfsFS{f: pool, rwa: pool, partOffset: off, sb: sb, fsTreeRoot: fsRoot, sm: sm}, nil
}

func closeAll(devs []BlockBackend) {
	for _, d := range devs {
		_ = d.Close()
	}
}

// Close releases resources held by the FS.
func (fs *btrfsFS) Close() error { return fs.f.Close() }

// ReadFile reads and returns the contents of the regular file at path.
func (fs *btrfsFS) ReadFile(path string) ([]byte, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	in, err := pathLookup(fs.f, fs.partOffset, fs.sb, fs.fsTreeRoot, path)
	if err != nil {
		return nil, err
	}
	if !in.isRegular() {
		return nil, fmt.Errorf("btrfs: %q is not a regular file", path)
	}
	return readFileData(fs.f, fs.partOffset, fs.sb, fs.fsTreeRoot, in)
}

// ListDir returns the directory entries of the directory at path.
func (fs *btrfsFS) ListDir(path string) ([]filesystem.DirEntry, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	in, err := pathLookup(fs.f, fs.partOffset, fs.sb, fs.fsTreeRoot, path)
	if err != nil {
		return nil, err
	}
	if !in.isDir() {
		return nil, fmt.Errorf("btrfs: %q is not a directory", path)
	}
	return readDir(fs.f, fs.partOffset, fs.sb, fs.fsTreeRoot, in.num)
}

// Stat returns basic metadata for the file or directory at path.
func (fs *btrfsFS) Stat(path string) (filesystem.Stat, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	in, err := pathLookup(fs.f, fs.partOffset, fs.sb, fs.fsTreeRoot, path)
	if err != nil {
		return nil, err
	}
	return filesystem.NewStat(uint16(in.mode), in.size, in.num), nil
}

// ReadLink returns the target of the symbolic link at path.
func (fs *btrfsFS) ReadLink(path string) (string, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	in, err := pathLookup(fs.f, fs.partOffset, fs.sb, fs.fsTreeRoot, path)
	if err != nil {
		return "", err
	}
	if !in.isSymlink() {
		return "", fmt.Errorf("btrfs: %q is not a symbolic link", path)
	}
	return readSymlink(fs.f, fs.partOffset, fs.sb, fs.fsTreeRoot, in)
}

// WriteFile creates or overwrites the file at path with the given data and permissions.
func (fs *btrfsFS) WriteFile(path string, data []byte, perm os.FileMode) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return writeFile(fs.rwa, fs.f, fs.partOffset, fs.sb, fs.sm, &fs.fsTreeRoot, path, data, perm)
}

// MkDir creates the directory at path with the given permissions.
func (fs *btrfsFS) MkDir(path string, perm os.FileMode) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return makeDir(fs.rwa, fs.f, fs.partOffset, fs.sb, fs.sm, &fs.fsTreeRoot, path, perm)
}

// DeleteFile removes the regular file at path.
func (fs *btrfsFS) DeleteFile(path string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return deleteFile(fs.rwa, fs.f, fs.partOffset, fs.sb, fs.sm, &fs.fsTreeRoot, path)
}

// DeleteDir removes the empty directory at path.
func (fs *btrfsFS) DeleteDir(path string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return deleteDir(fs.rwa, fs.f, fs.partOffset, fs.sb, fs.sm, &fs.fsTreeRoot, path)
}

// Rename moves or renames oldPath to newPath.
func (fs *btrfsFS) Rename(oldPath, newPath string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return renameEntry(fs.rwa, fs.f, fs.partOffset, fs.sb, fs.sm, &fs.fsTreeRoot, oldPath, newPath)
}

// Link creates a new hard link newPath pointing at the same inode as oldPath.
// The two paths share file data and metadata; deleting one only removes that
// directory entry, the data persists until the last link is gone.
// btrfs-specific — not part of the common filesystem.Filesystem interface.
func (fs *btrfsFS) Link(oldPath, newPath string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return linkInode(fs.rwa, fs.f, fs.partOffset, fs.sb, fs.sm, &fs.fsTreeRoot, oldPath, newPath)
}

// Symlink creates a symbolic link at linkPath whose target is the given
// string. The target is stored as the inline file body of a new symlink
// inode, which is what ReadLink reads back. The parent directory of
// linkPath must already exist. btrfs-specific.
func (fs *btrfsFS) Symlink(target, linkPath string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return symlinkInode(fs.rwa, fs.f, fs.partOffset, fs.sb, fs.sm, &fs.fsTreeRoot, target, linkPath)
}

// Chown changes the owner uid/gid of the inode at path. btrfs-specific.
// ctime is updated to the current time; mtime and the file body are
// unchanged.
func (fs *btrfsFS) Chown(path string, uid, gid uint32) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return chownInode(fs.rwa, fs.f, fs.partOffset, fs.sb, fs.sm, &fs.fsTreeRoot, path, uid, gid)
}

// Chmod changes the permission bits of the inode at path; the file-type
// bits (regular/dir/symlink) are preserved. ctime is updated. btrfs-
// specific.
func (fs *btrfsFS) Chmod(path string, perm os.FileMode) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return chmodInode(fs.rwa, fs.f, fs.partOffset, fs.sb, fs.sm, &fs.fsTreeRoot, path, perm)
}

// Chtimes sets atime and mtime on the inode at path; ctime is bumped to
// the current time (POSIX). otime is preserved. btrfs-specific.
func (fs *btrfsFS) Chtimes(path string, atime, mtime time.Time) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return chtimesInode(fs.rwa, fs.f, fs.partOffset, fs.sb, fs.sm, &fs.fsTreeRoot, path, atime, mtime)
}

// Truncate resizes the regular file at path to newSize bytes. Growing a
// file produces a sparse extension (no disk allocation); shrinking drops
// or trims EXTENT_DATA items past newSize. mtime / ctime are refreshed.
// btrfs-specific.
func (fs *btrfsFS) Truncate(path string, newSize int64) error {
	if newSize < 0 {
		return fmt.Errorf("btrfs truncate: %q: negative size %d", path, newSize)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return truncateInode(fs.rwa, fs.f, fs.partOffset, fs.sb, fs.sm, &fs.fsTreeRoot, path, uint64(newSize))
}

// GetXattr returns the value of a single xattr by exact name on the inode
// at path. Returns an error wrapping ErrNotFound when no such xattr exists.
// btrfs-specific.
func (fs *btrfsFS) GetXattr(path, name string) ([]byte, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return getXattrInode(fs.f, fs.partOffset, fs.sb, fs.fsTreeRoot, path, name)
}

// SetXattr attaches an xattr (name, value) to the inode at path, creating
// the entry if missing or replacing the existing one. ctime is refreshed.
// btrfs-specific.
func (fs *btrfsFS) SetXattr(path, name string, value []byte) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return setXattrInode(fs.rwa, fs.f, fs.partOffset, fs.sb, fs.sm, &fs.fsTreeRoot, path, name, value)
}

// RemoveXattr deletes the xattr named `name` from the inode at path.
// Returns an error wrapping ErrNotFound when no such xattr exists.
// btrfs-specific.
func (fs *btrfsFS) RemoveXattr(path, name string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return removeXattrInode(fs.rwa, fs.f, fs.partOffset, fs.sb, fs.sm, &fs.fsTreeRoot, path, name)
}

// Xattrs returns all extended attributes (XATTR_ITEM entries) attached to the
// inode at path, as a map of attribute name to raw value bytes. Returns an
// empty map for inodes without xattrs. This is btrfs-specific — the common
// filesystem.Filesystem interface does not expose xattrs — so callers must
// use the concrete *btrfsFS type via a type assertion.
func (fs *btrfsFS) Xattrs(path string) (map[string][]byte, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	in, err := pathLookup(fs.f, fs.partOffset, fs.sb, fs.fsTreeRoot, path)
	if err != nil {
		return nil, err
	}
	items, err := collectPrefixItems(fs.f, fs.partOffset, fs.sb, fs.fsTreeRoot, in.num, typeXattrItem)
	if err != nil {
		if isNotFoundErr(err) {
			return map[string][]byte{}, nil
		}
		return nil, err
	}
	out := make(map[string][]byte, len(items))
	for _, m := range items {
		name, value, ok := parseXattrItem(m.data)
		if !ok {
			continue
		}
		out[name] = value
	}
	return out, nil
}

// parseXattrItem decodes a single XATTR_ITEM payload. The on-disk layout
// reuses btrfs_dir_item: location_objid(8) + location_type(1) +
// transid(8) + data_len(2) + name_len(2) + type(1) + name + value. The
// `value` follows the name and runs to the end of the item data.
func parseXattrItem(d []byte) (string, []byte, bool) {
	if len(d) < dirItemHdrSize {
		return "", nil, false
	}
	dataLen := int(d[0x19]) | int(d[0x1A])<<8
	nameLen := int(d[0x1B]) | int(d[0x1C])<<8
	if dirItemHdrSize+nameLen+dataLen > len(d) {
		return "", nil, false
	}
	name := string(d[dirItemHdrSize : dirItemHdrSize+nameLen])
	value := append([]byte(nil), d[dirItemHdrSize+nameLen:dirItemHdrSize+nameLen+dataLen]...)
	return name, value, true
}
