package filesystem_btrfs

import (
	"bytes"
	"io"
	"testing"
)

// Sequential ReadAt calls on a compressed file are served from a read-ahead
// window: the bytes must equal the plain read at every stride, after a seek
// back, and at the end of the file.
func TestReadAheadSequentialEqualsPlain(t *testing.T) {
	bf, want, _ := buildParallelFile(t, 37)
	for _, w := range []int{1, 4} {
		bf.SetReadParallelism(w)
		for _, stride := range []int{1, 777, extPlain, 3*extPlain + 5, len(want)} {
			f, err := bf.OpenFile("/par.bin")
			if err != nil {
				t.Fatal(err)
			}
			if !f.(*btrfsFile).compressed {
				t.Fatal("fixture has compressed extents")
			}
			var got []byte
			buf := make([]byte, stride)
			for off := 0; off < len(want); {
				n, err := f.ReadAt(buf, int64(off))
				got = append(got, buf[:n]...)
				off += n
				if err != nil && err != io.EOF {
					t.Fatal(err)
				}
				if n == 0 {
					break
				}
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("workers=%d stride=%d: sequential read differs", w, stride)
			}
			// a seek back, inside the window already read ahead, and again forward
			for _, off := range []int{extPlain * 2, 5, extPlain * 20, extPlain*20 + 9} {
				b := make([]byte, 2*extPlain+3)
				n, _ := f.ReadAt(b, int64(off))
				if !bytes.Equal(b[:n], want[off:off+n]) {
					t.Fatalf("workers=%d stride=%d: ReadAt(%d) differs", w, stride, off)
				}
			}
		}
	}
}

// The read-ahead is used only for a sequential read with several workers.
func TestReadAheadFillsWindow(t *testing.T) {
	bf, want, _ := buildParallelFile(t, 37)
	bf.SetReadParallelism(4)
	f, _ := bf.OpenFile("/par.bin")
	bt := f.(*btrfsFile)
	buf := make([]byte, extPlain)
	f.ReadAt(buf, 0)
	if bt.ra != nil {
		t.Fatal("first read must not read ahead")
	}
	f.ReadAt(buf, extPlain)
	if len(bt.ra) != len(want)-extPlain || bt.raOff != extPlain {
		t.Fatalf("read-ahead window = %d bytes at %d, want %d at %d", len(bt.ra), bt.raOff, len(want)-extPlain, extPlain)
	}
	bf.SetReadParallelism(1)
	g, _ := bf.OpenFile("/par.bin")
	g.ReadAt(buf, 0)
	g.ReadAt(buf, extPlain)
	if g.(*btrfsFile).ra != nil {
		t.Fatal("one worker must not read ahead")
	}
}
