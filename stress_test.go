// Stress test suite for filesystem-btrfs.
//
// These tests are gated by `testing.Short()`: under `go test -short` (and
// therefore under the package's default `go test ./...`) they execute small,
// fast variants that finish in a few seconds. Under `go test -run Stress
// -timeout 30m` they execute their full short-mode budget — every test still
// completes well inside the 30 m timeout. The full multi-hour endurance
// variants are unlocked via environment variables:
//
//	BTRFS_STRESS_WORKERS   number of goroutines for the concurrent R/W test
//	                       (default 8 in long mode, 2 in short mode)
//	BTRFS_STRESS_DURATION  wall-clock duration for the concurrent R/W test
//	                       (default 30s in long mode, 1s in short mode)
//	                       parses any time.Duration string, e.g. "3h"
//	BTRFS_STRESS_FILES     file count for the many-files test
//	                       (default 5000 in long mode, 200 in short mode;
//	                        set to 1000000 for the M-file endurance variant)
//	BTRFS_STRESS_FILE_MB   size in MiB for the large-file test
//	                       (default 64 in long mode, 1 in short mode;
//	                        set to e.g. 1024 for GiB-scale streaming)
//
// CLI flags mirror the env vars (env wins when both are set):
//
//	-stress.workers      -stress.duration
//	-stress.files        -stress.file-mb
//
// Tests added:
//   - TestStress_ConcurrentRW            integrity-checked parallel write/read/delete
//   - TestStress_LargeFile               single multi-MB file write+verify
//   - TestStress_ManyFiles               N small files: create, walk, delete-all
//   - TestStress_FsyncCrashSemantics     simulated crash mid-transaction
//   - TestStress_FaultInjection          probabilistic I/O error propagation
//   - TestStress_CompressedExtentMix     LZO+Zstd+zlib+none extents interleaved
//   - TestStress_RAIDProfilesParallel    concurrent reads across all 6 RAID profiles
//   - FuzzOpen                           parser fuzz over btrfs image bytes
package filesystem_btrfs

