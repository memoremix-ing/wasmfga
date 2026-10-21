//go:build wasip1

package main

import (
	"unsafe"

	"github.com/openfga/openfga/wasmfga"
)

// The ABI has one bounded input buffer and one output buffer per instance.
// Hosts must serialize calls and copy the output before the next alloc/run.
var input []byte
var output []byte

//go:wasmexport alloc
func alloc(size uint32) uint32 {
	output = nil
	if size == 0 || size > engine.MaxInputBytes {
		input = nil
		return 0
	}
	input = make([]byte, size)
	return uint32(uintptr(unsafe.Pointer(&input[0])))
}

//go:wasmexport run
func run() uint64 {
	output = engine.Run(input)
	input = nil
	return uint64(uint32(uintptr(unsafe.Pointer(&output[0]))))<<32 | uint64(len(output))
}

func main() {}
