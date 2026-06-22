package filesystem_btrfs

import (
	"fmt"
	"io"
)

// devicePool implements blockBackend on top of one or more leg devices
// keyed by btrfs dev_item.devid. Every read goes through ReadAt which
// decodes the address as `partOff + logical` (sb.physAddr is a
// passthrough — see superblock.go) and dispatches to the right device(s)
// based on the chunk's RAID profile.
//
// Algorithm reference: fs/btrfs/volumes.c:btrfs_map_block (Linux v6.12).
type devicePool struct {
	partOff int64
	devices map[uint64]blockBackend
	sb      *superblock
	// `primary` is the device the FS was originally opened against
	// (the one whose dev_item.devid matches sb.devID). Sync / Size /
	// Truncate / Close delegate to it.
	primary blockBackend
}

func newDevicePool(primary blockBackend, partOff int64, sb *superblock) *devicePool {
	devs := map[uint64]blockBackend{sb.devID: primary}
	// Legacy compat: hand-built test fixtures leave dev_item.devid=0 in the
	// superblock but write stripe[0].devID=1. Mirror the primary under devid=1
	// so routing finds it.
	if sb.devID == 0 {
		devs[1] = primary
	}
	return &devicePool{
		partOff: partOff,
		devices: devs,
		sb:      sb,
		primary: primary,
	}
}

func (p *devicePool) addDevice(devID uint64, dev blockBackend) {
	p.devices[devID] = dev
}

