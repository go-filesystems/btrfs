package filesystem_btrfs

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"path/filepath"
	"testing"
)

// zlibCompress is a small helper that returns the zlib-compressed bytes of
// the given payload.
func zlibCompress(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatalf("zlib write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}
	return buf.Bytes()
}

// encodeRegularExtentData builds the on-disk bytes of an EXTENT_DATA item
// pointing at a compressed extent. The output is a btrfs_file_extent_item
// laid out exactly as readFileData expects.
func encodeRegularExtentData(diskBytenr, diskNumBytes, ramBytes, offset, numBytes, generation uint64, compression uint8) []byte {
	buf := make([]byte, extDataRegularSize)
	le := binary.LittleEndian
	le.PutUint64(buf[0x00:], generation)
	le.PutUint64(buf[extDataOffRamBytes:], ramBytes)
	buf[extDataOffCompression] = compression
	buf[extDataOffType] = extentDataRegular
	le.PutUint64(buf[extDataOffDiskBytenr:], diskBytenr)
	le.PutUint64(buf[extDataOffDiskNumBytes:], diskNumBytes)
	le.PutUint64(buf[extDataOffOffset:], offset)
	le.PutUint64(buf[extDataOffNumBytes:], numBytes)
	return buf
}

// TestZlib_ReadCompressedExtent crafts a file whose data lives in a
// zlib-compressed extent (not produced by our own writer, but matching the
// real btrfs on-disk shape), then reads it back through ReadFile.
func TestZlib_ReadCompressedExtent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, 8*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	bf := fs.(*btrfsFS)

	// First create the file the normal way so all the parent dir items and
	// the INODE_ITEM exist. We'll overwrite its single EXTENT_DATA item with
	// a hand-crafted compressed one.
	payload := bytes.Repeat([]byte("zlib-compresses-this-well-"), 256) // ~6.7 KiB
	// Start with placeholder content of the same size so the INODE size is right.
	placeholder := make([]byte, len(payload))
	if err := fs.WriteFile("/zfile.bin", placeholder, 0o644); err != nil {
		t.Fatalf("WriteFile placeholder: %v", err)
	}

	st, err := fs.Stat("/zfile.bin")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	ino := st.Inode()

	// Free the placeholder's data extents so we can install a fresh compressed
	// one in their place.
	freeInodeExtents(bf.f, bf.partOffset, bf.sb, bf.sm, bf.fsTreeRoot, ino)
	newRoot, err := cowDeletePrefix(nil, bf.f, bf.partOffset, bf.sb, bf.sm, bf.fsTreeRoot, ino, typeExtentData)
	if err != nil {
		t.Fatalf("cowDeletePrefix: %v", err)
	}
	bf.fsTreeRoot = newRoot

	// Compress the real payload and stage it on disk in a fresh allocation.
	compressed := zlibCompress(t, payload)
	physData, _, err := bf.sm.allocDataBytes(uint64(len(compressed)), uint64(bf.sb.sectorSize))
	if err != nil {
		t.Fatalf("allocDataBytes compressed payload: %v", err)
	}
	if _, err := bf.f.WriteAt(compressed, bf.partOffset+int64(physData)); err != nil {
		t.Fatalf("WriteAt compressed payload: %v", err)
	}
	logData := physToLog(bf.sb, physData)

	extData := encodeRegularExtentData(
		logData,
		uint64(len(compressed)), // disk_num_bytes (compressed size on disk)
		uint64(len(payload)),    // ram_bytes (decompressed size)
		0,                        // offset within the decompressed extent
		uint64(len(payload)),    // num_bytes used from the decompressed extent
		bf.sb.generation+1,
		compressionZlib,
	)
	newRoot, err = cowInsert(nil, bf.f, bf.partOffset, bf.sb, bf.sm, bf.fsTreeRoot, key{ino, typeExtentData, 0}, extData)
	if err != nil {
		t.Fatalf("cowInsert compressed extent: %v", err)
	}
	bf.fsTreeRoot = newRoot
	if err := updateFsTreeRoot(bf.f, bf.partOffset, bf.sb, bf.sm, bf.fsTreeRoot); err != nil {
		t.Fatalf("updateFsTreeRoot: %v", err)
	}

	got, err := fs.ReadFile("/zfile.bin")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("compressed read mismatch: got %d bytes (first 32 = %x), want %d bytes (first 32 = %x)",
			len(got), got[:min(32, len(got))], len(payload), payload[:min(32, len(payload))])
	}
}

// TestZlib_UnsupportedCompression returns an explicit error rather than
// silently returning garbage when the on-disk extent claims an unsupported
// compression algorithm.
func TestZlib_UnsupportedCompression(t *testing.T) {
	if _, err := decompressExtent([]byte("ignored"), compressionLzo, 16); err == nil {
		t.Fatalf("decompressExtent lzo: expected error, got nil")
	}
	if _, err := decompressExtent([]byte("ignored"), compressionZstd, 16); err == nil {
		t.Fatalf("decompressExtent zstd: expected error, got nil")
	}
	if _, err := decompressExtent([]byte("ignored"), 42, 16); err == nil {
		t.Fatalf("decompressExtent unknown code: expected error, got nil")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
