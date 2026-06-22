package filesystem_btrfs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// findBtrfsTool resolves a btrfs-progs tool, which on Linux CI may live in an
// sbin directory not on the default PATH.
func findBtrfsTool(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, d := range []string{"/usr/local/sbin", "/usr/sbin", "/sbin", "/usr/local/bin"} {
		c := filepath.Join(d, name)
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// canLoopMount reports whether the test can create a loopback mount: it needs to
// be root, or have passwordless sudo.
func canLoopMount() bool {
	if os.Geteuid() == 0 {
		return true
	}
	return exec.Command("sudo", "-n", "true").Run() == nil
}

// sudoSh runs a /bin/sh script with root privileges (directly when already root,
// otherwise via sudo).
func sudoSh(script string) ([]byte, error) {
	if os.Geteuid() == 0 {
		return exec.Command("sh", "-c", script).CombinedOutput()
	}
	return exec.Command("sudo", "sh", "-c", script).CombinedOutput()
}

// TestFormatKernelCompat formats a fresh image with this driver's Format,
// injects two files via WriteFile, then validates the result against the REAL
// Linux kernel and btrfs-progs:
//
//   - `btrfs check` must report a clean filesystem (no errors), proving the
//     superblock, chunk/root/extent/dev/csum/uuid/data-reloc trees and the
//     extent-tree accounting maintained on the write path are all consistent.
//   - the kernel must loop-mount the image read-only and read back both files'
//     contents byte-for-byte, proving open_ctree accepts our on-disk layout.
//
// The test is skipped when btrfs-progs or loop-mount privileges are unavailable,
// so the pure-Go suite still passes off-Linux / in unprivileged CI.
func TestFormatKernelCompat(t *testing.T) {
	btrfsBin := findBtrfsTool("btrfs")
	if btrfsBin == "" {
		t.Skip("btrfs-progs not available — skipping kernel cross-compat test")
	}
	if _, err := exec.LookPath("mount"); err != nil {
		t.Skip("mount not available — skipping kernel cross-compat test")
	}
	if !canLoopMount() {
		t.Skip("need root / passwordless sudo to loop-mount — skipping")
	}

	const helloContent = "hello-from-go-btrfs\n"
	const dataContent = "0123456789abcdef-deterministic-content"

	img := filepath.Join(t.TempDir(), "btrfs.img")
	fs, err := Format(img, 128<<20, FormatConfig{Label: "gofmtvol"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if err := fs.WriteFile("/hello.txt", []byte(helloContent), 0o644); err != nil {
		t.Fatalf("WriteFile /hello.txt: %v", err)
	}
	if err := fs.WriteFile("/data.bin", []byte(dataContent), 0o644); err != nil {
		t.Fatalf("WriteFile /data.bin: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// `btrfs check` must be clean.
	out, err := exec.Command(btrfsBin, "check", img).CombinedOutput()
	if err != nil {
		t.Fatalf("btrfs check failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "no error found") {
		t.Fatalf("btrfs check did not report a clean filesystem:\n%s", out)
	}

	// The kernel must mount it and read the files back.
	mnt := t.TempDir()
	helloOut := filepath.Join(t.TempDir(), "hello.out")
	dataOut := filepath.Join(t.TempDir(), "data.out")
	script := strings.Join([]string{
		"set -e",
		"mount -o loop,ro " + img + " " + mnt,
		"cat " + mnt + "/hello.txt > " + helloOut,
		"cat " + mnt + "/data.bin > " + dataOut,
		"umount " + mnt,
	}, " && ")
	if mout, merr := sudoSh(script); merr != nil {
		_, _ = sudoSh("umount " + mnt + " 2>/dev/null || true")
		t.Fatalf("kernel mount/read: %v\n%s", merr, mout)
	}

	if got, _ := os.ReadFile(helloOut); string(got) != helloContent {
		t.Errorf("kernel read /hello.txt = %q, want %q", got, helloContent)
	}
	if got, _ := os.ReadFile(dataOut); string(got) != dataContent {
		t.Errorf("kernel read /data.bin = %q, want %q", got, dataContent)
	}
}
