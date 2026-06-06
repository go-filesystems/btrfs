package filesystem_btrfs

import (
	"os"
	"testing"
)

// openImagesAsBackends opens each path with os.OpenFile and wraps in
// osFileBackend so the slice can be fed to OpenFromDevices.
func openImagesAsBackends(t *testing.T, paths []string) []BlockBackend {
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

// TestRAIDFixture_AllProfiles_MultiDevice opens each RAID fixture with
// OpenFromDevices feeding all legs of the multi-device pool, and verifies
// every profile yields the same /hello.txt + /sub/blob.bin contents that
// were written by mkfs.btrfs + mount + cat in Docker.
//
// This exercises the per-profile stripe math in multidev.go against real
// mkfs.btrfs-produced images:
//
//   - single  → readSingleOrMirror, 1 device
//   - raid0   → readStriped(nparity=0), 2 devices
//   - raid1   → readSingleOrMirror (mirror), 2 devices
//   - raid10  → readRAID10, 4 devices in 2 mirror pairs
//   - raid5   → readStriped(nparity=1), 3 devices, 1 parity column rotating
//   - raid6   → readStriped(nparity=2), 4 devices, 2 parity columns rotating
func TestRAIDFixture_AllProfiles_MultiDevice(t *testing.T) {
	for _, profile := range []string{"single", "raid0", "raid1", "raid10", "raid5", "raid6"} {
		t.Run(profile, func(t *testing.T) {
			imgs := extractRAIDFixture(t, profile)
			devs := openImagesAsBackends(t, imgs)
			fs, err := OpenFromDevices(devs, -1)
			if err != nil {
				t.Fatalf("OpenFromDevices(%s, %d legs): %v", profile, len(devs), err)
			}
			defer fs.Close()
			checkExpectations(t, fs, profile)
		})
	}
}
