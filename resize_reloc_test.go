package filesystem_btrfs

import (
	"bytes"
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// shrunkRelocFixture is a 24 MiB image our writer formatted, into which a
// 12 MiB /big.bin and a ~1 MiB /marker.bin were written (marker landing in the
// upper half), /big.bin then overwritten with tiny content (freeing the low
// 12 MiB), and finally Shrink(16 MiB) applied — forcing /marker.bin to be
// COW-relocated out of the [16 MiB, 24 MiB) tail into the freed low region.
// `btrfs check` reports it CLEAN and the kernel loop-mounts it with both files
// byte-identical (validated on cb-tpm-ubuntu, kernel 6.17 / btrfs-progs 6.6.3).
// Committed zstd-compressed so the emulated-arch CI (which cross-compiles the
// test binary) can still read it.
//
//go:embed testdata/resize/shrunk-reloc.img.zst
var shrunkRelocFixture []byte

// expected post-shrink file contents in the fixture.
var (
	fixtureMarker = bytes.Repeat([]byte("OWRELOC-"), 128*1024)
	fixtureBig    = []byte("now-small\n")
)

// decompressFixture inflates the embedded zstd image into a temp file and
// returns its path.
func decompressFixture(t *testing.T) string {
	t.Helper()
	zr, err := zstd.NewReader(bytes.NewReader(shrunkRelocFixture))
	if err != nil {
		t.Fatalf("zstd reader: %v", err)
	}
	defer zr.Close()
	raw, err := zr.DecodeAll(shrunkRelocFixture, nil)
	if err != nil {
		t.Fatalf("zstd decode: %v", err)
	}
	path := filepath.Join(t.TempDir(), "shrunk-reloc.img")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// TestReloc_FixtureReadback opens the committed post-shrink fixture with our
// own reader (no kernel needed, so it runs on every CI arch including the
// emulated big-endian s390x) and asserts the geometry is shrunk and the
// relocated files read back byte-for-byte.
func TestReloc_FixtureReadback(t *testing.T) {
	path := decompressFixture(t)
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open shrunk fixture: %v", err)
	}
	defer fs.Close()

	bfs := fs.(*btrfsFS)
	if got := bfs.sb.totalBytes; got != 16*1024*1024 {
		t.Errorf("fixture total_bytes = %d, want %d", got, 16*1024*1024)
	}
	// The data chunk must end at or below the new device size.
	for _, m := range bfs.sb.sysChunks {
		if m.localStripeIdx < 0 {
			continue
		}
		if end := m.physStart + m.size; end > 16*1024*1024 {
			t.Errorf("chunk at phys 0x%X size %d extends past shrunk device size", m.physStart, m.size)
		}
	}

	if got, err := fs.ReadFile("/marker.bin"); err != nil || !bytes.Equal(got, fixtureMarker) {
		t.Errorf("relocated /marker.bin mismatch: err=%v len=%d want %d", err, len(got), len(fixtureMarker))
	}
	if got, err := fs.ReadFile("/big.bin"); err != nil || !bytes.Equal(got, fixtureBig) {
		t.Errorf("/big.bin mismatch: err=%v got=%q", err, got)
	}
}

// TestReloc_RuntimeShrink reproduces the relocation end-to-end with our own
// reader as the oracle (no kernel): write a low big file + a high marker,
// overwrite the big file tiny to free the low region, shrink to clip the
// marker — then verify the marker survived relocation byte-for-byte and the
// device shrank. Runs on every arch.
func TestReloc_RuntimeShrink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rt.img")
	fs, err := Format(path, 24*1024*1024, FormatConfig{Label: "rt-reloc"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)
	marker := bytes.Repeat([]byte("RT-MARK-"), 96*1024) // ~768 KiB
	if err := fs.WriteFile("/big.bin", bytes.Repeat([]byte{'B'}, 12*1024*1024), 0o644); err != nil {
		fs.Close()
		t.Fatalf("big: %v", err)
	}
	if err := fs.WriteFile("/marker.bin", marker, 0o644); err != nil {
		fs.Close()
		t.Fatalf("marker: %v", err)
	}
	if err := fs.WriteFile("/big.bin", []byte("tiny\n"), 0o644); err != nil {
		fs.Close()
		t.Fatalf("overwrite: %v", err)
	}
	if err := bfs.Shrink(16 * 1024 * 1024); err != nil {
		fs.Close()
		t.Fatalf("Shrink with relocation: %v", err)
	}
	if got := readSBTotalBytes(t, bfs); got != 16*1024*1024 {
		t.Errorf("post-shrink SB total_bytes = %d, want %d", got, 16*1024*1024)
	}
	// Relocated marker must be intact and now physically below the new size.
	if got, err := fs.ReadFile("/marker.bin"); err != nil || !bytes.Equal(got, marker) {
		t.Errorf("relocated marker mismatch: err=%v len=%d", err, len(got))
	}
	fs.Close()

	// Reopen to confirm the on-disk shrink + relocation persisted.
	r2, err := Open(path, -1)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer r2.Close()
	if got, err := r2.ReadFile("/marker.bin"); err != nil || !bytes.Equal(got, marker) {
		t.Errorf("after reopen relocated marker mismatch: err=%v len=%d", err, len(got))
	}
}

// TestReloc_RefusesEntireChunkRemoval asserts the clear-error boundary when a
// shrink would remove the whole trailing chunk (chunk relocation is out of
// scope).
func TestReloc_RefusesEntireChunkRemoval(t *testing.T) {
	fs, _ := resizeTempImage(t, 16*1024*1024)
	// The DATA chunk starts at 5 MiB; shrinking to <= 5 MiB removes it whole.
	err := fs.Shrink(5 * 1024 * 1024)
	if err == nil {
		t.Fatal("Shrink removing entire trailing chunk accepted; want error")
	}
	if !strings.Contains(err.Error(), "chunk relocation not supported") &&
		!strings.Contains(err.Error(), "below minimum") {
		t.Errorf("expected chunk-relocation boundary error, got: %v", err)
	}
}

// TestReloc_InsufficientLowSpace exercises the relocation alloc-failure path:
// the tail holds live data but there is not enough free space BELOW the new
// size to receive it, so relocation must fail with a clear error and leave the
// image valid (still readable at the old size).
func TestReloc_InsufficientLowSpace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tight.img")
	fs, err := Format(path, 16*1024*1024, FormatConfig{Label: "tight"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)
	// Fill most of the data chunk contiguously so there is no low hole; the
	// last extent lands high in the [12 MiB, 16 MiB) tail.
	body := bytes.Repeat([]byte{'D'}, 9*1024*1024)
	if err := fs.WriteFile("/data.bin", body, 0o644); err != nil {
		fs.Close()
		t.Fatalf("write: %v", err)
	}
	// Shrinking to 12 MiB needs to relocate the high part of data.bin, but the
	// low region is already full — no room. Expect a relocation error.
	err = bfs.Shrink(12 * 1024 * 1024)
	if err == nil {
		t.Fatal("Shrink accepted despite insufficient low free space; want error")
	}
	// The image must remain readable at its original size.
	if got, rerr := fs.ReadFile("/data.bin"); rerr != nil || !bytes.Equal(got, body) {
		t.Errorf("image corrupted after failed shrink: err=%v len=%d", rerr, len(got))
	}
	fs.Close()
}

// TestReloc_LiveMetaInRange unit-tests the metadata-detection helper that
// gates the relocation post-condition: a metadata block inside the queried
// window is reported, and a window past all live metadata is reported clear.
func TestReloc_LiveMetaInRange(t *testing.T) {
	fs, _ := resizeTempImage(t, 16*1024*1024)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	// Metadata lives at the front of the data chunk (~5 MiB). A window covering
	// the whole device must find some; a window in the empty high tail must not.
	if _, found := fs.liveMetaInRange(0, 16*1024*1024); !found {
		t.Error("liveMetaInRange over whole device found no metadata; expected some")
	}
	if hit, found := fs.liveMetaInRange(15*1024*1024, 16*1024*1024); found {
		t.Errorf("liveMetaInRange in empty tail unexpectedly found block 0x%X", hit)
	}
}

// TestReloc_CollectTargetsEmptyTail confirms collectRelocTargets returns no
// targets for a tail with no data extents (the empty-tail fast path).
func TestReloc_CollectTargetsEmptyTail(t *testing.T) {
	fs, _ := resizeTempImage(t, 16*1024*1024)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if got := fs.collectRelocTargets(15*1024*1024, 16*1024*1024); len(got) != 0 {
		t.Errorf("collectRelocTargets on empty tail returned %d targets, want 0", len(got))
	}
}

// relocImageWithTailData formats an image through a failBackend, writes a low
// big file + a high marker, then overwrites the big file tiny so the marker
// stays in the [16 MiB, 24 MiB) tail with a low hole to relocate into. Returns
// the open FS and the wrapper for fault injection. The returned FS is mid-life
// (no faults armed yet).
func relocImageWithTailData(t *testing.T) (*btrfsFS, *failBackend) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reloc-fault.img")
	if _, err := Format(path, 24*1024*1024, FormatConfig{}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	f, err := osOpenFileRW(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	wrapper := &failBackend{inner: &osFileBackend{f: f}}
	fs, err := OpenFromDevice(wrapper, -1)
	if err != nil {
		t.Fatalf("OpenFromDevice: %v", err)
	}
	bfs := fs.(*btrfsFS)
	if err := bfs.WriteFile("/big.bin", bytes.Repeat([]byte{'B'}, 12*1024*1024), 0o644); err != nil {
		fs.Close()
		t.Fatalf("big: %v", err)
	}
	if err := bfs.WriteFile("/marker.bin", bytes.Repeat([]byte("M"), 1024*1024), 0o644); err != nil {
		fs.Close()
		t.Fatalf("marker: %v", err)
	}
	if err := bfs.WriteFile("/big.bin", []byte("tiny\n"), 0o644); err != nil {
		fs.Close()
		t.Fatalf("overwrite: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	return bfs, wrapper
}

// TestReloc_WriteFaultDuringRelocation injects a WriteAt failure so the data
// copy in relocateTailExtents fails; the shrink must error and not truncate.
func TestReloc_WriteFaultDuringRelocation(t *testing.T) {
	fs, fb := relocImageWithTailData(t)
	fb.failWriteAt = true
	if err := fs.Shrink(16 * 1024 * 1024); err == nil {
		t.Fatal("Shrink accepted despite WriteAt failure during relocation")
	}
}

// TestReloc_ReadFaultDuringRelocation injects a ReadAt failure that fires once
// relocation starts reading the extent bytes, so the copy fails.
func TestReloc_ReadFaultDuringRelocation(t *testing.T) {
	fs, fb := relocImageWithTailData(t)
	fb.failReadAt = true
	if err := fs.Shrink(16 * 1024 * 1024); err == nil {
		t.Fatal("Shrink accepted despite ReadAt failure during relocation")
	}
}

// TestReloc_KernelOracle is the real kernel oracle: it builds a fresh image,
// drives the same relocation shrink, then runs `btrfs check` and loop-mounts
// the result, asserting a clean check and byte-identical files. Skip-gated
// unless root + btrfs-progs + mount/umount/losetup are available (CI runs it
// only on the native Linux runners with the tools installed; macOS dev hosts
// and emulated short runs skip it).
func TestReloc_KernelOracle(t *testing.T) {
	if testing.Short() {
		t.Skip("kernel oracle is slow / needs root+tools; skipped in -short")
	}
	if os.Geteuid() != 0 {
		t.Skip("kernel oracle needs root for losetup/mount")
	}
	for _, bin := range []string{"btrfs", "mount", "umount", "losetup"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH; skipping kernel oracle", bin)
		}
	}

	dir := t.TempDir()
	img := filepath.Join(dir, "oracle.img")
	fs, err := Format(img, 24*1024*1024, FormatConfig{Label: "oracle"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	bfs := fs.(*btrfsFS)
	marker := bytes.Repeat([]byte("ORACLE-MARK-"), 80*1024)
	if err := fs.WriteFile("/big.bin", bytes.Repeat([]byte{'B'}, 12*1024*1024), 0o644); err != nil {
		fs.Close()
		t.Fatalf("big: %v", err)
	}
	if err := fs.WriteFile("/marker.bin", marker, 0o644); err != nil {
		fs.Close()
		t.Fatalf("marker: %v", err)
	}
	if err := fs.WriteFile("/big.bin", []byte("small\n"), 0o644); err != nil {
		fs.Close()
		t.Fatalf("overwrite: %v", err)
	}
	if err := bfs.Shrink(16 * 1024 * 1024); err != nil {
		fs.Close()
		t.Fatalf("Shrink: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// btrfs check must be clean.
	out, err := exec.Command("btrfs", "check", img).CombinedOutput()
	if err != nil {
		t.Fatalf("btrfs check failed: %v\n%s", err, out)
	}
	if bytes.Contains(out, []byte("ERROR")) || bytes.Contains(out, []byte("error(s) found")) {
		t.Fatalf("btrfs check reported errors:\n%s", out)
	}

	// Loop-mount and byte-compare.
	loopOut, err := exec.Command("losetup", "--find", "--show", img).CombinedOutput()
	if err != nil {
		t.Fatalf("losetup: %v\n%s", err, loopOut)
	}
	loop := strings.TrimSpace(string(loopOut))
	defer exec.Command("losetup", "-d", loop).Run()
	mnt := filepath.Join(dir, "mnt")
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		t.Fatalf("mkdir mnt: %v", err)
	}
	if mout, err := exec.Command("mount", "-o", "ro", loop, mnt).CombinedOutput(); err != nil {
		t.Fatalf("mount: %v\n%s", err, mout)
	}
	defer exec.Command("umount", mnt).Run()

	got, err := os.ReadFile(filepath.Join(mnt, "marker.bin"))
	if err != nil || !bytes.Equal(got, marker) {
		t.Fatalf("kernel-mounted relocated marker mismatch: err=%v len=%d want %d", err, len(got), len(marker))
	}
	gotBig, err := os.ReadFile(filepath.Join(mnt, "big.bin"))
	if err != nil || !bytes.Equal(gotBig, []byte("small\n")) {
		t.Fatalf("kernel-mounted big.bin mismatch: err=%v got=%q", err, gotBig)
	}
}
