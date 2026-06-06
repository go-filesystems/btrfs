package filesystem_btrfs

import (
	"encoding/binary"
	"fmt"
	"io"
)

const sectorSize = 512

// linuxPartTypeGPT is the GUID for a Linux filesystem partition (LE wire form).
var linuxPartTypeGPT = [16]byte{
	0xAF, 0x3D, 0xC6, 0x0F,
	0x83, 0x84, 0x72, 0x47,
	0x8E, 0x79, 0x3D, 0x69, 0xD8, 0x47, 0x7D, 0xE4,
}

// partitionOffset returns the byte offset to the start of the requested
// partition. Pass -1 to auto-select the first Linux data partition.
func partitionOffset(r io.ReaderAt, partIndex int) (int64, error) {
	var sig [8]byte
	if _, err := r.ReadAt(sig[:], 512); err == nil && string(sig[:]) == "EFI PART" {
		return gptPartOffset(r, partIndex)
	}
	var magic [2]byte
	if _, err := r.ReadAt(magic[:], 510); err == nil &&
		magic[0] == 0x55 && magic[1] == 0xAA {
		return mbrPartOffset(r, partIndex)
	}
	return 0, nil
}

func gptPartOffset(r io.ReaderAt, partIndex int) (int64, error) {
	hdr := make([]byte, 92)
	if _, err := r.ReadAt(hdr, 512); err != nil {
		return 0, fmt.Errorf("btrfs: read GPT header: %w", err)
	}
	le := binary.LittleEndian
	partEntryLBA := le.Uint64(hdr[72:])
	numParts := le.Uint32(hdr[80:])
	entrySize := le.Uint32(hdr[84:])
	if entrySize < 128 {
		return 0, fmt.Errorf("btrfs: unexpected GPT entry size %d", entrySize)
	}
	tableOff := int64(partEntryLBA) * sectorSize
	buf := make([]byte, entrySize)
	for i := uint32(0); i < numParts; i++ {
		if _, err := r.ReadAt(buf, tableOff+int64(i)*int64(entrySize)); err != nil {
			break
		}
		var typeGUID [16]byte
		copy(typeGUID[:], buf[:16])
		if typeGUID == ([16]byte{}) {
			continue
		}
		startLBA := le.Uint64(buf[32:])
		if partIndex >= 0 {
			if int(i) == partIndex {
				return int64(startLBA) * sectorSize, nil
			}
		} else if typeGUID == linuxPartTypeGPT {
			return int64(startLBA) * sectorSize, nil
		}
	}
	if partIndex >= 0 {
		return 0, fmt.Errorf("btrfs: GPT partition index %d not found", partIndex)
	}
	return 0, fmt.Errorf("btrfs: no Linux data partition found in GPT")
}

func mbrPartOffset(r io.ReaderAt, partIndex int) (int64, error) {
	table := make([]byte, 64)
	if _, err := r.ReadAt(table, 446); err != nil {
		return 0, fmt.Errorf("btrfs: read MBR partition table: %w", err)
	}
	for i := 0; i < 4; i++ {
		e := table[i*16:]
		ptype := e[4]
		startLBA := binary.LittleEndian.Uint32(e[8:])
		if startLBA == 0 {
			continue
		}
		if partIndex >= 0 {
			if i == partIndex {
				return int64(startLBA) * sectorSize, nil
			}
		} else if ptype == 0x83 {
			return int64(startLBA) * sectorSize, nil
		}
	}
	if partIndex >= 0 {
		return 0, fmt.Errorf("btrfs: MBR partition index %d not found", partIndex)
	}
	return 0, fmt.Errorf("btrfs: no Linux partition (type 0x83) found in MBR")
}
