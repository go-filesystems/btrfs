module github.com/go-filesystems/btrfs

go 1.26.4

require (
	github.com/go-filesystems/interface v0.0.0
	github.com/klauspost/compress v1.18.6
)

require (
	github.com/anchore/go-lzo v0.1.0
	github.com/go-volumes/gpt v0.0.0-20260622072431-e1d6ba3b531c
	github.com/go-volumes/safeio v0.0.0-20260622072324-7f8eb19f6f8c
)

replace github.com/go-diskimages/qcow2 => ../../go-diskimages/qcow2

replace github.com/go-filesystems/interface => ../interface
