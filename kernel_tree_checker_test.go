package filesystem_btrfs

import (
	"encoding/binary"
	"fmt"
	"path/filepath"
	"testing"
)

// These pin what the Linux tree-checker (fs/btrfs/tree-checker.c) demands of
// what this writer emits, found by go-filesystems/kerneljudge against a real
// kernel (#39, #40, #41): the kernel refuses a whole filesystem over any of them.

func openForItems(t *testing.T) (FS, *btrfsFS) {
	t.Helper()
	img := filepath.Join(t.TempDir(), "a.img")
	f, err := Format(img, 64<<20, FormatConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f.(FS), f.(*btrfsFS)
}

func TestRegularExtentFieldsAreSectorAligned(t *testing.T) {
	f, b := openForItems(t)
	for _, n := range []int{2049, 4095, 4097, 65537, 100000} {
		p := fmt.Sprintf("/f%d", n)
		if err := f.WriteFile(p, make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
		in, err := pathLookup(b.f, b.partOffset, b.sb, b.fsTreeRoot, p)
		if err != nil {
			t.Fatal(err)
		}
		items, err := collectPrefixItems(b.f, b.partOffset, b.sb, b.fsTreeRoot, in.num, typeExtentData)
		if err != nil || len(items) == 0 {
			t.Fatalf("%d bytes: items %d, %v", n, len(items), err)
		}
		sector := uint64(b.sb.sectorSize)
		for _, m := range items {
			d := m.data
			le := binary.LittleEndian
			for name, off := range map[string]int{"ram_bytes": extDataOffRamBytes, "disk_num_bytes": extDataOffDiskNumBytes, "num_bytes": extDataOffNumBytes, "offset": extDataOffOffset} {
				if v := le.Uint64(d[off:]); v%sector != 0 {
					t.Errorf("%d-byte file: %s = %d is not a multiple of %d", n, name, v, sector)
				}
			}
		}
		got, err := f.ReadFile(p)
		if err != nil || len(got) != n {
			t.Fatalf("%d bytes read back as %d (%v)", n, len(got), err)
		}
	}
}

func TestEmptyFileHasNoExtentItem(t *testing.T) {
	f, b := openForItems(t)
	if err := f.WriteFile("/e", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	in, err := pathLookup(b.f, b.partOffset, b.sb, b.fsTreeRoot, "/e")
	if err != nil {
		t.Fatal(err)
	}
	if items, _ := collectPrefixItems(b.f, b.partOffset, b.sb, b.fsTreeRoot, in.num, typeExtentData); len(items) != 0 {
		t.Fatalf("an empty file has %d EXTENT_DATA items", len(items))
	}
	if got, err := f.ReadFile("/e"); err != nil || len(got) != 0 {
		t.Fatalf("read back %d bytes, %v", len(got), err)
	}
}

func TestXattrItemHasTheXattrDirType(t *testing.T) {
	f, b := openForItems(t)
	if err := f.WriteFile("/x", []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.SetXattr("/x", "user.kj", []byte("v")); err != nil {
		t.Fatal(err)
	}
	in, _ := pathLookup(b.f, b.partOffset, b.sb, b.fsTreeRoot, "/x")
	buf, it, err := searchTree(b.f, b.partOffset, b.sb, b.fsTreeRoot, in.num, typeXattrItem, hashDirName("user.kj"))
	if err != nil {
		t.Fatal(err)
	}
	if typ := it.data(buf)[0x1D]; typ != ftXattr {
		t.Fatalf("xattr item dir type %d, want %d (BTRFS_FT_XATTR)", typ, ftXattr)
	}
	if v, err := f.GetXattr("/x", "user.kj"); err != nil || string(v) != "v" {
		t.Fatalf("read back %q, %v", v, err)
	}
}

func TestTruncateKeepsExtentNumBytesAligned(t *testing.T) {
	f, b := openForItems(t)
	if err := f.WriteFile("/t", make([]byte, 20000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate("/t", 5000); err != nil {
		t.Fatal(err)
	}
	in, _ := pathLookup(b.f, b.partOffset, b.sb, b.fsTreeRoot, "/t")
	items, _ := collectPrefixItems(b.f, b.partOffset, b.sb, b.fsTreeRoot, in.num, typeExtentData)
	for _, m := range items {
		if v := binary.LittleEndian.Uint64(m.data[extDataOffNumBytes:]); v%uint64(b.sb.sectorSize) != 0 {
			t.Errorf("num_bytes %d after truncate is not sector aligned", v)
		}
	}
	if got, err := f.ReadFile("/t"); err != nil || len(got) != 5000 {
		t.Fatalf("read back %d bytes, %v", len(got), err)
	}
}