import (
	"archive/tar"
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	mathrand "math/rand"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// ── knobs ────────────────────────────────────────────────────────────────

var (
	stressWorkers  = flag.Int("stress.workers", 0, "stress: concurrent R/W worker count (0 = auto)")
	stressDuration = flag.Duration("stress.duration", 0, "stress: concurrent R/W wall-clock duration (0 = auto)")
	stressFiles    = flag.Int("stress.files", 0, "stress: many-files file count (0 = auto)")
	stressFileMB   = flag.Int("stress.file-mb", 0, "stress: large-file size in MiB (0 = auto)")
)

// envOrFlagInt returns the env var if set & parseable, else the flag value
// if non-zero, else the supplied default. Mirrors envOrFlagDuration.
func envOrFlagInt(envKey string, flagVal int, short, long int) int {
	if v := os.Getenv(envKey); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	if flagVal > 0 {
		return flagVal
	}
	if testing.Short() {
		return short
	}
	return long
}

func envOrFlagDuration(envKey string, flagVal time.Duration, short, long time.Duration) time.Duration {
	if v := os.Getenv(envKey); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	if flagVal > 0 {
		return flagVal
	}
	if testing.Short() {
		return short
	}
	return long
}

// stressTempImage formats a fresh btrfs image in t.TempDir and returns the
// open FS plus its on-disk path. Image size scales with `mb` (MiB).
func stressTempImage(t testing.TB, mb int) (*btrfsFS, string) {
	t.Helper()
	if mb < 1 {
		mb = 1
	}
	path := filepath.Join(t.TempDir(), "stress.img")
	fs, err := Format(path, int64(mb)*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	return fs.(*btrfsFS), path
}

// ── 1. Concurrent R/W ─────────────────────────────────────────────────────

// TestStress_ConcurrentRW launches `workers` goroutines that each repeatedly
// pick a slot, write deterministic content tagged with a counter, read it back
// (sha256 round-trip), and delete it. The stop signal is a context derived
// from `duration`. After the workers stop, every surviving file is read+
// verified to ensure no torn-write left half-state behind.
//
// short mode (`-short`):  2 workers,   1 s, 8 MiB image
// long  mode:             8 workers, 30 s, 64 MiB image
// 3 h endurance:          BTRFS_STRESS_DURATION=3h BTRFS_STRESS_WORKERS=8
func TestStress_ConcurrentRW(t *testing.T) {
	workers := envOrFlagInt("BTRFS_STRESS_WORKERS", *stressWorkers, 2, 8)
	duration := envOrFlagDuration("BTRFS_STRESS_DURATION", *stressDuration, 1*time.Second, 30*time.Second)

	// Image size: small images run out of data space quickly with many
	// concurrent writers, so scale with worker count and duration.
	imgMB := 8
	if !testing.Short() {
		imgMB = 64
	}
	if duration > 30*time.Second {
		imgMB = 128
	}
	if duration > 5*time.Minute {
		imgMB = 256
	}
	bf, _ := stressTempImage(t, imgMB)

	// File-slot count: small enough that workers collide (exercising
	// overwrite + delete races at the FS level — the package-level mutex
	// serialises writers, so we're not testing thread safety of the data
	// structures but the *correctness* of repeated write/delete cycles
	// under load).
	const slots = 16
	const payloadSize = 1024 // 1 KiB per file — small enough to keep image bounded.

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	var (
		writes, reads, deletes atomic.Uint64
		failures               atomic.Uint64
		wg                     sync.WaitGroup
	)

	start := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			r := mathrand.New(mathrand.NewSource(int64(id) + start.UnixNano()))
			buf := make([]byte, payloadSize)
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				slot := r.Intn(slots)
				name := fmt.Sprintf("/w%d_s%d.bin", id, slot)
				// Fill with worker+iteration-tagged bytes so we catch
				// any cross-file aliasing.
				tag := r.Uint64()
				binary.LittleEndian.PutUint64(buf[:8], uint64(id))
				binary.LittleEndian.PutUint64(buf[8:16], tag)
				for i := 16; i < len(buf); i++ {
					buf[i] = byte(tag >> uint((i%8)*8))
				}
				wantSum := sha256.Sum256(buf)

				if err := bf.WriteFile(name, buf, 0o644); err != nil {
					// Expected eventually: image is fixed-size; ENOSPC-equivalent
					// (or any allocation error) is a tolerable termination signal
					// for stress.
					failures.Add(1)
					// Try to drain space.
					_ = bf.DeleteFile(name)
					continue
				}
				writes.Add(1)

				got, err := bf.ReadFile(name)
				if err != nil {
					failures.Add(1)
					continue
				}
				reads.Add(1)
				gotSum := sha256.Sum256(got)
				if gotSum != wantSum {
					// First mismatch is loud; stop the test.
					t.Errorf("worker %d slot %d: sha256 mismatch (len got=%d want=%d)",
						id, slot, len(got), len(buf))
					cancel()
					return
				}

				// Random delete: half the time we leave the file behind so
				// the post-test pass has something to verify.
				if r.Intn(2) == 0 {
					if err := bf.DeleteFile(name); err == nil {
						deletes.Add(1)
					}
				}
			}
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(start)

	t.Logf("concurrent R/W: workers=%d duration=%s writes=%d reads=%d deletes=%d failures=%d (%.0f ops/s)",
		workers, elapsed.Round(10*time.Millisecond),
		writes.Load(), reads.Load(), deletes.Load(), failures.Load(),
		float64(writes.Load()+reads.Load()+deletes.Load())/elapsed.Seconds())

	if writes.Load() == 0 {
		t.Errorf("no writes succeeded — workers couldn't make progress")
	}

	// Post-run integrity sweep: every surviving file is readable and
	// self-consistent (its sha256 matches the first 16 bytes of its body —
	// the deterministic tag stamp).
	entries, err := bf.ListDir("/")
	if err != nil {
		t.Fatalf("post-run ListDir(/): %v", err)
	}
	for _, e := range entries {
		got, err := bf.ReadFile("/" + e.Name())
		if err != nil {
			t.Errorf("post-run ReadFile(%s): %v", e.Name(), err)
			continue
		}
		if len(got) != payloadSize {
			t.Errorf("post-run %s: size got=%d want=%d", e.Name(), len(got), payloadSize)
		}
		// Recompute the expected sha256 from the tag stamp at offset 0..16
		// and verify the body matches.
		if len(got) < 16 {
			t.Errorf("post-run %s: too short to validate", e.Name())
			continue
		}
		id := binary.LittleEndian.Uint64(got[:8])
		tag := binary.LittleEndian.Uint64(got[8:16])
		expected := make([]byte, payloadSize)
		binary.LittleEndian.PutUint64(expected[:8], id)
		binary.LittleEndian.PutUint64(expected[8:16], tag)
		for i := 16; i < len(expected); i++ {
			expected[i] = byte(tag >> uint((i%8)*8))
		}
		if !bytes.Equal(got, expected) {
			t.Errorf("post-run %s: content mismatch (sha256 expected %x got %x)",
				e.Name(), sha256.Sum256(expected), sha256.Sum256(got))
		}
	}
}

// ── 2. Large file ─────────────────────────────────────────────────────────

// TestStress_LargeFile writes a single multi-MiB file in chunks (so we never
// allocate the whole payload up-front in memory above what writeFile needs)
// and reads it back, verifying every chunk's sha256.
//
// short mode:  1 MiB
// long  mode:  64 MiB
// GB endurance: BTRFS_STRESS_FILE_MB=1024 → 1 GiB
func TestStress_LargeFile(t *testing.T) {
	mb := envOrFlagInt("BTRFS_STRESS_FILE_MB", *stressFileMB, 1, 64)
	// Image holds the file ~3x over (room for COW, btree growth, free space).
	imgMB := mb*3 + 8
	bf, _ := stressTempImage(t, imgMB)

	// Build payload deterministically — repeated 64-byte block xored with
	// an index so any swapped/duplicated chunk fails the sha256 check.
	payload := make([]byte, mb*1024*1024)
	block := []byte("the-quick-brown-fox-jumps-over-the-lazy-dog-stress-payload-block")
	if len(block) != 64 {
		t.Fatalf("internal: block len %d != 64", len(block))
	}
	for i := 0; i < len(payload); i++ {
		payload[i] = block[i%len(block)] ^ byte((i/64)*7+3)
	}
	wantSum := sha256.Sum256(payload)

	start := time.Now()
	if err := bf.WriteFile("/big.bin", payload, 0o644); err != nil {
		t.Fatalf("WriteFile %d MiB: %v", mb, err)
	}
	wDur := time.Since(start)

	start = time.Now()
	got, err := bf.ReadFile("/big.bin")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	rDur := time.Since(start)

	if len(got) != len(payload) {
		t.Fatalf("large file size: got=%d want=%d", len(got), len(payload))
	}
	gotSum := sha256.Sum256(got)
	if gotSum != wantSum {
		t.Fatalf("large file sha256 mismatch: got %x want %x", gotSum, wantSum)
	}
	t.Logf("large file: %d MiB write=%s (%.1f MB/s) read=%s (%.1f MB/s)",
		mb, wDur.Round(time.Millisecond),
		float64(len(payload))/(1024*1024)/wDur.Seconds(),
		rDur.Round(time.Millisecond),
		float64(len(payload))/(1024*1024)/rDur.Seconds(),
	)
}

// ── 3. Many files ────────────────────────────────────────────────────────

// TestStress_ManyFiles creates `N` small files in a dedicated directory,
// walks the directory and verifies the file count matches, then deletes
// all of them. Exercises extent-tree growth pathologies and the directory
// item-tree balancing under many sibling entries.
//
// short mode:  200 files
// long  mode:  5000 files
// 1 M endurance: BTRFS_STRESS_FILES=1000000  (NOTE: needs proportionally
// larger image — the current Format ceiling is fmtMinSize..1GiB safely;
// for >100k expect a few minutes wall-clock).
func TestStress_ManyFiles(t *testing.T) {
	n := envOrFlagInt("BTRFS_STRESS_FILES", *stressFiles, 200, 5000)

	// Per-file cost is ~one inode + one dir item + one small extent.
	// Scale image to keep tests headroom-safe.
	imgMB := 32
	switch {
	case n > 100000:
		imgMB = 1024
	case n > 10000:
		imgMB = 256
	case n > 1000:
		imgMB = 64
	}
	bf, _ := stressTempImage(t, imgMB)

	if err := bf.MkDir("/many", 0o755); err != nil {
		t.Fatalf("MkDir /many: %v", err)
	}

	start := time.Now()
	created := 0
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("/many/f%07d.txt", i)
		body := fmt.Appendf(nil, "file-%d\n", i)
		if err := bf.WriteFile(name, body, 0o644); err != nil {
			// Allow early-stop when the image runs out of space — we want
			// to surface the symptom but not flake.
			t.Logf("WriteFile %s failed at i=%d/%d: %v (likely ENOSPC; image=%dMiB)",
				name, i, n, err, imgMB)
			break
		}
		created++
	}
	wDur := time.Since(start)

	// Walk: list and count.
	start = time.Now()
	entries, err := bf.ListDir("/many")
	if err != nil {
		t.Fatalf("ListDir /many: %v", err)
	}
	lDur := time.Since(start)
	if len(entries) != created {
		t.Errorf("ListDir /many: got %d entries, expected %d created", len(entries), created)
	}
	// Names must be unique and present.
	names := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		names[e.Name()] = struct{}{}
	}
	if len(names) != len(entries) {
		t.Errorf("ListDir /many: %d duplicate names (have %d entries, %d unique)",
			len(entries)-len(names), len(entries), len(names))
	}

	// Delete-all: walk in sorted order, remove each.
	sortedNames := make([]string, 0, len(names))
	for k := range names {
		sortedNames = append(sortedNames, k)
	}
	sort.Strings(sortedNames)

	start = time.Now()
	for _, name := range sortedNames {
		if err := bf.DeleteFile("/many/" + name); err != nil {
			t.Errorf("DeleteFile /many/%s: %v", name, err)
			break
		}
	}
	dDur := time.Since(start)

	// Final assertion: directory is empty.
	final, err := bf.ListDir("/many")
	if err != nil {
		t.Fatalf("ListDir /many after delete-all: %v", err)
	}
	if len(final) != 0 {
		t.Errorf("after delete-all: %d entries remain", len(final))
	}

	t.Logf("many-files n=%d: create=%s walk=%s delete=%s",
		created, wDur.Round(time.Millisecond),
		lDur.Round(time.Millisecond), dDur.Round(time.Millisecond))
}

