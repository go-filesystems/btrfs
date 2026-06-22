# Performance parity — go-filesystems/btrfs vs mkfs.btrfs / kernel btrfs  (2026-06-22)

## Methodology

- **Where**: the `cb-tpm-ubuntu` Tart VM (linux/arm64) on an Apple-silicon host.
  Our pure-Go driver and the reference C tools run in the same VM, same kernel,
  same hardware. Reads are **cold** (`echo 3 > drop_caches` before every
  iteration).
- **CPU / kernel**: aarch64, Linux 6.17.0-19-generic (Ubuntu 24.04).
- **Go**: 1.26.4 linux/arm64, CGO disabled.
- **Reference tools**: `mkfs.btrfs` (btrfs-progs), in-tree kernel btrfs.
- **Image set**: 2008 files — 2000 small (1–4 KiB) + 8 large (4 MiB) ≈ 38 MB in
  a 512 MiB image (metadata-heavy: 2000 tiny files dominate the tree-walk cost).
- **Sampling**: best-of-5; read cold; throughput on the ~38 MB payload.
- **Read**: image created+populated by `mkfs.btrfs` + loop-mount + `cp -a`, then
  read by ours and the kernel (`mount -o loop` + `tar`). No general-purpose
  userspace btrfs reader is shipped by default → no peer column.
- **Correctness (read) — verified**: reading a real `mkfs.btrfs` image, our
  driver returns exactly 2008 files byte-for-byte (before AND after the cache).
- **Correctness (Format)**: kernel-mountability fixed in `dcf7b09`.

## Results

| op | size | ours (MB/s, wall) | reference (MB/s, wall) | ratio | verdict |
|----|------|-------------------|------------------------|-------|---------|
| Read (cold) — **before** node cache | 38 MB | 69.9 MB/s, 526.2 ms | kernel: 1030 MB/s, 35.7 ms | 14.7× | baseline |
| Read (cold) — **after** node cache | 38 MB | **184.4 MB/s, 199.8 ms** | kernel: 832 MB/s, 44.3 ms | **4.5×** | **2.6× faster; gap 14.7×→4.5×** |

> Both rows re-measured on the **same** VM/hardware (`cb-tpm-ubuntu`, kernel
> 6.17) in one session, so the before/after delta is apples-to-apples. The
> earlier "32 MB/s / 49×" figure was on the slower `debian` VM (kernel 6.12);
> the 14.7× baseline above is the pre-cache number on this faster host.

## Summary

### Read — node cache closes most of the gap

The read fileset is metadata-dominated: 2000 tiny files, each requiring a
descent of the FS tree (and, for content, the extent items). Pre-cache, every
file re-read and re-decoded the **same** root + interior B-tree nodes, so the
upper tree was parsed ~2000× over. Btrfs is copy-on-write, so a logical block
address holds the same content for the life of an open handle — memoizing the
decoded node by logical address is exact and safe. The cache collapses those
thousands of repeated interior-node reads into one each.

**Effect (re-measured on `cb-tpm-ubuntu`):** 69.9 → 184.4 MB/s, a **2.6×**
speedup, shrinking the gap to the kernel from **14.7× to 4.5×**. The node cache
was by far the dominant lever; crc32c already routes through Go's hardware
`hash/crc32` Castagnoli path (HW CRC on amd64/arm64), and uncompressed extents
already read in a single `ReadAt` of the whole extent.

**Consistency:** the cache is per-open and read-only. Every mutating method
drops it (`invalidateCache()` deferred on the write paths) and `reader()`
additionally rebuilds whenever the superblock generation advances — so a
write-then-read can never observe a stale block at a COW-recycled address.
Covered by `TestNodeCache_*` (write-then-read sees fresh data; generation-guard
rebuild; memoized-buffer hit).

### Root causes (read) — status

1. ~~**B-tree nodes re-read and re-parsed** on every lookup; no node cache.~~
   **DONE** — logical-address node cache (`nodecache.go`).
2. **Per-block crc32c verification in scalar Go.** Already uses `hash/crc32`
   Castagnoli → HW CRC on amd64/arm64. A go-asmgen kernel remains optional.
3. **One `ReadAt` per leaf/extent block.** Uncompressed extents already read in
   one `ReadAt`; remaining wins (leaf coalescing/readahead) are secondary now
   that interior re-reads are eliminated.
4. Per-file / per-node allocation → GC pressure — largely absorbed by the cache
   (decoded nodes are retained, not re-allocated per descent).

### Action items

- [x] Cache decoded B-tree nodes across the walk (root + interior nodes).
- [ ] Coalesce contiguous leaf/extent reads + readahead (secondary).
- [ ] SIMD crc32c via go-asmgen (shared across go-filesystems; optional — HW
      crc32 already engaged via `hash/crc32`).
- [ ] Pool node/read buffers; optional parallel extract.

### Format

Kernel-mountability fixed in `dcf7b09` (`metadata_uuid`/`fsid` consistent with
`dev_item.uuid`; `bytes_used` computed from the metadata extents written).

## Reproduce

```sh
sudo ./benchmarks/run.sh btrfs <repo_dir> <work_dir> 5
```

`benchmarks/run.sh` is shared across the go-filesystems drivers;
`benchmarks/bench.go` is the btrfs harness. Standalone `main` package, excluded
from the coverage gate.
