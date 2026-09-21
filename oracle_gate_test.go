package filesystem_btrfs

// Gates for the kernel oracles, split by what each half actually needs.
//
// A kernel oracle here is three steps: build an image with our writer (pure
// Go), run `btrfs check` on it (btrfs-progs), then loop-mount it and compare
// bytes (the kernel). Only the third step needs privilege: losetup attaches a
// block device and mount attaches a filesystem. `btrfs check` reads an image
// FILE, and reading a file you own needs nothing at all.
//
// Every oracle used to be gated on `os.Geteuid() != 0` as one block, so the
// unprivileged btrfs-progs judge -- the one that validates OUR WRITES against
// upstream's own reader, which is the most valuable direction and the hardest
// to get -- could never run anywhere the mount could not. CI is unprivileged,
// so it never ran at all.
//
// Split in two:
//
//	requireBtrfsCheck   non-short + btrfs-progs. No privilege. Runs in CI.
//	requireLoopMount    root + mount/umount/losetup. Runs in a VM, not in CI.
//
// The mount half is a NAMED SUBTEST, so a run reports
//
//	--- PASS: TestReloc_KernelOracle
//	    --- SKIP: TestReloc_KernelOracle/kernel_mount
//
// rather than skipping the whole oracle and hiding that the check passed.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireBtrfsCheck gates the unprivileged half: the image build and the
// `btrfs check` that judges it.
//
// BTRFS_REQUIRE_PROGS=1 turns "btrfs-progs is not installed" from a skip into
// a failure. The lanes that install it set it, because a judge that can
// quietly not run is not a control -- and this one spent its whole life not
// running while the repository read as though it had a third-party witness.
func requireBtrfsCheck(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("kernel oracle builds a multi-megabyte image; skipped in -short")
	}
	if _, err := exec.LookPath("btrfs"); err != nil {
		if os.Getenv("BTRFS_REQUIRE_PROGS") != "" {
			t.Fatalf("BTRFS_REQUIRE_PROGS is set but btrfs-progs is not installed: %v", err)
		}
		t.Skipf("btrfs CLI not on PATH; install btrfs-progs to enable this oracle (got: %v)", err)
	}
}

// requireLoopMount gates the privileged half. It is called from inside the
// "kernel mount" subtest, so skipping it leaves the check half reported as the
// pass it is.
func requireLoopMount(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("loop-mounting needs root; the btrfs check half of this oracle has already run")
	}
	for _, bin := range []string{"mount", "umount", "losetup"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH; skipping the mount half (got: %v)", bin, err)
		}
	}
}

// btrfsCheckClean runs `btrfs check` on an image FILE and asserts it is clean.
// Unprivileged.
//
// btrfs-progs prints diagnostics without always reflecting them in the exit
// code, so the output is inspected as well as the status.
func btrfsCheckClean(t *testing.T, img string) {
	t.Helper()
	out, err := exec.Command("btrfs", "check", img).CombinedOutput()
	if err != nil {
		t.Fatalf("btrfs check failed: %v\n%s", err, out)
	}
	if bytes.Contains(out, []byte("ERROR")) || bytes.Contains(out, []byte("error(s) found")) {
		t.Fatalf("btrfs check reported errors:\n%s", out)
	}
}

// mountAndCompare loop-mounts img read-only and asserts every file in want is
// byte-identical. Privileged; call it inside the "kernel mount" subtest.
func mountAndCompare(t *testing.T, dir, img string, want map[string][]byte) {
	t.Helper()
	requireLoopMount(t)
	loopOut, err := exec.Command("losetup", "--find", "--show", img).CombinedOutput()
	if err != nil {
		t.Fatalf("losetup: %v\n%s", err, loopOut)
	}
	loop := strings.TrimSpace(string(loopOut))
	defer exec.Command("losetup", "-d", loop).Run()
	mnt := filepath.Join(dir, "mnt")
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if mout, err := exec.Command("mount", "-o", "ro", loop, mnt).CombinedOutput(); err != nil {
		t.Fatalf("mount: %v\n%s", err, mout)
	}
	defer exec.Command("umount", mnt).Run()
	for name, data := range want {
		got, err := os.ReadFile(filepath.Join(mnt, strings.TrimPrefix(name, "/")))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("kernel-mounted %s mismatch: err=%v len=%d want %d", name, err, len(got), len(data))
		}
	}
}

// checkThenMount is the whole oracle tail: judge the image with btrfs-progs
// here and now, then mount it in a named subtest that may skip on its own.
func checkThenMount(t *testing.T, dir, img string, want map[string][]byte) {
	t.Helper()
	btrfsCheckClean(t, img)
	t.Run("kernel mount", func(t *testing.T) {
		mountAndCompare(t, dir, img, want)
	})
}