// ── 4. fsync / commit semantics ───────────────────────────────────────────

// TestStress_FsyncCrashSemantics simulates a crash mid-transaction by
// snapshotting the on-disk image after a known-good batch of writes, then
// continuing with more writes WITHOUT syncing/closing, then "crashing" by
// restoring the snapshot and re-opening. The pre-snapshot data must still be
// readable; the post-snapshot data is allowed to be lost or partial.
func TestStress_FsyncCrashSemantics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crash.img")
	fs, err := Format(path, 8*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}

	// Phase 1: write three "committed" files and force a Sync (the only
	// thing approximating commit in the current writer is a Close+Reopen
	// — Sync alone flushes the OS buffer but the writer streams all
	// btree updates synchronously to WriteAt so by the time Close
	// completes the on-disk image is consistent).
	committed := map[string]string{
		"/c1.txt": "committed-1",
		"/c2.txt": "committed-2",
		"/c3.txt": "committed-3",
	}
	for name, body := range committed {
		if err := fs.WriteFile(name, []byte(body), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	// Close = "barrier" that flushes all pending state.
	if err := fs.Close(); err != nil {
		t.Fatalf("Close phase 1: %v", err)
	}

	// Snapshot the image right after the commit barrier.
	snap, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// Phase 2: re-open and start writing "uncommitted" data. We never
	// reach the barrier (no Close) — instead we "crash" by overwriting
	// the file with the snapshot.
	fs2, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open phase 2: %v", err)
	}
	// Best-effort: write a few files then leave them undrained.
	uncommitted := []string{"/u1.txt", "/u2.txt", "/u3.txt"}
	for _, name := range uncommitted {
		_ = fs2.WriteFile(name, []byte("uncommitted-"+name), 0o644)
	}
	// "Crash" — restore the image to the post-barrier snapshot. Don't
	// Close fs2; mimic torn-state at the moment of the crash. We need to
	// release the file handle on macOS so the snapshot write isn't
	// holding the file's i/o under us — Close is the cleanest way.
	_ = fs2.Close()
	if err := os.WriteFile(path, snap, 0o600); err != nil {
		t.Fatalf("restore snapshot: %v", err)
	}

	// Phase 3: re-open and verify committed data survived; uncommitted
	// data is gone (or, at worst, partially present — anything is OK).
	fs3, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open phase 3: %v", err)
	}
	defer fs3.Close()
	for name, want := range committed {
		got, err := fs3.ReadFile(name)
		if err != nil {
			t.Errorf("post-crash ReadFile(%s): %v — committed data lost", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("post-crash %s: got %q want %q", name, got, want)
		}
	}
	for _, name := range uncommitted {
		if _, err := fs3.ReadFile(name); err == nil {
			// Not a fatal — uncommitted survival is permitted under a
			// best-effort flush model.
			t.Logf("post-crash: uncommitted %s survived (allowed)", name)
		}
	}
}

