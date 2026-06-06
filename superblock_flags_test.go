package filesystem_btrfs

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// readSBField reads `length` bytes at sbfOff from the primary superblock.
func readSBField(t *testing.T, path string, sbfOff int64, length int) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	buf := make([]byte, length)
	if _, err := f.ReadAt(buf, int64(superblockOffset)+sbfOff); err != nil {
		t.Fatalf("read superblock field at +0x%X: %v", sbfOff, err)
	}
	return buf
}

func TestSuperblock_IncompatMixedBackrefSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs.Close()

	flags := binary.LittleEndian.Uint64(readSBField(t, path, sbfIncompatFlags, 8))
	if flags&incompatMixedBackref == 0 {
		t.Errorf("incompat_flags = 0x%X, expected MIXED_BACKREF (0x%X) to be set", flags, incompatMixedBackref)
	}
}

func TestSuperblock_CsumTypeIsCRC32C(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs.Close()

	csumType := binary.LittleEndian.Uint16(readSBField(t, path, sbfCsumType, 2))
	if csumType != csumTypeCRC32C {
		t.Errorf("csum_type = %d, want %d (CRC32C)", csumType, csumTypeCRC32C)
	}
}

func TestSuperblock_ChunkRootGenerationSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs.Close()

	chunkGen := binary.LittleEndian.Uint64(readSBField(t, path, sbfChunkRootGeneration, 8))
	if chunkGen != 1 {
		t.Errorf("chunk_root_generation = %d, want 1 at format time", chunkGen)
	}
}

func TestSuperblock_FieldsSurviveWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	// Trigger a writeSuperblock by performing a WriteFile (which COWs the
	// FS_TREE and persists the new root via writeSuperblock).
	if err := fs.WriteFile("/touch.txt", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fs.Close()

	// All three fields must still be intact in the on-disk superblock.
	if got := binary.LittleEndian.Uint64(readSBField(t, path, sbfIncompatFlags, 8)); got&incompatMixedBackref == 0 {
		t.Errorf("incompat_flags lost after write: 0x%X", got)
	}
	if got := binary.LittleEndian.Uint16(readSBField(t, path, sbfCsumType, 2)); got != csumTypeCRC32C {
		t.Errorf("csum_type lost after write: %d", got)
	}
	if got := binary.LittleEndian.Uint64(readSBField(t, path, sbfChunkRootGeneration, 8)); got != 1 {
		t.Errorf("chunk_root_generation lost after write: %d", got)
	}
}
