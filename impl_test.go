// Package-internal tests for filesystem-btrfs.
// Uses a programmatically built minimal btrfs image so no external tools are needed.
package filesystem_btrfs

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

const (
	testNodeSize  = 4096
	testImageSize = 0x080000
	testChunkPhys = 0x020000
	testRootPhys  = 0x021000
	testFsPhys    = 0x022000
)

func buildTestImageFile(t testing.TB) string {
	t.Helper()
	img := buildTestImageBytes()
	f, err := os.CreateTemp(t.TempDir(), "btrfs-*.img")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	if _, err := f.Write(img); err != nil {
		f.Close()
		t.Fatalf("write image: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close image: %v", err)
	}
	return f.Name()
}

func buildTestImageBytes() []byte {
	img := make([]byte, testImageSize)
	le := binary.LittleEndian
	sysChunk := buildSysChunkEntry(le, 0, testImageSize)
	sb := img[0x010000 : 0x010000+sbfSize]
	le.PutUint64(sb[sbfPhysAddr:], 0x010000)
	le.PutUint64(sb[sbfMagic:], sbMagic)
	le.PutUint64(sb[sbfGeneration:], 1)
	le.PutUint64(sb[sbfRootLogAddr:], testRootPhys)
	le.PutUint64(sb[sbfChunkLogAddr:], testChunkPhys)
	le.PutUint64(sb[sbfTotalBytes:], testImageSize)
	le.PutUint64(sb[sbfBytesUsed:], uint64(3*testNodeSize))
	le.PutUint64(sb[sbfRootDirObjID:], 6)
	le.PutUint64(sb[sbfNumDevices:], 1)
	le.PutUint32(sb[sbfSectorSize:], testNodeSize)
	le.PutUint32(sb[sbfNodeSize:], testNodeSize)
	le.PutUint32(sb[sbfLeafSize:], testNodeSize)
	le.PutUint32(sb[sbfStripeSize:], testNodeSize)
	le.PutUint32(sb[sbfSysChunkArrSz:], uint32(len(sysChunk)))
	copy(sb[sbfSysChunkArr:], sysChunk)
	updateSuperblockCRC(sb[:sbfSize])
	buildEmptyLeaf(img[testChunkPhys:testChunkPhys+testNodeSize], le, testChunkPhys)
	buildRootLeaf(img[testRootPhys:testRootPhys+testNodeSize], le, testRootPhys)
	buildFsLeaf(img[testFsPhys:testFsPhys+testNodeSize], le, testFsPhys)
	return img
}

func buildSysChunkEntry(le binary.ByteOrder, logStart, size uint64) []byte {
	k := make([]byte, keySize)
	le.PutUint64(k[0:], 1)
	k[8] = 0xE4
	le.PutUint64(k[9:], logStart)
	item := make([]byte, chunkHeaderSize+chunkStripeSize)
	le.PutUint64(item[chunkSize:], size)
	le.PutUint16(item[chunkNumStripes:], 1)
	le.PutUint16(item[chunkSubStripes:], 1)
	le.PutUint64(item[chunkHeaderSize+0:], 1)
	le.PutUint64(item[chunkHeaderSize+8:], 0)
	return append(k, item...)
}

func buildEmptyLeaf(buf []byte, le binary.ByteOrder, logAddr uint64) {
	le.PutUint64(buf[0x30:], logAddr)
	le.PutUint64(buf[0x38:], 1)
	le.PutUint64(buf[0x50:], 1)
	le.PutUint32(buf[0x60:], 0)
	buf[0x64] = 0
	updateNodeCRC(buf)
}

func buildRootLeaf(buf []byte, le binary.ByteOrder, logAddr uint64) {
	le.PutUint64(buf[0x30:], logAddr)
	le.PutUint64(buf[0x38:], 1)
	le.PutUint64(buf[0x50:], 1)
	buf[0x64] = 0
	rootItemData := make([]byte, 439)
	le.PutUint64(rootItemData[0xA0:], 1)          // generation
	le.PutUint64(rootItemData[0xA8:], 256)        // root_dirid
	le.PutUint64(rootItemData[0xB0:], testFsPhys) // bytenr (FS_TREE root node)
	_ = leafInsertItem(buf, key{fsTreeObjID, typeRootItem, 0}, rootItemData)
	le.PutUint64(buf[0x50:], 1)
	updateNodeCRC(buf)
}

