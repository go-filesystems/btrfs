package filesystem_btrfs

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// The framing and decoder refusals of decompressLzoInto, one per case.
func TestLzoInto_Refusals(t *testing.T) {
	payload := bytes.Repeat([]byte("abcdefgh"), btrfsLzoPageSize/8*2) // two pages
	good := btrfsLzoEncodeAllLiterals(payload)

	badTotal := append([]byte(nil), good...)
	binary.LittleEndian.PutUint32(badTotal[:4], uint32(len(good)+1)) // beyond the buffer
	smallTotal := append([]byte(nil), good...)
	binary.LittleEndian.PutUint32(smallTotal[:4], 3) // below the header
	corrupt := append([]byte(nil), good...)
	for i := 8; i < 72; i++ {
		corrupt[i] = 0xff
	}

	for _, tc := range []struct {
		name string
		src  []byte
		dst  int
		want string
	}{
		// The spare capacity keeps a missing length check from panicking, so a
		// mutant that drops it fails this assertion instead of crashing.
		{"empty", make([]byte, 0, 8), len(payload), "too short"},
		{"short-header", []byte{4, 0, 0, 0}[:3], len(payload), "too short"},
		{"header-only", []byte{4, 0, 0, 0}, 0, ""},
		{"total-beyond-buffer", badTotal, len(payload), "invalid total_size"},
		{"total-below-header", smallTotal, len(payload), "invalid total_size"},
		{"corrupt-payload", corrupt, len(payload), "segment at offset"},
		{"window-too-small", good, len(payload) - 1, "exceeds"},
		{"window-too-large", good, len(payload) + 1, "expected"},
	} {
		err := decompressLzoInto(make([]byte, tc.dst), tc.src)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error containing %q", tc.name, err, tc.want)
		}
	}
	if err := decompressLzoInto(make([]byte, len(payload)), good); err != nil {
		t.Fatalf("good extent: %v", err)
	}
}

// A job given the file's own window decodes into it and says so, so place has
// nothing to copy; the bytes are those of the buffered path.
func TestLzoInto_JobDecodesIntoItsWindow(t *testing.T) {
	bf, want, exts := buildParallelFile(t, 8)
	st, err := bf.Stat("/par.bin")
	if err != nil {
		t.Fatal(err)
	}
	items, err := collectPrefixItems(bf.reader(), bf.partOffset, bf.sb, bf.fsTreeRoot, st.Inode(), typeExtentData)
	if err != nil {
		t.Fatal(err)
	}
	in := &inodeItem{num: st.Inode(), size: uint64(len(want)), mode: 0x8000}
	jobs := planFileRead(items, in, len(want))
	n := 0
	for k := range jobs {
		j := &jobs[k]
		if exts[k].codec != compressionLzo {
			continue
		}
		n++
		out := make([]byte, len(want))
		j.dst = out[j.fileOffset : j.fileOffset+j.numBytes : j.fileOffset+j.numBytes]
		data, err := j.decode(bf.reader(), bf.partOffset, bf.sb, 1<<30, in.num)
		if err != nil {
			t.Fatal(err)
		}
		if !j.direct || data != nil {
			t.Fatalf("job %d: direct=%v, data %d bytes", k, j.direct, len(data))
		}
		if !bytes.Equal(j.dst, want[j.fileOffset:j.fileOffset+j.numBytes]) {
			t.Fatalf("job %d: window differs", k)
		}
	}
	if n == 0 {
		t.Fatal("no LZO extent in the fixture")
	}
}
