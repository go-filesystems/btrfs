// Command bench measures go-filesystems/btrfs Format and read throughput.
//
// It is part of the performance-parity harness (see BENCHMARKS.md). It is a
// standalone main package and is excluded from the library's coverage gate.
//
// Subcommands:
//
//	bench format <image> <sizeBytes>   -- Format an empty image, print wall time.
//	bench read   <image>               -- Open image, walk + read every file,
//	                                      print files, bytes, wall time.
package main

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strconv"
	"time"

	btrfs "github.com/go-filesystems/btrfs"
	filesystem "github.com/go-filesystems/interface"
)

func main() {
	if len(os.Args) < 2 {
		fail("usage: bench <format|read> ...")
	}
	switch os.Args[1] {
	case "format":
		doFormat(os.Args[2:])
	case "read":
		doRead(os.Args[2:])
	default:
		fail("unknown subcommand %q", os.Args[1])
	}
}

func doFormat(args []string) {
	if len(args) != 2 {
		fail("usage: bench format <image> <sizeBytes>")
	}
	img := args[0]
	size, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		fail("bad size: %v", err)
	}
	_ = os.Remove(img)
	start := time.Now()
	fs, err := btrfs.Format(img, size, btrfs.FormatConfig{Label: "bench"})
	if err != nil {
		fail("Format: %v", err)
	}
	if err := fs.Close(); err != nil {
		fail("Close: %v", err)
	}
	dur := time.Since(start)
	fmt.Printf("FORMAT ok size=%d wall_ns=%d\n", size, dur.Nanoseconds())
}

func doRead(args []string) {
	if len(args) != 1 {
		fail("usage: bench read <image>")
	}
	img := args[0]
	start := time.Now()
	fs, err := btrfs.Open(img, -1)
	if err != nil {
		fail("Open: %v", err)
	}
	defer fs.Close()
	var files, bytesRead int64
	walk(fs, "/", &files, &bytesRead)
	dur := time.Since(start)
	fmt.Printf("READ ok files=%d bytes=%d wall_ns=%d\n", files, bytesRead, dur.Nanoseconds())
}

// walk recursively reads every regular file under dir.
func walk(fs filesystem.Filesystem, dir string, files, bytesRead *int64) {
	entries, err := fs.ListDir(dir)
	if err != nil {
		fail("ListDir %q: %v", dir, err)
	}
	type ent struct{ name string }
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		n := e.Name()
		if n == "." || n == ".." {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := path.Join(dir, n)
		st, err := fs.Stat(p)
		if err != nil {
			fail("Stat %q: %v", p, err)
		}
		mode := st.Mode()
		switch {
		case mode&0xF000 == 0x4000: // S_IFDIR
			walk(fs, p, files, bytesRead)
		case mode&0xF000 == 0x8000, mode&0xF000 == 0: // S_IFREG (0 = FAT has no type bit for files in some encodings)
			data, err := fs.ReadFile(p)
			if err != nil {
				fail("ReadFile %q: %v", p, err)
			}
			*files++
			*bytesRead += int64(len(data))
		default:
			// symlink / device / fifo -- skip payload, count as visited
		}
	}
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "bench: "+format+"\n", a...)
	os.Exit(1)
}