func buildFsLeaf(buf []byte, le binary.ByteOrder, logAddr uint64) {
	le.PutUint64(buf[0x30:], logAddr)
	le.PutUint64(buf[0x38:], 1)
	le.PutUint64(buf[0x50:], 1)
	buf[0x64] = 0
	// root dir inode 256
	rinode := make([]byte, inodeItemSize)
	le.PutUint64(rinode[inodeOffGeneration:], 1)
	le.PutUint32(rinode[inodeOffNLink:], 1)
	le.PutUint32(rinode[inodeOffMode:], 0x41ED)
	_ = leafInsertItem(buf, key{rootDirObjID, typeInodeItem, 0}, rinode)
	_ = leafInsertItem(buf, key{rootDirObjID, typeDirIndex, 1}, encodeDirItem(rootDirObjID, typeInodeItem, ftDir, "."))
	_ = leafInsertItem(buf, key{rootDirObjID, typeDirIndex, 2}, encodeDirItem(rootDirObjID, typeInodeItem, ftDir, ".."))
	// regular file inode 257 "hello.txt" inline content="hello"
	const fileIno uint64 = 257
	finode := make([]byte, inodeItemSize)
	le.PutUint64(finode[inodeOffGeneration:], 1)
	le.PutUint64(finode[inodeOffSize:], 5)
	le.PutUint32(finode[inodeOffNLink:], 1)
	le.PutUint32(finode[inodeOffMode:], 0x81A4)
	_ = leafInsertItem(buf, key{fileIno, typeInodeItem, 0}, finode)
	_ = leafInsertItem(buf, key{fileIno, typeExtentData, 0}, encodeExtentDataInline([]byte("hello"), 1))
	fileDI := encodeDirItem(fileIno, typeInodeItem, ftRegFile, "hello.txt")
	_ = leafInsertItem(buf, key{rootDirObjID, typeDirIndex, 3}, fileDI)
	_ = leafInsertItem(buf, key{rootDirObjID, typeDirItem, hashDirName("hello.txt")}, fileDI)
	// symlink inode 258 "link" -> "hello.txt"
	const symIno uint64 = 258
	sinode := make([]byte, inodeItemSize)
	le.PutUint64(sinode[inodeOffGeneration:], 1)
	le.PutUint64(sinode[inodeOffSize:], 9)
	le.PutUint32(sinode[inodeOffNLink:], 1)
	le.PutUint32(sinode[inodeOffMode:], 0xA1FF)
	_ = leafInsertItem(buf, key{symIno, typeInodeItem, 0}, sinode)
	_ = leafInsertItem(buf, key{symIno, typeExtentData, 0}, encodeExtentDataInline([]byte("hello.txt"), 1))
	symDI := encodeDirItem(symIno, typeInodeItem, ftSymlink, "link")
	_ = leafInsertItem(buf, key{rootDirObjID, typeDirIndex, 4}, symDI)
	_ = leafInsertItem(buf, key{rootDirObjID, typeDirItem, hashDirName("link")}, symDI)
	le.PutUint64(buf[0x50:], 1)
	updateNodeCRC(buf)
}

func openTestFS(t testing.TB) filesystem.Filesystem {
	t.Helper()
	p := buildTestImageFile(t)
	fs, err := Open(p, 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { fs.Close() })
	return fs
}

// ── Open ──────────────────────────────────────────────────────────────────

