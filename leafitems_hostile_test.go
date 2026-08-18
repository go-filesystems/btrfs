// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package filesystem_btrfs

import (
	"encoding/binary"
	"runtime"
	"testing"
	"time"
)

// nritems in a node header is attacker-controlled and was passed straight to
// make() as a capacity hint. The loop below it is correctly bounded by the
// buffer, so nothing was ever read out of range -- but the reservation happened
// first, and a single corrupted byte made it 4.29e9 items.
func TestParseLeafItemsDoesNotAllocateFromTheHeader(t *testing.T) {
	const nodeSize = 16384
	buf := make([]byte, nodeSize)

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	items := parseLeafItems(buf, 0xFFFFFFFF)
	runtime.ReadMemStats(&after)

	if n := after.TotalAlloc - before.TotalAlloc; n > 1<<26 {
		t.Errorf("parseLeafItems reserved %d bytes for a %d-byte node", n, nodeSize)
	}
	if max := (nodeSize - nodeHdrSize) / itemSize; len(items) > max {
		t.Errorf("returned %d items, more than the %d that fit in the node", len(items), max)
	}
}

// The end-to-end reproducer: one byte, 0xED, at offset 135267 of the test
// image -- inside the root node's header. Measured on the unfixed code it
// allocated 237 GB and the fuzz worker was killed.
func TestMountDoesNotOOMOnACorruptedNodeHeader(t *testing.T) {
	img := buildTestImageBytes()
	img[135267] = 0xed
	updateSuperblockCRC(img[superblockOffset : superblockOffset+sbfSize])

	done := make(chan uint64, 1)
	go func() {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		mountBytes(t, img)
		runtime.ReadMemStats(&after)
		done <- after.TotalAlloc - before.TotalAlloc
	}()
	select {
	case n := <-done:
		if n > 1<<30 {
			t.Errorf("mounting a one-byte-corrupted image allocated %d bytes", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mounting a one-byte-corrupted image took more than 5s")
	}
}

// The clamp must not be refusing valid nodes: a well-formed leaf still yields
// exactly the items it declares.
func TestParseLeafItemsStillReadsAValidNode(t *testing.T) {
	img := buildTestImageBytes()
	node := img[testRootPhys : testRootPhys+testNodeSize]
	n := binary.LittleEndian.Uint32(node[0x60:])
	if n == 0 {
		t.Skip("test image root node declares no items")
	}
	if got := parseLeafItems(node, n); len(got) != int(n) {
		t.Errorf("parseLeafItems returned %d items, want the %d the node declares", len(got), n)
	}
}
