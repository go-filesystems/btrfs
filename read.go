package filesystem_btrfs

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/go-volumes/safeio"
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

// readFileData reads and reassembles the full content of a regular file, with
// the default read parallelism (see SetReadParallelism).
func readFileData(r io.ReaderAt, partOff int64, sb *superblock, fsTreeRoot uint64, in *inodeItem) ([]byte, error) {
	return readFileDataN(r, partOff, sb, fsTreeRoot, in, defaultReadParallelism())
}

// extent job kinds of a file read.
const (
	jobFail       = iota // a planning error: the job reports it, in file order
	jobInline            // inline data (possibly compressed), decoded in work
	jobRaw               // an uncompressed regular extent, read in emit
	jobCompressed        // a compressed regular extent, read and decoded in work
)

// readJob is one extent item of a file read, validated by the serial planning
// pass; work and emit are pure functions of it.
type readJob struct {
	kind         int
	err          error // jobFail
	fileOffset   uint64
	remaining    uint64 // file bytes from fileOffset to EOF
	numBytes     uint64
	extOffset    uint64
	diskBytenr   uint64
	diskNumBytes uint64
	ramBytes     uint64 // the item's ram_bytes, not yet clamped
	compression  uint8
	inline       []byte // jobInline: the payload, aliasing the item
}

// readFileDataN reads and reassembles the full content of a regular file,
// decoding up to workers compressed extents at once. Extent items are collected
// via collectPrefixItems so files whose EXTENT_DATA items span multiple leaves
// still read fully.
//
// The items are walked and checked serially (planFileRead); their data is then
// read and decoded by inOrder: the compressed extents on the workers, the copy
// into out in file order from this goroutine. The error returned is the first
// in file order, whatever the workers' speeds.
func readFileDataN(r io.ReaderAt, partOff int64, sb *superblock, fsTreeRoot uint64, in *inodeItem, workers int) ([]byte, error) {
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
	jobs := planFileRead(items, in, len(out))

	work := func(k int) ([]byte, error) { return jobs[k].decode(r, partOff, sb, dev, in.num) }
	emit := func(k int, data []byte) error { return jobs[k].place(out, data, r, partOff, sb, in.num) }
	if err := inOrder(len(jobs), workers, work, emit); err != nil {
		return nil, err
	}
	return out, nil
}

// planFileRead turns the EXTENT_DATA items of a file into jobs, in file order.
// It stops at the first item that cannot be read, which becomes a jobFail: the
// jobs before it still run, so the error reported is the one the serial walk
// meets first.
func planFileRead(items []prefixMatch, in *inodeItem, outLen int) []readJob {
	le := binary.LittleEndian
	var jobs []readJob

	// prevEnd is where the previous regular extent's file range ended. The
	// kernel refuses a leaf in which "file extent end range goes beyond start
	// offset of the next file extent" (check_extent_data_item,
	// fs/btrfs/tree-checker.c), and also refuses an end that overflows. A
	// reader that does not check pays the whole extent again for every item
	// that overlaps it: a leaf of tiny items each claiming the whole file made
	// the read cost items x file size instead of file size.
	var prevEnd uint64

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
		switch d[extDataOffType] {
		case extentDataInline:
			jobs = append(jobs, readJob{
				kind: jobInline, fileOffset: fileOffset, remaining: remaining,
				compression: compression, inline: d[extDataHdrSize:],
				// H4: clamp the attacker's ram_bytes to a hard ceiling and to the
				// remaining file bytes before using it as a decompression bound.
				ramBytes: clampRAM(le.Uint64(d[extDataOffRamBytes:]), remaining),
			})

		case extentDataRegular:
			if len(d) < extDataRegularSize {
				continue
			}
			end := fileOffset + le.Uint64(d[extDataOffNumBytes:])
			if end < fileOffset || fileOffset < prevEnd {
				return append(jobs, readJob{kind: jobFail, err: fmt.Errorf("btrfs: inode %d extent at file offset %d overlaps the previous extent (ends at %d): %w",
					in.num, fileOffset, prevEnd, safeio.ErrOutOfBounds)})
			}
			prevEnd = end
			diskBytenr := le.Uint64(d[extDataOffDiskBytenr:])
			if diskBytenr == 0 {
				// Sparse extent — already zero.
				continue
			}
			numBytes := le.Uint64(d[extDataOffNumBytes:])
			if numBytes > remaining {
				numBytes = remaining
			}
			// Overflow-safe destination bounds [fileOffset, fileOffset+numBytes).
			if err := safeio.CheckBounds(int(fileOffset), int(numBytes), outLen); err != nil {
				return append(jobs, readJob{kind: jobFail, err: fmt.Errorf("btrfs: inode %d extent dest [%d,+%d): %w", in.num, fileOffset, numBytes, err)})
			}
			j := readJob{
				kind: jobRaw, fileOffset: fileOffset, remaining: remaining, numBytes: numBytes,
				extOffset: le.Uint64(d[extDataOffOffset:]), diskBytenr: diskBytenr,
				diskNumBytes: le.Uint64(d[extDataOffDiskNumBytes:]), compression: compression,
			}
			if compression != compressionNone {
				j.kind = jobCompressed
				// H4: clamp ram_bytes to (extOffset+numBytes) and to the hard
				// ceiling so a huge declared ram_bytes can't drive a giant alloc.
				j.ramBytes = clampRAM(le.Uint64(d[extDataOffRamBytes:]), j.extOffset+numBytes)
			}
			jobs = append(jobs, j)
			// extentDataPrealloc: treat as zero/sparse.
		}
	}
	return jobs
}

