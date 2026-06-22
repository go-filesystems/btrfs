#!/usr/bin/env bash
# Performance-parity harness driver. Runs INSIDE the debian Tart VM (linux/arm64)
# so our pure-Go driver and the reference C tools execute on identical hardware
# and within the same Linux kernel, allowing fair cold-cache reads via
# drop_caches.
#
# Usage: sudo ./run.sh <fs> <repo_dir> <work_dir> <iters>
#   fs        = ext4|xfs|btrfs|fat32|squashfs
#   repo_dir  = path to the driver repo (contains benchmarks/bench.go)
#   work_dir  = scratch dir for images and the generated fileset
#   iters     = best-of-N iterations
set -euo pipefail
export PATH=$PATH:/sbin:/usr/sbin:/usr/local/go/bin

FS=$1
REPO=$2
WORK=$3
ITERS=${4:-5}

mkdir -p "$WORK"
SRC="$WORK/src"       # generated fileset
IMG="$WORK/img.bin"   # filesystem image under test
MNT="$WORK/mnt"
mkdir -p "$MNT"

drop() { sync; echo 3 > /proc/sys/vm/drop_caches; }
now()  { date +%s%N; }

# ---- 1. Generate a representative fileset (many small + a few large) --------
gen_fileset() {
  rm -rf "$SRC"; mkdir -p "$SRC"
  # 2000 small files (~1-4 KiB) spread over 50 dirs
  for d in $(seq 1 50); do
    mkdir -p "$SRC/d$d"
    for f in $(seq 1 40); do
      head -c $((1024 + (RANDOM % 3072))) /dev/urandom > "$SRC/d$d/f$f.bin"
    done
  done
  # 8 large files (4 MiB each = 32 MiB)
  mkdir -p "$SRC/big"
  for f in $(seq 1 8); do
    head -c $((4*1024*1024)) /dev/urandom > "$SRC/big/big$f.bin"
  done
}

# total bytes + file count of the source fileset
SRC_BYTES=0; SRC_FILES=0
measure_src() {
  SRC_BYTES=$(du -sb "$SRC" | cut -f1)
  SRC_FILES=$(find "$SRC" -type f | wc -l)
}

# ---- 2. Image size: round source up to leave slack -------------------------
img_size() {
  # source bytes * 3, rounded to MiB, with an fs-specific floor.
  # xfs requires > 300 MiB; btrfs DUP metadata needs generous slack; use 512 MiB
  # for those, 96 MiB otherwise.
  local b=$1
  local s=$(( b * 3 ))
  local min=$(( 96 * 1024 * 1024 ))
  case "$FS" in
    xfs|btrfs) min=$(( 512 * 1024 * 1024 )) ;;
  esac
  [ "$s" -lt "$min" ] && s=$min
  echo $(( (s / 1048576 + 1) * 1048576 ))
}

# ---- 3. Reference mkfs (empty image, format-time benchmark) ----------------
ref_mkfs() {
  local img=$1 size=$2
  rm -f "$img"; truncate -s "$size" "$img"
  case "$FS" in
    ext4)  mkfs.ext4  -q -F -L bench "$img" >/dev/null 2>&1 ;;
    # agsize=128 MiB forces a power-of-2 agblocks (32768). Our reader currently
    # assumes fsblock == agno*agblocks + agbno, which only holds when agblocks is
    # a power of two; mkfs.xfs's default agsize is not (see BENCHMARKS.md action
    # items). This keeps the read comparison valid while that gap is open.
    xfs)   mkfs.xfs   -q -f -L bench -d agsize=134217728 "$img" >/dev/null 2>&1 ;;
    btrfs) mkfs.btrfs -q -f -L bench "$img" >/dev/null 2>&1 ;;
    fat32) mkfs.vfat  -F 32 -n BENCH "$img" >/dev/null 2>&1 ;;
  esac
}

# ---- 4. Populate an image with the fileset (for read tests) ----------------
populate_writable() {
  local img=$1
  mount -o loop "$img" "$MNT"
  cp -a "$SRC/." "$MNT/"
  sync
  umount "$MNT"
}

# ---- timing helpers --------------------------------------------------------
best_of() {
  # best_of <iters> <cmd...> : echoes min wall ns over N runs (caller handles caches)
  local n=$1; shift
  local best=0
  for i in $(seq 1 "$n"); do
    "$@"
  done
}

echo "### fs=$FS iters=$ITERS host=$(uname -m) kernel=$(uname -r) go=$(go version | awk '{print $3}')"

gen_fileset
measure_src
SIZE=$(img_size "$SRC_BYTES")
echo "### src_bytes=$SRC_BYTES src_files=$SRC_FILES img_size=$SIZE"

# Build our driver once.
( cd "$REPO" && go build -o "$WORK/ourbench" ./benchmarks/ )

