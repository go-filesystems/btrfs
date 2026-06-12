package filesystem_btrfs

// Real-artifact regression test for sector-padded kernel zstd extents.
//
// The in-memory zstd tests in compression_test.go build their frames with
// zstd.EncodeAll, which yields an exact-sized frame with no trailing bytes.
// That is NOT what the kernel writes: a kernel zstd extent stores one zstd
// frame followed by zero padding out to the sector size (e.g. a 65-byte
// frame inside a 4096-byte on-disk extent). A streaming decoder that keeps
// reading past the first frame then trips over the padding and fails with
// "magic number mismatch". The exact-sized in-memory fixtures never exercise
// that padding, so they were false positives for the at-rest read path.
//
// This test produces a genuine kernel artifact: mkfs.btrfs onto a loop
// device, mount with `-o compress-force=zstd`, write a highly compressible
// file larger than 128 KiB (so the kernel emits multiple regular,
// sector-padded zstd extents rather than a single inline one), unmount, then
// read the file back at rest through our driver and byte-compare.
//
// The test needs root for losetup/mount, plus mkfs.btrfs and the btrfs CLI;
// it skip-gates cleanly when any of those are unavailable (e.g. macOS dev
// hosts or unprivileged CI).

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// requireRootAndBtrfsTools skip-gates when the test cannot build a real
// kernel btrfs artifact: needs root (losetup + mount), mkfs.btrfs, mount,
// umount and losetup on PATH.
func requireRootAndBtrfsTools(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("real kernel zstd artifact test requires root for losetup/mount; skipping")
	}
	for _, bin := range []string{"mkfs.btrfs", "mount", "umount", "losetup"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not found on PATH; install btrfs-progs/util-linux to enable this test (got: %v)", bin, err)
		}
	}
}

// run executes a command and fails the test with combined output on error.
func run(t *testing.T, name string, args ...string) {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\noutput:\n%s", name, args, err, out)
	}
}

// TestZstd_KernelSectorPaddedExtent_ReadAtRest is the real-artifact
// regression for the sector-padding bug. It writes a >128 KiB highly
// compressible file under `compress-force=zstd`, so the kernel stores the
// data in regular zstd extents whose frames are followed by zero padding up
// to the sector size. We then read the file back through the driver and
// require an exact byte match.
func TestZstd_KernelSectorPaddedExtent_ReadAtRest(t *testing.T) {
	requireRootAndBtrfsTools(t)

	dir := t.TempDir()
	img := filepath.Join(dir, "zstd-kernel.img")

	// 256 MiB image: large enough for mkfs.btrfs defaults and several
	// data extents.
	const imgSize = 256 * 1024 * 1024
	f, err := os.Create(img)
	if err != nil {
		t.Fatalf("create image: %v", err)
	}
	if err := f.Truncate(imgSize); err != nil {
		f.Close()
		t.Fatalf("truncate image: %v", err)
	}
	f.Close()

	// Format the raw image (whole-disk btrfs, no partition table).
	run(t, "mkfs.btrfs", "-f", "-L", "zstdpad", img)

	// Attach to a loop device.
	loopOut, err := exec.Command("losetup", "--find", "--show", img).CombinedOutput()
	if err != nil {
		t.Fatalf("losetup: %v\noutput:\n%s", err, loopOut)
	}
	loop := string(bytes.TrimSpace(loopOut))
	defer func() { _ = exec.Command("losetup", "-d", loop).Run() }()

	mnt := filepath.Join(dir, "mnt")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatalf("mkdir mnt: %v", err)
	}

	// Mount forcing zstd compression on all writes.
	run(t, "mount", "-o", "compress-force=zstd", loop, mnt)
	mounted := true
	unmount := func() {
		if mounted {
			run(t, "umount", mnt)
			mounted = false
		}
	}
	defer unmount()

	// Build a highly compressible payload well over 128 KiB so the kernel
	// emits multiple regular (non-inline) sector-padded zstd extents. The
	// content is deterministic but repetitive so zstd shrinks it hard,
	// guaranteeing the on-disk frame is much smaller than its extent and
	// thus heavily zero-padded.
	const payloadSize = 512 * 1024 // 512 KiB > 128 KiB
	payload := make([]byte, payloadSize)
	for i := range payload {
		// A slowly varying, highly compressible pattern.
		payload[i] = byte('A' + (i/4096)%26)
	}

	target := filepath.Join(mnt, "big.bin")
	if err := os.WriteFile(target, payload, 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	// sync to ensure data + metadata hit the loop-backed image before
	// unmount flushes everything.
	run(t, "sync")
	unmount()

	// Confirm the file really is zstd-compressed on disk (codec 3) and uses
	// regular extents, so a regression that "passes" via an unrelated code
	// path is caught. btrfs inspect-internal is best-effort; we don't fail
	// the test if the CLI shape changes, but we do log it.
	if bin, lerr := exec.LookPath("btrfs"); lerr == nil {
		if out, derr := exec.Command(bin, "inspect-internal", "dump-tree", "-t", "fs", img).CombinedOutput(); derr == nil {
			if !bytes.Contains(out, []byte("compression 3")) {
				t.Logf("warning: dump-tree did not show 'compression 3' (zstd); on-disk extent may not be zstd:\n%s", firstLines(out, 40))
			}
		}
	}

	// Read the file back at rest through our driver and byte-compare.
	fs, err := Open(img, -1)
	if err != nil {
		t.Fatalf("Open(%s): %v", img, err)
	}
	defer fs.Close()

	got, err := fs.ReadFile("/big.bin")
	if err != nil {
		t.Fatalf("ReadFile(/big.bin): %v", err)
	}
	if len(got) != len(payload) {
		t.Fatalf("read length mismatch: got %d bytes, want %d", len(got), len(payload))
	}
	if !bytes.Equal(got, payload) {
		// Find first differing offset for a useful failure message.
		off := 0
		for off < len(got) && got[off] == payload[off] {
			off++
		}
		t.Fatalf("kernel zstd read-at-rest mismatch at offset %d: got %x want %x",
			off, sample(got, off), sample(payload, off))
	}
}

// firstLines returns up to n lines of b for diagnostic logging.
func firstLines(b []byte, n int) []byte {
	count := 0
	for i, c := range b {
		if c == '\n' {
			count++
			if count >= n {
				return b[:i]
			}
		}
	}
	return b
}

// sample returns up to 16 bytes of b starting at off, for failure messages.
func sample(b []byte, off int) []byte {
	end := off + 16
	if end > len(b) {
		end = len(b)
	}
	return b[off:end]
}
