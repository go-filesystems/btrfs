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