# =====================  FORMAT BENCHMARK  ===================================
if [ "$FS" != "squashfs" ]; then
  echo "## FORMAT"
  # ours
  best=0
  for i in $(seq 1 "$ITERS"); do
    rm -f "$IMG"
    out=$("$WORK/ourbench" format "$IMG" "$SIZE")
    ns=$(echo "$out" | grep -o 'wall_ns=[0-9]*' | cut -d= -f2)
    { [ "$best" -eq 0 ] || [ "$ns" -lt "$best" ]; } && best=$ns
  done
  echo "OURS_FORMAT_NS=$best"
  # reference mkfs
  best=0
  for i in $(seq 1 "$ITERS"); do
    rm -f "$WORK/ref.bin"; truncate -s "$SIZE" "$WORK/ref.bin"
    t0=$(now); ref_mkfs "$WORK/ref.bin" "$SIZE"; t1=$(now)
    ns=$(( t1 - t0 ))
    { [ "$best" -eq 0 ] || [ "$ns" -lt "$best" ]; } && best=$ns
  done
  echo "REF_FORMAT_NS=$best"
fi

# =====================  READ BENCHMARK  =====================================
echo "## READ"
# Build the populated image once with reference tooling (its native creator).
if [ "$FS" = "squashfs" ]; then
  rm -f "$IMG"
  mksquashfs "$SRC" "$IMG" -no-progress -quiet -noappend >/dev/null 2>&1
else
  ref_mkfs "$IMG" "$SIZE"
  populate_writable "$IMG"
fi
IMG_BYTES=$(stat -c %s "$IMG")
echo "### populated_img_bytes=$IMG_BYTES"

# ---- correctness: our extract must match the source -------------------------
verify_read() {
  drop
  out=$("$WORK/ourbench" read "$IMG")
  echo "### ours_read_out: $out"
  ofiles=$(echo "$out" | grep -o 'files=[0-9]*' | cut -d= -f2)
  obytes=$(echo "$out" | grep -o 'bytes=[0-9]*' | cut -d= -f2)
  if [ "$ofiles" != "$SRC_FILES" ]; then
    echo "### WARN file count mismatch ours=$ofiles src=$SRC_FILES"
  fi
  if [ "$obytes" != "$SRC_BYTES" ]; then
    echo "### WARN byte count: ours=$obytes src(du)=$SRC_BYTES (du counts dirs/blocks)"
  fi
}
verify_read

# ---- OURS: cold read (drop caches before each run) -------------------------
best=0
for i in $(seq 1 "$ITERS"); do
  drop
  out=$("$WORK/ourbench" read "$IMG")
  ns=$(echo "$out" | grep -o 'wall_ns=[0-9]*' | cut -d= -f2)
  { [ "$best" -eq 0 ] || [ "$ns" -lt "$best" ]; } && best=$ns
done
echo "OURS_READ_NS=$best OURS_READ_BYTES=$SRC_BYTES"

# ---- KERNEL: mount + tar to /dev/null, cold ---------------------------------
kernel_read() {
  if [ "$FS" = "squashfs" ]; then
    mount -t squashfs -o loop "$IMG" "$MNT"
  else
    mount -o loop "$IMG" "$MNT"
  fi
  tar -cf /dev/null -C "$MNT" . 2>/dev/null
  umount "$MNT"
}
best=0
for i in $(seq 1 "$ITERS"); do
  drop
  t0=$(now); kernel_read; t1=$(now)
  ns=$(( t1 - t0 ))
  { [ "$best" -eq 0 ] || [ "$ns" -lt "$best" ]; } && best=$ns
done
echo "KERNEL_READ_NS=$best"

# ---- PEER userspace tool, cold ----------------------------------------------
peer_read() {
  case "$FS" in
    ext4)
      # debugfs rdump of root to a tmpfs-less scratch (read everything)
      rm -rf "$WORK/dbg"; mkdir -p "$WORK/dbg"
      debugfs -R "rdump / $WORK/dbg" "$IMG" >/dev/null 2>&1
      ;;
    squashfs)
      rm -rf "$WORK/unsq"; unsquashfs -no-progress -d "$WORK/unsq" "$IMG" >/dev/null 2>&1
      ;;
    *) return 1 ;;
  esac
}
if peer_read 2>/dev/null; then
  best=0
  for i in $(seq 1 "$ITERS"); do
    drop
    t0=$(now); peer_read; t1=$(now)
    ns=$(( t1 - t0 ))
    { [ "$best" -eq 0 ] || [ "$ns" -lt "$best" ]; } && best=$ns
  done
  case "$FS" in
    ext4) echo "PEER_TOOL=debugfs PEER_READ_NS=$best" ;;
    squashfs) echo "PEER_TOOL=unsquashfs PEER_READ_NS=$best" ;;
  esac
else
  echo "PEER_TOOL=none"
fi

echo "### DONE fs=$FS"
