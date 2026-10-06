// Nested module: isolates the benchmark harness (a standalone main package)
// from the library's go.mod so it is NOT part of `go list ./...` and never
// affects the coverage floor. See BENCHMARKS.md.
module github.com/go-filesystems/btrfs/benchmarks

go 1.27.1

require (
	github.com/go-filesystems/btrfs v0.3.0
	github.com/go-filesystems/interface v0.4.0
)

require (
	github.com/anchore/go-lzo v0.1.1 // indirect
	github.com/go-volumes/gpt v0.2.0 // indirect
	github.com/go-volumes/safeio v0.0.0-20260831125406-d8f54b2890d4 // indirect
	github.com/klauspost/compress v1.20.1 // indirect
)

replace github.com/go-filesystems/btrfs => ..