// decode is the part of a job that runs on a worker: it touches no shared
// state but the device (ReadAt). It returns the bytes to place, nil for a raw
// extent (read by place).
func (j *readJob) decode(r io.ReaderAt, partOff int64, sb *superblock, dev int64, ino uint64) ([]byte, error) {
	switch j.kind {
	case jobFail:
		return nil, j.err
	case jobInline:
		data, err := decompressExtent(j.inline, j.compression, j.ramBytes)
		if err != nil {
			return nil, fmt.Errorf("btrfs: inode %d inline extent at file offset %d: %w", ino, j.fileOffset, err)
		}
		return data, nil
	case jobCompressed:
		// Read the on-disk compressed payload at diskBytenr (size =
		// diskNumBytes), decompress to the full ram_bytes; place copies
		// [extOffset, extOffset+numBytes) into out.
		// H3: diskNumBytes is attacker-controlled; bound the read buffer
		// against the device size.
		compressed, release, err := getCompressed(j.diskNumBytes, dev)
		if err != nil {
			return nil, fmt.Errorf("btrfs: inode %d compressed extent disk_num_bytes %d: %w", ino, j.diskNumBytes, err)
		}
		defer release()
		phys, err := sb.physAddr(partOff, j.diskBytenr)
		if err != nil {
			return nil, fmt.Errorf("btrfs: inode %d extent at file offset %d: %w", ino, j.fileOffset, err)
		}
		if _, err := r.ReadAt(compressed, phys); err != nil {
			return nil, fmt.Errorf("btrfs: read compressed extent data: %w", err)
		}
		decompressed, err := decompressExtent(compressed, j.compression, j.ramBytes)
		if err != nil {
			return nil, fmt.Errorf("btrfs: inode %d extent at file offset %d: %w", ino, j.fileOffset, err)
		}
		// Validate the source window [extOffset, extOffset+numBytes) within
		// the decompressed buffer (overflow-safe) before slicing.
		if err := safeio.CheckBounds(int(j.extOffset), int(j.numBytes), len(decompressed)); err != nil {
			return nil, fmt.Errorf("btrfs: decompressed extent too short for inode %d at file offset %d: %w", ino, j.fileOffset, err)
		}
		return decompressed[j.extOffset : j.extOffset+j.numBytes], nil
	}
	return nil, nil
}

// place is the part of a job that runs in file order on the caller: it copies
// the decoded bytes into out, or reads an uncompressed extent straight into it.
func (j *readJob) place(out, data []byte, r io.ReaderAt, partOff int64, sb *superblock, ino uint64) error {
	switch j.kind {
	case jobInline:
		n := umin(uint64(len(data)), j.remaining)
		// Bounds-check the destination slice (overflow-safe) before copy.
		dst, err := safeio.Slice(out, int(j.fileOffset), int(n))
		if err != nil {
			return fmt.Errorf("btrfs: inode %d inline extent dest [%d,+%d): %w", ino, j.fileOffset, n, err)
		}
		copy(dst, data[:n])
	case jobCompressed:
		copy(out[j.fileOffset:j.fileOffset+j.numBytes], data)
	case jobRaw:
		phys, err := sb.physAddr(partOff, j.diskBytenr+j.extOffset)
		if err != nil {
			return fmt.Errorf("btrfs: inode %d extent at file offset %d: %w", ino, j.fileOffset, err)
		}
		if _, err := r.ReadAt(out[j.fileOffset:j.fileOffset+j.numBytes], phys); err != nil {
			return fmt.Errorf("btrfs: read extent data: %w", err)
		}
	}
	return nil
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
		zr, err := getZlibReader(src)
		if err != nil {
			return nil, fmt.Errorf("zlib reader: %w", err)
		}
		defer putZlibReader(zr)
		// Cap reads at ramBytes + a small slack so a malformed stream cannot
		// allocate unbounded memory. ramBytes is clamped to the hard ceiling
		// so a huge or overflowing declared size can't defeat the limit.
		out, err := readAllLimit(zr.fr, decompressLimit(ramBytes))
		if err != nil {
			return nil, fmt.Errorf("zlib decompress: %w", err)
		}
		if int64(len(out)) < decompressLimit(ramBytes) {
			if err := zr.zlibTrailer(out); err != nil {
				return nil, fmt.Errorf("zlib decompress: %w", err)
			}
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
	zr, err := getZstdReader(src)
	if err != nil {
		return nil, fmt.Errorf("zstd reader: %w", err)
	}
	defer putZstdReader(zr)
	// A kernel zstd extent is ONE frame followed by zero padding to the sector
	// size (Linux v6.17 fs/btrfs/zstd.c, zstd_decompress_bio, lines 630-640:
	// it leaves its loop once the output is complete or at the end of the
	// frame, and never looks at the bytes after it). Reading one byte past
	// ram_bytes, as a "stream is longer than declared" probe, makes the decoder
	// parse the padding as a second frame and fail with "magic number mismatch".
	// So stop at exactly ramBytes: output is still bounded by it. A frame that
	// ends BEFORE ram_bytes is refused by the decoder when it reaches the padding
	// (stricter than the kernel, which would accept it; no kernel-written extent
	// has that shape).
	if ramBytes > maxDecompressRAM {
		ramBytes = maxDecompressRAM
	}
	out, err := readAllLimit(zr, int64(ramBytes))
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
