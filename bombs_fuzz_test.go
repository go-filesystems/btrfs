package filesystem_btrfs

import "testing"

// fuzzAllocBudget is what one fuzz iteration may allocate: a fixed allowance
// (a compressed extent is at most 128 KiB, zlib/zstd keep tens of KiB of state)
// plus a multiple of the input. A bomb is an iteration whose allocation has no
// relation to the input's size.
func fuzzAllocBudget(inputLen int) uint64 { return 4<<20 + 64*uint64(inputLen) }

// FuzzDecompressExtent decodes arbitrary bytes as every extent codec with an
// arbitrary declared ram_bytes: the output may never exceed the kernel's
// 128 KiB (+1 overflow-detection byte) and the allocation must stay
// proportional to the input.
func FuzzDecompressExtent(f *testing.F) {
	f.Add(byte(1), uint64(4096), zstdFrame(17))
	f.Add(byte(3), uint64(1<<40), []byte{8, 0, 0, 0, 0, 0, 0, 0})
	f.Add(byte(2), uint64(0), []byte{})
	f.Fuzz(func(t *testing.T, codec byte, ram uint64, src []byte) {
		var out []byte
		var err error
		got := allocDelta(func() { out, err = decompressExtent(src, codec%5, ram) })
		if err == nil && codec%5 != compressionNone && len(out) > maxDecompressRAM+btrfsLzoPageSize {
			t.Fatalf("codec %d produced %d bytes", codec%5, len(out))
		}
		if codec%5 != compressionNone && got > fuzzAllocBudget(len(src)) {
			t.Fatalf("codec %d allocated %d bytes for a %d-byte extent (ram_bytes %d)", codec%5, got, len(src), ram)
		}
	})
}

// FuzzStripedRead reads through a device pool whose single chunk item is
// arbitrary: profile, stripe_len, sub_stripes, stripe count and read window all
// come from the input. It must neither panic nor issue more device reads than
// the window has stripes.
func FuzzStripedRead(f *testing.F) {
	f.Add(uint64(blockGroupRAID0), uint64(btrfsStripeLen), uint16(2), uint32(0), uint32(3*btrfsStripeLen))
	f.Add(uint64(blockGroupRAID10), uint64(1), uint16(2), uint32(5), uint32(4096))
	f.Add(uint64(blockGroupRAID5), uint64(1<<63+5), uint16(3), uint32(1), uint32(4096))
	f.Fuzz(func(t *testing.T, profile, stripeLen uint64, sub uint16, off, n uint32) {
		if n > 8*btrfsStripeLen {
			n = 8 * btrfsStripeLen
		}
		pool, dev := stripedPool(profile, stripeLen)
		pool.sb.sysChunks[0].subStripes = sub
		_, _ = pool.ReadAt(make([]byte, n), int64(off))
		if limit := int(n)/btrfsStripeLen + 4; dev.calls > limit {
			t.Fatalf("profile %#x stripe_len %d: %d device reads for %d bytes", profile, stripeLen, dev.calls, n)
		}
	})
}
