package filesystem_btrfs

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

// EXTENT_DATA item layout (btrfs_file_extent_item):
//
//	0x00 uint64 generation
//	0x08 uint64 ram_bytes  (size of decoded data)
//	0x10 uint8  compression
//	0x11 uint8  encryption
//	0x12 uint16 other_encoding
//	0x14 uint8  type  (0=inline, 1=regular, 2=prealloc)
//	0x15 ...    (inline: raw bytes; regular: disk_bytenr(8)+disk_num_bytes(8)+offset(8)+num_bytes(8))
const (
	extDataOffRamBytes     = 0x08
	extDataOffCompression  = 0x10
	extDataOffType         = 0x14
	extDataOffDiskBytenr   = 0x15
	extDataOffDiskNumBytes = 0x1D
	extDataOffOffset       = 0x25
	extDataOffNumBytes     = 0x2D
	extDataHdrSize         = 0x15                  // size before inline data / regular fields
	extDataRegularSize     = extDataHdrSize + 0x20 // 0x35 total for regular
)

// readFileData reads and reassembles the full content of a regular file.
// Extent items are collected via collectPrefixItems so files whose
// EXTENT_DATA items span multiple leaves still read fully.
func readFileData(r io.ReaderAt, partOff int64, sb *superblock, fsTreeRoot uint64, in *inodeItem) ([]byte, error) {
	if in.size == 0 {
		return []byte{}, nil
	}

	items, err := collectPrefixItems(r, partOff, sb, fsTreeRoot, in.num, typeExtentData)
	if err != nil {
		return nil, fmt.Errorf("btrfs: read extents for inode %d: %w", in.num, err)
	}

	out := make([]byte, in.size)
	le := binary.LittleEndian

	for _, m := range items {
		fileOffset := m.k.offset // byte offset within the file
		d := m.data
		if len(d) <= extDataOffType {
			continue
		}
		compression := d[extDataOffCompression]
		extType := d[extDataOffType]
		switch extType {
		case extentDataInline:
			// Inline: data immediately follows the header.
			rawInline := d[extDataHdrSize:]
			ramBytes := le.Uint64(d[extDataOffRamBytes:])
			inlineData, err := decompressExtent(rawInline, compression, ramBytes)
			if err != nil {
				return nil, fmt.Errorf("btrfs: inode %d inline extent at file offset %d: %w",
					in.num, fileOffset, err)
			}
			n := uint64(len(inlineData))
			if fileOffset+n > in.size {
				n = in.size - fileOffset
			}
			copy(out[fileOffset:], inlineData[:n])

		case extentDataRegular:
			if len(d) < extDataRegularSize {
				continue
			}
			diskBytenr := le.Uint64(d[extDataOffDiskBytenr:])
			if diskBytenr == 0 {
				// Sparse extent — already zero.
				continue
			}
			diskNumBytes := le.Uint64(d[extDataOffDiskNumBytes:])
			extOffset := le.Uint64(d[extDataOffOffset:])
			numBytes := le.Uint64(d[extDataOffNumBytes:])
			if fileOffset+numBytes > in.size {
				numBytes = in.size - fileOffset
			}
			if compression == compressionNone {
				phys, err := sb.physAddr(partOff, diskBytenr+extOffset)
				if err != nil {
					return nil, fmt.Errorf("btrfs: inode %d extent at file offset %d: %w",
						in.num, fileOffset, err)
				}
				if _, err := r.ReadAt(out[fileOffset:fileOffset+numBytes], phys); err != nil {
					return nil, fmt.Errorf("btrfs: read extent data: %w", err)
				}
				continue
			}
			// Compressed extent: read the on-disk compressed payload at
			// diskBytenr (size = diskNumBytes), decompress to the full
			// ram_bytes, then copy [extOffset, extOffset+numBytes) into out.
			ramBytes := le.Uint64(d[extDataOffRamBytes:])
			phys, err := sb.physAddr(partOff, diskBytenr)
			if err != nil {
				return nil, fmt.Errorf("btrfs: inode %d extent at file offset %d: %w",
					in.num, fileOffset, err)
			}
			compressed := make([]byte, diskNumBytes)
			if _, err := r.ReadAt(compressed, phys); err != nil {
				return nil, fmt.Errorf("btrfs: read compressed extent data: %w", err)
			}
			decompressed, err := decompressExtent(compressed, compression, ramBytes)
			if err != nil {
				return nil, fmt.Errorf("btrfs: inode %d extent at file offset %d: %w",
					in.num, fileOffset, err)
			}
			if uint64(len(decompressed)) < extOffset+numBytes {
				return nil, fmt.Errorf("btrfs: decompressed extent too short (%d < %d) for inode %d at file offset %d",
					len(decompressed), extOffset+numBytes, in.num, fileOffset)
			}
			copy(out[fileOffset:fileOffset+numBytes], decompressed[extOffset:extOffset+numBytes])
			// extentDataPrealloc: treat as zero/sparse.
		}
	}
	return out, nil
}

// decompressExtent expands a compressed extent payload. When compression is
// 0 (none), src is returned as-is. ramBytes is the expected decompressed
// length and bounds the output buffer to avoid runaway allocations on
// malformed input.
func decompressExtent(src []byte, compression uint8, ramBytes uint64) ([]byte, error) {
	switch compression {
	case compressionNone:
		return src, nil
	case compressionZlib:
		zr, err := zlib.NewReader(bytes.NewReader(src))
		if err != nil {
			return nil, fmt.Errorf("zlib reader: %w", err)
		}
		defer zr.Close()
		// Cap reads at ramBytes + a small slack so a malformed stream cannot
		// allocate unbounded memory.
		limit := int64(ramBytes) + 1
		if limit <= 0 {
			limit = 1 << 30
		}
		out, err := io.ReadAll(io.LimitReader(zr, limit))
		if err != nil {
			return nil, fmt.Errorf("zlib decompress: %w", err)
		}
		return out, nil
	case compressionLzo:
		return decompressLzo(src, ramBytes)
	case compressionZstd:
		return decompressZstd(src, ramBytes)
	default:
		return nil, fmt.Errorf("btrfs: unknown compression code %d", compression)
	}
}

// decompressZstd decompresses a btrfs zstd-compressed extent. The
// klauspost/compress zstd decoder handles all on-disk zstd frames produced
// by the kernel; ramBytes caps the output to defend against malformed
// input.
func decompressZstd(src []byte, ramBytes uint64) ([]byte, error) {
	zr, err := zstd.NewReader(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("zstd reader: %w", err)
	}
	defer zr.Close()
	limit := int64(ramBytes) + 1
	if limit <= 0 {
		limit = 1 << 30
	}
	out, err := io.ReadAll(io.LimitReader(zr, limit))
	if err != nil {
		return nil, fmt.Errorf("zstd decompress: %w", err)
	}
	return out, nil
}

// readSymlink reads the target of a symlink inode.
// On btrfs, symlink data is stored as an inline EXTENT_DATA item.
func readSymlink(r io.ReaderAt, partOff int64, sb *superblock, fsTreeRoot uint64, in *inodeItem) (string, error) {
	data, err := readFileData(r, partOff, sb, fsTreeRoot, in)
	if err != nil {
		return "", fmt.Errorf("btrfs: read symlink inode %d: %w", in.num, err)
	}
	return string(data), nil
}
