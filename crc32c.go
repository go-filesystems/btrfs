package filesystem_btrfs

import (
	"encoding/binary"
	"hash/crc32"
)

// crc32cTable is the Castagnoli polynomial table used by Btrfs.
var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// crc32c computes a CRC32c checksum over data with the given seed.
//
// Btrfs seeds its on-disk metadata/superblock checksum with 0 (NOT
// 0xFFFFFFFF) and applies no final inversion: it is the bare Castagnoli
// running value, matching the kernel's btrfs_crc32c()/crc32c(0, ...) and the
// value produced by mkfs.btrfs. crc32.Update with seed 0 reproduces that
// exactly. (The standard crc32.Checksum / IEEE-style pre-and-post inversion
// would produce a different, btrfs-incompatible value.)
func crc32cSum(data []byte, seed uint32) uint32 {
	return crc32.Update(seed, crc32cTable, data)
}

// btrfsCsumSeed is the seed Btrfs uses for its crc32c metadata/superblock
// checksums. The kernel and btrfs-progs compute crc32c(0, data, len) with no
// final XOR, so the seed is 0.
const btrfsCsumSeed uint32 = 0

// updateNodeCRC writes the CRC32c checksum of buf[32:] into buf[0:4] (LE).
// The Btrfs on-disk checksum covers bytes [32, nodeSize) — everything past the
// 32-byte csum field — matching the Linux kernel convention.
func updateNodeCRC(buf []byte) {
	const csumOff = 32 // csum starts at byte 32 in the block (past the 32-byte csum field)
	h := crc32cSum(buf[csumOff:], btrfsCsumSeed)
	binary.LittleEndian.PutUint32(buf[0:], h)
}

// updateSuperblockCRC writes the CRC of sb[32:0x1000] into sb[0:4] (LE).
func updateSuperblockCRC(buf []byte) {
	const sbCsumEnd = 0x1000
	h := crc32cSum(buf[32:sbCsumEnd], btrfsCsumSeed)
	binary.LittleEndian.PutUint32(buf[0:], h)
}
