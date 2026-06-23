package filesystem_btrfs

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// dirsizeFixture is a 32 MiB image our writer formatted and then mutated to
// exercise every path that removes a directory entry:
//
//   - DeleteFile  : /gone-file.txt created then deleted (a /keep.txt remains).
//   - DeleteDir   : /gone-dir (with nested files + a subdir) created then
//     removed recursively.
//   - Rename      : /old-name.txt -> /new-name.txt (same parent), plus the
//     dst-overwrite path /winner.txt -> /victim.txt (overwriting an existing
//     file, which goes through removeInode for the clobbered destination).
//
// Each of those removals must shrink the parent directory's i_size by the same
// per-entry amount the matching insert added (name_len counted once for the
// DIR_ITEM and once for the DIR_INDEX). Before the fix, removeInode forgot to
// do this and `btrfs check` reported "root 5 inode 256 errors 200, dir isize
// wrong". The committed fixture is CLEAN: `btrfs check` finds no error and the
// kernel loop-mounts it with exactly {keep.txt, new-name.txt, victim.txt}
// (validated on cb-tpm-ubuntu, kernel 6.17 / btrfs-progs 6.6.3).
//
// Committed gzip-compressed so the emulated-arch CI (which cross-compiles the
// test binary) can still read it without a kernel.
//
//go:embed testdata/delete/dirsize.img.gz
var dirsizeFixture []byte

// decompressDirsizeFixture inflates the embedded gzip image into a temp file
// and returns its path.
func decompressDirsizeFixture(t *testing.T) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(dirsizeFixture))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gzip decode: %v", err)
	}
	path := filepath.Join(t.TempDir(), "dirsize.img")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// buildDirsizeImage replays the exact fixture mutation sequence against a fresh
