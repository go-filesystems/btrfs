package filesystem_btrfs

// Read performance of compressed extents against the kernel (go-compressions/lzo#9):
// a sequential read of one 256 MiB file the kernel wrote with compress-force=
// lzo, zstd or zlib, kernel against this driver, at 1 and at 4 CPUs (chcpu),
// interleaved rounds in calibrated regimes (kerneljudge). Skipped unless
// FS_KERNEL_JUDGE_PERF=1; run as root in the Tart VM with
//
//	kerneljudge run --driver <clone> --perf --test-run KernelJudgeCompressedRead
//
// FS_KERNEL_JUDGE_PROFILE=1 adds a CPU profile of the driver's read per CPU
// count, logged as base64.

import (
	"bytes"
	"encoding/base64"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"testing"
	"time"

	filesystem "github.com/go-filesystems/interface"
	kj "github.com/go-filesystems/kerneljudge"
)

const lrLarge = 256 << 20

func lrRun(args ...string) (string, error) {
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	return string(out), err
}

func lrMust(t *testing.T, args ...string) {
	t.Helper()
	if out, err := lrRun(args...); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

func lrGen(w io.Writer) error {
	r := rand.New(rand.NewPCG(42, 43))
	vocab := make([][]byte, 200)
	for i := range vocab {
		b := make([]byte, 3+r.IntN(8))
		for j := range b {
			b[j] = byte('a' + r.IntN(26))
		}
		vocab[i] = b
	}
	buf := make([]byte, 0, 1<<20)
	for total := 0; total < lrLarge; total += len(buf) {
		buf = buf[:0]
		for len(buf) < 1<<20-16 {
			k := r.IntN(len(vocab))
			buf = append(buf, vocab[k]...)
			if r.IntN(12) == 0 {
				buf = append(buf, '\n')
			} else {
				buf = append(buf, ' ')
			}
		}
		for len(buf) < 1<<20 {
			buf = append(buf, ' ')
		}
		if _, err := w.Write(buf); err != nil {
			return err
		}
	}
	return nil
}

func lrMedian(r []float64) float64 {
	s := append([]float64(nil), r...)
	sort.Float64s(s)
	return s[len(s)/2]
}

func TestKernelJudgeCompressedRead(t *testing.T) {
	if os.Getenv("FS_KERNEL_JUDGE_PERF") != "1" {
		t.Skip("FS_KERNEL_JUDGE_PERF=1 to measure")
	}
	for _, codec := range []string{"lzo", "zstd", "zlib"} {
		t.Run(codec, func(t *testing.T) { lrCodec(t, codec) })
	}
}

func lrCodec(t *testing.T, codec string) {
	defer lrRun("chcpu", "-e", "1-3")
	dir := t.TempDir()
	img, mnt := filepath.Join(dir, "pool.img"), "/kjcmpmnt"
	if err := os.WriteFile(img, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(img, 1<<30); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(mnt, 0o755)
	lrMust(t, "mkfs.btrfs", "-q", "-f", img)
	lrMust(t, "mount", "-o", "compress-force="+codec, img, mnt)
	f, err := os.Create(filepath.Join(mnt, "large.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := lrGen(f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	lrMust(t, "umount", mnt)
	{
		fs0, err := Open(img, -1)
		if err != nil {
			t.Fatal(err)
		}
		got, err := fs0.ReadFile("/large.bin")
		var want bytes.Buffer
		lrGen(&want)
		t.Logf("CONTENT equal=%v len=%d err=%v", bytes.Equal(got, want.Bytes()), len(got), err)
		fs0.Close()
		du, _ := lrRun("du", "-B1", img)
		t.Logf("IMAGE allocated bytes: %s", du)
	}
	dropCaches := func() { os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0) }

	for _, ncpu := range []int{1, 4} {
		if ncpu == 1 {
			lrRun("chcpu", "-d", "1-3")
		} else {
			lrRun("chcpu", "-e", "1-3")
		}
		time.Sleep(time.Second)
		runtime.GOMAXPROCS(ncpu)

		oursRead := func() float64 {
			dropCaches()
			start := time.Now()
			fs, err := Open(img, -1)
			if err != nil {
				t.Fatal(err)
			}
			of, err := fs.(filesystem.Opener).OpenFile("/large.bin")
			if err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 1<<20)
			var off int64
			for off < of.Size() {
				n, err := of.ReadAt(buf, off)
				off += int64(n)
				if err != nil && err != io.EOF {
					t.Fatal(err)
				}
				if n == 0 {
					break
				}
			}
			el := time.Since(start)
			of.Close()
			fs.Close()
			if off != lrLarge {
				t.Fatalf("ours read %d", off)
			}
			return float64(off) / (1 << 20) / el.Seconds()
		}

		var kernel, ours []float64
		var regimes []kj.Regime
		var first *kj.Regime
		for r := 0; r < 5; r++ {
			var rg kj.Regime
			var nk, no int
			for attempt := 0; ; attempt++ {
				nk, no = len(kernel), len(ours)
				rg = kj.Regime{Before: kj.Calibrate(0).Mops, Tol: kj.DefaultRegimeTolerance}
				dropCaches()
				lrMust(t, "mount", "-o", "ro", img, mnt)
				start := time.Now()
				fl, err := os.Open(filepath.Join(mnt, "large.bin"))
				if err != nil {
					t.Fatal(err)
				}
				n, err := io.CopyBuffer(io.Discard, struct{ io.Reader }{fl}, make([]byte, 1<<20))
				el := time.Since(start)
				fl.Close()
				lrMust(t, "umount", mnt)
				if err != nil || n != lrLarge {
					t.Fatalf("kernel read %d: %v", n, err)
				}
				kernel = append(kernel, float64(n)/(1<<20)/el.Seconds())
				ours = append(ours, oursRead())
				rg.After = kj.Calibrate(0).Mops
				ref := rg
				if first != nil {
					ref = *first
				}
				if (rg.Steady() && kj.SameRegime(rg.Mops(), ref.Mops(), rg.Tol)) || attempt >= 2 {
					break
				}
				t.Logf("round %d ran in regime %s: measured again after a pause", r, rg.Label())
				kernel, ours = kernel[:nk], ours[:no]
				time.Sleep(10 * time.Second)
			}
			if first == nil {
				first = &rg
			}
			regimes = append(regimes, rg)
		}
		t.Logf("PERF btrfs-%s-cpus%d | seq read       | kernel | median %9.1f MiB/s | rounds %v | | %s", codec, ncpu, lrMedian(kernel), kernel, kj.RegimeText(regimes))
		t.Logf("PERF btrfs-%s-cpus%d | seq read       | ours   | median %9.1f MiB/s | rounds %v | | %s", codec, ncpu, lrMedian(ours), ours, kj.RegimeText(regimes))

		if os.Getenv("FS_KERNEL_JUDGE_PROFILE") == "1" {
			// CPU profile of more passes of ours (not part of the figures above).
			var pb bytes.Buffer
			if err := pprof.StartCPUProfile(&pb); err != nil {
				t.Fatal(err)
			}
			var prof []float64
			for range 25 {
				prof = append(prof, oursRead())
			}
			pprof.StopCPUProfile()
			t.Logf("PROFILE %s cpus%d passes MiB/s %v bytes %d", codec, ncpu, prof, pb.Len())
			t.Logf("PROFILEB64 %s cpus%d %s", codec, ncpu, base64.StdEncoding.EncodeToString(pb.Bytes()))
		}
	}
}
