package filesystem_btrfs

// The Linux kernel and btrfs-progs as the judge of this driver, in a Tart VM.
// Skipped unless FS_KERNEL_JUDGE=1; github.com/go-filesystems/kerneljudge's
// cmd/kerneljudge sets it and runs this as root in the VM:
//
//	kerneljudge run --driver <clone of this repository> [--perf]

import (
	"testing"
	"time"

	filesystem "github.com/go-filesystems/interface"
	kj "github.com/go-filesystems/kerneljudge"
)

func kernelJudgeDriver() kj.Driver {
	return kj.Driver{
		Name:   "btrfs",
		FSType: "btrfs",
		Open: func(img string) (filesystem.Filesystem, error) {
			f, err := Open(img, -1)
			if err != nil {
				return nil, err
			}
			return f, nil
		},
		Format: func(img string, size int64) (filesystem.Filesystem, error) {
			return Format(img, size, FormatConfig{Label: "kerneljudge"})
		},
		Mtime: func(f filesystem.Filesystem, path string) (time.Time, error) {
			st, err := f.(FS).ExtendedStat(path)
			if err != nil {
				return time.Time{}, err
			}
			return st.MTime, nil
		},
		// Defects that hide the rest of an image get a variant without them:
		// unaligned file sizes, xattrs, and an empty file.
		Variants: []kj.Variant{{
			Name: "aligned-plain",
			Opts: func(o *kj.Options) { o.SizeAlign = 4096; o.Caps.Xattrs = false },
			Map: func(e kj.Entry) (kj.Entry, bool) {
				return e, !(e.Kind == kj.File && e.Size == 0)
			},
		}},
		Known: []kj.Known{
			{Test: `/write$`, Match: `^(fsck btrfs|kernel mount):`, Issue: "btrfs#39, #40, #41"},
		},
		Mkfs: []string{"mkfs.btrfs", "-q", "-f", "{img}"},
		Fsck:    [][]string{{"btrfs", "check", "--readonly", "{img}"}},
		Compare: kj.Compare{Mtime: true, Xattrs: true, Perm: true},
	}
}

func TestKernelJudge(t *testing.T)     { kj.Run(t, kernelJudgeDriver()) }
func TestKernelJudgePerf(t *testing.T) { kj.RunPerf(t, kernelJudgeDriver(), kj.PerfOptions{}) }