// image at the given path. Shared by the runtime test and the fixture
// generator/kernel oracle so they stay in lockstep.
func buildDirsizeImage(t *testing.T, path string) {
	t.Helper()
	fs, err := Format(path, 32*1024*1024, FormatConfig{Label: "dirsize"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer func() {
		if err := fs.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}()

	// DeleteFile: create two root files, delete one.
	if err := fs.WriteFile("/keep.txt", []byte("kept\n"), 0o644); err != nil {
		t.Fatalf("write keep: %v", err)
	}
	if err := fs.WriteFile("/gone-file.txt", bytes.Repeat([]byte("X"), 4096), 0o644); err != nil {
		t.Fatalf("write gone-file: %v", err)
	}
	if err := fs.DeleteFile("/gone-file.txt"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	// DeleteDir: a populated subtree removed recursively.
	if err := fs.MkDir("/gone-dir", 0o755); err != nil {
		t.Fatalf("mkdir gone-dir: %v", err)
	}
	if err := fs.WriteFile("/gone-dir/inner.txt", []byte("inner\n"), 0o644); err != nil {
		t.Fatalf("write inner: %v", err)
	}
	if err := fs.MkDir("/gone-dir/sub", 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := fs.WriteFile("/gone-dir/sub/deep.txt", []byte("deep\n"), 0o644); err != nil {
		t.Fatalf("write deep: %v", err)
	}
	if err := fs.DeleteDir("/gone-dir"); err != nil {
		t.Fatalf("DeleteDir: %v", err)
	}

	// Rename (same parent) + dst-overwrite rename.
	if err := fs.WriteFile("/old-name.txt", []byte("renamed\n"), 0o644); err != nil {
		t.Fatalf("write old-name: %v", err)
	}
	if err := fs.Rename("/old-name.txt", "/new-name.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := fs.WriteFile("/victim.txt", []byte("old victim\n"), 0o644); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	if err := fs.WriteFile("/winner.txt", []byte("winner\n"), 0o644); err != nil {
		t.Fatalf("write winner: %v", err)
	}
	if err := fs.Rename("/winner.txt", "/victim.txt"); err != nil {
		t.Fatalf("Rename overwrite: %v", err)
	}
}

// dirsizeExpected is the post-mutation root listing the fixture must contain.
var dirsizeExpected = map[string]string{
	"/keep.txt":     "kept\n",
	"/new-name.txt": "renamed\n",
	"/victim.txt":   "winner\n",
}

// dirsizeAbsent are entries that must NOT survive the mutations.
var dirsizeAbsent = []string{
	"/gone-file.txt", "/gone-dir", "/gone-dir/inner.txt",
	"/old-name.txt", "/winner.txt",
}

// assertDirsizeImage opens an image with our own reader and asserts (a) the
// root directory's i_size equals the byte-sum of its surviving entries' names
// counted twice — the exact `btrfs check` "dir isize" accounting — and (b) the
// surviving/absent entries match. No kernel required, so it runs on every arch.
func assertDirsizeImage(t *testing.T, path string) {
	t.Helper()
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	bfs := fs.(*btrfsFS)

	// Expected root i_size = sum over surviving root entries of 2*len(name).
	var want int64
	for p := range dirsizeExpected {
		want += dirEntrySizeDelta(filepath.Base(p))
	}
	in, err := readInode(bfs.rwa, bfs.partOffset, bfs.sb, bfs.fsTreeRoot, rootDirObjID)
	if err != nil {
		t.Fatalf("readInode root: %v", err)
	}
	if int64(in.size) != want {
		t.Errorf("root dir i_size = %d, want %d (dir isize wrong)", in.size, want)
	}

	for p, body := range dirsizeExpected {
		got, err := fs.ReadFile(p)
		if err != nil || string(got) != body {
			t.Errorf("%s: err=%v got=%q want=%q", p, err, got, body)
		}
	}
	for _, p := range dirsizeAbsent {
		if _, err := fs.ReadFile(p); err == nil {
			t.Errorf("%s still present after removal", p)
		}
	}
}

// TestDirSize_FixtureReadback opens the committed fixture and asserts the root
// i_size accounting and surviving entries. Runs on every CI arch (no kernel),
// so it locks the on-disk accounting on big-endian s390x too.
func TestDirSize_FixtureReadback(t *testing.T) {
	assertDirsizeImage(t, decompressDirsizeFixture(t))
}

// TestDirSize_RuntimeRebuild rebuilds the same mutation sequence at runtime and
// asserts the freshly written image matches the accounting — proving the fix is
// in the live write path, not just baked into the committed fixture. Runs on
// every arch.
func TestDirSize_RuntimeRebuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rt-dirsize.img")
	buildDirsizeImage(t, path)
	assertDirsizeImage(t, path)
}

// TestDirSize_GenerateFixture regenerates testdata/delete/dirsize.img.gz from
// the live writer. Gated on GEN_FIXTURE so it never runs in normal CI; invoke
// with `GEN_FIXTURE=1 go test -run TestDirSize_GenerateFixture` after changing
// the mutation sequence, then re-validate with the kernel oracle.
func TestDirSize_GenerateFixture(t *testing.T) {
	if os.Getenv("GEN_FIXTURE") == "" {
		t.Skip("set GEN_FIXTURE=1 to regenerate the committed fixture")
	}
	raw := filepath.Join(t.TempDir(), "dirsize.img")
	buildDirsizeImage(t, raw)
	body, err := os.ReadFile(raw)
	if err != nil {
		t.Fatalf("read image: %v", err)
	}
	if err := os.MkdirAll("testdata/delete", 0o755); err != nil {
		t.Fatalf("mkdir testdata: %v", err)
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if _, err := zw.Write(body); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if err := os.WriteFile("testdata/delete/dirsize.img.gz", buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Logf("wrote testdata/delete/dirsize.img.gz (%d -> %d bytes)", len(body), buf.Len())
}

// TestDirSize_KernelOracle is the real kernel oracle: it builds a fresh image
// with the delete/deletedir/rename mutations, runs `btrfs check`, and asserts a
// clean result with NO "dir isize wrong", then loop-mounts and verifies the
// surviving entries. Skip-gated unless root + btrfs-progs + mount/umount/losetup
// are present (native Linux CI runners; macOS dev hosts and emulated short runs
// skip it). Validated on cb-tpm-ubuntu (kernel 6.17 / btrfs-progs 6.6.3).
func TestDirSize_KernelOracle(t *testing.T) {
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
	buildDirsizeImage(t, img)

	out, err := exec.Command("btrfs", "check", img).CombinedOutput()
	if err != nil {
		t.Fatalf("btrfs check failed: %v\n%s", err, out)
	}
	if bytes.Contains(out, []byte("dir isize wrong")) {
		t.Fatalf("btrfs check reported dir isize wrong:\n%s", out)
	}
	if bytes.Contains(out, []byte("ERROR")) || bytes.Contains(out, []byte("error(s) found")) {
		t.Fatalf("btrfs check reported errors:\n%s", out)
	}

	loopOut, err := exec.Command("losetup", "--find", "--show", img).CombinedOutput()
	if err != nil {
		t.Fatalf("losetup: %v\n%s", err, loopOut)
	}
	loop := string(bytes.TrimSpace(loopOut))
	defer exec.Command("losetup", "-d", loop).Run()
	mnt := filepath.Join(dir, "mnt")
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		t.Fatalf("mkdir mnt: %v", err)
	}
	if mout, err := exec.Command("mount", "-o", "ro", loop, mnt).CombinedOutput(); err != nil {
		t.Fatalf("mount: %v\n%s", err, mout)
	}
	defer exec.Command("umount", mnt).Run()

	for p, body := range dirsizeExpected {
		got, err := os.ReadFile(filepath.Join(mnt, filepath.Base(p)))
		if err != nil || string(got) != body {
			t.Fatalf("kernel-mounted %s mismatch: err=%v got=%q want=%q", p, err, got, body)
		}
	}
	for _, p := range dirsizeAbsent {
		if _, err := os.Stat(filepath.Join(mnt, filepath.Base(p))); err == nil {
			t.Errorf("kernel-mounted %s still present after removal", p)
		}
	}
}
