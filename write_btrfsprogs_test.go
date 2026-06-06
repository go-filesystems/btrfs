package filesystem_btrfs

// Cross-compatibility (write side): feed our writer's output through the
// real userland tools shipped by btrfs-progs (`btrfs check`,
// `btrfs inspect-internal dump-super`) and assert they accept it.
//
// Read-side cross-compat is covered by raid_fixtures_test.go (mkfs.btrfs
// images consumed by our reader). This file completes the loop the other
// way: image produced by us, validated by upstream.
//
// The btrfs(8) CLI ships only on Linux distributions (Fedora, Debian,
// Arch, ...) as btrfs-progs. macOS / *BSD don't have it, so these tests
// skip-gate cleanly when the binary is absent — they are not a hard CI
// requirement, but they fire automatically on any host where the tool is
// installed.

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireBtrfsProgs skip-gates the test when the `btrfs` CLI (btrfs-progs)
// is not on PATH. Returns the resolved absolute path on success.
func requireBtrfsProgs(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("btrfs")
	if err != nil {
		t.Skipf("btrfs CLI not found on PATH; install btrfs-progs to enable this cross-compat test (got: %v)", err)
	}
	return p
}

// runBtrfs executes the btrfs CLI with the given args and returns combined
// stdout+stderr along with the exit error (if any).
func runBtrfs(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	bin := requireBtrfsProgs(t)
	cmd := exec.Command(bin, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.Bytes(), err
}

// TestWriteThenBtrfsCheck builds a freshly formatted image with our writer,
// writes a small file into it, then validates the result with `btrfs check
// --readonly`. We assert both a clean exit (0) and the absence of any
// "ERROR" lines in the output — btrfs-progs prints diagnostics on stdout
// without always reflecting them in the exit code for read-only runs.
//
// Skip-gated when btrfs-progs is unavailable (e.g. macOS dev hosts).
func TestWriteThenBtrfsCheck(t *testing.T) {
	requireBtrfsProgs(t) // skip early if absent

	img := filepath.Join(t.TempDir(), "writer-out.img")

	// Use a slightly larger image (8 MiB) than the package default to give
	// btrfs check enough room to be happy with its sanity heuristics on
	// chunk/data layout. Still tiny by real-world standards.
	const size = 8 * 1024 * 1024

	fs, err := Format(img, size, FormatConfig{Label: "compat-write"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if err := fs.WriteFile("/hello.txt", []byte("hello from go-filesystems/btrfs writer\n"), 0o644); err != nil {
		fs.Close()
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	out, err := runBtrfs(t, "check", "--readonly", img)
	if err != nil {
		t.Fatalf("btrfs check --readonly exited non-zero: %v\noutput:\n%s", err, out)
	}
	// Scan output for ERROR lines that btrfs-progs may emit while still
	// returning exit 0 in --readonly mode.
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "ERROR") {
			t.Errorf("btrfs check reported error: %s\nfull output:\n%s", line, out)
			break
		}
	}
}

// TestWriteThenBtrfsDumpSuper validates that the primary superblock written
// by our Format() parses cleanly through `btrfs inspect-internal dump-super
// -f`. The "-f" flag forces a dump even for filesystems the tool deems
// incomplete, but a clean exit + non-empty "magic" field still proves the
// header layout is byte-correct.
//
// Skip-gated when btrfs-progs is unavailable.
func TestWriteThenBtrfsDumpSuper(t *testing.T) {
	requireBtrfsProgs(t)

	img := filepath.Join(t.TempDir(), "writer-super.img")
	const size = 8 * 1024 * 1024

	fs, err := Format(img, size, FormatConfig{Label: "compat-super"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	out, err := runBtrfs(t, "inspect-internal", "dump-super", "-f", img)
	if err != nil {
		t.Fatalf("btrfs inspect-internal dump-super exited non-zero: %v\noutput:\n%s", err, out)
	}
	// btrfs-progs prints "magic			_BHRfS_M" on a clean superblock.
	if !bytes.Contains(out, []byte("_BHRfS_M")) {
		t.Errorf("dump-super output missing expected btrfs magic; got:\n%s", out)
	}
	// And the label should round-trip.
	if !bytes.Contains(out, []byte("compat-super")) {
		t.Errorf("dump-super output missing label \"compat-super\"; got:\n%s", out)
	}
}
