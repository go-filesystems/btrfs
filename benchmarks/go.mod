// Nested module: isolates the benchmark harness (a standalone main package)
// from the library's go.mod so it is NOT part of `go list ./...` and never
// affects the coverage floor. See BENCHMARKS.md.
module github.com/go-filesystems/btrfs/benchmarks

go 1.26.4

require (
	github.com/go-filesystems/btrfs v0.1.0
	github.com/go-filesystems/interface v0.3.0
)

require (
	github.com/anchore/go-lzo v0.1.1 // indirect
	github.com/go-volumes/gpt v0.0.0-20260830080217-f939ebaffdf6 // indirect
	github.com/go-volumes/safeio v0.0.0-20260830080216-c99e29c86f27 // indirect
	github.com/klauspost/compress v1.19.2 // indirect
)

replace github.com/go-filesystems/btrfs => ..
