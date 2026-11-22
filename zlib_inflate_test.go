package filesystem_btrfs

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// inflateZlibNoPanic calls inflateZlib and turns a panic into a test failure
// that names the input, so a removed guard fails an assertion.
func inflateZlibNoPanic(t *testing.T, what string, src []byte, limit int64) (out []byte, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s: inflateZlib panicked: %v", what, r)
		}
	}()
	return inflateZlib(src, limit)
}

// TestInflateZlibGuards asserts the value inflateZlib returns on each side of
// its two trailer guards, so that a removed guard fails an assertion instead of
// crashing the slice after it.
func TestInflateZlibGuards(t *testing.T) {
	plain := bytes.Repeat([]byte("btrfs zlib extent "), 5000)
	good := zlibCompress(t, plain)
	limit := int64(len(plain)) + 1

	out, err := inflateZlib(good, limit)
	if err != nil || !bytes.Equal(out, plain) {
		t.Fatalf("good stream: err=%v equal=%v", err, bytes.Equal(out, plain))
	}

	// A trailer shorter than four bytes is an unexpected EOF, exactly.
	for cut := 1; cut <= 4; cut++ {
		if _, err := inflateZlibNoPanic(t, "short trailer", good[:len(good)-cut], limit); err != io.ErrUnexpectedEOF {
			t.Fatalf("trailer short by %d: err = %v, want io.ErrUnexpectedEOF", cut, err)
		}
	}

	// A DEFLATE body cut short is the decoder's error, neither a trailer error
	// nor a checksum error nor success.
	for _, keep := range []int{3, len(good) / 2, len(good) - 8} {
		_, err := inflateZlibNoPanic(t, "cut body", good[:keep], limit)
		if err == nil {
			t.Fatalf("body cut to %d bytes accepted", keep)
		}
		if err.Error() == "zlib: invalid checksum" {
			t.Fatalf("body cut to %d bytes reached the trailer check: %v", keep, err)
		}
	}

	// A wrong checksum is named as such.
	bad := append([]byte(nil), good...)
	bad[len(bad)-1] ^= 1
	if _, err := inflateZlib(bad, limit); err == nil || err.Error() != "zlib: invalid checksum" {
		t.Fatalf("bad checksum: err = %v", err)
	}

	// Output beyond limit is cut there, with no error.
	out, err = inflateZlib(good, 100)
	if err != nil || len(out) != 100 || errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("limit 100: len=%d err=%v", len(out), err)
	}
}
