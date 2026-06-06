package filesystem_btrfs

import (
	"encoding/binary"
	"hash/crc32"
)

// crc32cTable is the Castagnoli polynomial table used by Btrfs.
var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// crc32c computes a CRC32c checksum over data with the given seed.
// Btrfs uses seed = 0xFFFFFFFF (^uint32(0)).
func crc32cSum(data []byte, seed uint32) uint32 {
	return crc32.Update(seed, crc32cTable, data)
}

// updateNodeCRC writes the CRC32c checksum of buf[32:] into buf[0:4] (LE).
// The Btrfs node on-disk checksum covers bytes 20..end (past the 32-byte csum field).
// We mirror the Linux kernel convention: csum covers bytes [32, nodeSize).
func updateNodeCRC(buf []byte) {
	const csumOff = 32 // csum starts at byte 32 in the block (past the 32-byte csum field)
	h := crc32cSum(buf[csumOff:], ^uint32(0))
	binary.LittleEndian.PutUint32(buf[0:], h)
}

// updateSuperblockCRC writes the CRC of sb[32:1000] into sb[0:4] (LE).
func updateSuperblockCRC(buf []byte) {
	const sbCsumEnd = 0x1000
	h := crc32cSum(buf[32:sbCsumEnd], ^uint32(0))
	binary.LittleEndian.PutUint32(buf[0:], h)
}
