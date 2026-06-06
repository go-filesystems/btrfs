package filesystem_btrfs

import (
	"encoding/binary"
	"fmt"
)

// truncateInode resizes the file at path to newSize bytes.
//
//   - When newSize >= the current file size, the file is "grown" by simply
//     bumping the inode's size field. The hole between the old end and the
//     new end is implicit sparse; readFileData fills it with zeros via the
//     diskBytenr==0 skip path.
//   - When newSize < the current file size, every EXTENT_DATA item whose
//     range falls entirely past newSize is dropped, the one straddling
//     newSize (if any) is trimmed in-place, and the inode's size + nbytes
//     are updated to reflect the new on-disk footprint.
//
// mtime / ctime / transid / sequence are refreshed in all cases.
func truncateInode(rwaAt readerWriterAt, rws readerWriterAt, partOff int64,
	sb *superblock, sm *spaceManager, fsTreeRoot *uint64, path string, newSize uint64,
) error {
	in, err := pathLookup(rwaAt, partOff, sb, *fsTreeRoot, path)
	if err != nil {
		return fmt.Errorf("btrfs truncate: %q: %w", path, err)
	}
	if !in.isRegular() {
		return fmt.Errorf("btrfs truncate: %q is not a regular file", path)
	}

	if newSize > in.size {
		// Extension: bump the inode size and recompute nbytes. The new
		// region has no EXTENT_DATA items, so the reader treats it as
		// sparse (zeros) — no disk allocation needed.
		return updateInodeMetadata(rwaAt, rws, partOff, sb, sm, fsTreeRoot, in.num, func(d []byte) error {
			le := binary.LittleEndian
			le.PutUint64(d[inodeOffSize:], newSize)
			// nbytes only counts disk-resident extents; sparse extension adds
			// nothing. Leave nbytes alone — it remains correct.
			return nil
		})
	}

	if newSize == in.size {
		// No-op resize: still refresh mtime per POSIX (truncate(2) bumps
		// mtime even when size doesn't actually change).
		return updateInodeMetadata(rwaAt, rws, partOff, sb, sm, fsTreeRoot, in.num, func(d []byte) error {
			return nil
		})
	}

	// Shrink path: drop or trim the EXTENT_DATA items whose ranges fall
	// past newSize.
	items, err := collectPrefixItems(rwaAt, partOff, sb, *fsTreeRoot, in.num, typeExtentData)
	if err != nil && !isNotFoundErr(err) {
		return fmt.Errorf("btrfs truncate: scan extents: %w", err)
	}
	for _, m := range items {
		fileOffset := m.k.offset
		d := m.data
		if len(d) <= extDataOffType {
			continue
		}
		le := binary.LittleEndian
		extType := d[extDataOffType]
		// Determine the extent's logical span ([fileOffset, fileOffset+span)).
		var span uint64
		if extType == extentDataInline {
			span = uint64(len(d) - extDataHdrSize)
		} else {
			if len(d) < extDataRegularSize {
				continue
			}
			span = le.Uint64(d[extDataOffNumBytes:])
		}
		extEnd := fileOffset + span
		if extEnd <= newSize {
			// Entirely within the kept range; leave it alone.
			continue
		}
		if fileOffset >= newSize {
			// Entirely past newSize; drop it.
			newRoot, derr := cowDelete(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, m.k)
			if derr != nil {
				return fmt.Errorf("btrfs truncate: drop extent at file_offset=%d: %w", fileOffset, derr)
			}
			*fsTreeRoot = newRoot
			continue
		}
		// Straddling: trim to (newSize - fileOffset) bytes.
		kept := newSize - fileOffset
		var trimmed []byte
		if extType == extentDataInline {
			trimmed = make([]byte, extDataHdrSize+int(kept))
			copy(trimmed, d[:extDataHdrSize])
			copy(trimmed[extDataHdrSize:], d[extDataHdrSize:extDataHdrSize+int(kept)])
			le.PutUint64(trimmed[extDataOffRamBytes:], kept)
		} else {
			trimmed = make([]byte, len(d))
			copy(trimmed, d)
			le.PutUint64(trimmed[extDataOffRamBytes:], kept)
			le.PutUint64(trimmed[extDataOffNumBytes:], kept)
		}
		newRoot, uerr := cowUpdate(rws, rwaAt, partOff, sb, sm, *fsTreeRoot, m.k, trimmed)
		if uerr != nil {
			return fmt.Errorf("btrfs truncate: trim extent at file_offset=%d: %w", fileOffset, uerr)
		}
		*fsTreeRoot = newRoot
	}

	// Update size + nbytes on the inode.
	return updateInodeMetadata(rwaAt, rws, partOff, sb, sm, fsTreeRoot, in.num, func(d []byte) error {
		le := binary.LittleEndian
		le.PutUint64(d[inodeOffSize:], newSize)
		// Recompute nbytes by re-scanning the (post-trim) EXTENT_DATA items.
		// Inline + sparse contribute 0; regular extents contribute their
		// disk_num_bytes (already sector-aligned by allocDataBytes).
		nbytes := uint64(0)
		items, scanErr := collectPrefixItems(rwaAt, partOff, sb, *fsTreeRoot, in.num, typeExtentData)
		if scanErr == nil {
			for _, m := range items {
				ed := m.data
				if len(ed) < extDataRegularSize {
					continue
				}
				if ed[extDataOffType] != extentDataRegular {
					continue
				}
				diskBytenr := le.Uint64(ed[extDataOffDiskBytenr:])
				if diskBytenr == 0 {
					continue // sparse
				}
				nbytes += le.Uint64(ed[extDataOffDiskNumBytes:])
			}
		}
		le.PutUint64(d[inodeOffNBytes:], nbytes)
		return nil
	})
}
