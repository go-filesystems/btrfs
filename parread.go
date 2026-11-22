package filesystem_btrfs

// parread.go — parallel decoding of the compressed extents of a file.
//
// The extents of a file are independent once its EXTENT_DATA items are known:
// each compressed one is read from the device and inflated (zlib, LZO, zstd) on
// its own. Walking the items and checking them (overlap, bounds, the chunk map)
// stays serial and cheap; the expensive part, the device read and the decode,
// runs on up to readWorkers() goroutines under inflight.Ordered, which bounds
// the decoded extents in flight to the worker count and hands them back in file
// order. The copy into the output, and every read of an uncompressed extent,
// happen in emit, in file order, from the calling goroutine: a later extent
// overwrites an earlier one exactly as in the serial walk, and the output is
// never written from two goroutines.
//
// With one worker (or one job) nothing is spawned: the path is the serial loop.

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"runtime"
	"sync"

	"github.com/go-compressions/deflate"
	"github.com/go-filesystems/inflight"
	"github.com/go-simd/adler32"
	"github.com/go-volumes/safeio"
	"github.com/klauspost/compress/zstd"
)

// Decoders are pooled: a zstd decoder or a zlib reader allocates its window and
// tables on every NewReader, which with several workers costs more in the
// allocator and the GC than the pooling costs in code. Reset starts a new
// stream on a pooled one; a pooled decoder holds no reference to its input
// (Reset(nil) / an empty reader) and an error leaves it reusable after Reset.

var zstdPool sync.Pool // *zstd.Decoder

// getZstdDecoder returns a pooled decoder used only through DecodeAll: a
// whole extent is in memory, so the stream machinery (reader, goroutine,
// history copy) is not needed.
func getZstdDecoder() (*zstd.Decoder, error) {
	if v := zstdPool.Get(); v != nil {
		return v.(*zstd.Decoder), nil
	}
	return zstd.NewReader(nil,
		zstd.WithDecoderMaxWindow(zstdMaxWindow),
		zstd.WithDecoderMaxMemory(zstdMaxWindow),
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderLowmem(true))
}

func putZstdDecoder(zd *zstd.Decoder) { zstdPool.Put(zd) }

// A zlib extent is inflated as raw DEFLATE between the RFC 1950 header and
// trailer, and the Adler-32 is checked here with go-simd/adler32 (NEON, AVX2,
// AVX-512): klauspost's zlib reader sums with a scalar loop, about 14 % of the
// time of a 128 KiB extent on a 1-CPU read.

var errZlibHeader = errors.New("zlib: invalid header")

// inflateZlib expands the zlib stream src, whole, with go-compressions/deflate
// (a table-driven decoder over the whole buffer, several times klauspost's
// streaming reader) and checks the trailer. The result is at most limit bytes:
// a stream that would produce more stops there without error and without the
// trailer check, as readAllLimit did, so the caller sees len(out) == limit and
// refuses it as larger than declared.
func inflateZlib(src []byte, limit int64) ([]byte, error) {
	if len(src) < 2 {
		return nil, io.ErrUnexpectedEOF
	}
	if src[0]&0x0f != 8 || src[0]>>4 > 7 || (uint(src[0])<<8|uint(src[1]))%31 != 0 || src[1]&0x20 != 0 {
		return nil, errZlibHeader
	}
	// limit is decompressLimit's: at most maxDecompressRAM+1, so it fits an int
	// on every platform; the clamp keeps that true if a caller changes.
	if limit > maxDecompressRAM+1 {
		limit = maxDecompressRAM + 1
	}
	if limit < 0 {
		limit = 0
	}
	out, n, err := deflate.InflatePrefix(make([]byte, 0, int(limit)), src[2:], int(limit))
	if errors.Is(err, deflate.ErrTooLarge) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	trailer := src[2+n:]
	if len(trailer) < 4 {
		return out, io.ErrUnexpectedEOF
	}
	if binary.BigEndian.Uint32(trailer) != adler32.Checksum(out) {
		return out, errors.New("zlib: invalid checksum")
	}
	return out, nil
}

// maxReadParallelism caps the default worker count: past it the decoders are
// memory-bound and the extra goroutines buy nothing.
const maxReadParallelism = 8

// defaultReadParallelism is min(GOMAXPROCS, maxReadParallelism), at least 1.
func defaultReadParallelism() int {
	return max(1, min(runtime.GOMAXPROCS(0), maxReadParallelism))
}

// SetReadParallelism sets how many compressed extents of one read are decoded
// at once: n <= 1 decodes serially, n is capped at 64. The default is
// min(GOMAXPROCS, 8). Safe to call at any time.
func (fs *btrfsFS) SetReadParallelism(n int) {
	fs.workers.Store(int32(max(1, min(n, 64))))
}

// readWorkers is the worker count.
func (fs *btrfsFS) readWorkers() int {
	if w := int(fs.workers.Load()); w > 0 {
		return w
	}
	return defaultReadParallelism()
}

// inOrder runs work(0..n-1) on up to w goroutines and calls emit(i, v) in index
// order, from the caller. The first error in index order, from work or emit,
// stops the run and is returned, so a damaged extent reports exactly as the
// serial walk would. With w < 2 or n < 2 it is the plain loop.
func inOrder[T any](n, w int, work func(i int) (T, error), emit func(i int, v T) error) error {
	if w < 2 || n < 2 {
		for i := range n {
			v, err := work(i)
			if err != nil {
				return err
			}
			if err := emit(i, v); err != nil {
				return err
			}
		}
		return nil
	}
	return inflight.Ordered(context.Background(), n, w,
		func(_ context.Context, i int) (T, error) { return work(i) }, emit)
}

// extentScratchMax is the largest compressed payload whose read buffer is
// pooled: a kernel-written extent is at most 128 KiB, padded to a sector.
const extentScratchMax = 1 << 20

var extentScratch = sync.Pool{New: func() any { b := make([]byte, 0, 132<<10); return &b }}

// getCompressed returns a buffer of n bytes for a compressed payload, bounded
// against the device size as safeio.MakeBytes does. release gives it back once
// the payload has been decoded (the decoders keep no reference to their input).
func getCompressed(n uint64, dev int64) (buf []byte, release func(), err error) {
	if n == 0 || n > extentScratchMax || dev < 0 || n > uint64(dev) {
		buf, err = safeio.MakeBytes(int64(n), dev)
		return buf, func() {}, err
	}
	p := extentScratch.Get().(*[]byte)
	if uint64(cap(*p)) < n {
		*p = make([]byte, 0, n)
	}
	b := (*p)[:n]
	return b, func() { *p = b[:0]; extentScratch.Put(p) }, nil
}
