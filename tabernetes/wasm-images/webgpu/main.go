//go:build wasip1

// wasi-webgpu is the demo of WebGPU for WebAssembly pods: a Go program built
// with GOOS=wasip1 GOARCH=wasm that runs compute shaders on the GPU of the
// browser through the host module tabernetes_webgpu (web/src/wasi.worker.js).
// The kubelet of the tab hands the GPU only to containers with a ResourceClaim
// of the DeviceClass webgpu.tabernetes.io (k8s/tabernetes/tabnode_gpu.go).
//
// It prints the adapter, checks a kernel against the CPU and draws the
// Mandelbrot set from the GPU in ASCII. -every repeats the drawing.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"
	"unsafe"
)

//go:wasmimport tabernetes_webgpu adapter_info
func adapterInfo(buf unsafe.Pointer, n uint32) int32

//go:wasmimport tabernetes_webgpu compute
func compute(code unsafe.Pointer, codeLen uint32, data unsafe.Pointer, dataLen uint32, x, y, z uint32) int32

//go:wasmimport tabernetes_webgpu last_error
func lastError(buf unsafe.Pointer, n uint32) int32

// errorText is the message of the last failed call.
func errorText() string {
	buf := make([]byte, 1024)
	n := lastError(unsafe.Pointer(&buf[0]), uint32(len(buf)))
	return string(buf[:min(int(n), len(buf))])
}

func info() (string, error) {
	buf := make([]byte, 4096)
	n := adapterInfo(unsafe.Pointer(&buf[0]), uint32(len(buf)))
	if n < 0 {
		return "", fmt.Errorf("%s", errorText())
	}
	if int(n) > len(buf) {
		buf = make([]byte, n)
		n = adapterInfo(unsafe.Pointer(&buf[0]), uint32(len(buf)))
	}
	return string(buf[:n]), nil
}

// run dispatches the shader over data (u32 values, read-write) with
// workgroups along x.
func run(shader string, data []uint32, workgroups uint32) error {
	code := []byte(shader)
	raw := make([]byte, 4*len(data))
	for i, v := range data {
		binary.LittleEndian.PutUint32(raw[4*i:], v)
	}
	rc := compute(unsafe.Pointer(&code[0]), uint32(len(code)), unsafe.Pointer(&raw[0]), uint32(len(raw)), workgroups, 1, 1)
	runtime.KeepAlive(code)
	runtime.KeepAlive(raw)
	if rc != 0 {
		return fmt.Errorf("%s", errorText())
	}
	for i := range data {
		data[i] = binary.LittleEndian.Uint32(raw[4*i:])
	}
	return nil
}

const check = `
@group(0) @binding(0) var<storage, read_write> data: array<u32>;
@compute @workgroup_size(64)
fn main(@builtin(global_invocation_id) id: vec3u) {
  if (id.x < arrayLength(&data)) {
    let v = data[id.x];
    data[id.x] = v * v + 7u;
  }
}`

const mandelbrot = `
@group(0) @binding(0) var<storage, read_write> data: array<u32>;
const W = %du;
const H = %du;
const MAX = %du;
@compute @workgroup_size(64)
fn main(@builtin(global_invocation_id) id: vec3u) {
  if (id.x >= W * H) { return; }
  let c = vec2f(-2.2 + 3.0 * f32(id.x %% W) / f32(W), -1.2 + 2.4 * f32(id.x / W) / f32(H));
  var z = vec2f(0.0, 0.0);
  var i = 0u;
  while (i < MAX && dot(z, z) < 4.0) {
    z = vec2f(z.x * z.x - z.y * z.y, 2.0 * z.x * z.y) + c;
    i++;
  }
  data[id.x] = i;
}`

func main() {
	width := flag.Int("width", 72, "columns of the drawing")
	height := flag.Int("height", 24, "rows of the drawing")
	every := flag.Duration("every", 0, "draw again after this long, forever (0: once)")
	flag.Parse()

	fmt.Printf("wasi-webgpu: a Go program (%s, %s/%s) with WebGPU through DRA\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	fmt.Printf("%s=%s\n", "TABERNETES_WEBGPU_DEVICE", os.Getenv("TABERNETES_WEBGPU_DEVICE"))
	desc, err := info()
	if err != nil {
		fmt.Fprintln(os.Stderr, "no GPU:", err)
		os.Exit(1)
	}
	fmt.Println("device:", desc)

	// A kernel with an exact answer: the GPU must agree with the CPU.
	data := make([]uint32, 1<<16)
	for i := range data {
		data[i] = uint32(i)
	}
	start := time.Now()
	if err := run(check, data, uint32(len(data)/64)); err != nil {
		fmt.Fprintln(os.Stderr, "compute:", err)
		os.Exit(1)
	}
	for i, v := range data {
		if want := uint32(i)*uint32(i) + 7; v != want {
			fmt.Fprintf(os.Stderr, "GPU and CPU disagree at %d: %d, want %d\n", i, v, want)
			os.Exit(1)
		}
	}
	fmt.Printf("checked: %d values from the GPU match the CPU (%s)\n", len(data), time.Since(start).Round(time.Millisecond))

	const maxIter = 64
	ramp := " .:-=+*#%@"
	for {
		w, h := *width, *height
		cells := make([]uint32, w*h)
		start := time.Now()
		if err := run(fmt.Sprintf(mandelbrot, w, h, maxIter), cells, uint32((w*h+63)/64)); err != nil {
			fmt.Fprintln(os.Stderr, "compute:", err)
			os.Exit(1)
		}
		var b strings.Builder
		for y := range h {
			for x := range w {
				it := cells[y*w+x]
				if it >= maxIter {
					b.WriteByte('@')
				} else {
					b.WriteByte(ramp[int(it)*(len(ramp)-1)/maxIter])
				}
			}
			b.WriteByte('\n')
		}
		fmt.Print(b.String())
		fmt.Printf("the Mandelbrot set, %dx%d cells from the GPU in %s\n", w, h, time.Since(start).Round(time.Millisecond))
		if *every <= 0 {
			return
		}
		time.Sleep(*every)
	}
}
