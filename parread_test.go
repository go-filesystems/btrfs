package filesystem_btrfs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// A file of compressed extents of every codec, plus an uncompressed one, built
// in a formatted image: extent k covers file bytes [k*extPlain, (k+1)*extPlain).

const extPlain = 4096

// pextent is the fixture's account of one extent: the payload bytes it holds on
// disk (so a test can damage one) and where they are.
type pextent struct {
	codec uint8
	phys  uint64
}

func buildParallelFile(t *testing.T, n int) (bf *btrfsFS, want []byte, exts []pextent) {
	t.Helper()
	fsys, err := Format(filepath.Join(t.TempDir(), "par.img"), 8*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	t.Cleanup(func() { fsys.Close() })
	bf = fsys.(*btrfsFS)
	if err := fsys.WriteFile("/par.bin", make([]byte, n*extPlain), 0o644); err != nil {
		t.Fatalf("WriteFile placeholder: %v", err)
	}
	st, err := fsys.Stat("/par.bin")
	if err != nil {
		t.Fatal(err)
	}
	ino := st.Inode()
	freeInodeExtents(bf.f, bf.partOffset, bf.sb, bf.sm, bf.fsTreeRoot, ino)
	root, err := cowDeletePrefix(nil, bf.f, bf.partOffset, bf.sb, bf.sm, bf.fsTreeRoot, ino, typeExtentData)
	if err != nil {
		t.Fatal(err)
	}
	bf.fsTreeRoot = root
	codecs := []uint8{compressionZlib, compressionLzo, compressionZstd, compressionNone}
	for k := range n {
		plain := bytes.Repeat([]byte(fmt.Sprintf("extent-%03d-compresses-well;", k)), extPlain/8)[:extPlain]
		want = append(want, plain...)
		codec := codecs[k%len(codecs)]
		var disk []byte
		switch codec {
		case compressionZlib:
			disk = zlibCompress(t, plain)
		case compressionLzo:
			disk = btrfsLzoEncodeAllLiterals(plain)
		case compressionZstd:
			disk = zstdCompress(t, plain)
		default:
			disk = plain
		}
		phys, _, err := bf.sm.allocDataBytes(uint64(len(disk)), uint64(bf.sb.sectorSize))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := bf.f.WriteAt(disk, bf.partOffset+int64(phys)); err != nil {
			t.Fatal(err)
		}
		exts = append(exts, pextent{codec, phys})
		item := encodeRegularExtentData(physToLog(bf.sb, phys), uint64(len(disk)), extPlain, 0, extPlain, bf.sb.generation+1, codec)
		if root, err = cowInsert(nil, bf.f, bf.partOffset, bf.sb, bf.sm, bf.fsTreeRoot, key{ino, typeExtentData, uint64(k) * extPlain}, item); err != nil {
			t.Fatal(err)
		}
		bf.fsTreeRoot = root
	}
	if err := updateFsTreeRoot(bf.f, bf.partOffset, bf.sb, bf.sm, bf.fsTreeRoot); err != nil {
		t.Fatal(err)
	}
	return bf, want, exts
}

// The same bytes at every worker count, through ReadFile and through ReadAt
// windows that start and end inside extents.
func TestParallelReadMatchesSerial(t *testing.T) {
	bf, want, _ := buildParallelFile(t, 37)
	for _, w := range []int{1, 2, 3, 8, 64} {
		bf.SetReadParallelism(w)
		got, err := bf.ReadFile("/par.bin")
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("workers=%d: ReadFile: %d bytes, err %v, equal %v", w, len(got), err, bytes.Equal(got, want))
		}
		f, err := bf.OpenFile("/par.bin")
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct{ off, n int }{{0, len(want)}, {100, 3 * extPlain}, {extPlain - 1, 2}, {5*extPlain + 77, 20*extPlain + 5}, {len(want) - 10, 10}} {
			buf := make([]byte, tc.n)
			if n, err := f.ReadAt(buf, int64(tc.off)); n != tc.n || err != nil || !bytes.Equal(buf, want[tc.off:tc.off+tc.n]) {
				t.Fatalf("workers=%d: ReadAt(%d, %d) = %d, %v", w, tc.off, tc.n, n, err)
			}
		}
	}
}

