package filesystem_btrfs

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Subvolume / snapshot READ tests.
//
// btrfs-progs is NOT available in this environment, so there is no interop
// (round-trip against a kernel-created image) coverage — that lives behind the
// integration skip-gate in btrfs_test.go. The fixture built here is SYNTHETIC:
// we hand-assemble a btrfs image with the same single-SYSTEM-chunk layout that
// Format() emits, then add a SECOND fs-tree plus a SECOND ROOT_ITEM and a
// ROOT_REF naming it. This exercises exactly the new code path: enumerating
// ROOT_ITEM/ROOT_REF from the ROOT_TREE and reading inside a non-default
// subvolume tree using the existing fs-tree reader.

const (
	// Subvolume-test physical layout (single SYSTEM chunk maps logical 1:1 to
	// physical, exactly like Format()). All addresses are node-sized.
	svtChunkPhys     = 0x020000
	svtRootPhys      = 0x021000           // ROOT_TREE leaf (two ROOT_ITEMs + one ROOT_REF)
	svtFSPhys        = 0x022000           // default FS_TREE (id 5) leaf
	svtSubvolFSPhys  = 0x023000           // subvolume FS_TREE (id 256) leaf
	svtImageSize     = 0x100000           // 1 MiB
	svtSubvolID      = firstFreeObjID + 0 // 256: first user subvolume id
	svtSubvolName    = "snap1"
	svtSubvolFile    = "hello.txt"
	svtSubvolFileTxt = "inside the subvolume\n"
)

