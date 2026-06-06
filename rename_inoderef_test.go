package filesystem_btrfs

import (
	"bytes"
	"path/filepath"
	"testing"
)

// TestRenameKeepsInodeRefsIntact — moving a hardlink across parents must
// (a) drop only ONE entry from the source parent's INODE_REF (preserving
// any other hardlinks under that parent) and (b) merge into any existing
// INODE_REF on the destination parent rather than creating a duplicate-key
// item.
func TestRenameKeepsInodeRefsIntact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	if err := fs.MkDir("/src", 0o755); err != nil {
		t.Fatalf("MkDir /src: %v", err)
	}
	if err := fs.MkDir("/dst", 0o755); err != nil {
		t.Fatalf("MkDir /dst: %v", err)
	}

	body := []byte("payload shared by many names")
	if err := fs.WriteFile("/src/a", body, 0o644); err != nil {
		t.Fatalf("WriteFile /src/a: %v", err)
	}
	// Add another link under /src so the source parent's INODE_REF item has
	// two entries to start with. After the rename of /src/a → /dst/a, the
	// source INODE_REF must keep "b".
	if err := bf.Link("/src/a", "/src/b"); err != nil {
		t.Fatalf("Link /src/b: %v", err)
	}
	// Pre-populate /dst with a different hardlink to the same inode so the
	// destination INODE_REF item already exists. The rename must MERGE into
	// it, not create a duplicate item.
	if err := bf.Link("/src/a", "/dst/preexisting"); err != nil {
		t.Fatalf("Link /dst/preexisting: %v", err)
	}

	srcStat, _ := fs.Stat("/src")
	dstStat, _ := fs.Stat("/dst")
	aStat, _ := fs.Stat("/src/a")
	srcInoRef := readInodeRefNames(t, bf, aStat.Inode(), srcStat.Inode())
	dstInoRef := readInodeRefNames(t, bf, aStat.Inode(), dstStat.Inode())
	if !equalStringSlice(srcInoRef, []string{"a", "b"}) {
		t.Fatalf("pre-rename /src INODE_REF entries = %v, want [a b]", srcInoRef)
	}
	if !equalStringSlice(dstInoRef, []string{"preexisting"}) {
		t.Fatalf("pre-rename /dst INODE_REF entries = %v, want [preexisting]", dstInoRef)
	}

	if err := fs.Rename("/src/a", "/dst/a"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	srcInoRef = readInodeRefNames(t, bf, aStat.Inode(), srcStat.Inode())
	dstInoRef = readInodeRefNames(t, bf, aStat.Inode(), dstStat.Inode())
	if !equalStringSlice(srcInoRef, []string{"b"}) {
		t.Errorf("after rename: /src INODE_REF entries = %v, want [b]", srcInoRef)
	}
	if !equalStringSlice(dstInoRef, []string{"a", "preexisting"}) {
		t.Errorf("after rename: /dst INODE_REF entries = %v, want [a preexisting]", dstInoRef)
	}

	// All three surviving paths must still read the original content.
	for _, p := range []string{"/src/b", "/dst/a", "/dst/preexisting"} {
		got, err := fs.ReadFile(p)
		if err != nil {
			t.Errorf("ReadFile %q: %v", p, err)
			continue
		}
		if !bytes.Equal(got, body) {
			t.Errorf("ReadFile %q = %q, want %q", p, got, body)
		}
	}
	// /src/a must no longer exist.
	if _, err := fs.Stat("/src/a"); err == nil {
		t.Errorf("/src/a still exists after rename")
	}
	// nlink unchanged (rename moves, doesn't add/remove links).
	xs, _ := bf.ExtendedStat("/src/b")
	if xs.NLink != 3 {
		t.Errorf("nlink after rename = %d, want 3", xs.NLink)
	}
}