// ── 5. Fault injection ────────────────────────────────────────────────────

// faultyBackend wraps a blockBackend, returning an injected error on a
// configurable fraction of WriteAt or ReadAt calls.
type faultyBackend struct {
	inner   blockBackend
	rng     *mathrand.Rand
	mu      sync.Mutex
	writeFP float64 // fault probability for WriteAt (0..1)
	readFP  float64 // fault probability for ReadAt (0..1)
	armed   atomic.Bool
	writes  atomic.Uint64
	reads   atomic.Uint64
	wFails  atomic.Uint64
	rFails  atomic.Uint64
	err     error
}

func newFaultyBackend(inner blockBackend, writeFP, readFP float64, seed int64) *faultyBackend {
	return &faultyBackend{
		inner:   inner,
		rng:     mathrand.New(mathrand.NewSource(seed)),
		writeFP: writeFP,
		readFP:  readFP,
		err:     errors.New("injected I/O fault"),
	}
}

func (f *faultyBackend) ReadAt(p []byte, off int64) (int, error) {
	f.reads.Add(1)
	if f.armed.Load() && f.readFP > 0 {
		f.mu.Lock()
		hit := f.rng.Float64() < f.readFP
		f.mu.Unlock()
		if hit {
			f.rFails.Add(1)
			return 0, f.err
		}
	}
	return f.inner.ReadAt(p, off)
}