// TestFirstErrorInFileOrder: two damaged extents, the later one failing at
// once and the earlier one after a larger decode, must report the earlier one on
// every run and at every worker count (the btrfs counterpart of zfs's
// TestFirstErrorInFileOrderAcrossPointerAndRecord).
func TestFirstErrorInFileOrder(t *testing.T) {
	bf, _, exts := buildParallelFile(t, 40)
	const early, late = 9, 30 // 9 is zlib+1 = lzo; both compressed
	for _, k := range []int{early, late} {
		if exts[k].codec == compressionNone {
			t.Fatalf("extent %d is not compressed", k)
		}
		// Damage the payload in the middle: the decoders refuse it.
		junk := bytes.Repeat([]byte{0xff}, 64)
		if _, err := bf.f.WriteAt(junk, bf.partOffset+int64(exts[k].phys)+8); err != nil {
			t.Fatal(err)
		}
	}
	wantOff := fmt.Sprintf("file offset %d:", early*extPlain)
	for _, w := range []int{1, 2, 8} {
		bf.SetReadParallelism(w)
		for run := range 50 {
			_, err := bf.ReadFile("/par.bin")
			if err == nil || !strings.Contains(err.Error(), wantOff) {
				t.Fatalf("workers=%d run %d: ReadFile error %v, want the one at %s", w, run, err, wantOff)
			}
		}
		f, err := bf.OpenFile("/par.bin")
		if err != nil {
			t.Fatal(err)
		}
		for run := range 50 {
			_, err := f.ReadAt(make([]byte, 40*extPlain), 0)
			if err == nil || errors.Is(err, io.EOF) || !strings.Contains(err.Error(), wantOff) {
				t.Fatalf("workers=%d run %d: ReadAt error %v, want the one at %s", w, run, err, wantOff)
			}
		}
	}
}

// A planning error (an overlapping extent) comes after the decode error of an
// extent before it, in file order, not before it: readFileDataN is given items
// whose last one overlaps the one before, and whose first one is damaged.
func TestPlanErrorComesAfterEarlierDecodeError(t *testing.T) {
	bf, _, exts := buildParallelFile(t, 12)
	if _, err := bf.f.WriteAt(bytes.Repeat([]byte{0xff}, 64), bf.partOffset+int64(exts[1].phys)+8); err != nil {
		t.Fatal(err)
	}
	st, err := bf.Stat("/par.bin")
	if err != nil {
		t.Fatal(err)
	}
	items, err := collectPrefixItems(bf.reader(), bf.partOffset, bf.sb, bf.fsTreeRoot, st.Inode(), typeExtentData)
	if err != nil {
		t.Fatal(err)
	}
	// A last item that starts inside the extent before it overlaps it.
	dup := items[len(items)-1]
	dup.k.offset = items[len(items)-1].k.offset - 1
	items = append(items, dup)
	in := &inodeItem{num: st.Inode(), size: 12 * extPlain, mode: 0x8000}
	jobs := planFileRead(items, in, 12*extPlain)
	if last := jobs[len(jobs)-1]; last.kind != jobFail {
		t.Fatalf("last job kind %d, want a planning failure", last.kind)
	}
	for _, w := range []int{1, 8} {
		err := inOrder(len(jobs), w, func(k int) ([]byte, error) {
			return jobs[k].decode(bf.reader(), bf.partOffset, bf.sb, 1<<30, in.num)
		}, func(int, []byte) error { return nil })
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("file offset %d:", extPlain)) {
			t.Fatalf("workers=%d: %v", w, err)
		}
	}
}

func TestSetReadParallelism(t *testing.T) {
	var fs btrfsFS
	if got := fs.readWorkers(); got != defaultReadParallelism() {
		t.Fatalf("default = %d, want %d", got, defaultReadParallelism())
	}
	for in, want := range map[int]int{-3: 1, 0: 1, 1: 1, 5: 5, 1000: 64} {
		fs.SetReadParallelism(in)
		if got := fs.readWorkers(); got != want {
			t.Fatalf("SetReadParallelism(%d): workers = %d, want %d", in, got, want)
		}
	}
}
