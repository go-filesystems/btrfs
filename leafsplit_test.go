package filesystem_btrfs

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestLeafSplit_ManyFilesInOneDir verifies the leaf-split path: enough files
// in a single directory to grow the FS_TREE past a single leaf. Without leaf
// split, this fails at "leaf full".
func TestLeafSplit_ManyFilesInOneDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, 16*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()

	const n = 200
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("/f%05d.txt", i)
		body := []byte(fmt.Sprintf("content-%d\n", i))
		if err := fs.WriteFile(name, body, 0o644); err != nil {
			t.Fatalf("WriteFile %d (%q): %v", i, name, err)
		}
	}

	// Verify each file reads back its expected content.
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("/f%05d.txt", i)
		want := fmt.Sprintf("content-%d\n", i)
		got, err := fs.ReadFile(name)
		if err != nil {
			t.Fatalf("ReadFile %q: %v", name, err)
		}
		if string(got) != want {
			t.Fatalf("ReadFile %q: got %q want %q", name, got, want)
		}
	}

	// Verify listing returns the right number of entries.
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir /: %v", err)
	}
	if len(entries) < n {
		t.Fatalf("ListDir / returned %d entries, want at least %d", len(entries), n)
	}
}

// TestCowRecycling_SmallImageManyWrites verifies that COW reclaims old leaf /
// internal-node blocks back to the space manager. Without recycling the
// allocator runs out of space on a small image after a few hundred
// write/delete cycles even though the live working set is tiny.
func TestCowRecycling_SmallImageManyWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, 4*1024*1024, FormatConfig{}) // 4 MiB
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()

	// 500 write/delete cycles on a 4 MiB image would exhaust ~4096-byte
	// node allocations many times over (500 cycles × 5 node allocs each ≈
	// 10 MiB) — only possible if old COW nodes are returned to freeExts.
	for i := 0; i < 500; i++ {
		if err := fs.WriteFile("/x.txt", []byte("data"), 0o644); err != nil {
			t.Fatalf("WriteFile iter %d: %v", i, err)
		}
		if err := fs.DeleteFile("/x.txt"); err != nil {
			t.Fatalf("DeleteFile iter %d: %v", i, err)
		}
	}
}

// TestLeafSplit_DeepTree pushes enough files to force the tree past a single
// internal node — exercising the internal-node split path and the recursive
// "split-up-to-the-root" branch in cowInsertSplit.
func TestLeafSplit_DeepTree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, 128*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()

	// 2000 files exercises a much wider FS tree. With ~4 items per file +
	// roughly 30-40 items per leaf, that's ~250 leaves, well beyond a
	// single internal node's capacity (~121 key-ptrs at 4 KiB nodes).
	const n = 2000
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("/file_%06d", i)
		body := []byte(fmt.Sprintf("payload-%d\n", i))
		if err := fs.WriteFile(name, body, 0o644); err != nil {
			t.Fatalf("WriteFile %d: %v", i, err)
		}
	}

	// Spot-check reads at a few positions.
	for _, i := range []int{0, 1, 99, 777, 1234, n - 1} {
		name := fmt.Sprintf("/file_%06d", i)
		want := fmt.Sprintf("payload-%d\n", i)
		got, err := fs.ReadFile(name)
		if err != nil {
			t.Fatalf("ReadFile %q: %v", name, err)
		}
		if string(got) != want {
			t.Fatalf("ReadFile %q: got %q want %q", name, got, want)
		}
	}

	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir /: %v", err)
	}
	if len(entries) < n {
		t.Fatalf("ListDir / returned %d entries, want at least %d", len(entries), n)
	}
}