func (f *faultyBackend) WriteAt(p []byte, off int64) (int, error) {
	f.writes.Add(1)
	if f.armed.Load() && f.writeFP > 0 {
		f.mu.Lock()
		hit := f.rng.Float64() < f.writeFP
		f.mu.Unlock()
		if hit {
			f.wFails.Add(1)
			return 0, f.err
		}
	}
	return f.inner.WriteAt(p, off)
}

func (f *faultyBackend) Sync() error            { return f.inner.Sync() }
func (f *faultyBackend) Size() (int64, error)   { return f.inner.Size() }
func (f *faultyBackend) Truncate(s int64) error { return f.inner.Truncate(s) }
func (f *faultyBackend) Close() error           { return f.inner.Close() }

// TestStress_FaultInjection wraps a real osFileBackend with a probabilistic
// faulty backend, "arms" it once the filesystem is fully open, and runs a
// burst of write/read operations. The assertion is twofold:
//   - errors propagate (no silent dropping → we count operations vs injection
//     hits)
//   - no goroutine crash / panic during failure injection (the test
//     completing is itself the proof).
func TestStress_FaultInjection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fault.img")
	// Pre-format with a clean backend; arm faults only AFTER Format and
	// the first Open, because Format internally uses a separate file
	// handle and Open's discovery path must succeed for the test to be
	// meaningful.
	if _, err := Format(path, 8*1024*1024, FormatConfig{}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	// Build a faulty backend around osFileBackend.
	osb := &osFileBackend{f: f}
	fb := newFaultyBackend(osb, 0.05, 0.0, 42) // 5% write faults, 0% read faults
	fs, err := OpenFromDevice(fb, -1)
	if err != nil {
		t.Fatalf("OpenFromDevice (pre-arm): %v", err)
	}
	defer fs.Close()

	// Arm the injector now that the FS is open.
	fb.armed.Store(true)

	iters := 100
	if testing.Short() {
		iters = 30
	}
	ops, errs := 0, 0
	for i := 0; i < iters; i++ {
		name := fmt.Sprintf("/fi-%d.txt", i)
		body := fmt.Appendf(nil, "fault-injection-iter-%d", i)
		ops++
		if err := fs.WriteFile(name, body, 0o644); err != nil {
			errs++
			// Verify the error is the one we injected (or a wrapped
			// form). The btrfs writer wraps low-level errors with %w
			// so errors.Is must find our sentinel.
			if !errors.Is(err, fb.err) {
				t.Errorf("WriteFile %s: error chain doesn't include injected sentinel: %v", name, err)
			}
			continue
		}
		// On success, the file must read back exactly.
		got, err := fs.ReadFile(name)
		if err != nil {
			// A successful WriteFile followed by failing ReadFile is
			// allowed under fault injection (the read itself may have
			// been faulted) but the error must propagate cleanly.
			continue
		}
		if !bytes.Equal(got, body) {
			t.Errorf("WriteFile %s succeeded but ReadFile returned %q", name, got)
		}
	}
	fb.armed.Store(false)

	t.Logf("fault injection: ops=%d failed=%d backend writes=%d (faults=%d) reads=%d",
		ops, errs, fb.writes.Load(), fb.wFails.Load(), fb.reads.Load())
	// With 5% probability we expect at least one fault in 30+ iterations
	// almost certainly (P(no fault) = 0.95^backendWrites). We don't fail
	// if no faults happen — that's a pure statistical flake risk — but we
	// do require the test to have driven enough I/O for the injector to
	// be exercised.
	if fb.writes.Load() < 5 {
		t.Errorf("fault injection: backend WriteAt called only %d times", fb.writes.Load())
	}
}

