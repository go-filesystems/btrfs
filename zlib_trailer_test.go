package filesystem_btrfs

import (
	"bytes"
	"strings"
	"testing"
)

func TestZlibTrailerChecked(t *testing.T) {
	plain := bytes.Repeat([]byte("btrfs zlib extent "), 5000)
	good := zlibCompress(t, plain)
	got, err := decompressExtent(good, compressionZlib, uint64(len(plain)))
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("good stream: err=%v equal=%v", err, bytes.Equal(got, plain))
	}
	bad := append([]byte(nil), good...)
	bad[len(bad)-1] ^= 1
	if _, err := decompressExtent(bad, compressionZlib, uint64(len(plain))); err == nil {
		t.Fatal("bad Adler-32 accepted")
	}
	if _, err := decompressExtent(good[:len(good)-2], compressionZlib, uint64(len(plain))); err == nil {
		t.Fatal("truncated trailer accepted")
	}
	for cut := 1; cut <= 4; cut++ {
		if _, err := decompressExtent(good[:len(good)-cut], compressionZlib, uint64(len(plain))); err == nil {
			t.Fatalf("trailer short by %d bytes accepted", cut)
		}
	}
	for n := 0; n <= 2; n++ {
		_, err := decompressExtent(good[:n], compressionZlib, uint64(len(plain)))
		if err == nil || !strings.Contains(err.Error(), "unexpected EOF") {
			t.Fatalf("%d-byte stream: err = %v, want unexpected EOF", n, err)
		}
	}
	hdr := append([]byte(nil), good...)
	hdr[1] ^= 0x20
	if _, err := decompressExtent(hdr, compressionZlib, uint64(len(plain))); err == nil {
		t.Fatal("bad header accepted")
	}
	// kernel extents are padded with zeros to the sector size
	pad := append(append([]byte(nil), good...), make([]byte, 100)...)
	if got, err := decompressExtent(pad, compressionZlib, uint64(len(plain))); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("padded: %v", err)
	}
}
