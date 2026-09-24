// SPDX-License-Identifier: BSD-3-Clause

package filesystem_btrfs

import (
	"errors"
	iofs "io/fs"
	"path/filepath"
	"testing"
)

// ⛔ The error contract from go-filesystems/interface: a path that is not
// there must satisfy errors.Is(err, fs.ErrNotExist). Ten drivers in this
// family answer it; btrfs was one of the four left out, and left out for a
// reason worth keeping in view.
//
// btrfs raises ONE sentinel, ErrNotFound, from two very different places:
//
//   - lookupDirEntry, when a name is not in a directory. That is a 404.
//   - searchTree and collectPrefixItems, when a B-tree descent finds no item.
//     For the chunk tree or the root tree, that is a CORRUPT IMAGE.
//
// Wrapping the sentinel itself would make a server answer 404 for a broken
// filesystem, which hides a real fault behind a routine one -- the whole
// reason this driver was deferred rather than done with the others.
//
// So only lookupDirEntry's final return is marked, and it is the right place:
// it has already discarded both B-tree errors by then and reached its verdict
// on the name alone. TestACorruptTreeIsNotA404 below is the other half, and
// the one that has to keep passing.
func TestMissingPathsSatisfyErrNotExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fs.img")
	fsys, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()

	for _, tc := range []struct {
		what string
		err  error
	}{
		{"Stat", func() error { _, e := fsys.Stat("/nope.txt"); return e }()},
		{"ReadFile", func() error { _, e := fsys.ReadFile("/nope.txt"); return e }()},
		{"ListDir", func() error { _, e := fsys.ListDir("/nope"); return e }()},
		{"ReadLink", func() error { _, e := fsys.ReadLink("/nope"); return e }()},
		{"DeleteFile", fsys.DeleteFile("/nope.txt")},
		{"Rename", fsys.Rename("/nope.txt", "/other.txt")},
	} {
		if tc.err == nil {
			t.Errorf("%s on a missing path returned no error at all", tc.what)
			continue
		}
		if !errors.Is(tc.err, iofs.ErrNotExist) {
			t.Errorf("%s: errors.Is(err, fs.ErrNotExist) is false for %q", tc.what, tc.err)
		}
	}
}

// TestACorruptTreeIsNotA404 is the guard on the change above, and the reason
// the whole sentinel was not simply wrapped.
//
// A B-tree search that comes up empty is not a missing file: it means the
// image does not hold the structure it claims to. If that ever starts
// satisfying fs.ErrNotExist, every server in this family will report a broken
// filesystem as "not found" and the fault will go unnoticed.
func TestACorruptTreeIsNotA404(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fs.img")
	fsys, err := Format(path, btrfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()
	b, ok := fsys.(*btrfsFS)
	if !ok {
		t.Fatalf("Format returned %T, not the concrete filesystem this test drives", fsys)
	}

	// Straight at the B-tree, below any path resolution: an object id nothing
	// in this image could own. This is the shape a corrupt chunk tree or root
	// tree produces, and the one that must never read as 404.
	_, _, err = searchTree(b.reader(), b.partOffset, b.sb, b.fsTreeRoot, 1<<62, typeInodeItem, 0)
	if err == nil {
		t.Fatal("searching for an object id nothing owns returned no error")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("a B-tree miss must still be ErrNotFound: %v", err)
	}
	if errors.Is(err, iofs.ErrNotExist) {
		t.Fatal("a B-tree search that found nothing now reads as fs.ErrNotExist: " +
			"a server will answer 404 for a corrupt image and the fault will go unnoticed")
	}
}
