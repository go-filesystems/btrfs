package filesystem_btrfs

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/go-volumes/safeio"
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

	// H1: in.size is an attacker-controlled uint64 (up to 2^63). Bound the
	// output allocation against the backing device size — no file can be
	// larger than the device that holds it.
	dev := deviceSize(r)
	out, err := safeio.MakeBytes(int64(in.size), dev)
	if err != nil {
		return nil, fmt.Errorf("btrfs: inode %d declared size %d: %w", in.num, in.size, err)
	}
	outLen := len(out)
	le := binary.LittleEndian

	for _, m := range items {
		fileOffset := m.k.offset // byte offset within the file
		// H2: a forged EXTENT_DATA key offset can exceed the file size; an
		// unguarded `in.size - fileOffset` underflows (wraps huge) and
		// `out[fileOffset:]` slices out of bounds. Skip extents that start at
		// or beyond EOF.
		if fileOffset >= in.size {
			continue
		}
		remaining := in.size - fileOffset // > 0, no underflow
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
			// H4: clamp the attacker's ram_bytes to a hard ceiling and to the
			// remaining file bytes before using it as a decompression bound.
			ramBytes := clampRAM(le.Uint64(d[extDataOffRamBytes:]), remaining)
			inlineData, err := decompressExtent(rawInline, compression, ramBytes)
			if err != nil {
				return nil, fmt.Errorf("btrfs: inode %d inline extent at file offset %d: %w",
					in.num, fileOffset, err)
			}
			n := uint64(len(inlineData))
			if n > remaining {
				n = remaining
			}
			// Bounds-check the destination slice (overflow-safe) before copy.
			dst, err := safeio.Slice(out, int(fileOffset), int(n))
			if err != nil {
				return nil, fmt.Errorf("btrfs: inode %d inline extent dest [%d,+%d): %w", in.num, fileOffset, n, err)
			}
			copy(dst, inlineData[:n])

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
			if numBytes > remaining {
				numBytes = remaining
			}
			// Overflow-safe destination bounds [fileOffset, fileOffset+numBytes).
			if err := safeio.CheckBounds(int(fileOffset), int(numBytes), outLen); err != nil {
				return nil, fmt.Errorf("btrfs: inode %d extent dest [%d,+%d): %w", in.num, fileOffset, numBytes, err)
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
			// H3: diskNumBytes is attacker-controlled; bound the read buffer
			// against the device size.
			compressed, err := safeio.MakeBytes(int64(diskNumBytes), dev)
			if err != nil {
				return nil, fmt.Errorf("btrfs: inode %d compressed extent disk_num_bytes %d: %w", in.num, diskNumBytes, err)
			}
			phys, err := sb.physAddr(partOff, diskBytenr)
			if err != nil {
				return nil, fmt.Errorf("btrfs: inode %d extent at file offset %d: %w",
					in.num, fileOffset, err)
			}
			if _, err := r.ReadAt(compressed, phys); err != nil {
				return nil, fmt.Errorf("btrfs: read compressed extent data: %w", err)
			}
			// H4: clamp ram_bytes to (extOffset+numBytes) and to the hard
			// ceiling so a huge declared ram_bytes can't drive a giant alloc.
			ramBytes := clampRAM(le.Uint64(d[extDataOffRamBytes:]), extOffset+numBytes)
			decompressed, err := decompressExtent(compressed, compression, ramBytes)
			if err != nil {
				return nil, fmt.Errorf("btrfs: inode %d extent at file offset %d: %w",
					in.num, fileOffset, err)
			}
			// Validate the source window [extOffset, extOffset+numBytes) within
			// the decompressed buffer (overflow-safe) before slicing.
			if err := safeio.CheckBounds(int(extOffset), int(numBytes), len(decompressed)); err != nil {
				return nil, fmt.Errorf("btrfs: decompressed extent too short for inode %d at file offset %d: %w",
					in.num, fileOffset, err)
			}
			copy(out[fileOffset:fileOffset+numBytes], decompressed[extOffset:extOffset+numBytes])
			// extentDataPrealloc: treat as zero/sparse.
		}
	}
	return out, nil
}

// clampRAM bounds an attacker-controlled ram_bytes value to a hard ceiling
// (maxDecompressRAM) and to a context-specific upper bound (the remaining
// file bytes or the required window), defeating H4 where the decompression
// "limit" is the attacker's own declared size.
func clampRAM(ramBytes, contextMax uint64) uint64 {
	if ramBytes > maxDecompressRAM {
		ramBytes = maxDecompressRAM
	}
	if ramBytes > contextMax {
		ramBytes = contextMax
	}
	return ramBytes
}

// decompressLimit turns a (possibly attacker-controlled) ram_bytes into a
// safe io.LimitReader bound: ramBytes+1 (the +1 lets us detect a stream that
// produces more than declared), clamped to maxDecompressRAM+1 so a value near
// 2^64 cannot overflow the int64 limit or allow an unbounded read.
func decompressLimit(ramBytes uint64) int64 {
	if ramBytes > maxDecompressRAM {
		ramBytes = maxDecompressRAM
	}
	return int64(ramBytes) + 1
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
		// allocate unbounded memory. ramBytes is clamped to the hard ceiling
		// so a huge or overflowing declared size can't defeat the limit.
		out, err := io.ReadAll(io.LimitReader(zr, decompressLimit(ramBytes)))
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
	out, err := io.ReadAll(io.LimitReader(zr, decompressLimit(ramBytes)))
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
