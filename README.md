# filesystem-btrfs

Pure-Go read/write access to Btrfs filesystem images — no root privileges, no external tools, no CGO.

Supports single-device Btrfs images with CRC32c metadata checksums (btrfs-progs ≥ 5.x). MBR/GPT partition tables are auto-detected.

## References

https://btrfs.readthedocs.io/en/latest/

## Support summary

| Feature | Status | Notes |
|---|---:|---|
| Open / Close | ✅ | Single-device images supported |
| Format | ✅ | Creates a new Btrfs image |
| Resize | ✅ | `Grow` supported; `Shrink` limited (live-extent relocation not implemented) |
| ReadFile | ✅ | Full file reads supported |
| WriteFile | ✅ | Full file writes supported |
| MkDir / Delete / Rename | ✅ | Directory and rename operations supported |
| ReadLink / Symlinks | ✅ | Supported |
| Partitioned images | ✅ | MBR/GPT auto-detected |

## Limitations

- Advanced Btrfs features such as snapshots, send/receive, multi-device/RAID management, quotas and reflink are not fully implemented.
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

### Format

```go
type FormatConfig struct {
    UUID  [16]byte // zero = randomly generated
    Label string
}

func Format(path string, sizeBytes int64, cfg FormatConfig) (*FS, error)
```

### Open

```go
func Open(imagePath string, partIndex int) (*FS, error)
func (fs *FS) Close() error
```

### Read

```go
func (fs *FS) Stat(path string) (filesystem.Stat, error)
func (fs *FS) ListDir(path string) ([]filesystem.DirEntry, error)
func (fs *FS) ReadFile(path string) ([]byte, error)
func (fs *FS) ReadLink(path string) (string, error)
```

### Write

```go
func (fs *FS) WriteFile(path string, data []byte, perm os.FileMode) error
func (fs *FS) MkDir(path string, perm os.FileMode) error
func (fs *FS) DeleteFile(path string) error
func (fs *FS) DeleteDir(path string) error
func (fs *FS) Rename(oldPath, newPath string) error
```

## Integration test

Set `integrationImagePath` in `btrfs_test.go` and run:

```
go test -v -run TestOpen_Integration ./pkg/filesystem-btrfs
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
