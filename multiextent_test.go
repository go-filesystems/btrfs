package filesystem_btrfs

import (
	"bytes"
	"path/filepath"
	"testing"
)

// TestMultiExtent_FragmentedFreeSpace verifies that WriteFile succeeds even
// when no single free extent is large enough — it must stitch together
// multiple smaller extents.
func TestMultiExtent_FragmentedFreeSpace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, 8*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()

	bf := fs.(*btrfsFS)
	sectorSize := uint64(bf.sb.sectorSize)

	// Fragment the free list: replace it with many same-sized chunks that are
	// each individually smaller than what we will write next.
	bf.sm.freeExts = nil
	const chunkSize uint64 = 16 * 1024 // 16 KiB chunks
	const numChunks = 16
	base := uint64(0x100000) // start beyond format-time metadata
	for i := 0; i < numChunks; i++ {
		bf.sm.freeExts = append(bf.sm.freeExts, freeExtent{
			physStart: base + uint64(i)*2*chunkSize, // 16 KiB free, 16 KiB hole between
			size:      chunkSize,
		})
	}

	// Write 100 KiB — larger than any single 16 KiB chunk, but smaller than
	// the total fragmented free space (16 × 16 = 256 KiB).
	const target = 100 * 1024
	body := bytes.Repeat([]byte("multi-extent-payload!"), target/21+1)
	body = body[:target]
	if err := fs.WriteFile("/big.bin", body, 0o644); err != nil {
		t.Fatalf("WriteFile (100 KiB into fragmented space): %v", err)
	}

	got, err := fs.ReadFile("/big.bin")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("ReadFile content mismatch: got %d bytes, want %d bytes", len(got), len(body))
	}

	// Count how many EXTENT_DATA items the file actually uses. With every
	// free chunk being 16 KiB and 100 KiB needed, we expect ~7 extents.
	st, err := fs.Stat("/big.bin")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	items, err := collectPrefixItems(bf.f, bf.partOffset, bf.sb, bf.fsTreeRoot, st.Inode(), typeExtentData)
	if err != nil {
		t.Fatalf("collectPrefixItems: %v", err)
	}
	sectors := uint64(0)
	for _, m := range items {
		_ = m // present so future assertions can inspect
		sectors++
	}
	_ = sectorSize
	if sectors < 2 {
		t.Fatalf("expected multiple extents for a fragmented 100 KiB write, got %d", sectors)
	}
	t.Logf("100 KiB file stored across %d extents in fragmented free space", sectors)
}
