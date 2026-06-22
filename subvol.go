package filesystem_btrfs

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"

	filesystem "github.com/go-filesystems/interface"
	"github.com/go-volumes/safeio"
)

// Subvolume / snapshot READ support.
//
// Btrfs keeps a "tree of trees" — the ROOT_TREE (objectid 1) — whose
// ROOT_ITEM entries each describe a filesystem tree: the default FS_TREE
// (objectid 5), every user-created subvolume, and every snapshot (a snapshot
// is just a ROOT_ITEM pointing at a shared tree). Reading inside a subvolume
// or snapshot is identical to reading the default tree: the existing fs-tree
// reader is parameterised entirely by the tree's root bytenr, so all we need
// is to enumerate the ROOT_ITEMs (for ids and root bytenrs) and the ROOT_REF
// entries (for human-readable names and parent relationships).
//
// Creation of subvolumes/snapshots is out of scope here — that needs
// ref-counted extent backrefs. This file is READ only and does not alter any
// existing single-tree code path.

const (
	// ROOT_TREE item types (include/uapi/linux/btrfs_tree.h).
	//   BTRFS_ROOT_BACKREF_KEY = 144
	//   BTRFS_ROOT_REF_KEY     = 156
	typeRootBackref uint8 = 0x90
	typeRootRef     uint8 = 0x9C

	// Subvolume ids live in [BTRFS_FIRST_FREE_OBJECTID, BTRFS_LAST_FREE_OBJECTID].
	// The FS_TREE (5) is below this range; the various internal trees (extent,
	// chunk, dev, csum, …) have small well-known ids too. Snapshots and
	// user subvolumes are always allocated from the free range.
	firstFreeObjID uint64 = 256                // BTRFS_FIRST_FREE_OBJECTID
	lastFreeObjID  uint64 = 0xFFFFFFFFFFFFFF00 // BTRFS_LAST_FREE_OBJECTID

	// btrfs_root_item field offsets we read.
	rootItemOffGeneration = 0xA0 // __le64 generation
	rootItemOffRootDirID  = 0xA8 // __le64 root_dirid
	rootItemOffBytenr     = 0xB0 // __le64 bytenr (root node logical addr)
	rootItemMinSize       = rootItemOffBytenr + 8

	// btrfs_root_ref layout: dirid(8) + sequence(8) + name_len(2) + name.
	rootRefHdrSize = 18
)

// Subvolume describes one subvolume or snapshot enumerated from the ROOT_TREE.
//
// A snapshot is indistinguishable from a subvolume at this layer: both are
// ROOT_ITEMs in the ROOT_TREE pointing at an fs-tree root node. Name and
// ParentID come from the corresponding ROOT_REF; the top-level FS_TREE (id 5)
// has no ROOT_REF and is reported with an empty Name and ParentID 0.
type Subvolume struct {
	ID         uint64 // ROOT_ITEM objectid (subvolume / tree id)
	ParentID   uint64 // parent subvolume id (0 for the FS_TREE / unreferenced roots)
	Name       string // name within the parent's directory (empty for FS_TREE)
	RootBytenr uint64 // logical address of this tree's root node
	Generation uint64 // root_item.generation
}

// rootRef is one decoded ROOT_REF / ROOT_BACKREF tuple.
type rootRef struct {
	dirID uint64
	name  string
}

// parseRootRef decodes a single btrfs_root_ref payload. Returns ok=false on
// a short/garbled buffer.
func parseRootRef(d []byte) (rootRef, bool) {
	if len(d) < rootRefHdrSize {
		return rootRef{}, false
	}
	le := binary.LittleEndian
	nameLen := int(le.Uint16(d[16:]))
	if rootRefHdrSize+nameLen > len(d) {
		return rootRef{}, false
	}
	return rootRef{
		dirID: le.Uint64(d[0:]),
		name:  string(d[rootRefHdrSize : rootRefHdrSize+nameLen]),
	}, true
}

