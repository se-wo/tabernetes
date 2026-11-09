package main

// STUB: replaced by tabnode_wasi.go (wasi agent). Delete this file then.

import (
	"context"
	"errors"

	v1 "k8s.io/api/core/v1"
)

// isWasmPod reports whether the pod runs WebAssembly modules (runtimeClassName wasmtime and friends).
func isWasmPod(pod *v1.Pod) bool { return false }

// startWasm runs the container as a WASI module in a Web Worker.
func (c *ctr) startWasm(ctx context.Context, logs *logBuf) (*process, error) {
	return nil, errors.New("WebAssembly containers are not implemented yet")
}
