package filesystem_btrfs

import (
	"errors"
	"io"

	"github.com/klauspost/compress/zstd"
)

var errZstdFrame = errors.New("btrfs: zstd extent is not one plain frame")

// zstdFrameLen returns the length of the zstd frame at the start of src
// without decoding it, so the sector padding (or anything else) after the
// frame is never handed to the decoder: the kernel stops at the end of the
// frame and ignores the tail (Linux fs/btrfs/zstd.c, zstd_decompress_bio).
//
// The layout is RFC 8878 section 3.1.1: Magic_Number and Frame_Header (their
// size is zstd.Header.HeaderSize), then Data_Blocks, then an optional 4-byte
// Content_Checksum when Content_Checksum_Flag is set. A block starts with a
// 3-byte little-endian Block_Header (section 3.1.1.2): bit 0 Last_Block, bits
// 1-2 Block_Type, bits 3-23 Block_Size. Block_Content is Block_Size bytes for
// a Raw or Compressed block and ONE byte for an RLE block, whatever its
// Block_Size. Block_Type 3 is Reserved and must be rejected. A skippable frame
// is refused: a btrfs extent holds one real frame.
func zstdFrameLen(src []byte) (int, error) {
	var h zstd.Header
	if err := h.Decode(src); err != nil {
		return 0, err
	}
	if h.Skippable {
		return 0, errZstdFrame
	}
	pos := h.HeaderSize
	for {
		if len(src)-pos < 3 {
			return 0, io.ErrUnexpectedEOF
		}
		bh := uint32(src[pos]) | uint32(src[pos+1])<<8 | uint32(src[pos+2])<<16
		last, typ, size := bh&1 != 0, (bh>>1)&3, int(bh>>3)
		if typ == 3 {
			return 0, errZstdFrame
		}
		if typ == 1 {
			size = 1
		}
		if size > len(src)-pos-3 {
			return 0, io.ErrUnexpectedEOF
		}
		pos += 3 + size
		if last {
			break
		}
	}
	if h.HasCheckSum {
		if len(src)-pos < 4 {
			return 0, io.ErrUnexpectedEOF
		}
		pos += 4
	}
	return pos, nil
}
