package filesystem_btrfs

import (
	"testing"
)

// TestRAIDFixture_Single verifies the single-profile mkfs.btrfs fixture
// opens cleanly and exposes both /hello.txt and /sub/blob.bin.
func TestRAIDFixture_Single(t *testing.T) {
	imgs := extractRAIDFixture(t, "single")
	if len(imgs) != 1 {
		t.Fatalf("single profile: expected 1 image, got %d", len(imgs))
	}
	fs, err := Open(imgs[0], -1)
	if err != nil {
		t.Fatalf("Open(%s): %v", imgs[0], err)
	}
	defer fs.Close()
	checkExpectations(t, fs, "single")
}

// TestRAIDFixture_SingleLegMirrorProfiles verifies that opening ONE leg of
// a pure-mirror RAID profile (RAID1) via the existing dev_item.devid-aware
// chunk parser yields the same file contents — single-leg open of a healthy
// mirror is the simplest multi-device read mode and covers the common
// Proxmox/Ubuntu redundancy installs.
//
// RAID10 / RAID0 / RAID5 / RAID6 cannot be opened single-leg because every
// chunk has only a partial view on any one device. Those profiles need
// OpenFromDevices — see TestRAIDFixture_AllProfiles_MultiDevice.
func TestRAIDFixture_SingleLegMirrorProfiles(t *testing.T) {
	for _, profile := range []string{"raid1"} {
		t.Run(profile, func(t *testing.T) {
			imgs := extractRAIDFixture(t, profile)
			// Try each leg in turn — every leg of a pure mirror must
			// contain a full readable view.
			for _, img := range imgs {
				fs, err := Open(img, -1)
				if err != nil {
					t.Fatalf("Open(%s) for %s: %v", img, profile, err)
				}
				checkExpectations(t, fs, profile)
				fs.Close()
			}
		})
	}
}
