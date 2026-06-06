package filesystem_btrfs

import (
	"archive/tar"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// extractRAIDFixture decompresses testdata/raid/<profile>.tar.zst into a temp
// directory and returns the list of *.img file paths in lexical order
// (d0.img, d1.img, ...). Each btrfs RAID fixture was created with
// `mkfs.btrfs -d <profile> -m <profile> -f /dev/loopN ...` against 128 MiB
// per-device loopbacks, then populated with /hello.txt and /sub/blob.bin
// before unmount. See docker/genprofile in the originating script.
func extractRAIDFixture(t *testing.T, profile string) []string {
	t.Helper()
	src := filepath.Join("testdata", "raid", profile+".tar.zst")
	f, err := os.Open(src)
	if err != nil {
		t.Fatalf("open fixture %s: %v", src, err)
	}
	defer f.Close()
	zr, err := zstd.NewReader(f)
	if err != nil {
		t.Fatalf("zstd reader: %v", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	dir := t.TempDir()
	var out []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		dst := filepath.Join(dir, hdr.Name)
		w, err := os.Create(dst)
		if err != nil {
			t.Fatalf("create %s: %v", dst, err)
		}
		if _, err := io.Copy(w, tr); err != nil {
			t.Fatalf("extract %s: %v", dst, err)
		}
		w.Close()
		out = append(out, dst)
	}
	return out
}

// raidProfileExpectations describes what the test fixture contains.
type raidProfileExpectations struct {
	helloContent string
	blobMD5      string
	blobSize     int
}

func raidExpectations(profile string) raidProfileExpectations {
	return raidProfileExpectations{
		helloContent: fmt.Sprintf("hello-from-%s\n", profile),
		blobMD5: map[string]string{
			"single": "9528d8a76a41a9809f82229ed77d0191",
			"raid0":  "ec625662bcc25b4309c77b840118938b",
			"raid1":  "3e70e020d7a312e412f6eb308823f3a4",
			"raid10": "38a3e5848dea00ae6f0b93386ea60430",
			"raid5":  "a176df37793e09c064251c733ac2fa5f",
			"raid6":  "1ecb66994b91e6b9cfaae72c6196196c",
		}[profile],
		blobSize: 64 * 1024,
	}
}

func checkExpectations(t *testing.T, fs FS, profile string) {
	t.Helper()
	exp := raidExpectations(profile)
	hello, err := fs.ReadFile("/hello.txt")
	if err != nil {
		t.Fatalf("read /hello.txt: %v", err)
	}
	if string(hello) != exp.helloContent {
		t.Fatalf("hello.txt = %q want %q", string(hello), exp.helloContent)
	}
	blob, err := fs.ReadFile("/sub/blob.bin")
	if err != nil {
		t.Fatalf("read /sub/blob.bin: %v", err)
	}
	if len(blob) != exp.blobSize {
		t.Fatalf("blob.bin size = %d want %d", len(blob), exp.blobSize)
	}
	sum := md5.Sum(blob)
	got := hex.EncodeToString(sum[:])
	if got != exp.blobMD5 {
		t.Fatalf("blob.bin md5 = %s want %s", got, exp.blobMD5)
	}
}
