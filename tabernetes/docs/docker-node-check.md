# Checks with a joined Docker node

The CKAD features of #19 to #34 were built and tested on tab nodes. Issue #56
asks for the same checks on a Docker node, which runs the real kubelet,
containerd and runc. `docker-node-check/` does that without a person at the
keyboard: headless Chromium runs the page, a script types `join`, starts the
node container and pastes its `accept` line.

It needs a Docker daemon that can run a privileged or nearly privileged
container, a built site and about 15 minutes. The workflow
`.github/workflows/docker-check.yaml` runs the steps `images`, `join` and
`wasi-*` on a GitHub runner.

## What is in it

- `browser.mjs` starts Chromium with the page and keeps it open. It writes
  every console line of the page to `browser-console.log`.
- `tab.mjs` connects to that browser on the DevTools port:
  `run '<command line>'` runs a command in the tab's shell, `join` and
  `accept '<line>'` drive the join, `status`, `screen` and `logs` show the
  status line, the terminal and the control plane log.
- `check.sh` runs the steps `images join apply coredns storage metrics
  ingress sanity wasi-images wasi-command wasi-proxy wasi-sockets wasi-udp
  leave` and prints PASS, FAIL or SKIP for each.
- `manifests.yaml` has the test objects. All pods run on `node-1` and use
  busybox.
- `wasi-*.sh` and `wasi-*.yaml` are the steps for WebAssembly components and
  their objects, see section 10.2 of `wasi-sockets.md`. `wasi-images` imports
  the test images from the OCI mirror of the built site (`OCI`, default
  `.build/site/oci`).

## How to run it

You need Docker, Node.js with Playwright and its Chromium, jq, and the site:

```bash
mise run build && mise run site
python3 -m http.server -d .build/site 8080 &
node docs/docker-node-check/browser.mjs &   # waits for the terminal
docs/docker-node-check/check.sh             # all steps, output in ./out
```

`browser.mjs` opens `http://127.0.0.1:8080/#boot`, `SITE_URL` changes that.
Run single steps with `check.sh join apply`, or single commands:

```bash
node docs/docker-node-check/tab.mjs run 'kubectl get pods -A -o wide'
node docs/docker-node-check/tab.mjs screen 30
```

The shell of the tab runs one command line at a time, so do not start two
`tab.mjs run` at once.

Settings of `check.sh`, as environment variables:

- `NODE_FLAGS`: the `docker run` flags. The default is what `join` prints,
  without `--pull always`.
- `IMPORT=1`: copy the host's `ctr` into the node and import busybox and
  local-path-provisioner from the host's Docker.
- `OOM_WORKAROUND=1`: let containerd in the node keep the `oom_score_adj` of
  its parent, see below.

## Known limits

- **A host with cgroups v1 needs `--privileged`.** With the flags of `join`
  the agent runs in a user namespace and mounts `cgroup2`. On a cgroups v1
  host that tree has no controllers, and every pod sandbox fails with
  `openat2 /sys/fs/cgroup/kubepods/.../cpu.weight: no such file or directory`.
  The node joins but stays NotReady.
- **Nested Docker without `CAP_SYS_RESOURCE`.** Some sandboxes run dockerd
  without this capability. Then runc cannot lower `oom_score_adj`, and every
  sandbox fails with `failed to update /proc/self/oom_score_adj: Permission
  denied`, also with `--privileged`. `OOM_WORKAROUND=1` sets
  `restrict_oom_score_adj` in the node's containerd config.
- **The node has no internet without a proxy.** It pulls only what its image
  brings (the image mirror of `node/agent/imagemirror.go`). Images from Docker
  Hub need `IMPORT=1` in a node image built before the mirror fix of #56.
- **The real ingress-nginx controller** comes from `registry.k8s.io` and is not
  in the node image. Without that registry the controller stays on `tab`, and
  the `ingress` step checks only the tab controller with a backend on node-1.
- **`leave` takes about 6 minutes.** A lost Docker node stays `NotReady`, like
  in a real cluster without a cloud controller. Its pods move only after the
  default 300 second toleration for `not-ready`, and the old pods stay
  `Terminating` until `kubectl delete node node-1`.
- **The host of a Docker node does not reach tab pods (#61).** Pods of the
  Docker node reach them, by pod IP and by service IP. A connection from the
  node's own network namespace times out: the kernel drops it on the pod link
  (`tabpods1`), because its source is an address of the node itself. So a
  NodePort on the Docker node does not answer while the only endpoint is a pod
  on `tab`, for example the Ingress on `<node IP>:30080`.
