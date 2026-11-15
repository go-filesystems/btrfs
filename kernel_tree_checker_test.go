package filesystem_btrfs

import (
	"bytes"
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
		if err := f.WriteFile(p, pattern(n), 0o644); err != nil {
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
		if err != nil || !bytes.Equal(got, pattern(n)) {
			t.Fatalf("%d bytes read back as %d different bytes (%v)", n, len(got), err)
		}
	}
}

// pattern is non-zero data: all-zero data takes the hole path and would test
// nothing of the data extents.
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i%251 + 1)
	}
	return b
}

func TestShrinkThenGrowReadsZeros(t *testing.T) {
	f, _ := openForItems(t)
	if err := f.WriteFile("/g", pattern(20000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate("/g", 5000); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate("/g", 8000); err != nil {
		t.Fatal(err)
	}
	got, err := f.ReadFile("/g")
	if err != nil || len(got) != 8000 {
		t.Fatalf("%d bytes, %v", len(got), err)
	}
	if !bytes.Equal(got[:5000], pattern(5000)) {
		t.Error("the kept bytes changed")
	}
	if !bytes.Equal(got[5000:], make([]byte, 3000)) {
		t.Error("bytes 5000..8000 are not zero: the discarded data came back")
	}
}

func TestReusedExtentTailDoesNotLeakIntoAGrownFile(t *testing.T) {
	f, _ := openForItems(t)
	if err := f.WriteFile("/old", pattern(20000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.DeleteFile("/old"); err != nil {
		t.Fatal(err)
	}
	if err := f.WriteFile("/new", pattern(5000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate("/new", 8000); err != nil {
		t.Fatal(err)
	}
	got, _ := f.ReadFile("/new")
	if len(got) != 8000 || !bytes.Equal(got[5000:], make([]byte, 3000)) {
		t.Error("the grown tail holds bytes of the deleted file")
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
	if err := f.WriteFile("/t", pattern(20000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate("/t", 5000); err != nil {
		t.Fatal(err)
	}
	in, _ := pathLookup(b.f, b.partOffset, b.sb, b.fsTreeRoot, "/t")
	items, _ := collectPrefixItems(b.f, b.partOffset, b.sb, b.fsTreeRoot, in.num, typeExtentData)
	for _, m := range items {
		for name, off := range map[string]int{"ram_bytes": extDataOffRamBytes, "num_bytes": extDataOffNumBytes, "offset": extDataOffOffset} {
			if v := binary.LittleEndian.Uint64(m.data[off:]); v%uint64(b.sb.sectorSize) != 0 {
				t.Errorf("%s %d after truncate is not sector aligned", name, v)
			}
		}
	}
	if got, err := f.ReadFile("/t"); err != nil || len(got) != 5000 {
		t.Fatalf("read back %d bytes, %v", len(got), err)
	}
}
