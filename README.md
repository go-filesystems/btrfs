<p align="center"><img src="https://raw.githubusercontent.com/go-filesystems/brand/main/social/go-filesystems-btrfs.png" alt="go-filesystems/btrfs" width="720"></p>

# filesystem-btrfs

[![Go Reference](https://pkg.go.dev/badge/github.com/go-filesystems/btrfs.svg)](https://pkg.go.dev/github.com/go-filesystems/btrfs)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD%203--Clause-blue.svg)](https://opensource.org/licenses/BSD-3-Clause)
[![CI](https://github.com/go-filesystems/btrfs/actions/workflows/ci.yml/badge.svg)](https://github.com/go-filesystems/btrfs/actions/workflows/ci.yml)

Pure-Go read/write access to Btrfs filesystem images — no root privileges, no external tools, no CGO.

Supports single-device Btrfs images with CRC32c metadata checksums (btrfs-progs ≥ 5.x). MBR/GPT partition tables are auto-detected.

## References

https://btrfs.readthedocs.io/en/latest/

## Support summary

| Feature | Status | Notes |
|---|---:|---|
| Open / Close | ✅ | Single-device images; `OpenFromDevice`/`OpenFromDevices` for layered/multi-device backends |
| Format | ✅ | Creates a new single-device Btrfs image |
| ReadFile / WriteFile | ✅ | Full file I/O supported |
| OpenFile (`filesystem.Opener`), also on a subvolume | ✅ | Positional reads: the file's extent items are collected at open, each `ReadAt` reads (and, for a compressed extent, decompresses) only the extents of its window. A 1 MiB read at 66 MiB of a 72 MiB file allocates nothing through it and 75.5 MB through `ReadFile` |
| MkDir / Delete / Rename | ✅ | Directory and rename operations supported |
| ReadLink / Symlinks | ✅ | Read + create (`FS.Symlink`) |
| Hardlinks | ✅ | `FS.Link` |
| Xattrs | ✅ | `Xattrs` / `GetXattr` / `SetXattr` / `RemoveXattr` |
| Extended metadata | ✅ | `ExtendedStat` (uid/gid, timestamps, nlink, nbytes, transid/sequence, flags); `Chown` / `Chmod` / `Chtimes` / `Truncate` |
| Volume label | ✅ | `Label` / `SetLabel` |
| Subvolumes / snapshots | ✅ read-only | `Subvolumes` enumerates ROOT_TREE entries; `OpenSubvolumeByID`/`OpenSubvolumeByName` open one read-only. Creating a subvolume or snapshot is not supported (needs ref-counted extent backrefs) |
| Multi-device / RAID (RAID0/1/10/5/6/DUP) | ✅ read-only | Decoded via `OpenFromDevices`; writes are single-device only |
| Grow / Shrink / Resize | ✅ | `Shrink` refuses to discard live data; requires an idle filesystem (no concurrent writers during resize) |
| Partitioned images | ✅ | MBR/GPT auto-detected |

## Limitations

- Subvolume/snapshot *creation* and send/receive are not implemented (read-only support exists — see above).
- Quotas and reflink are not implemented.
- Multi-device/RAID *writes* are not implemented (reading multi-device pools is).
- No online device add/remove or balance operations.
- Intended for testing and tooling; not recommended for production use.

## Judged by

The Linux kernel (6.17) and btrfs-progs 6.6.3, run as root in a Tart VM by
[kerneljudge](https://github.com/go-filesystems/kerneljudge) (`kerneljudge run
--driver <clone>`; the test is `kerneljudge_test.go`, behind
`FS_KERNEL_JUDGE=1`). Each image is held to a fixed corpus (nested directories,
UTF-8 names, sparse and 32 MiB files, symlinks, xattrs, times) by `btrfs check
--readonly`, a read-only kernel mount and this reader, for images written by this
driver and, in reverse, by `mkfs.btrfs` plus a kernel read-write mount: both
directions are clean, with no known defect. It found, and this release fixed,
[#39](https://github.com/go-filesystems/btrfs/issues/39) (extent fields not a
multiple of the sector size: the kernel refused the filesystem),
[#40](https://github.com/go-filesystems/btrfs/issues/40) (an empty file carried
an EXTENT_DATA item) and
[#41](https://github.com/go-filesystems/btrfs/issues/41) (xattr item type).
Against the kernel on one image (4000 small files, a 256 MiB file): metadata walk
10369 files/s against 82447 for the kernel; the sequential read figure is not a
storage comparison (host-cached VM disk), see the kerneljudge README.

## Parallel reads of compressed extents

`ReadFile` and `OpenFile`'s `ReadAt` read and decode the compressed extents of a
file (zlib, LZO, zstd) on up to `min(GOMAXPROCS, 8)` goroutines
(`inflight.Ordered`: bounded to that many decoded extents in memory, delivered in
file order; `SetReadParallelism(n)` on the filesystem, 1 = serial and nothing is
spawned). The EXTENT_DATA items are still walked and checked serially, and the
error returned is the first in file order whatever the workers' speeds. The zstd
and zlib decoders are pooled.

Sequential read of a 256 MiB text-like file the kernel (6.17) wrote with
`compress-force`, MiB/s, kernel against this driver in the same VM, medians of 5
interleaved rounds in a calibrated uniform regime (`kerneljudge` v0.4.0,
`compressedread_perf_test.go`; the ~650-700 Mops/s and ~2600 Mops/s regimes are
1 and 4 CPUs):

| codec | 1 CPU kernel | 1 CPU v0.5.6 | 1 CPU now | 4 CPUs kernel | 4 CPUs v0.5.6 | 4 CPUs now |
|---|---:|---:|---:|---:|---:|---:|
| lzo  | 1184 | 1340 | 1376 | 3313 | 1326 | 2536 |
| zstd |  838 |  722 | 1175 | 2512 |  667 | 2290 |
| zlib |  374 |  237 |  303 | 1226 |  236 |  924 |

(Kernel figures are those of the v0.5.6 run; the run of the new code measured
1169 / 809 / 360 at 1 CPU and 3317 / 2471 / 1233 at 4.) zlib at 1 CPU and LZO at
4 CPUs are still behind the kernel.

## Untrusted input

An image is attacker-controlled bytes. Every allocation, walk and recursion the
driver sizes from a value read off the image is checked against what the format
can produce **before** it happens (tests assert the allocation or the device-read
count for each refused road). Bounds that come from the kernel source are cited
in `harden.go`:

| Input | Bound |
|---|---|
| compressed extent (`ram_bytes`, zlib / LZO / zstd) | 128 KiB: the kernel's `BTRFS_MAX_UNCOMPRESSED` (`fs/btrfs/compression.h`); LZO output capacity follows the input (one page per 5 input bytes), not the declared size |
| zstd frame window | 128 KiB: the window the kernel decodes btrfs extents with (`ZSTD_BTRFS_MAX_WINDOWLOG`, `fs/btrfs/zstd.c`); a frame declaring more is refused |
| file extent items | must not overlap (`check_extent_data_item`, `fs/btrfs/tree-checker.c`): an overlapping leaf cost items x file size to read |
| chunk `stripe_len` (RAID0/5/6/10) | exactly 64 KiB (`check_chunk_item`): `stripe_len = 1` was one device read and one stack frame per byte |
| file size, tree nodes, tree depth | device size, 2^20 nodes, 64 levels (unchanged) |

Fuzz targets: `FuzzDecompressExtent`, `FuzzStripedRead` (allocation and device-read
count stay proportional to the input), plus `FuzzMountImage`, `FuzzReadFileData`,
`FuzzMountPatched`.

## Supported operations

| Operation    | Status         |
|--------------|----------------|
| Open / Close | ✅ implemented |
| Format       | ✅ implemented |
| Stat         | ✅ implemented |
| ListDir      | ✅ implemented |
| ReadFile     | ✅ implemented |
| WriteFile    | ✅ implemented |
| MkDir        | ✅ implemented |
| DeleteFile   | ✅ implemented |
| DeleteDir    | ✅ implemented |
| Rename       | ✅ implemented |
| ReadLink     | ✅ implemented |

## API

`FS` is an interface (not a struct) — `Open`/`OpenFromDevice`/`OpenFromDevices`
return `FS`; `Format` returns the narrower `filesystem.Filesystem`. Every
method below is called through the interface value (`fs.Symlink(...)`, not
`(*FS).Symlink`).

### Format / Open

```go
type FormatConfig struct {
    UUID  [16]byte // zero = randomly generated
    Label string    // up to 255 bytes, NUL-padded on disk
}

func Format(path string, sizeBytes int64, cfg FormatConfig) (filesystem.Filesystem, error)
func Open(imagePath string, partIndex int) (FS, error)
func OpenFromDevice(dev BlockBackend, partIndex int) (FS, error)
func OpenFromDevices(devs []BlockBackend, partIndex int) (FS, error) // multi-device RAID, read-only
```

### FS interface

```go
type FS interface {
    filesystem.Filesystem // Close, ReadFile, ListDir, Stat, WriteFile, ReadLink,
                           // MkDir, DeleteFile, DeleteDir, Rename

    Link(oldPath, newPath string) error
    Symlink(target, linkPath string) error

    Xattrs(path string) (map[string][]byte, error)
    GetXattr(path, name string) ([]byte, error)
    SetXattr(path, name string, value []byte) error
    RemoveXattr(path, name string) error

    ExtendedStat(path string) (*InodeStat, error)
    Chown(path string, uid, gid uint32) error
    Chmod(path string, perm os.FileMode) error
    Chtimes(path string, atime, mtime time.Time) error
    Truncate(path string, newSize int64) error

    Label() string
    SetLabel(label string) error

    // Subvolume / snapshot read support (creation is not supported).
    Subvolumes() ([]Subvolume, error)
    OpenSubvolumeByID(id uint64) (filesystem.Filesystem, error)
    OpenSubvolumeByName(name string) (filesystem.Filesystem, error)

    // Filesystem-level resize. Requires an idle FS (no concurrent writers).
    Grow(newSizeBytes int64) error
    Shrink(newSizeBytes int64) error
    Resize(newSizeBytes int64) error
}
```

## Integration test

Set `integrationImagePath` in `btrfs_test.go` and run (from the repo root):

```bash
go test -v -run TestOpen_Integration .
```

## Implements

This package implements the `filesystem.Filesystem` contract from
`github.com/go-filesystems/interface`. Use the interface in higher-level
tools to operate on multiple filesystem backends interchangeably.

Example:

```go
import (
    filesystem "github.com/go-filesystems/interface"
    fsb "github.com/go-filesystems/btrfs"
)

f, _ := fsb.Open("btrfs.img", -1)
defer f.Close()
var fs filesystem.Filesystem = f
_, _ = fs.ReadFile("/data")
```
