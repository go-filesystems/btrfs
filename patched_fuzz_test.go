// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package filesystem_btrfs

import "testing"

// FuzzMountImage feeds whole byte blobs to OpenFromDevice, and almost none of
// them are btrfs. Measured by logging every input that survived the open, a
// 45-second run produced 45 -- essentially only the seeds. The superblock
// carries a CRC32C over its own bytes, so a random mutation invalidates the
// image before any of the decoder is reached.
//
// This target splices fuzzer-chosen bytes into a *valid* image and then
// recomputes the superblock checksum, so a corrupted field is reached by code
// that has already accepted the superblock: the chunk tree, the root tree, and
// the extent decoder behind them.

func FuzzMountPatched(f *testing.F) {
	base := buildTestImageBytes()

	// Seeds aimed at the fields worth corrupting.
	sb := int64(superblockOffset)
	f.Add(sb+int64(sbfSysChunkArrSz), []byte{0xff, 0xff, 0xff, 0xff})
	f.Add(sb+int64(sbfNodeSize), []byte{0x00, 0x00, 0x00, 0x00})
	f.Add(sb+int64(sbfRootLogAddr), []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	f.Add(sb+int64(sbfChunkLogAddr), []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	f.Add(int64(testRootPhys), []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	f.Add(int64(testRootPhys)+0x38, []byte{0x00})

	f.Fuzz(func(t *testing.T, off int64, patch []byte) {
		if len(patch) == 0 || len(patch) > 4096 {
			return
		}
		if off < 0 || off >= int64(len(base)) {
			return
		}
		img := append([]byte(nil), base...)
		copy(img[off:], patch)
		// Re-checksum the superblock so a patch landing inside it still
		// reaches the decoder rather than being rejected at the door.
		updateSuperblockCRC(img[superblockOffset : superblockOffset+sbfSize])
		mountBytes(t, img)

		// The write path decodes the same untrusted items, and mounting an
		// attacker-supplied image read-write is the realistic case. Every one
		// of these may fail; none may panic or allocate from the image.
		fs, err := OpenFromDevice(&memBackend{&rwaBuf{data: img}}, -1)
		if err != nil {
			return
		}
		defer func() { _ = fs.Close() }()
		_ = fs.MkDir("/d", 0o755)
		_ = fs.WriteFile("/d/f", []byte("x"), 0o644)
		_ = fs.Truncate("/hello.txt", 4)
		_ = fs.DeleteFile("/hello.txt")
	})
}
