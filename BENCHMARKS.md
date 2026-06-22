# Performance parity — go-filesystems/btrfs vs mkfs.btrfs / kernel btrfs  (2026-06-22)

## Methodology

- **Where**: the `debian` Tart VM (linux/arm64) on an Apple-silicon (M4) host.
  Our pure-Go driver and the reference C tools run in the same VM, same kernel,
  same hardware. Reads are **cold** (caches dropped before every iteration).
- **CPU / kernel**: 4 vCPU aarch64, Linux 6.12.74 (Debian 13).
- **Go**: 1.26.4 linux/arm64, CGO disabled.
- **Reference tools**: btrfs-progs 6.14 (`mkfs.btrfs`), in-tree kernel btrfs.
- **Image set**: 2008 files — 2000 small (1–4 KiB) + 8 large (4 MiB) ≈ 38 MB in
  a 513 MiB image.
- **Sampling**: best-of-5; format and read timed separately; read cold;
  throughput on the ~38 MB payload.
- **Format**: ours `btrfs.Format(path, size, cfg)` vs `truncate` + `mkfs.btrfs`.
- **Read**: image created+populated by `mkfs.btrfs` + loop-mount + `cp -a`, then
  read by ours and the kernel (`mount -o loop` + `tar`). No general-purpose
  userspace btrfs reader is shipped by default → no peer column.
- **Correctness (read) — verified**: reading a real `mkfs.btrfs` image, our
  driver returns exactly 2008 files byte-for-byte.
- **Correctness (Format) — FAILS, see below.**

## Results

| op | size | ours (MB/s, wall) | reference (MB/s, wall) | ratio | verdict |
|----|------|-------------------|------------------------|-------|---------|
| Format | 513 MiB | — , 0.055 ms | mkfs.btrfs: — , 205.7 ms | — | **⚠ ours output is NOT kernel-mountable — see below** |
| Read (cold) | 38 MB | 32 MB/s, 1134.8 ms | kernel: 1595 MB/s, 23.1 ms | 49.1× | ours 49× slower |

## Summary

### ⚠ Format correctness failure (headline)

Our `btrfs.Format` round-trips through **our own** reader but the Linux kernel
**refuses to mount it**. `dmesg` from a loop-mount of our output:

```
BTRFS error (device loopN): dev_item UUID does not match metadata fsid:
    18f86df4-…-6534e23eea60 != 00000000-0000-0000-0000-000000000000
BTRFS error (device loopN): bytes_used is too small 12288
BTRFS error (device loopN): superblock contains fatal errors
open_ctree failed: -22
```

Two concrete bugs:

1. **`dev_item.uuid` ≠ superblock `metadata_uuid`/`fsid`.** The device item carries
   a real UUID while the superblock's metadata fsid field is left all-zero; the
   kernel requires them to match (or `metadata_uuid` to equal `fsid`).
2. **`bytes_used` underreported (12288).** The superblock `bytes_used` does not
   account for the metadata extents actually written, so the kernel's sanity
   check rejects it.

Because the image never reaches a mountable state, the 0.055 ms "format time" is
**not a meaningful parity number** — it is the cost of writing an incomplete
superblock + a few tree blocks. The `mkfs.btrfs` 205.7 ms reflects a *complete,
mountable* filesystem (DUP metadata trees, checksum + extent trees, etc.).

### Read

- **Read: we lag the kernel 49× (32 vs 1595 MB/s)** — our slowest writable-fs
  reader, tied with squashfs in spirit. btrfs read is the most metadata-heavy:
  every file requires walking the FS tree + extent tree + verifying csum-tree
  checksums, and our implementation does all of that without caching or
  batching. (The kernel number includes loop+mount+tar overhead, so the
  pure-parse gap is somewhat smaller, but the order of magnitude is real.)

### Root causes (read)

1. **B-tree nodes re-read and re-parsed** on every lookup; no node cache, so the
   root/upper-level nodes are decoded once per file.
2. **Per-block crc32c verification in scalar Go** over every metadata + data
   block.
3. **One `ReadAt` per leaf/extent block**, no coalescing of contiguous extents.
4. Per-file / per-node allocation → GC pressure.

### Action items

- [ ] **FIX (correctness, Format):** set the superblock `metadata_uuid`/`fsid`
      consistently with `dev_item.uuid`, and compute `bytes_used` from the actual
      metadata extents written. Add a CI gate that loop-mounts the formatted
      image in the Tart VM (or runs `btrfs check`) so this can't regress.
- [ ] LRU-cache decoded B-tree nodes across the walk (root + interior nodes).
- [ ] Coalesce contiguous extent reads into one `ReadAt`.
- [ ] SIMD crc32c via go-asmgen (shared across go-filesystems).
- [ ] Pool node/read buffers; optional parallel extract.

## Reproduce

```sh
sudo ./benchmarks/run.sh btrfs <repo_dir> <work_dir> 5
# Format-mountability check:
btrfs.Format(img,size,cfg); mount -o loop img /mnt   # currently fails -22
```

`benchmarks/run.sh` is shared across the go-filesystems drivers;
`benchmarks/bench.go` is the btrfs harness. Standalone `main` package, excluded
from the coverage gate.