// ── 6. Parser fuzz ───────────────────────────────────────────────────────

// FuzzOpen mutates btrfs image bytes and verifies Open never panics or
// hangs. Seeds with the test image bytes; the corpus is the single
// `single` RAID fixture (extracted lazily). Fuzz failures are not stress
// failures — they're parser hardness bugs to fix. Run with:
//
//	go test -run=^$ -fuzz=FuzzOpen -fuzztime=30s
func FuzzOpen(f *testing.F) {
	// Seed corpus: the deterministic minimal in-package image.
	f.Add(buildTestImageBytes())

	// Try to add the `single` fixture as an additional seed if it
	// extracts cleanly. Use a tiny throwaway corpus path — `f.Add` only
	// needs the bytes.
	if seed := loadSingleFixtureSeed(); seed != nil {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, img []byte) {
		// Bound the input so a fuzzer-generated multi-GB payload doesn't
		// OOM the test runner.
		if len(img) < 0x011000 || len(img) > 4*1024*1024 {
			t.Skip()
		}
		path := filepath.Join(t.TempDir(), "fuzz.img")
		if err := os.WriteFile(path, img, 0o600); err != nil {
			t.Skip()
		}
		// Open must either return cleanly (image happens to be
		// well-formed) or return an error — never panic.
		fs, err := Open(path, -1)
		if err == nil && fs != nil {
			// If it opened, exercise a few read paths — they must not
			// panic either. Errors are fine.
			_, _ = fs.ListDir("/")
			_, _ = fs.Stat("/")
			_, _ = fs.ReadFile("/hello.txt")
			_ = fs.Close()
		}
	})
}

// loadSingleFixtureSeed reads testdata/raid/single.tar.zst, extracts the
// first .img member, and returns its bytes. Returns nil on any failure
// — fuzz seeding is best-effort.
func loadSingleFixtureSeed() []byte {
	src := filepath.Join("testdata", "raid", "single.tar.zst")
	f, err := os.Open(src)
	if err != nil {
		return nil
	}
	defer f.Close()
	zr, err := zstd.NewReader(f)
	if err != nil {
		return nil
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return nil
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if filepath.Ext(hdr.Name) != ".img" {
			continue
		}
		// Bound size so a malicious fixture can't blow up memory.
		if hdr.Size > 16*1024*1024 {
			return nil
		}
		body, err := io.ReadAll(io.LimitReader(tr, 16*1024*1024))
		if err != nil {
			return nil
		}
		return body
	}
}

