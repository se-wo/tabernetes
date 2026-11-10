# Tabernetes

A real Kubernetes control plane in a browser tab. kube-apiserver,
kube-controller-manager, kube-scheduler and kubectl are compiled with
`GOOS=js GOARCH=wasm` into a single binary. The page
(original: [jkroepke.de/tabernetes](https://jkroepke.de/tabernetes/), this
version: [se-wo.github.io/tabernetes](https://se-wo.github.io/tabernetes/))
boots it in a terminal.
Pods run right away on the node `tab`, a kubelet inside the same binary. A
real node can join with Docker. The node talks to the browser over WebRTC.

## How it works

```text
browser tab (single wasm binary)                     docker run tabernetes
+---------------------------------------------+      +---------------------------+
| kubectl  --->  kube-apiserver :6443         |      | kubelet, kube-proxy, pods |
|                   |                         |      |        |                  |
| controller-manager, scheduler               |      |   127.0.0.1:6443          |
|                   |                         |      |   10.0.0.1:6443           |
|          etcd shim :2379 (in memory)       |      |        |                  |
|                                             |      |   tabernetes agent        |
| <ip>:10250 ---------- WebRTC data channels -------------> kubelet :10250    |
| 127.0.0.1:6443 <----- (one per TCP stream) <------------- 127.0.0.1:6443    |
+---------------------------------------------+      +---------------------------+
```

- Go's `js/wasm` port has an in-process fake network. `net.Listen` and
  `net.Dial` work between goroutines of the same binary. All components
  talk to each other over it, without any change to their code.
- etcd is replaced by `k8s/tabernetes/etcdshim`. It is a small in-memory
  implementation of the etcd v3 gRPC API (KV, Txn, Watch, Lease). It
  implements only what kube-apiserver uses. It persists nothing.
- The browser has no file system. `web/src/fs.js` is an in-memory file
  system with the Node.js callback API that Go's `syscall/js` expects.
- The terminal runs a real POSIX shell: [mvdan.cc/sh](https://github.com/mvdan/sh),
  a shell interpreter in Go, compiled into the same binary. Pipes, variables,
  loops, `$(...)`, redirects and here-documents work. There are no processes,
  so every command is a function: kubectl runs in-process, the shell tools
  live in `k8s/tabernetes/coreutils*.go`, and
  `join`, `logs` and `clear` are JavaScript. Files land in the in-memory file
  system, so `kubectl get cm x -o yaml > cm.yaml` works.
  `tar` and `gzip` (`k8s/tabernetes/coreutils_tar.go`) work in the terminal and
  in containers, so `kubectl cp` works in both directions.
  Tab completes commands and files, and kubectl through its own `__complete`
  command, so resource types and names come from the cluster.
  `wget` downloads through the `fetch()` of the browser, so it only works for
  servers that allow CORS, for example `raw.githubusercontent.com`.
  `localhost` and `127.0.0.1` stay in the in-process network of the Wasm, so
  `k port-forward deploy/web 8080:80 &` and `wget -qO- localhost:8080` work.
- `helm` (Helm v4, `HELM_VERSION`) runs in-process like kubectl
  (`k8s/tabernetes/helm.go`). Releases are Secrets in the cluster, repositories
  and the cache are files of the terminal. Helm downloads chart repositories with
  `fetch()`, so a repository must send CORS headers: repositories on GitHub
  Pages work directly. `https://charts.bitnami.com/bitnami` and
  `oci://registry-1.docker.io/bitnamicharts/<chart>` come from `charts/bitnami/`
  next to the page instead: small charts in the style of Bitnami's
  (`tabernetes/charts/`, nginx, apache and redis) that run the official images on
  the tab node, so `helm repo add bitnami https://charts.bitnami.com/bitnami` and
  `helm install web bitnami/nginx` work.
- The shell tools are small Go versions with the common flags: `cp`, `mv`,
  `ln`, `chmod`, `chown`, `stat`, `find`, `diff`, `sed`, `awk`
  ([goawk](https://github.com/benhoyt/goawk)), `jq` and `yq`
  ([gojq](https://github.com/itchyny/gojq)), `date`, `watch`, `less`, `df`,
  `du`, `mount`, `nc`, `ping`, `telnet`, `openssl` and more; `help` lists them
  all. They work in the terminal and in `kubectl exec` of tab node
  containers, where paths go through the root of the container and `nc`,
  `ping` and `telnet` dial from the pod IP. `openssl` covers keys, requests,
  signing and `x509 -text` for the CertificateSigningRequest exercise.
  `kubectl convert` is compiled in, because plugins cannot run in Wasm.
- `history` lists the command lines of the tab, and Ctrl-R searches them
  backwards like Bash. The history ends with the tab.
- `cmd &` runs in the background, `jobs` lists the jobs, `kill %1` stops one.
  Ctrl-C stops the foreground command, `kubectl port-forward` and
  `kubectl proxy` included. On Windows and Linux, Ctrl-C copies instead while
  text is selected, and Ctrl-V pastes.
- `kubectl exec -it`, `run -it`, `attach -it` and `debug -it` get the terminal
  as their TTY: the moby/term package of kubectl asks hooks of the terminal
  (`k8s/_overlay/vendor/github.com/moby/term/term_js.go`, `tty.go`), which
  switch it to raw mode, so every key goes to the container, Ctrl-C included,
  and report its size. On the tab node, a container with a TTY gets what the
  line discipline of a kernel tty does (`tabnode_tty.go`): echo, backspace,
  Ctrl-U, Ctrl-W, Ctrl-C ends the running command, Ctrl-D ends the input, and
  "\n" becomes "\r\n". A shell on a TTY shows a prompt like `/ #` (BusyBox),
  `root@<pod>:/#` (Bash) or its `PS1`.
- `proxy svc/web` shows the service `web` in a panel next to the terminal
  (`-n <namespace>`, `svc/web:8080` or a port name for another port).
  Every request goes through the service proxy of the tab on a connection of
  its own, so each reload of the panel shows the next ready replica, wherever
  it runs: on the tab node, a node in a Web Worker or a Docker node.
  `proxy 8080` shows `localhost:8080` of the Wasm instead, typically
  `kubectl port-forward`. `--tab` opens a new browser tab instead of the panel,
  `proxy --close` closes the panel. A service worker under `tabernetes-proxy/`
  next to the page (`web/proxy-sw.js`) hands every request of the panel to the
  terminal tab, which sends it to the in-process network. It works as long as
  the terminal tab is open; leaving the terminal page unregisters the worker.
  While `kubectl port-forward` runs, the status line under the terminal links
  each of its local ports the same way (`k8s/tabernetes/forwards.go`): a click
  shows it in the panel, Ctrl-click in a new tab.
- `vnc <port>` shows a VNC server on `localhost` of the Wasm (a
  `kubectl port-forward`) on top of the terminal.
  [noVNC](https://github.com/novnc/noVNC) speaks RFB, and `tabernetes.dial`
  (`k8s/tabernetes/dial.go`) gives it a TCP connection into the in-process
  network. `doom` uses it: it starts
  [KubeDoom](https://github.com/storax/kubedoom) on a Docker node with a
  Deployment of five pause pods (`k8s/tabernetes/doom/kubedoom.yaml`), and
  every pod of the default namespace is a monster. Shoot one, and KubeDoom
  deletes the pod. KubeDoom is a real container image, so it needs a node
  from `join`: node affinity keeps it off the nodes with the label
  `tabernetes.io/tab` (the tab node and the nodes in Web Workers). The
  monsters are pause pods and run anywhere, the tab node included.
- `free` shows the WebAssembly memory (wasm32 ends at 4 GiB, and the linear
  memory never shrinks), the Go heap and the number of goroutines.
- `vi` (`k8s/tabernetes/vi.go`) is a small vi for manifests. It runs in raw mode:
  while it runs, the terminal sends every key to it. `kubectl edit` opens it
  in-process, because js/Wasm cannot start an editor process.
- `join` creates a bootstrap token and a WebRTC offer. Signaling is copy and
  paste: the offer goes into the `docker run` command, and `join` then asks
  for the answer of the node (`accept <answer>` works later, too). A status
  line under the terminal shows every node and the state of its WebRTC
  connection. STUN is used for NAT traversal. The node agent
  also advertises `127.0.0.1` for the `-p 127.0.0.1:<port>:<port>/udp`
  mapping. That way it works on Docker Desktop, where the browser cannot
  reach container IPs.

## The tab node

`k8s/tabernetes/tabnode*.go` is a kubelet inside the tab. It registers the
Node `tab` (InternalIP 10.0.0.1, labels `kubernetes.io/arch=wasm`,
`tabernetes.io/tab=true`, `tabernetes.io/wasm=true`) as `system:node:tab`,
keeps its Lease and runs the pods the scheduler binds to it. Like the first
node of kubeadm, `tab` is the control plane node: it has the role label
`node-role.kubernetes.io/control-plane` (set with the admin credentials, as
NodeRestriction forbids a kubelet to set it) and two taints,
`node-role.kubernetes.io/control-plane:PreferNoSchedule` and
`tabernetes.io/tab=true:PreferNoSchedule`. Both are soft, not NoSchedule:
while `tab` is the only node, everything lands here. With two taints, the
scheduler prefers a node in a Web Worker (one taint) over `tab`, and a Docker
node (none) over both. flannel and kube-proxy stay off it (the tab has a
kube-proxy of its own). CoreDNS runs here as a built-in image until a Docker
node is Ready, then moves there.

`kubectl get pods -n kube-system` shows the control plane as static pods, as
on a kubeadm node: `kube-apiserver-tab`, `kube-controller-manager-tab`,
`kube-scheduler-tab` and `etcd-tab` (`tabnode_static.go`). They are mirror
pods of the kubelet, with the manifests in `/etc/kubernetes/manifests`, the
certificates in `/etc/kubernetes/pki` and the kubeconfigs in
`/etc/kubernetes/*.conf`. The command of a pod is the real command line of
its component. `kubectl logs` shows the lines of that component only: a hook
in the shared logger sorts them by the pprof labels of the goroutine
(`complogs.go`). Ready comes from the health endpoints of the components.
`kubectl delete pod` creates the mirror pod again, with the start time of
the running component. `kubectl exec` is refused, like on a distroless image.
The node info shows Tabernetes as the OS and `6.0.0-tabernetes` as the
kernel, as `uname -r` does in a container.

A container is not a process:

- **Native containers** run their command line in a shell of their own
  (mvdan.cc/sh again) as goroutines, with the coreutils of the terminal,
  kubectl (with the service account of the pod, through 10.96.0.1) and the
  programs of the built-in images (`tabnode_images*.go`): nginx (it reads
  nginx.conf and conf.d), httpd, pause, whoami, http-echo, echo-server,
  agnhost, podinfo, hello-app, redis with redis-cli, the ingress-nginx
  controller, the `busybox`/`alpine`/`ubuntu`
  family, `curlimages/curl`, `bitnami/kubectl`. Any other image runs its command
  in the shell. A wrong image fails like on a real node: an unknown Docker Hub
  name such as `ngnix` or a tag such as `nginx:doesnotexist` waits in
  `ErrImagePull` and `ImagePullBackOff` (`tabnode_pull.go`). The tab cannot
  ask a registry, so it knows the official images of Docker Hub and the tags
  of the built-in images; other images pull, unless their name is a typo of a
  built-in one (`curlimages/crul`). Such an image without a command fails with
  `CreateContainerError`. A new image in the pod spec (`kubectl set image`)
  restarts the container with it. Each container sees its own root directory
  with the files of its image and its volume mounts.
- **WebAssembly containers** (`runtimeClassName: wasmtime`, the RuntimeClass
  of runwasi's wasmtime shim) run a WASI preview 1 module in a Web Worker of
  their own (`tabnode_wasi.go`, `web/src/wasi*.js`, with
  [browser_wasi_shim](https://github.com/bjorn3/browser_wasi_shim)). Workers
  are real threads: such a pod does not take CPU from the control plane. The
  browser cannot pull from ghcr.io or Docker Hub (no CORS headers), so the
  page ships an OCI mirror (`wasm-images/`, `images.txt`) next to it; other
  registries are tried over HTTPS. On a Docker node the same manifest runs
  with runwasi.

What works like on a real node: init and sidecar containers, restart
policies with CrashLoopBackOff, Jobs, emptyDir/configMap/secret/projected/
downwardAPI/hostPath volumes (hostPath is the file system of the terminal),
persistentVolumeClaim and generic ephemeral volumes (see below),
env from ConfigMaps, Secrets and the downward API, service links, probes,
graceful deletion, events, `kubectl logs/exec/attach/port-forward`
(`tabnode_server.go` serves the kubelet API on 10.0.0.1:10250 with the CRI
streaming library of Kubernetes).
configMap, secret, projected and downwardAPI volumes look like on a real
node (`tabnode_volwriter.go`): the files sit in a `..<timestamp>` directory,
`..data` links to it and every key links into `..data`. They get `defaultMode`
or `items[].mode`, so `ls -lL` shows `-r--------` for `defaultMode: 0400`.
Every minute the node writes them again (`tabnode_volrefresh.go`), so
`kubectl edit cm` reaches a running pod within about a minute. `subPath`
mounts and immutable objects keep their content, like on a real node.
Projected service account tokens are renewed at 80 % of their lifetime.
- `grpc` probes call `grpc.health.v1.Health/Check` with the gRPC prober of
  the kubelet, so a closed port or `NOT_SERVING` fails with the same
  `Unhealthy` event. `agnhost grpc-health-checking` (`--port`, `--http-port`,
  `--service`, `--delay-unhealthy-sec`; `/make-not-serving` and
  `/make-serving` on the HTTP port) and `podinfo --grpc-port` serve it.
`tabnode_lifecycle.go` adds what the kubelet drives on its own: `postStart`
and `preStop` hooks with `exec`, `httpGet` and `sleep` (a failed postStart
kills the container, preStop counts against the grace period),
`activeDeadlineSeconds` (phase Failed, reason `DeadlineExceeded`) and
ephemeral containers: `kubectl debug -i <pod> --image=busybox` starts one,
and `kubectl logs/exec/attach -c` reach it. `kubectl debug node/tab` runs its
pod with hostNetwork on the node IP and the file system of the terminal under
`/host`.

`tabnode_security.go` reads `securityContext` of the pod and the container.
It is emulation: the commands of the tab node check on behalf of the
container.

- `runAsUser`, `runAsGroup`, `supplementalGroups` and `fsGroup` show in `id`,
  `whoami`, `ps` and `/proc/1/status`. Without them, the user of the image
  counts: most built-in images run as root, `curlimages/curl` as `curl_user`.
  A uid without an entry in `/etc/passwd` shows as
  `uid=1000 gid=0(root) groups=0(root)`.
- Files have an owner and a mode. A user who is not root cannot write to
  `/etc`, but to `/tmp`, an emptyDir and its home (`Permission denied`).
  New files belong to the user, `ls -l` shows it. With `fsGroup`, volumes
  belong to that group and have the setgid bit.
- `readOnlyRootFilesystem: true`: writes outside volume mounts fail with
  `Read-only file system`.
- `runAsNonRoot: true` with a root image: `CreateContainerConfigError` with
  the message of the kubelet.
- `capabilities`, `privileged` and `allowPrivilegeEscalation` show in
  `CapPrm`, `CapEff`, `CapBnd` and `NoNewPrivs` of `/proc/1/status`. Root
  without `DAC_OVERRIDE` passes no permission check.
- nginx fails like the real image as another user or with a read-only root
  file system, until emptyDir volumes cover `/var/cache/nginx` and `/var/run`.

Only the shell and the coreutils check permissions. The servers of the
built-in images write their own files as before, and nothing checks the
execute bit, ports below 1024 or `seccompProfile`.

The kubelet serves `/stats/summary` and `/metrics/resource`, so
`kubectl get --raw /api/v1/nodes/tab/proxy/stats/summary` shows numbers, and
a metrics-server can scrape the node. The numbers are a model
(`tabnode_acct.go`): CPU comes from the commands of the shell and the work of
the built-in programs, `limits.cpu` works like the CFS quota, and the memory
of a container is the size of its image plus what its programs allocate. The
memory of the node comes from the Go runtime. The endpoints take a client
certificate of the cluster CA or a bearer token that the apiserver allows to
get `nodes/stats` or `nodes/metrics`.

The addon metrics-server (`addons/metrics-server.yaml`, the manifest of the
upstream release) serves `metrics.k8s.io`, so `kubectl top` and
HorizontalPodAutoscalers work. The image is a built-in image
(`tabnode_images_metrics.go`, `metricsserver*.go`): an extension apiserver of
`k8s.io/apiserver` behind the aggregator, with delegated authentication and
authorization. Every 15 seconds it scrapes `/metrics/resource` of every
kubelet with its service account token. Unlike upstream, the Deployment stays
on the node `tab`: only its network reaches the kubelets of all nodes. HTTPS
probes skip the certificate check, like the ones of the kubelet.

Pods listen on their pod IP in the in-process network of the Wasm. There is
no loopback per pod: localhost of a pod is its pod IP. `tabnode_net.go` is
kube-proxy and DNS of the tab: it listens on every cluster IP and node port
and forwards to ready endpoints (endpoints on a Docker node through the
port-forward of the apiserver). `wget`, `curl` and `nslookup` resolve service names
with the search path of the pod, in pods and in the terminal:
`k expose deploy web --port 80; wget -qO- web`. Endpoints rotate, but with
`sessionAffinity: ClientIP` a client IP (a pod, or the node IP for the
terminal) keeps its endpoint for `timeoutSeconds`, and
`internalTrafficPolicy: Local` uses the endpoints on the tab node only. The apiserver dials pods
directly (`k8s/patch.go`), so its service and pod proxy work, too.

`tabnode_coredns.go` is the CoreDNS of the tab: the built-in image
`registry.k8s.io/coredns/coredns` logs like the real one, serves `/health`,
`/ready` and `/metrics` as the Corefile asks, and answers DNS over TCP from
the same service proxy. There is no UDP in the tab. `hosts` entries and
`rewrite name` rules of `kubectl edit cm coredns -n kube-system` apply to the
names of the tab at once. `forward` and the other plugins are ignored: the
browser has no DNS. Pods on a Docker node need UDP, so once a Docker node is
Ready, each tab node evicts its CoreDNS pods (the Eviction API, at most every
two minutes) and the real CoreDNS runs there.

### Persistent volumes

kube-controller-manager binds claims to PersistentVolumes, and the scheduler
places a pod with a claim on a node that fits the node affinity of the volume.
The kubelet then follows the claim to its volume (`tabnode_pvc.go`):

- A `hostPath` or `local` volume is a directory in the file system of the
  node. The data outlives the pod, and pods on the same node share it. A node
  in a Web Worker has a file system of its own, so give a volume there node
  affinity for that node.
- `hostPath` creates its directory unless its type is `Directory`. A `local`
  path must exist, as on a real node.
- `readOnly: true` in the claim source makes every mount of the volume
  read-only.
- A claim that is not bound keeps the pod in `ContainerCreating`, with the
  `FailedMount` event of a real kubelet. Other volume sources (NFS, CSI) fail
  with an event.
- `persistentVolumeReclaimPolicy` is handled by the controller-manager, as on a
  real cluster.

### More nodes in Web Workers

`addnode [n]` starts more nodes, `tab-worker-1` and so on, each in a Web
Worker of its own (`k8s/tabernetes/nodeworker.go`, `web/src/nodes.js`,
`web/src/node.worker.js`). The worker gets the compiled WebAssembly module of
the page, so nothing is downloaded or compiled again, and runs it as a node
without the control plane. A worker is a thread with its own memory: the pods
of different nodes run in parallel, for example a Deployment with
`topologySpreadConstraints` on `kubernetes.io/hostname`. Real Wasm threads are
not needed for that; they would need a cross-origin isolated page, which
GitHub Pages cannot serve.

A worker node talks to the control plane like a Docker node, only over
MessageChannels instead of WebRTC data channels (`web/src/channel.js`):
`api` channels to the apiserver, `kubelet:10250` channels from it. Its node IP
comes from the same range as the Docker nodes, so both kinds mix: services
balance over the pods of all nodes, pods on other nodes are reached through the
port-forward of the apiserver. `rmnode <name>` first runs `kubectl drain` on
the node while its kubelet still runs: it cordons the node and evicts the pods,
respecting PodDisruptionBudgets, so the pods end gracefully and their
controllers make new ones elsewhere. Then it stops the worker and deletes the
node. A drain that fails (a PodDisruptionBudget blocks it) keeps the node,
cordoned; `rmnode --force <name>` skips the drain. Each worker is a
whole Go runtime of 100 MB and more, `addnode` stops at eight.

Wasm has no preemption: a busy loop in a goroutine would freeze the tab.
Every command of a native container yields, and a container that runs 10ms
without blocking sleeps as long, so it gets half of the thread at most (less
with `limits.cpu` below 500m).

`limits.memory` works like the memory cgroup: a container that passes it ends
with `OOMKilled` and exit code 137, and restarts by its restart policy. What
counts: the base size of its image, the memory `stress` and `stress-ng` hold
(`polinux/stress`, `progrium/stress`, `alexeiled/stress-ng`), files in
`emptyDir` volumes with `medium: Memory`, and the linear memory of a
WebAssembly container. A container without a limit is never killed.

### WebGPU through DRA

The kubelets in the browser offer the WebGPU adapter of the browser to
WebAssembly pods as a device of the DRA driver `webgpu.tabernetes.io`
([Dynamic Resource Allocation](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/),
`resource.k8s.io/v1`). The tab node always offers it. A node in a Web Worker
offers it when you start it with `addnode --gpu`.

```console
$ k get resourceslices
NAME                  NODE           DRIVER                 POOL           AGE
tab-webgpu            tab            webgpu.tabernetes.io   tab            53s
tab-worker-1-webgpu   tab-worker-1   webgpu.tabernetes.io   tab-worker-1   25s
```

A pod asks for the GPU with a claim and lists the claim in the container that
uses it:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: webgpu
spec:
  spec:
    devices:
      requests:
      - name: gpu
        exactly:
          deviceClassName: webgpu.tabernetes.io
---
apiVersion: v1
kind: Pod
metadata:
  name: mandelbrot
spec:
  runtimeClassName: wasmtime
  restartPolicy: Never
  resourceClaims:
  - name: gpu
    resourceClaimTemplateName: webgpu
  containers:
  - name: mandelbrot
    image: ghcr.io/se-wo/tabernetes/wasi-webgpu:latest
    resources:
      claims:
      - name: gpu
```

`k logs mandelbrot` shows the adapter, a kernel whose 65,536 results are
checked against the CPU, and the Mandelbrot set computed on the GPU.
`resources.limits: {tabernetes.io/webgpu: 1}` works, too: the DeviceClass has
`extendedResourceName`, so the scheduler creates the claim. Where no node has
WebGPU, the pod stays `Pending` with `cannot allocate all claims`.

How it works (`k8s/tabernetes/tabnode_gpu.go`, `web/src/gpu.js`,
`web/src/wasi.worker.js`, `k8s/tabernetes/addons/webgpu.yaml`):

1. **Detection.** `gpu.js` calls `navigator.gpu.requestAdapter()` in the tab,
   or in the worker of the node; Chrome and Edge offer WebGPU in dedicated
   workers. The kubelet probes at its start and again with every node status
   update, once a minute. These adapter fields become device attributes:
   `vendor` and `architecture` (from `adapter.info`, always set), plus
   `device` and `description` when the browser fills them. Chrome often
   leaves them empty. Also published:
   - `fallback` for a software adapter such as SwiftShader;
   - `shaderF16`, `subgroups` and `timestampQuery` for those features;
   - the limits `maxBufferSize`, `maxStorageBufferBindingSize`,
     `maxComputeWorkgroupStorageSize`, `maxComputeInvocations`
     (maxComputeInvocationsPerWorkgroup; attribute names have at most 32
     characters) and `maxComputeWorkgroupsPerDimension`;
   - `wasmJSPI`, whether WebAssembly can wait for the GPU;
   - `browser` and `type: webgpu`.

   A request selects on them with CEL, for example
   `device.attributes["webgpu.tabernetes.io"].maxBufferSize >= 1073741824` or
   `!device.attributes["webgpu.tabernetes.io"].fallback`. WebGPU has no
   attribute for the GPU memory, so there is no memory capacity.
2. **Publishing.** The addons install the DeviceClass `webgpu.tabernetes.io`.
   Its selector is `device.driver == "webgpu.tabernetes.io"`. Each node writes
   one ResourceSlice `<node>-webgpu`: the pool is the node, the slice is owned
   by the Node, and there is one device `gpu-0`. A real DRA kubelet plugin
   sends its slices to the kubelet over gRPC. Here the kubelet is the driver
   and writes the slice itself as `system:node:<name>`, and the Node
   authorizer and NodeRestriction allow a node its own slices. When the
   adapter goes away, the slice goes, too. The build needs no patch for
   this. `resource.k8s.io/v1` is in `stableAPIGroupVersionsEnabledByDefault`
   of kube-apiserver, the feature gate `DynamicResourceAllocation` is GA and
   locked on, and the scheduler adds the `DynamicResources` plugin to its
   default profile. The `resourceclaim` controller of the controller-manager
   (`--controllers=*`) creates claims from templates and writes
   `status.resourceClaimStatuses` of the pod.
3. **Allocation and preparation.** The scheduler allocates and binds. Before
   anything of the pod starts, the tab kubelet reads the claims of the pod;
   this is `PrepareDynamicResources` of the kubelet. They must be allocated
   and reserved for the pod, and their results must be devices of this
   node's pool. Otherwise the pod waits with the event
   `FailedPrepareDynamicResources`. The device goes only to containers that
   list the claim in `resources.claims`, or that the extended resource
   mapping names:
   - their WebAssembly worker gets the `tabernetes_webgpu` imports;
   - they get `TABERNETES_WEBGPU_DEVICE=webgpu.tabernetes.io/<node>/gpu-0`,
     which is what a CDI spec would add;
   - their status gets `allocatedResourcesStatus`
     (`claim:<name>[/<request>]`, `Healthy` while the adapter is there,
     `Unhealthy` after it is gone). This is the beta gate
     `ResourceHealthStatus`.

   The claim gets `status.devices`, a `Ready` condition with the adapter as
   `data`. The Node authorizer lets a node only read claims, and a real driver
   writes this with a service account of its own; here the admin credentials
   of the tab stand in for it. **Sharing:** the browser shares the GPU between
   all tabs and workers anyway, so the device has
   `allowMultipleAllocations: true`. That needs the gate
   `DRAConsumableCapacity`, beta and on in 1.37. The capacity `shares` is 8.
   A claim takes 1 by default, and one that wants the GPU alone asks for all
   of it with `capacity.requests.shares: "8"`. That pod waits until the other
   claims are gone, and later ones wait for it. Shares are a scheduling budget,
   not isolation: all pods of a tab share the memory of the GPU.
4. **Workload API.** We implemented a host import module, `tabernetes_webgpu`
   in `wasi.worker.js`. Its functions are `adapter_info`, `last_error`, and
   `compute(wgsl, data, x, y, z)`, which runs the entry point `main` of a
   WGSL compute shader over a read-write storage buffer. It waits for the GPU
   with JavaScript Promise Integration, so a WASI preview 1 module, such as a
   Go program with `//go:wasmimport` (`wasm-images/webgpu`), sees plain
   synchronous calls. We set aside two other options:
   - `wasi:webgpu` of [wasi-gfx](https://github.com/WebAssembly/wasi-gfx) is
     the standard to aim for, but it is a component-model interface, and the
     tab runs only preview 1 modules. It needs a preview 2 path first, for
     example components transpiled with jco.
   - A built-in image runtime like upstream's `webllm:<model>` gives the
     biggest demo, but it is no WASI workload and adds WebLLM to the bundle.
     It fits as a next step that asks for a claim of the same class instead
     of the extended resource `tabernetes.dev/webgpu`.
5. **GPU tabs.** All nodes of one tab use the same physical GPU. If every
   worker published it, the cluster would count the GPU several times, so
   only the tab offers it by default and `addnode --gpu` adds more on
   purpose. A pod there runs WebGPU in a worker of the node worker. Upstream's
   "every tab is a node" (control plane in a SharedWorker, `tabs.go`,
   `web/src/cluster.js`, not merged) composes with this: the probe belongs to
   the kubelet, not to the control plane. Every tab node would publish its
   own slice from the tab, and the SharedWorker needs no WebGPU. The slices
   are owned by their Node, so a closed tab takes its device with it.
6. **Docker nodes.** An existing DRA driver, such as the NVIDIA one
   (`gpu.nvidia.com`), publishes its own driver name. A single claim can
   still fit both kinds of node with `firstAvailable` (prioritized list, GA
   in 1.37) over `webgpu.tabernetes.io` and the class of that driver. The
   workload is the harder part: on a Docker node, runwasi's wasmtime has no
   `tabernetes_webgpu`, and wasi-gfx needs a wasmtime with `wasi:webgpu`. So
   one manifest needs one image that runs on both, and that comes only with
   `wasi:webgpu` on both sides. The node container would also need
   `docker run --gpus` and the driver in the node image. With
   `extendedResourceName`, a device plugin that announces
   `tabernetes.io/webgpu` on a Docker node would serve the same
   `resources.limits` there.

Limits of the prototype: the software adapter of a headless Chrome
(`--enable-unsafe-webgpu --use-webgpu-adapter=swiftshader`) is published like
a real one, with `fallback: true`. Firefox and Safari have no JSPI or no
WebGPU in workers in some versions; `wasmJSPI` and a missing slice show that.
A lost device is noticed at the next probe, within a minute.

## NetworkPolicy

NetworkPolicies and ClusterNetworkPolicies (`policy.networking.k8s.io/v1alpha2`)
are enforced, on every node. The decisions come from the engine of
[kube-network-policies](https://github.com/kubernetes-sigs/kube-network-policies),
the NetworkPolicy implementation of SIG Network. The design is in
`docs/networking-policy.md`.

- The tab nodes have the engine in the binary (`tabnode_netpol.go`). It
  decides when a connection opens, which is what it gets on a real node: the
  first packet of a connection. A pod dials from its pod IP, and a service IP
  goes to one endpoint first, like kube-proxy's DNAT. A denied connection
  times out, like a dropped SYN. A service without endpoints is refused.
- Pods of different nodes talk over a `pod` channel (`podlink.go`), not the
  port-forward of the apiserver. Its first line names the source and the
  destination. The page names the node a channel comes from, and checks that
  the source belongs to that node.
- Docker nodes run the upstream DaemonSet with nftables and nfqueue,
  next to flannel and kube-proxy. The agent dials pods for the tab from the
  tab pod's IP as uid 65534, so those rules decide it. Pods of a Docker node
  reach tab pods: a route into a veth pair and TPROXY hand their connections
  to the agent. The DaemonSet needs `nft_queue` in the kernel of the Docker
  host; without it, it restarts and the Docker node enforces nothing.
- A policy change also applies to open connections (strict mode): a denied
  one is closed.
- Never blocked, like on a real node: a pod to itself, and the host of a
  node to its own pods. The host of the tab node is the terminal, the
  kubelet, the apiserver and hostNetwork pods. To pods of other nodes, its
  traffic comes from the node IP, so only `ipBlock` rules can allow it.
- `agnhost connect <host:port> --timeout=1s` exits 0, or prints `TIMEOUT`
  or `REFUSED`, like in the NetworkPolicy end-to-end tests.

```bash
k run web --image=nginx -l app=web --port 80; k expose pod web
k run client --image=registry.k8s.io/e2e-test-images/agnhost:2.53 -l role=client -- pause
printf 'apiVersion: networking.k8s.io/v1\nkind: NetworkPolicy\nmetadata: {name: web}\nspec:\n  podSelector: {matchLabels: {app: web}}\n  ingress: [{from: [{podSelector: {matchLabels: {role: other}}}]}]\n' | k apply -f -
k exec client -- /agnhost connect web:80 --timeout=2s   # TIMEOUT
```

Not covered: UDP (the tab nodes have none), rules on domain names (egress
to the internet goes through `fetch()`, without an IP), and an exact
`except` of `ipBlock` for internet egress.

## Ingress

The addons install ingress-nginx (`k8s/tabernetes/addons/ingress-nginx.yaml`):
the upstream bare-metal manifest without the admission webhook. It has the
namespace `ingress-nginx`, the IngressClass `nginx`, and the NodePort service
`ingress-nginx-controller` on 30080 (http) and 30443 (https). Unlike the
upstream manifest, the class is the default class, so an Ingress without
`ingressClassName` works, and the node ports are fixed. On the tab node the controller is a built-in image
(`tabnode_images_ingress.go`). It watches Ingresses with the service account
of its pod, renders `/etc/nginx/nginx.conf` like ingress-nginx and reloads
the built-in nginx. On a Docker node the same manifest runs the real image.

What works: hosts and wildcard hosts, the path types `Exact`, `Prefix` and
`ImplementationSpecific`, `defaultBackend`, TLS with a Secret of type
`kubernetes.io/tls` (SNI, redirect to https), the status address, events, and
the annotations `rewrite-target`, `use-regex`, `ssl-redirect`,
`force-ssl-redirect`, `backend-protocol` and `service-upstream`. A service
without ready endpoints answers 503, an unknown host 404.

```bash
k create deploy web --image=nginxdemos/hello:plain-text --replicas=2; k expose deploy web --port 80
k create ingress web --rule="demo.local/*=web:80"
k get ingress                                # ADDRESS 10.0.0.1
curl -H 'Host: demo.local' 10.0.0.1:30080    # also from pods
k -n ingress-nginx logs deploy/ingress-nginx-controller   # access log
```

Not covered: the admission webhook, most other annotations and the ConfigMap
options, and TCP/UDP services. Gateway API is not provided, although
ingress-nginx is retired upstream: the CKAD material still uses it.

## Patches

`k8s/patch.go` patches a Kubernetes checkout. It is plain Go, so it works the
same on macOS and Linux:

- Build-tag stubs for unix-only syscalls (journal, flock, termios, mount
  helpers, SO_LINGER, `syscall.Exec` for kubectl plugins, signals).
- ECDSA instead of RSA for self-signed certificates. RSA key generation takes
  ages in single-threaded Wasm.
- A sleep in the APF bootstrap ensurer. It busy-loops until the informer
  catches up, and Wasm has no preemption, so the informer never gets the CPU.
- An idempotent `SetupSignalContext`, because all components share one process.
- A settable default stream for the text logger of `component-base/logs`.
  All components share one klog; the stream sorts the lines into the logs of
  the control plane pods.
- mvdan.cc/sh is vendored into the checkout at `MVDAN_SH_VERSION` from
  `mise.toml`. Kubernetes already vendors its only dependencies. A hook in
  its interpreter gives background jobs their own context, so they outlive
  their command line.
- gojq (with timefmt-go) and goawk are vendored the same way
  (`GOJQ_VERSION`, `TIMEFMT_GO_VERSION`, `GOAWK_VERSION`).
- The decision packages of kube-network-policies and the client of
  network-policy-api are vendored the same way
  (`KUBE_NETWORK_POLICIES_VERSION`, `NETWORK_POLICY_API_VERSION`), without
  the nftables dataplane. Their `packet.go` gets the IANA numbers of five
  `syscall.IPPROTO_*` constants that js lacks.
- Helm is vendored at `HELM_VERSION` with its whole module graph
  (`vendorGraph`): modules Kubernetes vendors stay at its version, only the
  missing modules and packages are added. Hooks give each helm the settings of
  its shell, its context and a `log.Fatal` that does not end the tab, send
  chart repositories without CORS headers to `charts/` next to the page, and
  replace flock with locks in the process.
- `kubectl port-forward` and `kubectl proxy` stop with their command
  context. Otherwise their listeners would stay until the tab closes.

Every content patch fails when its target is gone. A Kubernetes update shows
exactly what needs attention.

## Build

The tasks live in `tabernetes/mise.toml`. mise installs Go and Node.js.

```bash
cd tabernetes
mise run build       # .build/assets/{tabernetes.wasm.zst,tabernetes.js,unzstd.js,xterm.css,...}
mise run site        # .build/site/: the page, the assets, the proxy worker, the licenses
mise run node-image  # $NODE_IMAGE:latest for the local platform
python3 -m http.server -d .build/site 8080   # http://localhost:8080/
```

`site/build.mjs` builds the standalone page from `site/index.md` (the text),
`site/page.html` and `site/terminal.html`. All its URLs are relative, so it
works under any base path, such as `/tabernetes/` on GitHub Pages
(`.github/workflows/pages.yaml`). It also writes `THIRD_PARTY_LICENSES.txt`:
the Go modules in the binary (`go list -deps`, collected by the `wasm` task
while the Kubernetes checkout is there) and the npm packages in the bundles,
each with its license text. `site/charts.mjs` packages the charts of
`charts/` into chart repositories under `charts/` of the site. `join` prints
`TABERNETES_IMAGE` if set at build time, else `$NODE_IMAGE:latest`.

The binary is around 310 MB, or 30 MB with zstd (gzip: 47 MB). GitHub Pages
does not send `Content-Encoding` for it, and `DecompressionStream` has no
zstd. The page fetches the `.zst` file and unpacks it with
[fzstd](https://github.com/101arrowz/fzstd) in a Web Worker, block by block
while it downloads. The browser compiles the WebAssembly while the bytes
arrive, so download, unpacking and compiling overlap.

The page links every file with `?v=<hash of the content>`, so the URL of the
binary changes with its content. The page keeps the binary in the Cache Storage of
the browser and the next visit downloads nothing. GitHub Pages itself sends
only `max-age=600`.

## Update Kubernetes

1. Bump `KUBERNETES_VERSION` in `mise.toml`.
2. Run `mise run build`. When `patch.go` stops with "patch target not found",
   adapt that patch.
3. Run `mise run test` (etcd shim tests), then boot the page and run
   `join`.

The node image is built by `.github/workflows/tabernetes-image.yaml` with the same
version and pushed as `latest` only. The printed `docker run` uses
`--pull always`, so a join always runs the current image.

The node image also brings the images that every join needs: pause,
kube-proxy, flannel and CoreDNS. The `images` stage of `node/Dockerfile` takes
the list from the addon manifests in `k8s/tabernetes/addons/` and the pause
image from containerd, so an update needs no extra step. The agent serves them
to containerd from a read-only registry on 127.0.0.1
(`node/agent/imagemirror.go`); every other image comes from its registry as
before. Downloads from the registries stalled now and then on Docker Desktop,
and the join waited for the retries.

## Node without --privileged

The node container runs a kubelet, containerd and runc, but it needs neither
`--privileged` nor extra capabilities. The printed `docker run` uses:

| Flag | Why |
| --- | --- |
| `--pull always` | the image has only the `latest` tag; this fetches the current one instead of a cached old one |
| `--memory 2g --pids-limit 4096` | the node evicts and garbage collects almost never, so the limits keep a runaway pod from taking down the host |
| `--security-opt seccomp=unconfined` | Docker's profile allows a new user namespace only with `CAP_SYS_ADMIN` |
| `--security-opt systempaths=unconfined` | the kernel refuses a new `sysfs` and `cgroup2` mount while Docker masks parts of `/proc` and `/sys` |
| `--device /dev/net/tun` | pasta connects the node's network namespace through a tap device |
| `--security-opt apparmor=unconfined` | the `docker-default` AppArmor profile denies `mount` (Linux hosts with AppArmor, untested) |

Without `CAP_SYS_ADMIN`, the agent starts itself again in new user, mount,
network, cgroup, UTS and IPC namespaces (`agent/userns.go`). There it is root
with all capabilities, but only over these namespaces:

- uid and gid 0-65535 map to themselves, so files keep their owners.
- pasta copies the container's address into the new network namespace, NATs
  outgoing traffic and forwards the WebRTC UDP port. WebRTC, flannel and
  kube-proxy work as before.
- A new `cgroup2` mount gives the kubelet a writable cgroup tree.
- The kubelet runs with the feature gate `KubeletInUserNamespace`.
  containerd runs with `restrict_oom_score_adj` and without AppArmor.
- Docker's `/sys` is read-only, and in a user namespace the kernel allows only
  read-only `sysfs` mounts. containerd calls runc through the agent
  (`agent/runc.go`), which marks the `sysfs` mounts of containers read-only.
- `/dev/kmsg` links to `/dev/null`, so the kubelet's OOM watcher starts. OOM
  kills still show up as `OOMKilled` in the pod status.

Alternative that keeps Docker's seccomp profile: capabilities instead of a user
namespace.

```text
--cap-add SYS_ADMIN --cap-add NET_ADMIN --cap-add SYS_RESOURCE --cap-add SYS_PTRACE --security-opt apparmor=unconfined
```

Then the agent remounts `/sys/fs/cgroup` and `/proc/sys` read-write and calls
runc with `--no-new-keyring --no-pivot`, because the profile blocks `keyctl`
and `pivot_root`. `CAP_SYS_ADMIN` is close to root, though. `--privileged`
still works, too. Real isolation from the host needs rootless Docker or Podman.

WebAssembly pods (`runtimeClassName: wasmtime`) run with the wasmtime shim of
[runwasi](https://github.com/containerd/runwasi), and the node has the label
`tabernetes.io/wasm=true`. The shim creates containers with libcontainer
instead of runc. In the user namespace containerd starts it through the agent
(`agent/wasm.go`), which turns the `sysfs` mounts into read-only bind mounts of
`/sys`: libcontainer mounts `sysfs` read-write first, which the kernel refuses
there. With capabilities instead of the user namespace, the node runs no
WebAssembly pods and has no label: libcontainer has no `--no-pivot`.

## Limits

- Wasm runs on one thread. The boot takes about 15 seconds. Everything shares
  one CPU core.
- Containers on the tab nodes get no signals. A container that stops gets
  its preStop hook and is then killed at once; the rest of the grace period
  is not waited for. `kubectl debug --target` shares no processes.
- No persistence: a reload starts a new cluster.
- The static pod manifests in `/etc/kubernetes/manifests` are not watched:
  an edit changes nothing. A component that exits stays down, its pod shows
  `CrashLoopBackOff`. The `etcd-tab` pod is the etcd shim.
- `yq` runs jq expressions on YAML with gojq: the yq v4 commands and flags
  work, its own functions mostly do not (`strenv` and `env` do). `jq` and
  `yq` print keys sorted. `awk` cannot run commands (`system()`, pipes), and
  `ping` has no ICMP: a TCP connection to the host stands in for it.
  `less` and `more` print the whole input.
- `kubectl exec`, `logs` and `port-forward` go through the WebRTC tunnel to
  the kubelet of a Docker node. The apiserver cannot reach pod IPs of a
  Docker node, so service proxy URLs work only for pods on the tab node.
- Pods on Docker nodes reach pods on the tab nodes over TCP only, through
  the agent (TPROXY). If the tab node denies such a connection, the pod sees
  it open and then end, not a timeout.
- The tab node is not a sandbox: all its containers share the tab, its
  memory and its in-memory file system. Native containers only look like
  their images; nothing of a real image runs. The TTY of a tab node container
  has canonical mode only: full screen programs in a container (vi, top)
  see lines, not keys. UDP services,
  WASI preview 2 components and sockets in WebAssembly pods are not
  supported. A shell loop without any command (`for ((;;)); do x=1; done`)
  still freezes the tab.
- The printed `docker run` works when Docker runs on the same machine as the
  browser. For Docker on another machine in the LAN, publish the port on all
  interfaces (`-p 50001:50001/udp` for the first node) and pass
  `--advertise <LAN IP>` before the token. The browser hides its own addresses
  behind mDNS names, and the container cannot resolve them, so the browser has
  to reach the container.
- Across different networks, WebRTC needs a port forward or TURN. There is no
  TURN server.
