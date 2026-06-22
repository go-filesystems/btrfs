package filesystem_btrfs

import (
	"path/filepath"
	"testing"
)

// TestNodeCache_WriteThenReadSeesFreshData proves the metadata node cache stays
// consistent with writes: a read primes the cache, a subsequent overwrite must
// invalidate it (both via invalidateCache() and the superblock-generation
// guard), and the next read must observe the new content — never a stale cached
// tree block at a COW-recycled logical address.
func TestNodeCache_WriteThenReadSeesFreshData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()

	bfs, ok := fs.(*btrfsFS)
	if !ok {
		t.Fatalf("Format returned %T, want *btrfsFS", fs)
	}

	if err := fs.WriteFile("/f.bin", []byte("first"), 0o644); err != nil {
		t.Fatalf("WriteFile first: %v", err)
	}
	// Prime the cache.
	got, err := fs.ReadFile("/f.bin")
	if err != nil {
		t.Fatalf("ReadFile first: %v", err)
	}
	if string(got) != "first" {
		t.Fatalf("first read got %q, want %q", got, "first")
	}
	if bfs.cache == nil {
		t.Fatalf("expected node cache to be populated after a read")
	}

	// Overwrite: this COW-mutates the tree and must drop the cache.
	if err := fs.WriteFile("/f.bin", []byte("second-value"), 0o644); err != nil {
		t.Fatalf("WriteFile second: %v", err)
	}
	if bfs.cache != nil {
		t.Errorf("write did not invalidate the node cache")
	}

	got, err = fs.ReadFile("/f.bin")
	if err != nil {
		t.Fatalf("ReadFile second: %v", err)
	}
	if string(got) != "second-value" {
		t.Errorf("post-write read returned stale data %q, want %q", got, "second-value")
	}
}

// TestNodeCache_GenerationGuardRebuilds covers the generation-based staleness
// key directly: if a cache survives with a generation older than the live
// superblock (e.g. a fixture committed a new superblock without going through a
// public mutator), reader() must rebuild rather than serve a stale block.
func TestNodeCache_GenerationGuardRebuilds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bfs := fs.(*btrfsFS)

	if err := fs.WriteFile("/g.bin", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := fs.ReadFile("/g.bin"); err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if bfs.cache == nil {
		t.Fatalf("expected cache after read")
	}
	prev := bfs.cache
	// Simulate an out-of-band commit: bump the live generation while leaving the
	// (now stale) cache installed.
	bfs.sb.generation++
	r := bfs.reader()
	if r == prev {
		t.Errorf("reader() served a stale cache after generation advanced")
	}
	if bfs.cache == prev {
		t.Errorf("reader() did not rebuild the cache after generation advanced")
	}
	if bfs.cache.builtGen != bfs.sb.generation {
		t.Errorf("rebuilt cache builtGen=%d, want %d", bfs.cache.builtGen, bfs.sb.generation)
	}
}

// TestNodeCache_HitReturnsSameBuffer verifies the cache actually memoizes: a
// second readNode for the same logical address returns the identical buffer
// from the cache rather than re-reading the device.
func TestNodeCache_HitReturnsSameBuffer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bfs := fs.(*btrfsFS)

	if err := fs.WriteFile("/h.bin", []byte("y"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cr := newCachedReader(bfs.f, bfs.sb.generation)
	first, err := readNode(cr, bfs.partOffset, bfs.sb, bfs.fsTreeRoot)
	if err != nil {
		t.Fatalf("readNode first: %v", err)
	}
	if _, hit := cr.cachedNode(bfs.fsTreeRoot); !hit {
		t.Fatalf("expected fsTreeRoot to be cached after first readNode")
	}
	second, err := readNode(cr, bfs.partOffset, bfs.sb, bfs.fsTreeRoot)
	if err != nil {
		t.Fatalf("readNode second: %v", err)
	}
	if &first[0] != &second[0] {
		t.Errorf("cache hit returned a different buffer; expected the memoized slice")
	}
}
