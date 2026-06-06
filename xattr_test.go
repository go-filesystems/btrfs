package filesystem_btrfs

import (
	"bytes"
	"encoding/binary"
	"path/filepath"
	"testing"
)

// encodeXattrItem builds the on-disk bytes of a btrfs XATTR_ITEM, which
// reuses the btrfs_dir_item layout: 30-byte header + name + value.
func encodeXattrItem(name string, value []byte) []byte {
	nameBytes := []byte(name)
	buf := make([]byte, dirItemHdrSize+len(nameBytes)+len(value))
	le := binary.LittleEndian
	// location_objid(8) + location_type(1) + transid(8) all zero — the
	// referenced object is unused for xattrs.
	le.PutUint16(buf[0x19:], uint16(len(value))) // data_len
	le.PutUint16(buf[0x1B:], uint16(len(nameBytes)))
	// type byte at 0x1D — xattrs use 0 (no file-type meaning).
	copy(buf[dirItemHdrSize:], nameBytes)
	copy(buf[dirItemHdrSize+len(nameBytes):], value)
	return buf
}

func TestXattrs_ReadAfterCraft(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	if err := fs.WriteFile("/labeled.bin", []byte("hi"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	st, err := fs.Stat("/labeled.bin")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	ino := st.Inode()

	// Craft two xattrs on the inode by inserting XATTR_ITEM entries
	// directly. Distinct hash-style offsets keep the keys ordered.
	cases := []struct {
		name  string
		value []byte
	}{
		{"security.selinux", []byte("system_u:object_r:default_t:s0\x00")},
		{"user.note", []byte("written-by-our-test")},
	}
	for i, c := range cases {
		payload := encodeXattrItem(c.name, c.value)
		newRoot, err := cowInsert(nil, bf.f, bf.partOffset, bf.sb, bf.sm, bf.fsTreeRoot,
			key{ino, typeXattrItem, uint64(0x1000 + i)}, payload)
		if err != nil {
			t.Fatalf("cowInsert xattr %d: %v", i, err)
		}
		bf.fsTreeRoot = newRoot
	}
	if err := updateFsTreeRoot(bf.f, bf.partOffset, bf.sb, bf.sm, bf.fsTreeRoot); err != nil {
		t.Fatalf("updateFsTreeRoot: %v", err)
	}

	xs, err := bf.Xattrs("/labeled.bin")
	if err != nil {
		t.Fatalf("Xattrs: %v", err)
	}
	for _, c := range cases {
		got, ok := xs[c.name]
		if !ok {
			t.Errorf("missing xattr %q in result %v", c.name, xs)
			continue
		}
		if !bytes.Equal(got, c.value) {
			t.Errorf("xattr %q: got %q, want %q", c.name, got, c.value)
		}
	}
}

func TestXattrs_CleanedUpOnDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	if err := fs.WriteFile("/will-die.bin", []byte("ephemeral"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	st, err := fs.Stat("/will-die.bin")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	ino := st.Inode()

	payload := encodeXattrItem("user.note", []byte("orphan-candidate"))
	newRoot, err := cowInsert(nil, bf.f, bf.partOffset, bf.sb, bf.sm, bf.fsTreeRoot,
		key{ino, typeXattrItem, 0x4242}, payload)
	if err != nil {
		t.Fatalf("cowInsert: %v", err)
	}
	bf.fsTreeRoot = newRoot
	if err := updateFsTreeRoot(bf.f, bf.partOffset, bf.sb, bf.sm, bf.fsTreeRoot); err != nil {
		t.Fatalf("updateFsTreeRoot: %v", err)
	}

	// Sanity: the xattr is there before delete.
	if _, _, err := searchTree(bf.f, bf.partOffset, bf.sb, bf.fsTreeRoot, ino, typeXattrItem, 0x4242); err != nil {
		t.Fatalf("xattr not present before delete: %v", err)
	}

	if err := fs.DeleteFile("/will-die.bin"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	if _, _, err := searchTree(bf.f, bf.partOffset, bf.sb, bf.fsTreeRoot, ino, typeXattrItem, 0x4242); err == nil {
		t.Fatalf("xattr still present after DeleteFile — it should have been cleaned up")
	}
}