// enumerateSubvolumes walks the ROOT_TREE and returns one Subvolume per
// ROOT_ITEM in the free-objectid range plus the default FS_TREE (id 5).
// Names and parent ids are filled in from ROOT_REF entries when present.
func enumerateSubvolumes(fs *btrfsFS) ([]Subvolume, error) {
	r := fs.f
	partOff := fs.partOffset
	sb := fs.sb

	// 1. Collect every ROOT_ITEM in the ROOT_TREE. A given root id can carry
	//    several ROOT_ITEMs (keyed by offset = a snapshot generation); btrfs
	//    treats offset==0 as the live tree, so we keep the highest offset seen
	//    per id, which matches how the kernel resolves the current root.
	type rootInfo struct {
		bytenr     uint64
		generation uint64
		offset     uint64
		seen       bool
	}
	roots := map[uint64]rootInfo{}
	le := binary.LittleEndian

	// walkPrefixLeaves keys on a single objID, but the ROOT_TREE holds many
	// distinct objIDs (one per tree), so we walk every leaf of the ROOT_TREE
	// and pick out ROOT_ITEM / ROOT_REF / ROOT_BACKREF items.
	refs := map[uint64]rootRef{} // child root id -> ref carrying the leaf name
	parents := map[uint64]uint64{}
	if err := walkAllLeaves(r, partOff, sb, sb.rootLogAddr, func(buf []byte, it leafItem) bool {
		switch it.k.typ {
		case typeRootItem:
			id := it.k.objID
			d := it.data(buf)
			if len(d) < rootItemMinSize {
				return true
			}
			info := roots[id]
			// Prefer the entry with the highest key offset (live tree wins).
			if info.seen && it.k.offset < info.offset {
				return true
			}
			roots[id] = rootInfo{
				bytenr:     le.Uint64(d[rootItemOffBytenr:]),
				generation: le.Uint64(d[rootItemOffGeneration:]),
				offset:     it.k.offset,
				seen:       true,
			}
		case typeRootRef:
			// Key: (parent_root_id, ROOT_REF, child_root_id).
			if rr, ok := parseRootRef(it.data(buf)); ok {
				child := it.k.offset
				refs[child] = rr
				parents[child] = it.k.objID
			}
		case typeRootBackref:
			// Key: (child_root_id, ROOT_BACKREF, parent_root_id). Carries the
			// same name; use it only to fill gaps the ROOT_REF didn't cover.
			if rr, ok := parseRootRef(it.data(buf)); ok {
				child := it.k.objID
				if _, have := refs[child]; !have {
					refs[child] = rr
					parents[child] = it.k.offset
				}
			}
		}
		return true
	}); err != nil {
		return nil, fmt.Errorf("btrfs: walk ROOT_TREE: %w", err)
	}

	out := make([]Subvolume, 0, len(roots))
	for id, info := range roots {
		// Report the default FS_TREE and every subvolume/snapshot in the free
		// range. Internal trees (extent/chunk/dev/csum/uuid/…) are skipped.
		if id != fsTreeObjID && (id < firstFreeObjID || id > lastFreeObjID) {
			continue
		}
		sv := Subvolume{
			ID:         id,
			RootBytenr: info.bytenr,
			Generation: info.generation,
		}
		if rr, ok := refs[id]; ok {
			sv.Name = rr.name
			sv.ParentID = parents[id]
		}
		out = append(out, sv)
	}
	return out, nil
}

// walkAllLeaves performs a left-to-right depth-first traversal of every leaf
// in the tree rooted at rootLogAddr, invoking visit for every item. The walk
// stops early when visit returns false. Unlike walkPrefixLeaves it does not
// prune by key, so it is suitable for the ROOT_TREE where many distinct
// objIDs coexist.
func walkAllLeaves(r io.ReaderAt, partOff int64, sb *superblock, rootLogAddr uint64,
	visit func(buf []byte, it leafItem) bool,
) error {
	type frame struct {
		logAddr uint64
		depth   int
	}
	// Bound the ROOT_TREE walk: reject a revisited block pointer (cycle) and
	// cap the total node count and depth so a forged tree cannot livelock or
	// exhaust memory.
	var seen safeio.VisitSet
	nodeGuard := safeio.NewLoopGuard(maxTreeNodes)
	stack := []frame{{logAddr: rootLogAddr, depth: 0}}
	for len(stack) > 0 {
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if top.depth > maxBtreeDepth {
			return fmt.Errorf("btrfs: ROOT_TREE depth exceeds %d: %w", maxBtreeDepth, safeio.ErrLoopLimit)
		}
		if err := nodeGuard.Next(); err != nil {
			return fmt.Errorf("btrfs: walkAllLeaves: %w", err)
		}
		if err := seen.Check(top.logAddr); err != nil {
			return fmt.Errorf("btrfs: walkAllLeaves: %w", err)
		}
		buf, err := readNode(r, partOff, sb, top.logAddr)
		if err != nil {
			return err
		}
		hdr := parseNodeHeader(buf)
		if hdr.level == 0 {
			items := parseLeafItems(buf, hdr.nItems)
			for _, it := range items {
				if !visit(buf, it) {
					return nil
				}
			}
			continue
		}
		le := binary.LittleEndian
		var children []uint64
		for i := uint32(0); i < hdr.nItems; i++ {
			off := nodeHdrSize + int(i)*keyPtrSize
			if off+keyPtrSize > len(buf) {
				break
			}
			if child := le.Uint64(buf[off+17:]); child != 0 {
				children = append(children, child)
			}
		}
		// Push in reverse so the LIFO stack pops them left-to-right.
		for i := len(children) - 1; i >= 0; i-- {
			stack = append(stack, frame{logAddr: children[i], depth: top.depth + 1})
		}
	}
	return nil
}

// ─── Public subvolume API ──────────────────────────────────────────────────

// Subvolumes enumerates the subvolumes and snapshots recorded in the
// filesystem's ROOT_TREE. The slice includes the default FS_TREE (id 5,
// empty Name) plus every user subvolume and snapshot, each with its id,
// name, parent id, and the logical address of its tree's root node.
//
// Snapshots are not distinguished from subvolumes at this layer — both are
// ROOT_ITEMs pointing at an fs-tree. btrfs-specific; callers reach it via the
// concrete *btrfsFS (the FS interface, see below).
func (fs *btrfsFS) Subvolumes() ([]Subvolume, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return enumerateSubvolumes(fs)
}