// buildSubvolFixture writes a synthetic btrfs image with a default FS_TREE and
// one extra subvolume tree (id 256) named "snap1" carrying a single regular
// file. Returns the image path.
func buildSubvolFixture(t *testing.T) string {
	t.Helper()
	le := binary.LittleEndian
	var uuid [16]byte
	for i := range uuid {
		uuid[i] = byte(i + 1)
	}

	buildEmptyLeaf := func(physAddr uint64) []byte {
		buf := make([]byte, fmtNodeSize)
		copy(buf[32:48], uuid[:])
		le.PutUint64(buf[0x30:], physAddr) // bytenr
		le.PutUint64(buf[0x50:], 1)        // generation
		le.PutUint32(buf[0x60:], 0)        // nritems
		buf[0x64] = 0                      // level = leaf
		return buf
	}

	// A minimal root-dir inode for a fs tree, plus "." / ".." dir-index entries.
	now := time.Now().UTC()
	rootDirInode := func() []byte {
		rinode := make([]byte, inodeItemSize)
		le.PutUint64(rinode[inodeOffGeneration:], 1)
		le.PutUint32(rinode[inodeOffNLink:], 2)
		le.PutUint32(rinode[inodeOffMode:], 0x41ED) // dir rwxr-xr-x
		le.PutUint64(rinode[inodeOffFlags:], inodeFlagNoDataSum)
		writeBtrfsTimespec(rinode[inodeOffATime:], now)
		writeBtrfsTimespec(rinode[inodeOffCTime:], now)
		writeBtrfsTimespec(rinode[inodeOffMTime:], now)
		writeBtrfsTimespec(rinode[inodeOffOTime:], now)
		return rinode
	}

	// ── Chunk tree leaf ─────────────────────────────────────────────────────
	chunkLeaf := buildEmptyLeaf(svtChunkPhys)
	_ = leafInsertItem(chunkLeaf, key{1, 0xE4, 0}, buildSysChunkItemBytes(le, svtImageSize))
	updateNodeCRC(chunkLeaf)

	// ── Default FS_TREE leaf (id 5): just the root dir ──────────────────────
	fsLeaf := buildEmptyLeaf(svtFSPhys)
	_ = leafInsertItem(fsLeaf, key{rootDirObjID, typeInodeItem, 0}, rootDirInode())
	_ = leafInsertItem(fsLeaf, key{rootDirObjID, typeDirIndex, 1}, encodeDirItem(rootDirObjID, typeInodeItem, ftDir, "."))
	_ = leafInsertItem(fsLeaf, key{rootDirObjID, typeDirIndex, 2}, encodeDirItem(rootDirObjID, typeInodeItem, ftDir, ".."))
	updateNodeCRC(fsLeaf)

	// ── Subvolume FS_TREE leaf (id 256): root dir + one regular file ────────
	subLeaf := buildEmptyLeaf(svtSubvolFSPhys)
	// Root dir inode with nlink bumped for the child entry is unnecessary for
	// reads; keep it simple.
	_ = leafInsertItem(subLeaf, key{rootDirObjID, typeInodeItem, 0}, rootDirInode())
	_ = leafInsertItem(subLeaf, key{rootDirObjID, typeDirIndex, 1}, encodeDirItem(rootDirObjID, typeInodeItem, ftDir, "."))
	_ = leafInsertItem(subLeaf, key{rootDirObjID, typeDirIndex, 2}, encodeDirItem(rootDirObjID, typeInodeItem, ftDir, ".."))

	// File inode (ino 257) with an inline EXTENT_DATA holding the text.
	const fileIno = rootDirObjID + 1
	finode := make([]byte, inodeItemSize)
	le.PutUint64(finode[inodeOffGeneration:], 1)
	le.PutUint32(finode[inodeOffNLink:], 1)
	le.PutUint32(finode[inodeOffMode:], 0x81A4) // regular rw-r--r--
	le.PutUint64(finode[inodeOffSize:], uint64(len(svtSubvolFileTxt)))
	le.PutUint64(finode[inodeOffFlags:], inodeFlagNoDataSum)
	writeBtrfsTimespec(finode[inodeOffATime:], now)
	writeBtrfsTimespec(finode[inodeOffCTime:], now)
	writeBtrfsTimespec(finode[inodeOffMTime:], now)
	writeBtrfsTimespec(finode[inodeOffOTime:], now)
	_ = leafInsertItem(subLeaf, key{fileIno, typeInodeItem, 0}, finode)

	// Inline EXTENT_DATA: header (0x15 bytes) + raw bytes.
	ext := make([]byte, extDataHdrSize+len(svtSubvolFileTxt))
	le.PutUint64(ext[extDataOffRamBytes:], uint64(len(svtSubvolFileTxt)))
	ext[extDataOffCompression] = compressionNone
	ext[extDataOffType] = extentDataInline
	copy(ext[extDataHdrSize:], svtSubvolFileTxt)
	_ = leafInsertItem(subLeaf, key{fileIno, typeExtentData, 0}, ext)

	// Directory entry in the subvolume root pointing at the file.
	_ = leafInsertItem(subLeaf, key{rootDirObjID, typeDirIndex, 3},
		encodeDirItem(fileIno, typeInodeItem, ftRegFile, svtSubvolFile))
	updateNodeCRC(subLeaf)

	// ── ROOT_TREE leaf: ROOT_ITEM(5), ROOT_ITEM(256), ROOT_REF(5->256) ──────
	rootLeaf := buildEmptyLeaf(svtRootPhys)

	rootItem := func(bytenr uint64) []byte {
		d := make([]byte, 439)
		le.PutUint64(d[rootItemOffGeneration:], 1)
		le.PutUint64(d[rootItemOffRootDirID:], rootDirObjID)
		le.PutUint64(d[rootItemOffBytenr:], bytenr)
		return d
	}
	_ = leafInsertItem(rootLeaf, key{fsTreeObjID, typeRootItem, 0}, rootItem(svtFSPhys))
	_ = leafInsertItem(rootLeaf, key{svtSubvolID, typeRootItem, 0}, rootItem(svtSubvolFSPhys))

	// ROOT_REF key: (parent=FS_TREE, ROOT_REF, child=subvolID). Payload:
	// dirid(8) + sequence(8) + name_len(2) + name.
	rref := make([]byte, rootRefHdrSize+len(svtSubvolName))
	le.PutUint64(rref[0:], rootDirObjID) // dirid in parent
	le.PutUint64(rref[8:], 2)            // sequence
	le.PutUint16(rref[16:], uint16(len(svtSubvolName)))
	copy(rref[rootRefHdrSize:], svtSubvolName)
	_ = leafInsertItem(rootLeaf, key{fsTreeObjID, typeRootRef, svtSubvolID}, rref)
	updateNodeCRC(rootLeaf)

	// ── Superblock (reuse Format's builder; point root tree at our root leaf) ─
	sb := buildSuperblockBytes(le, uuid, "subvoltest", svtImageSize)
	le.PutUint64(sb[sbfRootLogAddr:], svtRootPhys)
	le.PutUint64(sb[sbfChunkLogAddr:], svtChunkPhys)
	updateSuperblockCRC(sb)

	// ── Assemble and write the image file ───────────────────────────────────
	path := filepath.Join(t.TempDir(), "subvol.img")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("create image: %v", err)
	}
	if err := f.Truncate(svtImageSize); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	writes := []struct {
		off  int64
		data []byte
	}{
		{fmtSuperblockOff, sb},
		{svtChunkPhys, chunkLeaf},
		{svtRootPhys, rootLeaf},
		{svtFSPhys, fsLeaf},
		{svtSubvolFSPhys, subLeaf},
	}
	for _, w := range writes {
		if _, err := f.WriteAt(w.data, w.off); err != nil {
			t.Fatalf("write at 0x%X: %v", w.off, err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return path
}

func TestSubvolumes_Enumerate(t *testing.T) {
	path := buildSubvolFixture(t)
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	subs, err := fs.Subvolumes()
	if err != nil {
		t.Fatalf("Subvolumes: %v", err)
	}

	byID := map[uint64]Subvolume{}
	for _, s := range subs {
		byID[s.ID] = s
	}
	if len(byID) != 2 {
		t.Fatalf("expected 2 subvolumes (FS_TREE + snap1), got %d: %+v", len(byID), subs)
	}

	def, ok := byID[fsTreeObjID]
	if !ok {
		t.Fatalf("default FS_TREE (id %d) missing from enumeration", fsTreeObjID)
	}
	if def.RootBytenr != svtFSPhys {
		t.Errorf("FS_TREE root bytenr = 0x%X, want 0x%X", def.RootBytenr, svtFSPhys)
	}

	sub, ok := byID[svtSubvolID]
	if !ok {
		t.Fatalf("subvolume id %d missing from enumeration", svtSubvolID)
	}
	if sub.Name != svtSubvolName {
		t.Errorf("subvolume name = %q, want %q", sub.Name, svtSubvolName)
	}
	if sub.ParentID != fsTreeObjID {
		t.Errorf("subvolume parent = %d, want %d", sub.ParentID, fsTreeObjID)
	}
	if sub.RootBytenr != svtSubvolFSPhys {
		t.Errorf("subvolume root bytenr = 0x%X, want 0x%X", sub.RootBytenr, svtSubvolFSPhys)
	}
}

func TestSubvolume_ReadByID(t *testing.T) {
	path := buildSubvolFixture(t)
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	view, err := fs.OpenSubvolumeByID(svtSubvolID)
	if err != nil {
		t.Fatalf("OpenSubvolumeByID(%d): %v", svtSubvolID, err)
	}
	defer view.Close()

	entries, err := view.ListDir("/")
	if err != nil {
		t.Fatalf("subvol ListDir(/): %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Name() == svtSubvolFile {
			found = true
		}
	}
	if !found {
		t.Fatalf("subvolume root does not list %q; entries=%v", svtSubvolFile, entries)
	}

	data, err := view.ReadFile("/" + svtSubvolFile)
	if err != nil {
		t.Fatalf("subvol ReadFile: %v", err)
	}
	if string(data) != svtSubvolFileTxt {
		t.Errorf("subvol file content = %q, want %q", data, svtSubvolFileTxt)
	}

	// The default tree must NOT contain the subvolume's file — proves the read
	// is genuinely rooted at the subvolume tree, not the default one.
	if _, err := fs.ReadFile("/" + svtSubvolFile); err == nil {
		t.Errorf("default tree unexpectedly contains %q", svtSubvolFile)
	}
}

func TestSubvolume_ReadByName(t *testing.T) {
	path := buildSubvolFixture(t)
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	view, err := fs.OpenSubvolumeByName(svtSubvolName)
	if err != nil {
		t.Fatalf("OpenSubvolumeByName(%q): %v", svtSubvolName, err)
	}
	defer view.Close()

	data, err := view.ReadFile("/" + svtSubvolFile)
	if err != nil {
		t.Fatalf("subvol ReadFile: %v", err)
	}
	if string(data) != svtSubvolFileTxt {
		t.Errorf("subvol file content = %q, want %q", data, svtSubvolFileTxt)
	}
}

func TestSubvolume_NotFound(t *testing.T) {
	path := buildSubvolFixture(t)
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	if _, err := fs.OpenSubvolumeByID(9999); err == nil {
		t.Error("expected error opening nonexistent subvolume id")
	}
	if _, err := fs.OpenSubvolumeByName("does-not-exist"); err == nil {
		t.Error("expected error opening nonexistent subvolume name")
	}
}

// TestSubvolume_DefaultTreeStillReads guards against the new code disturbing
// the existing single-tree read path: the default FS_TREE must remain usable
// exactly as before.
func TestSubvolume_DefaultTreeStillReads(t *testing.T) {
	path := buildSubvolFixture(t)
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	if _, err := fs.ListDir("/"); err != nil {
		t.Fatalf("default ListDir(/): %v", err)
	}
	// Opening the default tree by id 5 must also work.
	view, err := fs.OpenSubvolumeByID(fsTreeObjID)
	if err != nil {
		t.Fatalf("OpenSubvolumeByID(5): %v", err)
	}
	defer view.Close()
	if _, err := view.ListDir("/"); err != nil {
		t.Fatalf("default subvol view ListDir(/): %v", err)
	}
}
