package filesystem_btrfs

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// These tests exercise read-path scenarios for compressed extents that the
// existing compression_test.go suite does not cover: reading a windowed
// sub-range of a regular compressed extent (non-zero extent offset, as
// produced when a compressed extent is shared or partially overwritten), and
// reading inline compressed extents for every supported codec (zlib, LZO,
// zstd).
//
// All fixtures are built entirely in memory with buildSingleLeafImage — no
// external mkfs tools, no mounting and no root are required. The minimal
// superblock maps logical addresses 1:1 onto physical offsets, so a
// compressed payload written at a physical offset can be referenced directly
// as the extent's disk_bytenr.

// payloadPhys is a physical offset clear of the FS leaf (testFsPhys =
// 0x022000, one node = 4096 bytes) and within testImageSize (0x080000) where
// the tests stash compressed extent payloads. Because the test superblock
// maps logical == physical, this value is also the extent disk_bytenr.
const payloadPhys = 0x030000

// TestReadFileData_CompressedExtentWindow reads a regular zlib-compressed
// extent through a non-zero extent offset. btrfs decompresses the whole
// on-disk extent (ram_bytes) and then copies the [extOffset, extOffset+
// numBytes) window into the file. This mirrors the on-disk shape left behind
// when a single compressed extent backs only a sub-range of a file (extent
// sharing / partial overwrite) and validates read.go's windowing arithmetic.
func TestReadFileData_CompressedExtentWindow(t *testing.T) {
	// The full decompressed extent. The file only consumes a middle window.
	full := bytes.Repeat([]byte("0123456789abcdef"), 64) // 1024 bytes, ram_bytes
	const extOffset = uint64(256)
	const fileSize = uint64(512) // window = full[256:768]
	want := full[extOffset : extOffset+fileSize]

	compressed := zlibCompress(t, full)

	const inoNum = uint64(610)
	imgBuf, sb := buildSingleLeafImage(t, func(leaf []byte) {
		le := binary.LittleEndian
		inodeBuf := make([]byte, inodeItemSize)
		le.PutUint64(inodeBuf[inodeOffSize:], fileSize)
		le.PutUint32(inodeBuf[inodeOffMode:], 0x81A4)
		_ = leafInsertItem(leaf, key{inoNum, typeInodeItem, 0}, inodeBuf)

		extBuf := encodeRegularExtentData(
			payloadPhys,             // disk_bytenr (== physical offset of payload)
			uint64(len(compressed)), // disk_num_bytes
			uint64(len(full)),       // ram_bytes (full decompressed extent)
			extOffset,               // window start within decompressed extent
			fileSize,                // window length
			1,                       // generation
			compressionZlib,
		)
		_ = leafInsertItem(leaf, key{inoNum, typeExtentData, 0}, extBuf)
	})

	// Stash the compressed payload at payloadPhys in the image buffer the
	// helper just allocated.
	if _, err := imgBuf.WriteAt(compressed, payloadPhys); err != nil {
		t.Fatalf("stash compressed payload: %v", err)
	}

	in := &inodeItem{num: inoNum, size: fileSize, mode: 0x8000}
	got, err := readFileData(imgBuf, 0, sb, testFsPhys, in)
	if err != nil {
		t.Fatalf("readFileData compressed window: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("compressed window mismatch:\n got %q\nwant %q", got, want)
	}
}

// TestReadFileData_InlineCompressed reads inline compressed extents through
// the read path for every supported codec. Inline extents store the
// compressed bytes directly after the file_extent_item header; readFileData
// must decompress them to ram_bytes and copy into the file. No existing test
// exercises the inline + compression combination.
func TestReadFileData_InlineCompressed(t *testing.T) {
	cases := []struct {
		name     string
		codec    uint8
		compress func(*testing.T, []byte) []byte
	}{
		{"zlib", compressionZlib, func(t *testing.T, p []byte) []byte { return zlibCompress(t, p) }},
		{"lzo", compressionLzo, func(t *testing.T, p []byte) []byte { return btrfsLzoEncodeAllLiterals(p) }},
		{"zstd", compressionZstd, func(t *testing.T, p []byte) []byte { return zstdCompress(t, p) }},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := bytes.Repeat([]byte(tc.name+"-inline-compresses-well-"), 8)
			fileSize := uint64(len(payload))
			compressed := tc.compress(t, payload)
			inoNum := uint64(620 + i)

			imgBuf, sb := buildSingleLeafImage(t, func(leaf []byte) {
				le := binary.LittleEndian
				inodeBuf := make([]byte, inodeItemSize)
				le.PutUint64(inodeBuf[inodeOffSize:], fileSize)
				le.PutUint32(inodeBuf[inodeOffMode:], 0x81A4)
				_ = leafInsertItem(leaf, key{inoNum, typeInodeItem, 0}, inodeBuf)

				// Inline extent: header followed immediately by the
				// compressed bytes.
				extBuf := make([]byte, extDataHdrSize+len(compressed))
				le.PutUint64(extBuf[extDataOffRamBytes:], fileSize)
				extBuf[extDataOffCompression] = tc.codec
				extBuf[extDataOffType] = extentDataInline
				copy(extBuf[extDataHdrSize:], compressed)
				_ = leafInsertItem(leaf, key{inoNum, typeExtentData, 0}, extBuf)
			})

			in := &inodeItem{num: inoNum, size: fileSize, mode: 0x8000}
			got, err := readFileData(imgBuf, 0, sb, testFsPhys, in)
			if err != nil {
				t.Fatalf("readFileData inline %s: %v", tc.name, err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("inline %s mismatch: got %d bytes, want %d bytes",
					tc.name, len(got), len(payload))
			}
		})
	}
}
