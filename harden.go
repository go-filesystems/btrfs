package filesystem_btrfs

import "io"

// Hardening bounds for parsing UNTRUSTED on-disk Btrfs images.
//
// A malicious or corrupt image must never panic the host, read out of
// bounds, integer-overflow into a bad allocation/slice, loop forever, or
// OOM. These ceilings turn every nonsensical on-disk field into a graceful
// error rather than an unbounded allocation or runaway loop.
const (
	// maxNodeSize is the largest legal Btrfs metadata node. The on-disk
	// format permits up to 64 KiB nodes (mkfs default is 16 KiB); anything
	// larger is rejected so a hostile nodeSize cannot drive a multi-GiB
	// allocation in readNode.
	maxNodeSize = 64 * 1024

	// maxBtreeDepth bounds B-tree descent. Real Btrfs trees are at most ~8
	// levels deep even at multi-terabyte scale; this is a generous ceiling
	// that still terminates an adversarial node whose level field claims a
	// huge depth or whose child pointers never reach a leaf.
	maxBtreeDepth = 64

	// maxTreeNodes bounds the total number of nodes any single tree walk
	// will visit. Combined with the per-node VisitSet (which rejects a
	// block pointer revisited within one walk), this caps the work a
	// cyclic or fan-out-bomb tree can force, defending against OOM/livelock
	// even when every visited node id is distinct.
	maxTreeNodes = 1 << 20

	// hardSizeCeiling is the fallback allocation ceiling used when the
	// backing device's size cannot be determined. 1 GiB is far larger than
	// any single metadata node or inline/compressed extent the reader
	// produces, yet small enough that a single bad on-disk length cannot
	// exhaust host memory.
	hardSizeCeiling = 1 << 30

	// maxDecompressRAM caps the declared decompressed size (ram_bytes) of a
	// compressed extent. ram_bytes is fully attacker-controlled, so it is
	// clamped to this ceiling (and to the remaining file size at the call
	// site) before being used as a decompression output bound.
	maxDecompressRAM = 1 << 30
)

// deviceSize returns a sane allocation ceiling derived from the backing
// device. The readers thread an io.ReaderAt around, but the concrete value is
// always a blockBackend / devicePool exposing Size(); when that is available
// and positive it is used as the ceiling. Otherwise hardSizeCeiling is
// returned so callers still have a finite bound.
func deviceSize(r io.ReaderAt) int64 {
	type sizer interface{ Size() (int64, error) }
	if s, ok := r.(sizer); ok {
		if n, err := s.Size(); err == nil && n > 0 {
			return n
		}
	}
	return hardSizeCeiling
}
