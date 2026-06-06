package filesystem_btrfs

import (
	"bytes"
	"path/filepath"
	"sort"
	"testing"
)

// readInodeRefNames returns the list of back-reference names recorded in
// the INODE_REF item for (child, parent). Empty slice when the item is
// absent. Sorted for deterministic comparison.
func readInodeRefNames(t *testing.T, fs *btrfsFS, child, parent uint64) []string {
	t.Helper()
	buf, it, err := searchTree(fs.f, fs.partOffset, fs.sb, fs.fsTreeRoot, child, typeInodeRef, parent)
	if err != nil {
		return nil
	}
	entries := parseInodeRefEntries(it.data(buf))
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.name
	}
	sort.Strings(names)
	return names
}

func TestLinkSameParent_MultipleEntriesShareOneItem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	body := []byte("shared body across many names")
	if err := fs.WriteFile("/a", body, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := bf.Link("/a", "/b"); err != nil {
		t.Fatalf("Link /a → /b: %v", err)
	}
	if err := bf.Link("/a", "/c"); err != nil {
		t.Fatalf("Link /a → /c: %v", err)
	}

	st, _ := fs.Stat("/a")
	names := readInodeRefNames(t, bf, st.Inode(), rootDirObjID)
	want := []string{"a", "b", "c"}
	if !equalStringSlice(names, want) {
		t.Fatalf("INODE_REF entries = %v, want %v", names, want)
	}
	// All three paths must still read the original content.
	for _, p := range []string{"/a", "/b", "/c"} {
		got, err := fs.ReadFile(p)
		if err != nil {
			t.Errorf("ReadFile %q: %v", p, err)
			continue
		}
		if !bytes.Equal(got, body) {
			t.Errorf("ReadFile %q = %q, want %q", p, got, body)
		}
	}
	// nlink should be 3.
	xs, err := bf.ExtendedStat("/a")
	if err != nil {
		t.Fatalf("ExtendedStat: %v", err)
	}
	if xs.NLink != 3 {
		t.Errorf("nlink = %d, want 3", xs.NLink)
	}
}

func TestLinkSameParent_PartialUnlinkKeepsOthers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	body := []byte("survives the carnage")
	if err := fs.WriteFile("/x", body, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := bf.Link("/x", "/y"); err != nil {
		t.Fatalf("Link /y: %v", err)
	}
	if err := bf.Link("/x", "/z"); err != nil {
		t.Fatalf("Link /z: %v", err)
	}
	st, _ := fs.Stat("/x")
	ino := st.Inode()

	// Drop one middle entry: the INODE_REF item must lose ONLY the "y"
	// tuple, not the whole item — the back-refs for "x" and "z" must
	// survive.
	if err := fs.DeleteFile("/y"); err != nil {
		t.Fatalf("DeleteFile /y: %v", err)
	}
	names := readInodeRefNames(t, bf, ino, rootDirObjID)
	want := []string{"x", "z"}
	if !equalStringSlice(names, want) {
		t.Fatalf("after unlink /y, INODE_REF entries = %v, want %v", names, want)
	}
	for _, p := range []string{"/x", "/z"} {
		got, err := fs.ReadFile(p)
		if err != nil {
			t.Errorf("ReadFile %q after unlink /y: %v", p, err)
			continue
		}
		if !bytes.Equal(got, body) {
			t.Errorf("ReadFile %q = %q, want %q", p, got, body)
		}
	}

	// Drop another; only "x" remains.
	if err := fs.DeleteFile("/z"); err != nil {
		t.Fatalf("DeleteFile /z: %v", err)
	}
	names = readInodeRefNames(t, bf, ino, rootDirObjID)
	if !equalStringSlice(names, []string{"x"}) {
		t.Fatalf("after unlink /z, INODE_REF entries = %v, want [\"x\"]", names)
	}

	// Final delete: inode is gone.
	if err := fs.DeleteFile("/x"); err != nil {
		t.Fatalf("DeleteFile /x: %v", err)
	}
	if _, _, err := searchTree(bf.f, bf.partOffset, bf.sb, bf.fsTreeRoot, ino, typeInodeItem, 0); err == nil {
		t.Fatalf("inode %d still present after final unlink", ino)
	}
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
