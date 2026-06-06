package filesystem_btrfs

import (
	"encoding/binary"
	"path/filepath"
	"testing"
)

// readInodeGenTransSeq reads the on-disk generation, transid and sequence
// fields of the INODE_ITEM for the given inode number.
func readInodeGenTransSeq(t *testing.T, fs *btrfsFS, ino uint64) (generation, transid, sequence uint64) {
	t.Helper()
	buf, it, err := searchTree(fs.f, fs.partOffset, fs.sb, fs.fsTreeRoot, ino, typeInodeItem, 0)
	if err != nil {
		t.Fatalf("searchTree inode %d: %v", ino, err)
	}
	d := it.data(buf)
	if len(d) < inodeItemSize {
		t.Fatalf("INODE_ITEM too short")
	}
	le := binary.LittleEndian
	return le.Uint64(d[inodeOffGeneration:]), le.Uint64(d[inodeOffTransID:]), le.Uint64(d[inodeOffSequence:])
}

func TestTransIDSequence_InitialValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	if err := fs.WriteFile("/a.txt", []byte("v1"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	st, _ := fs.Stat("/a.txt")
	gen, trans, seq := readInodeGenTransSeq(t, bf, st.Inode())
	if gen == 0 {
		t.Errorf("freshly created inode has generation=0; expected the creation tx (>0)")
	}
	if trans != gen {
		t.Errorf("freshly created inode: transid=%d != generation=%d (should match at creation)", trans, gen)
	}
	if seq != 0 {
		t.Errorf("freshly created inode: sequence=%d, want 0", seq)
	}
}

func TestTransIDSequence_BumpsOnOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	if err := fs.WriteFile("/x.txt", []byte("v1"), 0o644); err != nil {
		t.Fatalf("WriteFile v1: %v", err)
	}
	st, _ := fs.Stat("/x.txt")
	ino := st.Inode()
	gen0, trans0, seq0 := readInodeGenTransSeq(t, bf, ino)

	if err := fs.WriteFile("/x.txt", []byte("v2"), 0o644); err != nil {
		t.Fatalf("WriteFile v2: %v", err)
	}
	gen1, trans1, seq1 := readInodeGenTransSeq(t, bf, ino)

	if gen1 != gen0 {
		t.Errorf("generation must be immutable across modifications: was %d, now %d", gen0, gen1)
	}
	if trans1 <= trans0 {
		t.Errorf("transid did not advance: was %d, now %d", trans0, trans1)
	}
	if seq1 <= seq0 {
		t.Errorf("sequence did not advance: was %d, now %d", seq0, seq1)
	}
}

func TestTransIDSequence_BumpsOnLink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	if err := fs.WriteFile("/a", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	st, _ := fs.Stat("/a")
	ino := st.Inode()
	_, _, seq0 := readInodeGenTransSeq(t, bf, ino)

	if err := bf.Link("/a", "/b"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	_, _, seq1 := readInodeGenTransSeq(t, bf, ino)
	if seq1 <= seq0 {
		t.Errorf("sequence did not advance after Link: was %d, now %d", seq0, seq1)
	}
}

func TestTransIDSequence_BumpsOnPartialUnlink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	if err := fs.WriteFile("/a", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := bf.Link("/a", "/b"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	st, _ := fs.Stat("/a")
	ino := st.Inode()
	_, _, seq0 := readInodeGenTransSeq(t, bf, ino)

	// Partial unlink: nlink drops 2 → 1, inode still exists, sequence must advance.
	if err := fs.DeleteFile("/b"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	_, _, seq1 := readInodeGenTransSeq(t, bf, ino)
	if seq1 <= seq0 {
		t.Errorf("sequence did not advance after partial unlink: was %d, now %d", seq0, seq1)
	}
}
