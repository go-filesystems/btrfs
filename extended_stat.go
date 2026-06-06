package filesystem_btrfs

import (
	"encoding/binary"
	"fmt"
	"time"
)

// InodeStat is the full metadata view of a btrfs INODE_ITEM. The common
// filesystem.Filesystem interface only exposes mode/size/inode via Stat;
// callers that need the rich btrfs picture (uid/gid, timestamps, nlink,
// nbytes, transid/sequence, flags) use ExtendedStat instead.
//
// All four timestamp fields are returned in UTC.
type InodeStat struct {
	Inode      uint64
	Mode       uint16
	Size       uint64
	NBytes     uint64
	NLink      uint32
	UID        uint32
	GID        uint32
	Generation uint64
	TransID    uint64
	Sequence   uint64
	Flags      uint64
	ATime      time.Time
	CTime      time.Time
	MTime      time.Time
	OTime      time.Time
}

// IsDir / IsRegular / IsSymlink mirror the on-disk mode predicates.
func (s *InodeStat) IsDir() bool     { return s.Mode&0xF000 == 0x4000 }
func (s *InodeStat) IsRegular() bool { return s.Mode&0xF000 == 0x8000 }
func (s *InodeStat) IsSymlink() bool { return s.Mode&0xF000 == 0xA000 }

// ExtendedStat returns the full INODE_ITEM metadata for the inode at path.
// btrfs-specific — not part of the common filesystem.Filesystem interface;
// callers must use the concrete *btrfsFS type via a type assertion.
func (fs *btrfsFS) ExtendedStat(path string) (*InodeStat, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	in, err := pathLookup(fs.f, fs.partOffset, fs.sb, fs.fsTreeRoot, path)
	if err != nil {
		return nil, err
	}
	buf, it, err := searchTree(fs.f, fs.partOffset, fs.sb, fs.fsTreeRoot, in.num, typeInodeItem, 0)
	if err != nil {
		return nil, fmt.Errorf("btrfs ExtendedStat %q: %w", path, err)
	}
	d := it.data(buf)
	if len(d) < inodeItemSize {
		return nil, fmt.Errorf("btrfs ExtendedStat %q: inode item too short (%d bytes)", path, len(d))
	}
	le := binary.LittleEndian
	readTime := func(off int) time.Time {
		sec := int64(le.Uint64(d[off:]))
		nsec := int64(le.Uint32(d[off+8:]))
		return time.Unix(sec, nsec).UTC()
	}
	return &InodeStat{
		Inode:      in.num,
		Mode:       uint16(le.Uint32(d[inodeOffMode:])),
		Size:       le.Uint64(d[inodeOffSize:]),
		NBytes:     le.Uint64(d[inodeOffNBytes:]),
		NLink:      le.Uint32(d[inodeOffNLink:]),
		UID:        le.Uint32(d[inodeOffUID:]),
		GID:        le.Uint32(d[inodeOffGID:]),
		Generation: le.Uint64(d[inodeOffGeneration:]),
		TransID:    le.Uint64(d[inodeOffTransID:]),
		Sequence:   le.Uint64(d[inodeOffSequence:]),
		Flags:      le.Uint64(d[inodeOffFlags:]),
		ATime:      readTime(inodeOffATime),
		CTime:      readTime(inodeOffCTime),
		MTime:      readTime(inodeOffMTime),
		OTime:      readTime(inodeOffOTime),
	}, nil
}