func (p *devicePool) Sync() error            { return p.primary.Sync() }
func (p *devicePool) Size() (int64, error)   { return p.primary.Size() }
func (p *devicePool) Truncate(s int64) error { return p.primary.Truncate(s) }
func (p *devicePool) Close() error {
	// Dedupe by backend pointer: the legacy compat aliases the primary
	// under multiple devid keys (e.g. devid=0 and devid=1 for hand-built
	// fixtures), so a naive iteration would double-close.
	seen := make(map[blockBackend]struct{}, len(p.devices))
	var firstErr error
	for _, dev := range p.devices {
		if _, dup := seen[dev]; dup {
			continue
		}
		seen[dev] = struct{}{}
		if err := dev.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// WriteAt is best-effort: for SINGLE / DUP-without-mirror it writes to the
// primary device only; multi-device writes (mirrors, parity rewrites) are
// not supported. cloud-boot opens FS images read-only, so this path is
// only exercised by the lib's own self-tests against single-device
// images — they continue to work because the pool collapses to the
// primary device.
func (p *devicePool) WriteAt(buf []byte, off int64) (int, error) {
	return p.primary.WriteAt(buf, off)
}

// ReadAt routes the read based on the chunk profile.
func (p *devicePool) ReadAt(buf []byte, off int64) (int, error) {
	if p.sb == nil {
		// Pool not yet bound to a chunk map (early-open path while reading
		// the partition table / superblock). Fall through to the primary.
		return p.primary.ReadAt(buf, off)
	}
	logical := uint64(off - p.partOff)
	chunk, err := p.sb.lookupChunk(logical)
	if err != nil {
		// Either we're reading at a logical address outside the chunk map
		// (e.g. the superblock itself), or the address is genuinely
		// unmapped. Defer to the primary — its absolute byte offset is
		// what the caller intended in the no-chunk-map case.
		return p.primary.ReadAt(buf, off)
	}
	return p.readFromChunk(buf, logical, chunk)
}

// readFromChunk dispatches on chunk profile and reads from the appropriate
// device(s). The read can span multiple stripes for RAID0 / RAID10 /
// RAID5 / RAID6; this implementation handles that by recursing on the
// remainder when a single ReadAt would cross a stripe boundary.
func (p *devicePool) readFromChunk(buf []byte, logical uint64, c *chunkMapping) (int, error) {
	profile := c.profile & blockGroupProfileMask
	switch {
	case profile == 0 || profile == blockGroupDup:
		// SINGLE (no bits set) or DUP. Both have stripes that each carry a
		// full copy of the chunk data at chunk.logStart.
		return p.readSingleOrMirror(buf, logical, c)
	case profile&(blockGroupRAID1|blockGroupRAID1C3|blockGroupRAID1C4) != 0:
		// All stripes are mirror copies of each other.
		return p.readSingleOrMirror(buf, logical, c)
	case profile == blockGroupRAID0:
		return p.readStriped(buf, logical, c, 0)
	case profile == blockGroupRAID10:
		return p.readRAID10(buf, logical, c)
	case profile == blockGroupRAID5:
		return p.readStriped(buf, logical, c, 1)
	case profile == blockGroupRAID6:
		return p.readStriped(buf, logical, c, 2)
	}
	return 0, fmt.Errorf("btrfs: unsupported chunk profile 0x%X at log 0x%X", profile, logical)
}

// readSingleOrMirror handles SINGLE / DUP / RAID1 / RAID1C{3,4}. Every
// stripe in the chunk holds the same data; we try them in order until one
// succeeds (or the local stripe first when we have it, to avoid a useless
// cross-device hop on healthy mirrors).
//
// Note on partOff: stripe offsets in the chunk_item are relative to the
// start of the btrfs partition. For partitioned images (GPT/MBR) the
// partition starts at p.partOff inside the file/device, so we add it back
// before passing to the underlying ReadAt (which takes file-absolute
// offsets).
func (p *devicePool) readSingleOrMirror(buf []byte, logical uint64, c *chunkMapping) (int, error) {
	inChunk := logical - c.logStart
	tryStripe := func(s chunkStripe) (int, error, bool) {
		dev := p.devices[s.devID]
		if dev == nil {
			return 0, nil, false
		}
		n, err := dev.ReadAt(buf, p.partOff+int64(s.offset+inChunk))
		return n, err, true
	}
	// Prefer the local stripe (same device the FS was opened on) when
	// present — avoids waking up a sibling backend for healthy reads.
	if c.localStripeIdx >= 0 && c.localStripeIdx < len(c.stripes) {
		if n, err, ok := tryStripe(c.stripes[c.localStripeIdx]); ok && err == nil {
			return n, nil
		}
	}
	var lastErr error
	for i, s := range c.stripes {
		if i == c.localStripeIdx {
			continue // already tried
		}
		n, err, ok := tryStripe(s)
		if !ok {
			continue
		}
		if err == nil {
			return n, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("btrfs: no available device for chunk at log 0x%X (profile 0x%X, %d stripes, %d in pool)",
			c.logStart, c.profile&blockGroupProfileMask, len(c.stripes), len(p.devices))
	}
	return 0, lastErr
}

// readStriped handles RAID0 (nparity=0), RAID5 (nparity=1), RAID6
// (nparity=2). Data is striped across (numStripes - nparity) data
// columns; parity occupies the last nparity columns in each stripe row,
// rotating left per row.
//
// btrfs_map_block math:
//
//	stripe_nr   = logical_in_chunk / stripe_len
//	stripe_off  = logical_in_chunk % stripe_len
//	row_nr      = stripe_nr / num_data_stripes
//	col_in_row  = stripe_nr % num_data_stripes
//
// Parity column for row N rotates: parity_col = (num_stripes - nparity + N) %
// num_stripes (RAID5, P), and for RAID6 (num_stripes - nparity + N + 1) %
// num_stripes for Q. Data columns are the non-parity positions, in order.
//
// For a healthy-read (all data devices present, no reconstruction needed)
// we just need to pick the data column, then map (col_in_row, row_nr) to
// the device + per-device offset.
func (p *devicePool) readStriped(buf []byte, logical uint64, c *chunkMapping, nparity int) (int, error) {
	if c.stripeLen == 0 {
		return 0, fmt.Errorf("btrfs: chunk at log 0x%X has stripeLen=0 (corrupt chunk_item?)", c.logStart)
	}
	numStripes := len(c.stripes)
	if numStripes <= nparity {
		return 0, fmt.Errorf("btrfs: chunk at log 0x%X has %d stripes but profile needs at least %d for %d-parity",
			c.logStart, numStripes, nparity+1, nparity)
	}
	numData := numStripes - nparity

	inChunk := logical - c.logStart
	stripeNr := inChunk / c.stripeLen
	stripeOff := inChunk % c.stripeLen
	rowNr := stripeNr / uint64(numData)
	colInRow := int(stripeNr % uint64(numData))

	var stripeIdx int
	if nparity == 0 {
		// RAID0: no parity, no rotation — data column maps directly
		// to the stripe at the same index.
		stripeIdx = colInRow
	} else {
		// RAID5 / RAID6: parity columns rotate LEFT per stripe row.
		// btrfs's btrfs_map_block places parity at positions
		// (numStripes - nparity + rowNr) % numStripes for P, and
		// shifts Q by one for RAID6. Data columns occupy the
		// numData positions starting after the parity columns,
		// wrapping around. The data column at logical position
		// colInRow lands at stripe index:
		//   (parityStartCol + nparity + colInRow) % numStripes
		parityStartCol := (numStripes - nparity + int(rowNr%uint64(numStripes))) % numStripes
		stripeIdx = (parityStartCol + nparity + colInRow) % numStripes
	}

	if stripeIdx >= numStripes {
		return 0, fmt.Errorf("btrfs: internal: computed stripeIdx %d out of range %d", stripeIdx, numStripes)
	}
	s := c.stripes[stripeIdx]
	dev := p.devices[s.devID]
	if dev == nil {
		return 0, fmt.Errorf("btrfs: RAID%d read needs devid %d which is not in the pool (chunk at log 0x%X col %d row %d)",
			nparity*5, s.devID, c.logStart, colInRow, rowNr)
	}
	perDevOff := p.partOff + int64(s.offset+rowNr*c.stripeLen+stripeOff)

	// Cap the read at the stripe boundary; recurse if more is requested.
	remainInStripe := int64(c.stripeLen - stripeOff)
	if int64(len(buf)) <= remainInStripe {
		return dev.ReadAt(buf, perDevOff)
	}
	n, err := dev.ReadAt(buf[:remainInStripe], perDevOff)
	if err != nil {
		return n, err
	}
	// Recurse on the remainder, starting at the next stripe.
	rest := buf[remainInStripe:]
	n2, err := p.ReadAt(rest, p.partOff+int64(logical+uint64(remainInStripe)))
	return n + n2, err
}

// readRAID10 handles RAID10: data is mirrored within sub_stripes pairs,
// then striped across pairs. With sub_stripes=2 and num_stripes=N, there
// are N/2 stripe groups, each a mirror pair. Reads pick any leg of the
// mirror group corresponding to the logical stripe.
//
//	stripe_nr     = logical_in_chunk / stripe_len
//	stripe_off    = logical_in_chunk % stripe_len
//	groups        = num_stripes / sub_stripes
//	group_idx     = stripe_nr % groups
//	row_nr        = stripe_nr / groups
//	stripe_base   = group_idx * sub_stripes          // first leg of pair
//	per_dev_off   = row_nr * stripe_len + stripe_off
func (p *devicePool) readRAID10(buf []byte, logical uint64, c *chunkMapping) (int, error) {
	if c.stripeLen == 0 {
		return 0, fmt.Errorf("btrfs: RAID10 chunk at log 0x%X has stripeLen=0", c.logStart)
	}
	subStripes := int(c.subStripes)
	if subStripes < 2 {
		subStripes = 2 // mkfs.btrfs always writes 2; tolerate the field being unset
	}
	numStripes := len(c.stripes)
	if numStripes < subStripes {
		return 0, fmt.Errorf("btrfs: RAID10 chunk at log 0x%X has %d stripes < subStripes %d", c.logStart, numStripes, subStripes)
	}
	groups := numStripes / subStripes

	inChunk := logical - c.logStart
	stripeNr := inChunk / c.stripeLen
	stripeOff := inChunk % c.stripeLen
	groupIdx := int(stripeNr % uint64(groups))
	rowNr := stripeNr / uint64(groups)
	stripeBase := groupIdx * subStripes
	perDevOff := p.partOff + int64(rowNr*c.stripeLen+stripeOff)

	// Try each leg of the mirror pair in turn.
	var lastErr error
	for leg := 0; leg < subStripes; leg++ {
		s := c.stripes[stripeBase+leg]
		dev := p.devices[s.devID]
		if dev == nil {
			continue
		}
		remainInStripe := int64(c.stripeLen - stripeOff)
		// perDevOff already includes p.partOff; s.offset is partition-
		// relative so add it (without partOff again).
		absOff := perDevOff + int64(s.offset)
		if int64(len(buf)) <= remainInStripe {
			n, err := dev.ReadAt(buf, absOff)
			if err == nil {
				return n, nil
			}
			lastErr = err
			continue
		}
		// Cap at stripe boundary, then recurse.
		n, err := dev.ReadAt(buf[:remainInStripe], absOff)
		if err == nil {
			rest := buf[remainInStripe:]
			n2, err2 := p.ReadAt(rest, p.partOff+int64(logical+uint64(remainInStripe)))
			return n + n2, err2
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("btrfs: RAID10 chunk at log 0x%X group %d has no available leg", c.logStart, groupIdx)
	}
	return 0, lastErr
}

// Compile-time assertion: *devicePool satisfies blockBackend.
var _ blockBackend = (*devicePool)(nil)

// Sanity-check ReadAt actually satisfies io.ReaderAt for callers that
// take the narrower interface.
var _ io.ReaderAt = (*devicePool)(nil)
