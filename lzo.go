package filesystem_btrfs

import (
	"encoding/binary"
	"fmt"

	lzo "github.com/anchore/go-lzo"
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
// The actual LZO1X-1 decoder lives in github.com/anchore/go-lzo (MIT
// licensed, pure Go, derived from the kernel docs and the lzokay reference).

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
	out := make([]byte, 0, limit)

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
			return nil, fmt.Errorf("lzo: truncated segment header at offset %d", in)
		}
		segLen := binary.LittleEndian.Uint32(src[in : in+4])
		in += 4
		if segLen == 0 {
			continue
		}
		if uint64(segLen) > uint64(end-in) {
			return nil, fmt.Errorf("lzo: segment length %d exceeds remaining %d", segLen, end-in)
		}
		// Each segment expands to at most one page; allocate exactly
		// that much and let the decoder return the produced length.
		segOut := make([]byte, btrfsLzoPageSize)
		n, err := lzo.Decompress(src[in:in+int(segLen)], segOut)
		if err != nil {
			return nil, fmt.Errorf("lzo: segment at offset %d: %w", in-4, err)
		}
		if uint64(len(out)+n) > limit {
			return nil, fmt.Errorf("lzo: decompressed size exceeds ram_bytes %d", ramBytes)
		}
		out = append(out, segOut[:n]...)
		in += int(segLen)
	}
	return out, nil
}
