module github.com/go-filesystems/btrfs

go 1.26.0

require (
	github.com/go-filesystems/interface v0.0.0
	github.com/klauspost/compress v1.18.6
)

require github.com/anchore/go-lzo v0.1.0

replace github.com/go-diskimages/qcow2 => ../../go-diskimages/qcow2

replace github.com/go-filesystems/interface => ../interface
