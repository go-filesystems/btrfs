package filesystem_btrfs

import (
	"fmt"
	"path/filepath"
	"testing"
)

// treeHeight returns the level of the root node + 1. A bare empty FS_TREE
// (single leaf) is height 1.
func treeHeight(t *testing.T, fs *btrfsFS) int {
	t.Helper()
	buf, err := readNode(fs.f, fs.partOffset, fs.sb, fs.fsTreeRoot)
	if err != nil {
		t.Fatalf("readNode root: %v", err)
	}
	hdr := parseNodeHeader(buf)
	return int(hdr.level) + 1
}

// TestLeafShrink_BackToSingleLeaf grows the FS_TREE past a single leaf
// (forcing leaf splits and at least one internal-node level), then deletes
// every file. The tree must shrink back to a single empty leaf — empty
// leaves and their now-orphan parents are pruned by cowMutate's shrink
// path, not left dangling.
func TestLeafShrink_BackToSingleLeaf(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, 16*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()

	bf := fs.(*btrfsFS)
	if h := treeHeight(t, bf); h != 1 {
		t.Fatalf("expected fresh tree to be a single leaf (height 1), got %d", h)
	}

	const n = 200
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("/f%04d.txt", i)
		body := []byte(fmt.Sprintf("payload-%d\n", i))
		if err := fs.WriteFile(name, body, 0o644); err != nil {
			t.Fatalf("WriteFile %d: %v", i, err)
		}
	}
	if h := treeHeight(t, bf); h < 2 {
		t.Fatalf("expected tree to grow past a single leaf after %d files, height=%d", n, h)
	}
	grownHeight := treeHeight(t, bf)
	t.Logf("after %d files: tree height = %d", n, grownHeight)

	for i := 0; i < n; i++ {
		name := fmt.Sprintf("/f%04d.txt", i)
		if err := fs.DeleteFile(name); err != nil {
			t.Fatalf("DeleteFile %d: %v", i, err)
		}
	}

	if h := treeHeight(t, bf); h != 1 {
		t.Errorf("after deleting all files, expected tree to shrink to a single leaf (height 1), got %d", h)
	}

	// Sanity check: a fresh WriteFile still works after the shrink.
	if err := fs.WriteFile("/after.txt", []byte("ok"), 0o644); err != nil {
		t.Fatalf("WriteFile after shrink: %v", err)
	}
	got, err := fs.ReadFile("/after.txt")
	if err != nil {
		t.Fatalf("ReadFile after shrink: %v", err)
	}
	if string(got) != "ok" {
		t.Fatalf("ReadFile after shrink: got %q want %q", got, "ok")
	}
}
