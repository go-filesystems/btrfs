package filesystem_btrfs

// openfile.go — filesystem.Opener: positional reads of a regular file. The
// EXTENT_DATA items of the file (metadata) are collected once, at open; each
// ReadAt then reads, and where needed decompresses, only the extents that
// overlap its window, so a server answering a READ of a few bytes of a large
// file (NFS, SFTP) does not rebuild the whole file through ReadFile for every
// request. The per-extent rules are those of readFileData.

import (
	"encoding/binary"
	"fmt"
	"io"

	filesystem "github.com/go-filesystems/interface"
	"github.com/go-volumes/safeio"
)

var (
	_ filesystem.Opener = (*btrfsFS)(nil)
	_ filesystem.Opener = (*subvolView)(nil)
)

// OpenFile opens the regular file at path for positional reads.
func (fs *btrfsFS) OpenFile(path string) (filesystem.File, error) {
	return openBtrfsFile(fs, fs.fsTreeRoot, path)
}

// OpenFile opens the regular file at path of the subvolume for positional reads.
func (v *subvolView) OpenFile(path string) (filesystem.File, error) {
	return openBtrfsFile(v.parent, v.fsTreeRoot, path)
}

func openBtrfsFile(fs *btrfsFS, root uint64, path string) (filesystem.File, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	r := fs.reader()
	in, err := pathLookup(r, fs.partOffset, fs.sb, root, path)
	if err != nil {
		return nil, err
	}
	if !in.isRegular() {
		return nil, fmt.Errorf("btrfs: %q is not a regular file", path)
	}
	f := &btrfsFile{fs: fs, num: in.num, size: in.size}
	if f.size > 1<<62 { // a forged size is no size
		f.size = 1 << 62
	}
	if in.size == 0 {
		return f, nil
	}
	if f.items, err = collectPrefixItems(r, fs.partOffset, fs.sb, root, in.num, typeExtentData); err != nil {
		return nil, fmt.Errorf("btrfs: read extents for inode %d: %w", in.num, err)
	}
	// The kernel refuses overlapping extents (tree-checker); so does the reader,
	// once, here, instead of paying an extent again for every item that overlaps.
	var prevEnd uint64
	le := binary.LittleEndian
	for _, m := range f.items {
		d := m.data
		if len(d) <= extDataOffType || d[extDataOffType] != extentDataRegular || len(d) < extDataRegularSize {
			continue
		}
		end := m.k.offset + le.Uint64(d[extDataOffNumBytes:])
		if end < m.k.offset || m.k.offset < prevEnd {
			return nil, fmt.Errorf("btrfs: inode %d extent at file offset %d overlaps the previous extent (ends at %d): %w",
				in.num, m.k.offset, prevEnd, safeio.ErrOutOfBounds)
		}
		prevEnd = end
		if d[extDataOffCompression] != compressionNone {
			f.compressed = true
		}
	}
	return f, nil
}

type btrfsFile struct {
	fs    *btrfsFS
	num   uint64
	size  uint64
	items []prefixMatch

	// Read-ahead state, guarded by fs.mu (see fill). compressed is set at open.
	compressed bool
	seqEnd     uint64 // where the previous read ended
	raOff      uint64 // file offset of ra[0]
	ra         []byte // the decoded window read ahead, or nil
}

// readAheadBytes is the window a sequential read of a compressed file decodes
// at once. A ReadAt of 1 MiB holds 8 extents of 128 KiB: decoded alone, each
// call fills the workers and then drains them (head-of-line wait on the last
// extents), which capped lzo at 4 CPUs at about 2600 MiB/s against 3800 for a
// window of 16-64 MiB (btrfs#56).
const readAheadBytes = 16 << 20

func (f *btrfsFile) Size() int64 { return int64(f.size) }

func (f *btrfsFile) Close() error { return nil }

// ReadAt fills p from off; a hole or a preallocated extent reads as zeros, and
// the window past the end of the file is io.EOF with the bytes there are.
func (f *btrfsFile) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("btrfs: negative offset %d", off)
	}
	if uint64(off) >= f.size {
		return 0, io.EOF
	}
	want := uint64(len(p))
	if uint64(off)+want > f.size {
		want = f.size - uint64(off)
	}
	dst := p[:want]
	clear(dst)
	if err := f.fill(dst, uint64(off)); err != nil {
		return 0, err
	}
	if want < uint64(len(p)) {
		return int(want), io.EOF
	}
	return int(want), nil
}

