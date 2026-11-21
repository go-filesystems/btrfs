package filesystem_btrfs

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/go-compressions/lzo"
)

// Btrfs LZO framing layer.
//
// Btrfs stores LZO-compressed extents using a small framing format on top of
// raw LZO1X-1 segments (see Linux kernel fs/btrfs/lzo.c). The on-disk extent
// payload is:
//
//	| total_size (4B LE) | seg0_len (4B LE) | seg0 lzo data | seg1_len | seg1 lzo data | ... |
//
// total_size counts itself plus every segment header and payload. Each
// segment decompresses to at most one kernel page (btrfsLzoPageSize bytes).
//
// A segment header may not straddle an on-disk page boundary: when the next
// 4-byte header would cross a page boundary, the encoder pads to the next
// page-aligned offset and the decoder mirrors that rule.
//
// The LZO1X decoder is the fleet's, github.com/go-compressions/lzo (BSD-3,
// pure Go, DecodeBlock, checked against liblzo2 blocks).

// btrfsLzoPageSize is the on-disk page size used by the btrfs LZO framing.
// btrfs-progs hardcodes 4096; we match that producer behavior.
const btrfsLzoPageSize = 4096

// decompressLzo decodes a btrfs-framed LZO compressed extent. ramBytes caps
// the output size so malformed input cannot allocate unboundedly.
func decompressLzo(src []byte, ramBytes uint64) ([]byte, error) {
	if len(src) < 4 {
		return nil, fmt.Errorf("lzo: extent too short (%d bytes) for header", len(src))
	}
	totalSize := binary.LittleEndian.Uint32(src[:4])
	if int(totalSize) > len(src) || totalSize < 4 {
		return nil, fmt.Errorf("lzo: invalid total_size %d (buffer %d)", totalSize, len(src))
	}
	// Cap the output at ramBytes (the on-disk declared decompressed size).
	// A modest fallback handles ramBytes==0, which can occur for empty
	// extents or in tests that don't supply a hint.
	limit := ramBytes
	if limit == 0 {
		limit = uint64(totalSize) * 16
	}
	// H4: ramBytes is fully attacker-controlled; clamp the output ceiling so
	// a huge declared size cannot drive a multi-GiB make() up front.
	if limit > maxDecompressRAM {
		limit = maxDecompressRAM
	}
	// Structural bound: a segment costs at least 5 input bytes (its 4-byte
	// length and one payload byte) and yields at most one page, so the output
	// cannot exceed (totalSize/5 + 1) pages whatever ram_bytes claims. The
	// slice grows by append from there instead of being sized up front from
	// the declared size.
	if maxOut := (uint64(totalSize)/5 + 1) * btrfsLzoPageSize; limit > maxOut {
		limit = maxOut
	}
	out := make([]byte, 0, limit)
	segOut := make([]byte, btrfsLzoPageSize)

	err := lzoSegments(src, func(off int, blk []byte) error {
		// Each segment expands to at most one page: one buffer of that
		// size serves every segment, the decoder returns the length.
		n, err := lzo.DecodeBlock(segOut, blk)
		if err != nil {
			return fmt.Errorf("lzo: segment at offset %d: %w", off, err)
		}
		if uint64(len(out)+n) > limit {
			return fmt.Errorf("lzo: decompressed size exceeds ram_bytes %d", ramBytes)
		}
		out = append(out, segOut[:n]...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// decompressLzoInto decodes a btrfs-framed LZO extent straight into dst, whose
// length is the exact decoded size the caller expects: no intermediate buffer,
// no copy. Each segment is decoded into the next page-sized window of dst, so
// the decoder's scratch writes stay inside dst and inside the segments still
// to come. A stream that yields more or fewer bytes than len(dst) is refused.
func decompressLzoInto(dst, src []byte) error {
	pos := 0
	err := lzoSegments(src, func(off int, blk []byte) error {
		win := dst[pos:]
		if len(win) > btrfsLzoPageSize {
			win = win[:btrfsLzoPageSize]
		}
		n, err := lzo.DecodeBlock(win, blk)
		if err != nil {
			if errors.Is(err, lzo.ErrShortDst) {
				return fmt.Errorf("lzo: decompressed size exceeds %d bytes", len(dst))
			}
			return fmt.Errorf("lzo: segment at offset %d: %w", off, err)
		}
		pos += n
		return nil
	})
	if err != nil {
		return err
	}
	if pos != len(dst) {
		return fmt.Errorf("lzo: decompressed %d bytes, expected %d", pos, len(dst))
	}
	return nil
}

// lzoSegments walks the framing of a btrfs LZO extent and calls fn with each
// non-empty segment's payload and the offset of its length header.
func lzoSegments(src []byte, fn func(off int, blk []byte) error) error {
	if len(src) < 4 {
		return fmt.Errorf("lzo: extent too short (%d bytes) for header", len(src))
	}
	totalSize := binary.LittleEndian.Uint32(src[:4])
	if int(totalSize) > len(src) || totalSize < 4 {
		return fmt.Errorf("lzo: invalid total_size %d (buffer %d)", totalSize, len(src))
	}
	in := 4
	end := int(totalSize)
	for in < end {
		// A segment header may not straddle a page boundary in the
		// compressed buffer; if it would, skip to the next page.
		if in/btrfsLzoPageSize != (in+4-1)/btrfsLzoPageSize {
			in = ((in / btrfsLzoPageSize) + 1) * btrfsLzoPageSize
			if in >= end {
				break
			}
		}
		if in+4 > end {
			return fmt.Errorf("lzo: truncated segment header at offset %d", in)
		}
		segLen := binary.LittleEndian.Uint32(src[in : in+4])
		in += 4
		if segLen == 0 {
			continue
		}
		if uint64(segLen) > uint64(end-in) {
			return fmt.Errorf("lzo: segment length %d exceeds remaining %d", segLen, end-in)
		}
		if err := fn(in-4, src[in:in+int(segLen)]); err != nil {
			return err
		}
		in += int(segLen)
	}
	return nil
}
