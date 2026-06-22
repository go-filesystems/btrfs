package filesystem_btrfs

import (
	"io"
)

// nodeCacher is the optional interface a reader may implement so that
// readNode serves decoded B-tree nodes from an in-memory cache instead of
// re-reading them from the backing device on every tree walk.
//
// Btrfs is copy-on-write: while a filesystem is open for reading, the node
// at a given logical address never changes content — so caching the decoded
// block by logical address is safe and exact. Every metadata-heavy read
// (path lookup, directory listing, extent walk) re-descends from the same
// FS-tree root, so the upper interior nodes would otherwise be re-read once
// per file. Memoizing them collapses thousands of repeated reads into one
// each. This is the single biggest lever closing the read-throughput gap to
// the kernel (see BENCHMARKS.md).
//
// A write mutates the tree (COW allocates new logical addresses and may
// recycle freed ones), so any write path must drop the cache; the btrfsFS
// read methods install a fresh cache and the mutating methods clear it.
type nodeCacher interface {
	// cachedNode returns the decoded node previously stored for logAddr, or
	// (nil,false) on a miss.
	cachedNode(logAddr uint64) ([]byte, bool)
	// putNode stores buf for logAddr. buf must not be mutated afterwards.
	putNode(logAddr uint64, buf []byte)
}

// cachedReader wraps an io.ReaderAt with a logical-address-keyed cache of
// decoded B-tree nodes. ReadAt is delegated verbatim to the backing reader
// (so raw data-extent reads are unaffected); only readNode consults the
// cache. It is not safe for concurrent use; btrfsFS serializes all access
// behind its mutex.
type cachedReader struct {
	io.ReaderAt
	nodes map[uint64][]byte
	// builtGen records the superblock generation in effect when this cache was
	// created. btrfs bumps the generation on every transaction commit
	// (writeSuperblock), so a changed generation means the on-disk tree was
	// mutated — possibly recycling a freed logical address whose old block is
	// still cached. reader() compares the live generation and rebuilds on any
	// change, which is correct even for writes that bypass the public API
	// (test fixtures that craft tree items directly) as long as they commit a
	// new superblock. This is the exact, COW-safe staleness key.
	builtGen uint64
}

// newCachedReader returns a read-through node cache over r, tagged with the
// superblock generation it was built against.
func newCachedReader(r io.ReaderAt, gen uint64) *cachedReader {
	return &cachedReader{ReaderAt: r, nodes: make(map[uint64][]byte), builtGen: gen}
}

func (c *cachedReader) cachedNode(logAddr uint64) ([]byte, bool) {
	b, ok := c.nodes[logAddr]
	return b, ok
}

func (c *cachedReader) putNode(logAddr uint64, buf []byte) {
	c.nodes[logAddr] = buf
}

// readerWithCache returns a reader that serves node reads from a fresh cache
// layered over the underlying device. Each public read entry point builds one
// so a single multi-descent operation (e.g. a deep path lookup) reuses
// interior nodes within itself and across files in the same call sequence,
// without ever caching stale metadata across a write.
func (fs *btrfsFS) reader() io.ReaderAt {
	// Rebuild when the on-disk generation advanced under the cache. The public
	// mutators already drop the cache via invalidateCache(); this generation
	// guard additionally catches mutations that commit a new superblock without
	// going through those methods (e.g. test fixtures that craft tree items via
	// cowInsert + updateFsTreeRoot), so a stale block at a COW-recycled logical
	// address can never be served.
	if fs.cache == nil || fs.cache.builtGen != fs.sb.generation {
		fs.cache = newCachedReader(fs.f, fs.sb.generation)
	}
	return fs.cache
}

// invalidateCache drops the node cache. Called by every mutating path before
// it returns so a subsequent read never observes a pre-write tree block at a
// logical address the write may have recycled.
func (fs *btrfsFS) invalidateCache() {
	fs.cache = nil
}
