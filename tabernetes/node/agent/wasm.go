package main

// WebAssembly pods (runtimeClassName: wasmtime) run with the wasmtime shim of
// runwasi. The RuntimeClass "wasmtime" of the addons has the handler
// "wasmtime" and selects nodes with the label below, so the same manifest runs
// on this node and on the tab node.
//
// The shim does not call runc: it creates the containers itself with
// libcontainer (youki). In the user namespace (userns.go) libcontainer mounts
// sysfs read-write first and makes it read-only afterwards, which the kernel
// refuses there (see runc.go). So containerd starts the shim through the agent
// (as tabernetes-wasmtime), which turns the sysfs mounts into read-only bind
// mounts of the node's /sys, as runc does in a rootless container.

import (
	"log"
	"os"
	"os/exec"
	"syscall"
)

const (
	wasmShim        = "containerd-shim-wasmtime-v1"
	wasmShimWrapper = "/usr/local/bin/tabernetes-wasmtime"
	wasmHandler     = "wasmtime"
	wasmNodeLabel   = "tabernetes.io/wasm=true"
)

// wasmRuntime reports whether this node can run WebAssembly pods. Under
// Docker's seccomp profile (--cap-add instead of the user namespace) it cannot:
// the profile blocks pivot_root, and libcontainer has no --no-pivot like runc.
// The node gets no label then, and wasm pods go to other nodes.
func wasmRuntime() bool {
	if _, err := exec.LookPath(wasmShim); err != nil {
		return false
	}
	if seccompFiltered() {
		log.Print("no WebAssembly pods on this node: the seccomp profile blocks pivot_root")
		return false
	}
	return true
}

// wasmContainerdConfig is the CRI runtime for the handler of the RuntimeClass.
// containerd finds the shim in PATH by the runtime type.
func wasmContainerdConfig() string {
	c := "[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes." + wasmHandler + "]\n" +
		"  runtime_type = 'io.containerd.wasmtime.v1'\n"
	if inUserNS {
		c += "  runtime_path = '" + wasmShimWrapper + "'\n"
	}
	return c
}

// runWasmShimWrapper runs the shim. containerd starts it with "start" in the
// bundle directory for every container, also for the further containers of a
// pod, which go to the shim of the pod. config.json is written by then.
func runWasmShimWrapper(args []string) {
	if len(args) > 0 && args[len(args)-1] == "start" && runningInUserNS() {
		if err := bindSysfs("config.json"); err != nil {
			log.Printf("tabernetes-wasmtime: %v", err)
		}
	}
	shim, err := exec.LookPath(wasmShim)
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(syscall.Exec(shim, append([]string{wasmShim}, args...), os.Environ()))
}

// bindSysfs replaces the sysfs mounts of a spec with recursive read-only bind
// mounts of /sys. A pod sees the node's network devices in /sys/class/net then.
func bindSysfs(path string) error {
	return editMounts(path, func(mount map[string]any) bool {
		if mount["type"] != "sysfs" {
			return false
		}
		opts := []any{"rbind", "ro", "rro"}
		old, _ := mount["options"].([]any)
		for _, o := range old {
			switch o {
			case "nosuid", "nodev", "noexec":
				opts = append(opts, o)
			}
		}
		mount["type"] = "bind"
		mount["source"] = "/sys"
		mount["options"] = opts
		return true
	})
}