func TestOpen_Nonexistent(t *testing.T) {
	_, err := Open("/no/such/file", 0)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestOpen_BadMagic(t *testing.T) {
	img := buildTestImageBytes()
	binary.LittleEndian.PutUint64(img[0x010000+sbfMagic:], 0xDEADBEEF)
	p := filepath.Join(t.TempDir(), "bad.img")
	_ = os.WriteFile(p, img, 0o600)
	_, err := Open(p, 0)
	if err == nil {
		t.Fatal("expected bad-magic error")
	}
}

func TestOpen_BadGeometry(t *testing.T) {
	img := buildTestImageBytes()
	binary.LittleEndian.PutUint32(img[0x010000+sbfNodeSize:], 0)
	updateSuperblockCRC(img[0x010000 : 0x010000+sbfSize])
	p := filepath.Join(t.TempDir(), "geom.img")
	_ = os.WriteFile(p, img, 0o600)
	_, err := Open(p, 0)
	if err == nil {
		t.Fatal("expected geometry error")
	}
}

func TestOpen_Close(t *testing.T) {
	fs := openTestFS(t)
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// ── ReadFile ──────────────────────────────────────────────────────────────

func TestReadFile_Hello(t *testing.T) {
	fs := openTestFS(t)
	data, err := fs.ReadFile("/hello.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("got %q want hello", data)
	}
}

func TestReadFile_NotFound(t *testing.T) {
	fs := openTestFS(t)
	_, err := fs.ReadFile("/noexist")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestReadFile_OnDir(t *testing.T) {
	fs := openTestFS(t)
	_, err := fs.ReadFile("/")
	if err == nil {
		t.Fatal("expected error reading dir as file")
	}
}

func TestReadFile_OnSymlink(t *testing.T) {
	fs := openTestFS(t)
	_, err := fs.ReadFile("/link")
	if err == nil {
		t.Fatal("expected error reading symlink as file")
	}
}

// ── ListDir ───────────────────────────────────────────────────────────────

func TestListDir_Root(t *testing.T) {
	fs := openTestFS(t)
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}
	if !names["hello.txt"] {
		t.Error("expected hello.txt")
	}
	if !names["link"] {
		t.Error("expected link")
	}
}

func TestListDir_NotFound(t *testing.T) {
	fs := openTestFS(t)
	_, err := fs.ListDir("/noexist")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestListDir_OnFile(t *testing.T) {
	fs := openTestFS(t)
	_, err := fs.ListDir("/hello.txt")
	if err == nil {
		t.Fatal("expected error listing file as dir")
	}
}

// ── Stat ──────────────────────────────────────────────────────────────────

func TestStat_File(t *testing.T) {
	fs := openTestFS(t)
	st, err := fs.Stat("/hello.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Size() != 5 {
		t.Fatalf("size got %d want 5", st.Size())
	}
}

func TestStat_Dir(t *testing.T) {
	fs := openTestFS(t)
	st, err := fs.Stat("/")
	if err != nil {
		t.Fatalf("Stat /: %v", err)
	}
	if st.Inode() != rootDirObjID {
		t.Fatalf("inode got %d want %d", st.Inode(), rootDirObjID)
	}
}

func TestStat_NotFound(t *testing.T) {
	fs := openTestFS(t)
	_, err := fs.Stat("/noexist")
	if err == nil {
		t.Fatal("expected error")
	}
}

// ── ReadLink ──────────────────────────────────────────────────────────────

func TestReadLink_OK(t *testing.T) {
	fs := openTestFS(t)
	target, err := fs.ReadLink("/link")
	if err != nil {
		t.Fatalf("ReadLink: %v", err)
	}
	if target != "hello.txt" {
		t.Fatalf("got %q want hello.txt", target)
	}
}

func TestReadLink_OnFile(t *testing.T) {
	fs := openTestFS(t)
	_, err := fs.ReadLink("/hello.txt")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestReadLink_NotFound(t *testing.T) {
	fs := openTestFS(t)
	_, err := fs.ReadLink("/noexist")
	if err == nil {
		t.Fatal("expected error")
	}
}

// -- MkDir ------------------------------------------------------------------

func TestMkDir_AndVerify(t *testing.T) {
	fs := openTestFS(t)
	if err := fs.MkDir("/newdir", 0o755); err != nil {
		t.Fatalf("MkDir: %v", err)
	}
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir after MkDir: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Name() == "newdir" {
			found = true
		}
	}
	if !found {
		t.Error("newdir not found after MkDir")
	}
}

func TestMkDir_ParentNotFound(t *testing.T) {
	fs := openTestFS(t)
	err := fs.MkDir("/noparent/child", 0o755)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestMkDir_AlreadyExists(t *testing.T) {
	fs := openTestFS(t)
	_ = fs.MkDir("/d1", 0o755)
	err := fs.MkDir("/d1", 0o755)
	if err == nil {
		t.Fatal("expected error for duplicate MkDir")
	}
}

// -- WriteFile --------------------------------------------------------------

func TestWriteFile_Create(t *testing.T) {
	fs := openTestFS(t)
	if err := fs.WriteFile("/new.txt", []byte("world"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	data, err := fs.ReadFile("/new.txt")
	if err != nil {
		t.Fatalf("ReadFile after WriteFile: %v", err)
	}
	if string(data) != "world" {
		t.Fatalf("got %q want world", data)
	}
}

func TestWriteFile_Overwrite(t *testing.T) {
	fs := openTestFS(t)
	if err := fs.WriteFile("/hello.txt", []byte("updated"), 0o644); err != nil {
		t.Fatalf("WriteFile overwrite: %v", err)
	}
	data, err := fs.ReadFile("/hello.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "updated" {
		t.Fatalf("got %q want updated", data)
	}
}

func TestWriteFile_Empty(t *testing.T) {
	fs := openTestFS(t)
	if err := fs.WriteFile("/empty.txt", nil, 0o644); err != nil {
		t.Fatalf("WriteFile empty: %v", err)
	}
	data, err := fs.ReadFile("/empty.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("expected empty, got %q", data)
	}
}

func TestWriteFile_ParentNotFound(t *testing.T) {
	fs := openTestFS(t)
	err := fs.WriteFile("/nopar/file.txt", []byte("x"), 0o644)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestWriteFile_OnDir(t *testing.T) {
	fs := openTestFS(t)
	_ = fs.MkDir("/mydir", 0o755)
	err := fs.WriteFile("/mydir", []byte("x"), 0o644)
	if err == nil {
		t.Fatal("expected error writing to directory path")
	}
}

func TestWriteFile_LargeData(t *testing.T) {
	fs := openTestFS(t)
	data := bytes.Repeat([]byte("A"), 12*1024)
	if err := fs.WriteFile("/large.txt", data, 0o644); err != nil {
		t.Fatalf("WriteFile large: %v", err)
	}
	got, err := fs.ReadFile("/large.txt")
	if err != nil {
		t.Fatalf("ReadFile large: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("large file content mismatch len got=%d want=%d", len(got), len(data))
	}
}

func TestWriteFile_LargeOverwrite(t *testing.T) {
	fs := openTestFS(t)
	data1 := bytes.Repeat([]byte("B"), 12*1024)
	data2 := bytes.Repeat([]byte("C"), 8*1024)
	if err := fs.WriteFile("/big.txt", data1, 0o644); err != nil {
		t.Fatalf("WriteFile large1: %v", err)
	}
	if err := fs.WriteFile("/big.txt", data2, 0o644); err != nil {
		t.Fatalf("WriteFile overwrite large: %v", err)
	}
	got, err := fs.ReadFile("/big.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, data2) {
		t.Fatalf("overwrite content mismatch len got=%d want=%d", len(got), len(data2))
	}
}

// -- DeleteFile -------------------------------------------------------------

func TestDeleteFile_OK(t *testing.T) {
	fs := openTestFS(t)
	if err := fs.DeleteFile("/hello.txt"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	_, err := fs.ReadFile("/hello.txt")
	if err == nil {
		t.Fatal("file still readable after delete")
	}
}

func TestDeleteFile_NotFound(t *testing.T) {
	fs := openTestFS(t)
	err := fs.DeleteFile("/noexist.txt")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestDeleteFile_OnDir(t *testing.T) {
	fs := openTestFS(t)
	_ = fs.MkDir("/todel", 0o755)
	err := fs.DeleteFile("/todel")
	if err == nil {
		t.Fatal("expected error deleting dir as file")
	}
}

func TestDeleteFile_LargeThenVerify(t *testing.T) {
	fs := openTestFS(t)
	data := bytes.Repeat([]byte("Z"), 12*1024)
	if err := fs.WriteFile("/large2.txt", data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.DeleteFile("/large2.txt"); err != nil {
		t.Fatalf("DeleteFile large: %v", err)
	}
	_, err := fs.ReadFile("/large2.txt")
	if err == nil {
		t.Fatal("large file still readable after delete")
	}
}

// -- DeleteDir --------------------------------------------------------------

func TestDeleteDir_OK(t *testing.T) {
	fs := openTestFS(t)
	_ = fs.MkDir("/todel2", 0o755)
	if err := fs.DeleteDir("/todel2"); err != nil {
		t.Fatalf("DeleteDir: %v", err)
	}
	_, err := fs.ListDir("/todel2")
	if err == nil {
		t.Fatal("dir still listable after delete")
	}
}

func TestDeleteDir_NotFound(t *testing.T) {
	fs := openTestFS(t)
	err := fs.DeleteDir("/noexist")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestDeleteDir_OnFile(t *testing.T) {
	fs := openTestFS(t)
	err := fs.DeleteDir("/hello.txt")
	if err == nil {
		t.Fatal("expected error deleting file as dir")
	}
}

func TestDeleteDir_NonEmpty(t *testing.T) {
	fs := openTestFS(t)
	_ = fs.MkDir("/parent", 0o755)
	_ = fs.WriteFile("/parent/child.txt", []byte("x"), 0o644)
	if err := fs.DeleteDir("/parent"); err != nil {
		t.Fatalf("DeleteDir non-empty: %v", err)
	}
	_, err := fs.ListDir("/parent")
	if err == nil {
		t.Fatal("parent dir still exists after recursive delete")
	}
}

func TestDeleteDir_Recursive(t *testing.T) {
	fs := openTestFS(t)
	_ = fs.MkDir("/top", 0o755)
	_ = fs.MkDir("/top/sub", 0o755)
	_ = fs.WriteFile("/top/sub/leaf.txt", []byte("leaf"), 0o644)
	_ = fs.WriteFile("/top/file.txt", []byte("top"), 0o644)
	if err := fs.DeleteDir("/top"); err != nil {
		t.Fatalf("DeleteDir recursive: %v", err)
	}
	_, err := fs.ListDir("/top")
	if err == nil {
		t.Fatal("dir still listable after recursive delete")
	}
}

func TestDeleteRoot(t *testing.T) {
	fs := openTestFS(t)
	err := fs.DeleteDir("/")
	if err == nil {
		t.Fatal("expected error deleting root")
	}
}

// -- Rename -----------------------------------------------------------------

func TestRename_File(t *testing.T) {
	fs := openTestFS(t)
	if err := fs.Rename("/hello.txt", "/bye.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	data, err := fs.ReadFile("/bye.txt")
	if err != nil {
		t.Fatalf("ReadFile after rename: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("got %q want hello", data)
	}
	_, err = fs.ReadFile("/hello.txt")
	if err == nil {
		t.Fatal("old name still reachable after rename")
	}
}

func TestRename_OverwriteFile(t *testing.T) {
	fs := openTestFS(t)
	_ = fs.WriteFile("/other.txt", []byte("other"), 0o644)
	if err := fs.Rename("/hello.txt", "/other.txt"); err != nil {
		t.Fatalf("Rename overwrite: %v", err)
	}
	data, err := fs.ReadFile("/other.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("got %q want hello after overwrite rename", data)
	}
}

func TestRename_SrcNotFound(t *testing.T) {
	fs := openTestFS(t)
	err := fs.Rename("/noexist.txt", "/dst.txt")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRename_SrcParentNotFound(t *testing.T) {
	fs := openTestFS(t)
	err := fs.Rename("/nosrc/file.txt", "/dst.txt")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRename_DstParentNotFound(t *testing.T) {
	fs := openTestFS(t)
	err := fs.Rename("/hello.txt", "/nodst/file.txt")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRename_ToSameDir(t *testing.T) {
	fs := openTestFS(t)
	_ = fs.MkDir("/rdir", 0o755)
	_ = fs.WriteFile("/rdir/f.txt", []byte("X"), 0o644)
	if err := fs.Rename("/rdir/f.txt", "/rdir/g.txt"); err != nil {
		t.Fatalf("Rename same dir: %v", err)
	}
	data, err := fs.ReadFile("/rdir/g.txt")
	if err != nil {
		t.Fatalf("ReadFile renamed: %v", err)
	}
	if string(data) != "X" {
		t.Fatalf("got %q want X", data)
	}
}

func TestRename_Dir(t *testing.T) {
	fs := openTestFS(t)
	_ = fs.MkDir("/srcdir", 0o755)
	_ = fs.WriteFile("/srcdir/inner.txt", []byte("inner"), 0o644)
	if err := fs.Rename("/srcdir", "/dstdir"); err != nil {
		t.Fatalf("Rename dir: %v", err)
	}
	data, err := fs.ReadFile("/dstdir/inner.txt")
	if err != nil {
		t.Fatalf("ReadFile in renamed dir: %v", err)
	}
	if string(data) != "inner" {
		t.Fatalf("got %q", data)
	}
}

// ── Leaf-level operations ────────────────────────────────────────────────