// fill reads the window. A read that continues the previous one, on a file with
// compressed extents and with several workers, decodes readAheadBytes at once
// into f.ra and serves this and the next reads from it.
func (f *btrfsFile) fill(dst []byte, off uint64) error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	end := off + uint64(len(dst))
	if f.ra != nil && off >= f.raOff && end <= f.raOff+uint64(len(f.ra)) {
		copy(dst, f.ra[off-f.raOff:end-f.raOff])
		f.seqEnd = end
		return nil
	}
	sequential := off != 0 && off == f.seqEnd
	f.seqEnd = end
	if sequential && f.compressed && f.fs.readWorkers() > 1 && len(dst) < readAheadBytes {
		if n := umin(readAheadBytes, f.size-off); n > uint64(len(dst)) {
			if uint64(cap(f.ra)) < n {
				f.ra = make([]byte, n)
			}
			buf := f.ra[:n]
			clear(buf)
			f.ra = nil
			if err := f.fillLocked(buf, off); err == nil {
				f.ra, f.raOff = buf, off
				copy(dst, buf)
				return nil
			}
			// the plain path below reports the error for dst's own window
		}
	}
	return f.fillLocked(dst, off)
}

func (f *btrfsFile) fillLocked(dst []byte, off uint64) error {
	r := f.fs.reader()
	sb, partOff := f.fs.sb, f.fs.partOffset
	hi := off + uint64(len(dst))
	le := binary.LittleEndian
	dev := deviceSize(r)
	// The extents that overlap the window become jobs, in file order; the
	// compressed ones are read and decoded on the workers (parread.go), the
	// copies into dst happen in file order from here, and the error returned is
	// the first in file order.
	var jobs []readJob
	for _, m := range f.items {
		fileOffset := m.k.offset
		if fileOffset >= hi { // items are in file order
			break
		}
		if fileOffset >= f.size {
			continue
		}
		remaining := f.size - fileOffset
		d := m.data
		if len(d) <= extDataOffType {
			continue
		}
		compression := d[extDataOffCompression]
		switch d[extDataOffType] {
		case extentDataInline:
			jobs = append(jobs, readJob{
				kind: jobInline, fileOffset: fileOffset, remaining: remaining,
				compression: compression, inline: d[extDataHdrSize:],
				ramBytes: clampRAM(le.Uint64(d[extDataOffRamBytes:]), remaining),
			})

		case extentDataRegular:
			if len(d) < extDataRegularSize {
				continue
			}
			numBytes := umin(le.Uint64(d[extDataOffNumBytes:]), remaining)
			if fileOffset+numBytes < fileOffset || fileOffset+numBytes <= off {
				continue // wraps, or ends before the window
			}
			diskBytenr := le.Uint64(d[extDataOffDiskBytenr:])
			if diskBytenr == 0 {
				continue // sparse: already zero
			}
			extOffset := le.Uint64(d[extDataOffOffset:])
			j := readJob{
				kind: jobRaw, fileOffset: fileOffset, remaining: remaining, numBytes: numBytes,
				extOffset: extOffset, diskBytenr: diskBytenr,
				diskNumBytes: le.Uint64(d[extDataOffDiskNumBytes:]), compression: compression,
			}
			if compression != compressionNone {
				// A compressed extent is read and decompressed whole (at most
				// ram_bytes, 128 KiB for the kernel's writer), and the window is
				// copied out of it.
				j.kind = jobCompressed
				j.ramBytes = clampRAM(le.Uint64(d[extDataOffRamBytes:]), extOffset+numBytes)
			}
			jobs = append(jobs, j)
		}
	}
	work := func(k int) ([]byte, error) { return jobs[k].decode(r, partOff, sb, dev, f.num) }
	emit := func(k int, data []byte) error {
		j := &jobs[k]
		switch j.kind {
		case jobInline:
			copyWindow(dst, off, j.fileOffset, data[:umin(uint64(len(data)), j.remaining)])
		case jobCompressed:
			copyWindow(dst, off, j.fileOffset, data)
		case jobRaw:
			lo, top := umax(j.fileOffset, off), umin(j.fileOffset+j.numBytes, hi)
			phys, err := sb.physAddr(partOff, j.diskBytenr+j.extOffset+(lo-j.fileOffset))
			if err != nil {
				return fmt.Errorf("btrfs: inode %d extent at file offset %d: %w", f.num, j.fileOffset, err)
			}
			if _, err := r.ReadAt(dst[lo-off:top-off], phys); err != nil {
				return fmt.Errorf("btrfs: read extent data: %w", err)
			}
		}
		return nil
	}
	return inOrder(len(jobs), f.fs.readWorkers(), work, emit)
}

// The package has its own min and max (of ints); these are the uint64 ones.
func umin(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

func umax(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

// copyWindow copies the part of src (which starts at file offset srcOff) that
// lies in the window dst (which starts at file offset off).
func copyWindow(dst []byte, off, srcOff uint64, src []byte) {
	hi := off + uint64(len(dst))
	srcEnd := srcOff + uint64(len(src))
	lo, top := umax(srcOff, off), umin(srcEnd, hi)
	if top <= lo {
		return
	}
	copy(dst[lo-off:top-off], src[lo-srcOff:top-srcOff])
}