// subvolView is a read-only filesystem.Filesystem rooted at one subvolume /
// snapshot tree. It shares the parent btrfsFS's device, partition offset, and
// superblock, differing only in which fs-tree root the read helpers descend.
// Write operations are intentionally unsupported — subvolume writes need
// extent-backref bookkeeping that is out of scope here.
type subvolView struct {
	parent     *btrfsFS
	fsTreeRoot uint64
	sub        Subvolume
}

// errSubvolReadOnly is returned by all mutating operations on a subvolume view.
var errSubvolReadOnly = fmt.Errorf("btrfs: subvolume view is read-only")

// OpenSubvolumeByID returns a read-only view of the subvolume/snapshot with
// the given tree id (e.g. 5 for the default FS_TREE, or a value ≥256 for a
// user subvolume or snapshot). ReadFile / ListDir / Stat / ReadLink on the
// returned view operate within that tree.
func (fs *btrfsFS) OpenSubvolumeByID(id uint64) (filesystem.Filesystem, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	subs, err := enumerateSubvolumes(fs)
	if err != nil {
		return nil, err
	}
	for _, s := range subs {
		if s.ID == id {
			if s.RootBytenr == 0 {
				return nil, fmt.Errorf("btrfs: subvolume id %d has no root bytenr", id)
			}
			return &subvolView{parent: fs, fsTreeRoot: s.RootBytenr, sub: s}, nil
		}
	}
	return nil, fmt.Errorf("btrfs: subvolume id %d: %w", id, ErrNotFound)
}

// OpenSubvolumeByName returns a read-only view of the subvolume/snapshot whose
// ROOT_REF name matches. Names are matched exactly against the name recorded
// in the parent's ROOT_REF (the leaf name, not a full path).
func (fs *btrfsFS) OpenSubvolumeByName(name string) (filesystem.Filesystem, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	subs, err := enumerateSubvolumes(fs)
	if err != nil {
		return nil, err
	}
	for _, s := range subs {
		if s.Name == name && s.Name != "" {
			if s.RootBytenr == 0 {
				return nil, fmt.Errorf("btrfs: subvolume %q has no root bytenr", name)
			}
			return &subvolView{parent: fs, fsTreeRoot: s.RootBytenr, sub: s}, nil
		}
	}
	return nil, fmt.Errorf("btrfs: subvolume %q: %w", name, ErrNotFound)
}

var _ filesystem.Filesystem = (*subvolView)(nil)

// Close is a no-op: the view borrows the parent FS's backend, which the caller
// closes via the parent.
func (v *subvolView) Close() error { return nil }

func (v *subvolView) ReadFile(path string) ([]byte, error) {
	p := v.parent
	p.mu.Lock()
	defer p.mu.Unlock()
	in, err := pathLookup(p.f, p.partOffset, p.sb, v.fsTreeRoot, path)
	if err != nil {
		return nil, err
	}
	if !in.isRegular() {
		return nil, fmt.Errorf("btrfs: %q is not a regular file", path)
	}
	return readFileData(p.f, p.partOffset, p.sb, v.fsTreeRoot, in)
}

func (v *subvolView) ListDir(path string) ([]filesystem.DirEntry, error) {
	p := v.parent
	p.mu.Lock()
	defer p.mu.Unlock()
	in, err := pathLookup(p.f, p.partOffset, p.sb, v.fsTreeRoot, path)
	if err != nil {
		return nil, err
	}
	if !in.isDir() {
		return nil, fmt.Errorf("btrfs: %q is not a directory", path)
	}
	return readDir(p.f, p.partOffset, p.sb, v.fsTreeRoot, in.num)
}

func (v *subvolView) Stat(path string) (filesystem.Stat, error) {
	p := v.parent
	p.mu.Lock()
	defer p.mu.Unlock()
	in, err := pathLookup(p.f, p.partOffset, p.sb, v.fsTreeRoot, path)
	if err != nil {
		return nil, err
	}
	return filesystem.NewStat(uint16(in.mode), in.size, in.num), nil
}

func (v *subvolView) ReadLink(path string) (string, error) {
	p := v.parent
	p.mu.Lock()
	defer p.mu.Unlock()
	in, err := pathLookup(p.f, p.partOffset, p.sb, v.fsTreeRoot, path)
	if err != nil {
		return "", err
	}
	if !in.isSymlink() {
		return "", fmt.Errorf("btrfs: %q is not a symbolic link", path)
	}
	return readSymlink(p.f, p.partOffset, p.sb, v.fsTreeRoot, in)
}

// Mutating operations are unsupported on a subvolume view.
func (v *subvolView) WriteFile(string, []byte, os.FileMode) error { return errSubvolReadOnly }
func (v *subvolView) MkDir(string, os.FileMode) error             { return errSubvolReadOnly }
func (v *subvolView) DeleteFile(string) error                     { return errSubvolReadOnly }
func (v *subvolView) DeleteDir(string) error                      { return errSubvolReadOnly }
func (v *subvolView) Rename(string, string) error                 { return errSubvolReadOnly }