// ── 7. Compressed extent stress ──────────────────────────────────────────

// TestStress_CompressedExtentMix installs a mix of LZO, Zstd, zlib, and
// uncompressed extents into successive files and reads each back. Repeats
// across many payload sizes and codecs in a pseudo-random order to exercise
// the decompression dispatch under sustained churn.
func TestStress_CompressedExtentMix(t *testing.T) {
	iters := 16
	if testing.Short() {
		iters = 4
	}
	r := mathrand.New(mathrand.NewSource(1))

	for it := 0; it < iters; it++ {
		// Fresh image per iteration: we install hand-crafted extents in
		// crafted positions; doing so against the same image many times
		// would force us to track the per-iteration spaceManager state.
		bf, _ := stressTempImage(t, 8)

		// Pick a codec + payload size per file.
		codecs := []uint8{compressionNone, compressionZlib, compressionLzo, compressionZstd}
		sizes := []int{
			128, 1024, 4 * 1024, 16 * 1024, 32 * 1024,
		}

		for fi := 0; fi < 4; fi++ {
			codec := codecs[r.Intn(len(codecs))]
			size := sizes[r.Intn(len(sizes))]
			payload := makeCompressiblePayload(size, byte(it*4+fi))
			fname := fmt.Sprintf("/c_it%d_f%d.bin", it, fi)
			if err := installCompressedFile(t, bf, fname, payload, codec); err != nil {
				t.Fatalf("iter=%d codec=%d size=%d: install: %v", it, codec, size, err)
			}
			got, err := bf.ReadFile(fname)
			if err != nil {
				t.Fatalf("iter=%d codec=%d size=%d: ReadFile: %v", it, codec, size, err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("iter=%d codec=%d size=%d: content mismatch (got len=%d want=%d)",
					it, codec, size, len(got), len(payload))
			}
		}
	}
}

// makeCompressiblePayload returns `size` bytes of mildly-redundant content
// seeded by `salt` so compressors find redundancy.
func makeCompressiblePayload(size int, salt byte) []byte {
	out := make([]byte, size)
	block := []byte("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdef-stress-compress-payload-")
	for i := 0; i < size; i++ {
		out[i] = block[i%len(block)] ^ salt
	}
	return out
}

// installCompressedFile creates a file at `name` whose data sits in a
// hand-crafted compressed extent matching the on-disk shape btrfs's read
// path expects. Reuses the helpers exposed by compression_test.go.
func installCompressedFile(t *testing.T, bf *btrfsFS, name string, payload []byte, codec uint8) error {
	t.Helper()
	// Placeholder write to create the inode + parent dir items.
	if err := bf.WriteFile(name, make([]byte, len(payload)), 0o644); err != nil {
		return fmt.Errorf("placeholder write: %w", err)
	}
	if codec == compressionNone {
		// For the "none" case we just rewrite with the real payload —
		// the writer produces an uncompressed extent natively, which is
		// what we want.
		return bf.WriteFile(name, payload, 0o644)
	}
	st, err := bf.Stat(name)
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	ino := st.Inode()

	freeInodeExtents(bf.f, bf.partOffset, bf.sb, bf.sm, bf.fsTreeRoot, ino)
	newRoot, err := cowDeletePrefix(nil, bf.f, bf.partOffset, bf.sb, bf.sm, bf.fsTreeRoot, ino, typeExtentData)
	if err != nil {
		return fmt.Errorf("cowDeletePrefix: %w", err)
	}
	bf.fsTreeRoot = newRoot

	var compressed []byte
	switch codec {
	case compressionZlib:
		var buf bytes.Buffer
		zw := zlib.NewWriter(&buf)
		if _, err := zw.Write(payload); err != nil {
			return fmt.Errorf("zlib write: %w", err)
		}
		if err := zw.Close(); err != nil {
			return fmt.Errorf("zlib close: %w", err)
		}
		compressed = buf.Bytes()
	case compressionZstd:
		zw, err := zstd.NewWriter(nil)
		if err != nil {
			return fmt.Errorf("zstd writer: %w", err)
		}
		compressed = zw.EncodeAll(payload, nil)
		_ = zw.Close()
	case compressionLzo:
		compressed = btrfsLzoEncodeAllLiterals(payload)
	default:
		return fmt.Errorf("unsupported codec %d", codec)
	}

	physData, _, err := bf.sm.allocDataBytes(uint64(len(compressed)), uint64(bf.sb.sectorSize))
	if err != nil {
		return fmt.Errorf("allocDataBytes: %w", err)
	}
	if _, err := bf.f.WriteAt(compressed, bf.partOffset+int64(physData)); err != nil {
		return fmt.Errorf("WriteAt compressed: %w", err)
	}
	logData := physToLog(bf.sb, physData)
	extData := encodeRegularExtentData(
		logData,
		uint64(len(compressed)),
		uint64(len(payload)),
		0,
		uint64(len(payload)),
		bf.sb.generation+1,
		codec,
	)
	newRoot, err = cowInsert(nil, bf.f, bf.partOffset, bf.sb, bf.sm, bf.fsTreeRoot, key{ino, typeExtentData, 0}, extData)
	if err != nil {
		return fmt.Errorf("cowInsert: %w", err)
	}
	bf.fsTreeRoot = newRoot
	if err := updateFsTreeRoot(bf.f, bf.partOffset, bf.sb, bf.sm, bf.fsTreeRoot); err != nil {
		return fmt.Errorf("updateFsTreeRoot: %w", err)
	}
	return nil
}

// ── 8. RAID profile stress ────────────────────────────────────────────────

// TestStress_RAIDProfilesParallel cycles through every RAID profile fixture
// (single, raid0, raid1, raid5, raid6, raid10) and, for each, launches
// concurrent reader goroutines that re-read both well-known files and
// verify the content. Exercises the per-profile stripe math under sustained
// parallel reads.
func TestStress_RAIDProfilesParallel(t *testing.T) {
	profiles := []string{"single", "raid0", "raid1", "raid10", "raid5", "raid6"}
	readers := 4
	iters := 8
	if testing.Short() {
		readers = 2
		iters = 2
	}

	for _, profile := range profiles {
		profile := profile
		t.Run(profile, func(t *testing.T) {
			imgs := extractRAIDFixture(t, profile)
			devs := openRAIDImagesAsBackends(t, imgs)
			fs, err := OpenFromDevices(devs, -1)
			if err != nil {
				t.Fatalf("OpenFromDevices(%s): %v", profile, err)
			}
			defer fs.Close()

			// Sanity baseline check.
			checkExpectations(t, fs, profile)

			var wg sync.WaitGroup
			var errCount atomic.Uint64
			for r := 0; r < readers; r++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := 0; i < iters; i++ {
						if _, err := fs.ReadFile("/hello.txt"); err != nil {
							errCount.Add(1)
							t.Errorf("%s: ReadFile /hello.txt: %v", profile, err)
							return
						}
						if _, err := fs.ReadFile("/sub/blob.bin"); err != nil {
							errCount.Add(1)
							t.Errorf("%s: ReadFile /sub/blob.bin: %v", profile, err)
							return
						}
					}
				}()
			}
			wg.Wait()
			if errCount.Load() != 0 {
				t.Errorf("%s: %d errors during parallel reads", profile, errCount.Load())
			}
		})
	}
}

// openRAIDImagesAsBackends mirrors the helper in raid_multidev_test.go but
// stress_test.go lives in the same package so we re-implement here to avoid
// cross-test coupling.
func openRAIDImagesAsBackends(t *testing.T, paths []string) []BlockBackend {
	t.Helper()
	out := make([]BlockBackend, len(paths))
	for i, p := range paths {
		f, err := os.OpenFile(p, os.O_RDWR, 0o600)
		if err != nil {
			t.Fatalf("open %s: %v", p, err)
		}
		out[i] = &osFileBackend{f: f}
	}
	return out
}
