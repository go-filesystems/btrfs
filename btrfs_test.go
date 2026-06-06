package filesystem_btrfs_test

import (
	"os"
	"testing"

	btrfs "github.com/go-filesystems/btrfs"
)

// integrationImagePath can be set to a path of a real Btrfs image for manual
// integration testing. Skipped in automated runs.
const integrationImagePath = ""

func TestOpen_NoImage(t *testing.T) {
	_, err := btrfs.Open("/nonexistent/path/to/image.img", -1)
	if err == nil {
		t.Fatal("expected error opening nonexistent image, got nil")
	}
}

func TestOpen_Integration(t *testing.T) {
	if integrationImagePath == "" {
		t.Skip("no test image provided; set integrationImagePath to run")
	}
	if _, err := os.Stat(integrationImagePath); os.IsNotExist(err) {
		t.Skipf("image not found: %s", integrationImagePath)
	}
	fs, err := btrfs.Open(integrationImagePath, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir(/): %v", err)
	}
	t.Logf("/ contains %d entries", len(entries))
	for _, e := range entries {
		t.Logf("  %s (ino=%d ft=%d)", e.Name(), e.Inode(), e.FileType())
	}
}
