# Boot time

How long the page takes from the navigation to the first shell prompt, phase
by phase, and what the measurements say about where to cut. This is a
measurement only: nothing in the boot was changed for it. Measured on
2026-11-20 at commit `38a6658` (Kubernetes v1.37.2, Go 1.27.3) plus the
marks of this change.

## Summary

- **The boot takes about 18 seconds**, not 15: median 18.2 s over 24 boots
  (p25-p75 16.8-19.7 s), 17.9 s without the marks. A first visit, a return
  visit and a reload take the same time within the spread: nothing of a
  cluster is kept, and the compiled Wasm is not cached either.
- The page loads, downloads, unpacks and compiles the binary in 1.6 s
  (local server). The rest, ~16.5 s, is the Go thread: **87 % busy** (the
  garbage collection alone 8 % by `gcCPU`) and 13 % idle. Almost all of the idle
  time (2.1 of 2.2 s) is the tab node waiting for its pod CIDR.
- The largest blocks: kube-apiserver to its first answer 3.8 s, to `/readyz`
  2.1 s more, the addons 3.3 s, the wait for the pod CIDR 4.6 s, and 1.2 s
  from the end of `boot()` to the prompt.
- The pod CIDR comes late because kube-controller-manager first builds all
  45 controllers (5.9 s from its start, each with its own ServiceAccount and
  token), then starts the informers, then all controllers, the node IPAM
  controller among the last. The build mostly waits for the client rate
  limit of the controller-manager (QPS 20, burst 30): a test build with a
  higher limit and the same per-controller ServiceAccounts reached the
  prompt **3.2 s earlier** (14.2 s), with the first pod as fast as before.
  This is the top [candidate](#candidates).
- The prompt waits for all of the controller-manager. So a pod typed at the
  prompt is Ready in 0.7 s and answers HTTP in 0.7 s, the same as 1.5 s or
  3 s later.
- Go never returns to the event loop for long stretches: the first call
  (`go.run`) runs ~7 s without a break, from the runtime start into the
  kube-apiserver; the page cannot paint meanwhile.

## Method

- Machine: cloud container (KVM guest), Intel Xeon @ 2.30 GHz, 4 cores,
  16 GB RAM, Linux 6.18. Headless Chromium of Playwright
  (`chromium-1243`), Node.js 22. The numbers of a desktop machine will be
  different; the shares should hold.
- Build: `mise run build` and `mise run site`, as CI builds the page. The
  script serves `.build/site` itself on `127.0.0.1` with
  `Cache-Control: max-age=600` and ETags, like GitHub Pages. So the
  download is local; see [The binary](#the-binary) for a real network.
- Cases:
  - **first**: a new browser with a fresh profile, so no HTTP cache and no
    Cache Storage.
  - **return**: a new browser with a profile that booted the page before
    (HTTP cache and the binary in Cache Storage).
  - **reload**: `page.reload()` in the tab of the return visit, after it
    booted.
- Rounds: 8 rounds of the three cases, the first and the return visit in
  alternating order, 24 boots. After the prompt each boot types
  `kubectl run web --image=nginx` at 0, 1.5 or 3 s (rotated over the cases,
  8 boots each) and is watched for 30 s more for the pods of the addons.
  Medians with p25-p75 and min-max.
- Marks: `?bootmarks` in the URL turns them on. The page sets
  `performance.mark("js:…")` (`web/src/terminal.js`), Go sets
  `performance.mark("go:…")` (`k8s/tabernetes/bootmarks.go`), each with
  the busy time of the Go thread (`tabernetesBusy()`, `web/src/clock.js`)
  and the GC CPU time (`gcCPU`, `tabnode_stats.go`) in its detail. Go also
  marks each `/readyz` check of kube-apiserver the first time it passes
  (from the existing 100 ms poll of `waitReady`), the controller-manager and
  scheduler health endpoints (a 50 ms poll that only runs with marks) and
  every pod of the tab node when it is first seen and first Ready.
  `clock.js` records every call into Go with its start, its end and the
  timer Go asked for before it went idle, and `fs.js` counts the file system
  calls. The controller-manager times come from the time stamps of its
  klog lines. Without `?bootmarks` all of this is one check of a flag.
- Cost of the marks: 24 boots without them took 17.9 s (17.2-18.6) against
  18.2 s (16.8-19.7) with them, inside the spread.
- CPU profile: the Chrome DevTools profiler over CDP (200 µs), two first
  visits. `k8s/build.sh` links with `-s -w`, which drops the name section
  of the Wasm, so the release binary shows no Go names. The profile runs
  used the same tree linked without `-s -w` (16 MB larger).
- Busy time: `countBusy` counts the calls through `go._resume`. The first
  call, `go.run`, does not go through it: the runtime start, the package
  init and `main` up to its first wait (here ~7 s, until kube-apiserver
  answers) are missing from `tabernetesBusy()` and so from the CPU of the
  node in `kubectl top`. With marks, the page counts that call as well.

## Time to the prompt

| case | with marks (ms) | without marks (ms) |
| --- | --- | --- |
| first visit | 18769 (17457-19902, 17268-21031) | 17946 (17703-18661, 17024-20834) |
| return visit | 18918 (17175-20059, 15923-20754) | 17652 (17122-18325, 16788-19263) |
| reload | 16320 (15897-18046, 15083-19939) | 18180 (17368-18878, 16113-20950) |

The cases differ by less than the spread of one case. The reload was
faster with marks and slower without them: noise. The spread itself is
large (5 s between the fastest and the slowest boot of a case); the order
in which goroutines get the thread differs from boot to boot, see
[No preemption](#no-preemption).

## Phases

Medians (p25-p75, min-max) in ms. The rows of the binary overlap with each
other: the download, the zstd workers and the streaming
compiler run at the same time, and the rows only show what is left after
the previous step.

| phase | first visit | return visit | reload |
| --- | --- | --- | --- |
| HTML and `tabernetes.js` | 73 (64-75, 53-78) | 58 (52-64, 45-77) | 27 (26-30, 24-43) |
| binary: request to first bytes | 24 (19-27, 13-40) | 13 (13-15, 8-19) | 4 (3-6, 2-10) |
| binary: transfer of 31.3 MB (local) | 265 (227-295, 142-309) | 79 (75-87, 58-121) | 58 (56-62, 52-68) |
| binary: zstd frames left after the last byte | 1094 (1032-1161, 940-1300) | 1110 (1085-1162, 1046-1239) | 1074 (1004-1116, 943-1191) |
| binary: compile left after unpacking | 270 (262-294, 241-397) | 282 (267-288, 244-299) | 464 (435-527, 422-680) |
| **`go.run` (ms after navigation)** | 1722 (1621-1837) | 1553 (1540-1597) | 1677 (1622-1737) |
| Go runtime start and package init | 1063 (1001-1182, 941-1239) | 1006 (978-1071, 940-1086) | 939 (911-960, 298-1032) |
| PKI and kubeconfigs (ECDSA) | 64 (54-65, 40-68) | 72 (64-78, 51-90) | 53 (48-78, 13-87) |
| etcd shim start | 5 (5-8, 3-33) | 7 (7-9, 4-17) | 5 (4-7, 0-10) |
| kube-apiserver: start to first answer | 3887 (3755-4209, 3634-4239) | 3714 (3689-3794, 3631-4048) | 3654 (3467-3775, 2374-4417) |
| kube-apiserver: first answer to `/readyz` ok | 2403 (2088-2562, 1915-2781) | 2084 (1989-2207, 1918-2483) | 2092 (1894-2363, 1649-2703) |
| controller-manager and scheduler started (goroutines) | 7 | 7 | 7 |
| addons: `kubectl apply --server-side` | 3505 (3399-3695, 3293-4140) | 3221 (2749-3452, 2600-3773) | 3191 (2862-3331, 2598-3807) |
| tab node: register (Node, status) | 64 (59-70, 51-79) | 64 (62-191, 54-554) | 62 (53-67, 50-81) |
| tab node: kubelet server, static pods, network policy | 78 (76-83, 66-95) | 77 (75-80, 73-98) | 76 (66-82, 43-88) |
| tab node: wait for the pod CIDR | 4431 (3981-5159, 3609-5881) | 5047 (4656-6293, 3944-7696) | 4313 (3904-5011, 2501-5589) |
| end of `boot()` to the prompt | 1176 (1024-1602, 731-2459) | 1114 (728-1577, 8-2236) | 1170 (863-1497, 9-1793) |
| **prompt** | 18769 | 18918 | 16320 |

Milestones (ms after navigation, first visit): `main` 2747, kube-apiserver
answers 6803 and is ready 9290, controller-manager healthy 9502, scheduler
healthy 9969, addons applied 12835, node registered 12903 (its Lease 12932),
pod CIDR 17769, prompt 18769.

Notes on the phases:

- **The binary.** The return visit and the reload read it from Cache
  Storage, but unpack and compile it again: `instantiateStreaming` gets a
  `Response` built from a stream, which has no URL, so V8 keeps no code
  cache of it. Unpacking the last 32 MiB frames after the last byte takes
  ~1.1 s on 4 cores. The page's own JavaScript that cuts the frames and
  feeds the compiler takes ~0.8 s of the main thread (profile, see below).
- **Go runtime start and package init**, ~1.0 s: the package init of a
  317 MB binary (`runtime.doInit1` and the init functions ~0.38 s self,
  plus allocation and GC).
- **PKI**: 5 ECDSA P-256 keys and certificates and 4 kubeconfigs take
  ~60 ms. Not worth touching.
- **kube-apiserver to its first answer**, 3.8 s, all of it CPU: building
  the storage and handlers of every resource, the admission plugins, the
  OpenAPI specs (see the profile). GC is 0.53 s of it.
- **kube-apiserver to `/readyz`**, 2.1 s: the checks pass in this order
  (ms after the first answer, medians): `etcd-readiness` 110,
  `informer-sync` 127, `crd-informer-synced` 211, `bootstrap-controller`
  221, `start-apiextensions-controllers` 315,
  `apiservice-registration-controller` 398,
  `start-service-ip-repair-controllers` 478,
  `apiservice-discovery-controller` 659,
  `priority-and-fairness-config-producer` 796,
  `start-kubernetes-service-cidr-controller` 1708, `rbac/bootstrap-roles`
  2076, `scheduling/bootstrap-system-priority-classes` and
  `autoregister-completion` 2107. The last two hooks start with
  `wait.Poll(1*time.Second, …)`, so they wait a second before their first
  try; that second is hidden behind other work (the phase is 98 % busy).
  API priority and fairness is off (`--enable-priority-and-fairness=false`);
  its config producer still passes at 0.8 s.
- **The addons**, 3.3 s: `kubectl apply --server-side` of
  `k8s/tabernetes/addons/` succeeded at the first try in every boot. The
  phase is 100 % busy, but the controller-manager builds its controllers in
  the same window, so this time is not all kubectl and apiserver work for
  the apply. The marks cannot split them.
- **The tab node** registers, writes its Lease and starts its server in
  ~0.15 s, then waits 4.6 s for the pod CIDR (next section).
- **End of `boot()` to the prompt**, 1.2 s, all busy: `boot()` resolves its
  promise at once, but JavaScript sees it only when Go returns to the event
  loop, which is when no goroutine can run: the controllers do their first
  syncs first.

### The controller-manager and the pod CIDR

From the klog lines of the controller-manager (ms after navigation):

| | first visit | return visit | reload |
| --- | --- | --- | --- |
| command starts (`"Starting"`) | 9452 (9209-9772) | 8804 (8768-9499) | 8684 (8181-9098) |
| first `"Controller starting..."` | 15571 (14764-16591) | 15735 (15353-16386) | 14478 (13752-15459) |
| build time of the controllers | 5941 (5553-7055) | 6566 (5546-6911) | 5575 (5544-6126) |
| last `"Controller starting..."` | 15617 | 15764 | 14513 |
| controllers started | 45 | 45 | 45 |
| node IPAM controller `"Starting ipam controller"` | 16044 (14965-16840) | 16240 (15859-17623) | 15241 (14222-15912) |
| pod CIDR on the tab node | 17769 | 17016 | 15648 |

The controller-manager builds all controllers first (`BuildControllers`),
then starts the informers, then all controllers within ~50 ms
(`--controller-start-interval=0s`). Building takes ~5.9 s of wall time, in
which the addons and the scheduler also run. With
`--use-service-account-credentials=true` each controller gets its client
from `DynamicControllerClientBuilder`: a GET or create of its
ServiceAccount when it is built, and a TokenRequest (plus the GET again) on
its first request. What that costs:
[Per-controller ServiceAccounts](#per-controller-serviceaccounts).

## Busy time, garbage collection and waits

From `go.run` to the prompt, all 24 boots (ms):

| | median | p25-p75 | min-max |
| --- | --- | --- | --- |
| wall time | 16537 | 15258-17913 | 13605-19207 |
| busy (Go runs) | 14442 | 13231-15581 | 10700-16672 |
| of it GC (`gcCPU`) | 1361 | 1301-1432 | 1200-1643 |
| idle (Go waits) | 2208 | 1599-2703 | 1267-3830 |

Busy 87 %, GC 8 % of the wall time (9 % of the busy time), idle 13 %.

The idle time by the timer Go asked for before it went idle (per boot,
medians): 270 gaps with a 1-9 ms timer (1.3 s), 54 gaps with a 10-49 ms
timer (0.8 s), 2 gaps without a timer (4 ms: a JavaScript event woke Go),
none with a timer of 50 ms or more. 2.1 s of the 2.2 s are in the wait for
the pod CIDR; before it, Go never waits longer than a few milliseconds.
Which goroutines set these short timers is not recorded; client-side rate
limiting and the 5 ms base delay of work queue retries fit them. The long
polls of the boot (`waitReady` 100 ms,
`waitPodCIDR` 250 ms, the 1 s polls of the two hooks) never show up as
idle time because other goroutines always had work at that moment.

The GC share differs by the source: `gcCPU` (runtime/metrics) says 1.36 s;
the profile puts 2.5 s of self time into mark, scan and sweep functions
(assists of allocating goroutines included), 0.43 s into allocation and
0.56 s into stacks and tracebacks (the GC scans stacks, too). The two count
differently, and the profiled boot is a little slower; both put the GC
between 8 and 15 % of the boot.

### No preemption

Go on js/wasm runs one goroutine until it blocks; there is no preemption
and the page's event loop only runs when every goroutine is blocked.

- 330 calls into Go (median) until the prompt. The longest one is the first:
  `go.run` runs 7.0 s (6.7-7.6 s) without a break, from the runtime start
  through PKI, etcd and kube-apiserver until it answers. The page cannot
  paint in that time: the status lines of the boot appear in one piece.
  The second and third longest calls take 3.9 and 3.0 s.
- The controller-manager and the addons start at the same time and share
  the thread in an order that changes from boot to boot. The command of
  the controller-manager usually logs its start 0.1-0.4 s after its
  goroutine started; in 2 of 24 boots it was 2.7 s, because the apply ran
  first. This order is a large part of the spread.
- A file system call of Go (`syscall.fsCall`, `web/src/fs.js`) runs its
  callback synchronously. The callback is a call into Go, and `handleEvent`
  of the runtime returns to JavaScript only once all goroutines are idle:
  every write of a log line can run other goroutines until they all block.
  This does not cost CPU, but it reorders work, and it makes the "total"
  times of a CPU profile meaningless for Go callers.
- The file system itself is cheap: ~1200 writes (log lines of all
  components) and ~3000 other calls take 0.25-0.30 s up to the prompt,
  `console.debug` of the log lines included.

## The first pod after the prompt

`kubectl run web --image=nginx` (built-in image), 8 boots per delay, ms
after the command:

| typed | shell returns | Ready (tab node sees it) | answers HTTP |
| --- | --- | --- | --- |
| at the prompt | 87 (76-130, 75-1946) | 716 (710-756, 706-1856) | 704 (684-738, 668-1954) |
| 1.5 s after the prompt | 78 (75-84, 72-109) | 710 (705-713, 704-743) | 678 (675-698, 667-751) |
| 3 s after the prompt | 97 (73-119, 65-287) | 726 (700-744, 694-924) | 698 (665-714, 659-896) |

The delay makes no difference, because the prompt comes only after all
controllers run. One boot (a return visit, at the prompt) took 1.9 s.
HTTP answers a little before the Ready mark: the mark is when the tab
node's informer sees the Ready condition it wrote.

The addons after the prompt (ms after the prompt, all boots):
local-path-provisioner Ready -90 (before the prompt), CoreDNS 9386
(9141-9933), ingress-nginx controller 9640 (9100-10362), metrics-server
19458 (18958-19865). So `kubectl top` works ~20 s after the prompt.

## The binary

- `tabernetes.wasm` is 317 MB, `tabernetes.wasm.zst` 31.3 MB (32 MiB zstd
  frames, level 19).
- A first visit fetches 5 files, 33.4 MB, all from the page's own origin.
  No request goes to another host before the prompt (checked in all 24
  boots).
- The return visit makes no request at all (HTTP cache within `max-age`,
  binary from Cache Storage); the reload revalidates `index.html` (304).
- Locally the transfer takes 0.27 s. Over a real network, 31.3 MB take
  ~2.5 s at 100 Mbit/s and ~5 s at 50 Mbit/s (computed, not measured); the
  unpacking and compiling then overlap with it.
- The Wasm memory is ~390 MiB at the prompt.

## CPU profile

Two first visits with the build that has function names, Chrome DevTools
profiler over CDP, 200 µs sampling, samples from the navigation to the
prompt. Self time, averaged over the two runs (they differ by less than
10 %). The profiled boots reached the prompt after 19.6 and 20.4 s, a
little later than without the profiler.

By kind: Wasm 14.9 s, JavaScript 1.7 s, idle 3.1 s, V8 "(program)" 0.3 s,
V8 GC (of JavaScript objects) 0.05 s.

The Wasm self time by group:

| group | ms |
| --- | --- |
| Go GC (mark, scan, sweep) | 2519 |
| Go runtime, other (maps, interface tables, write barriers, atomics, memmove, …) | 1729 |
| protobuf, gRPC, HTTP/2 | 920 |
| Go scheduler, channels, timers, locks | 879 |
| other Go | 824 |
| Go maps | 720 |
| OpenAPI (kube-openapi, generated definitions) | 713 |
| k8s.io/apiserver | 668 |
| TLS and crypto | 638 |
| Go stacks and tracebacks | 555 |
| JSON | 507 |
| fmt, strings, strconv, bytes, time, … | 506 |
| k8s.io/apimachinery | 505 |
| Go allocation | 432 |
| k8s.io/kubernetes | 426 |
| k8s.io/api types | 411 |
| Go package init | 379 |
| reflect | 338 |
| net, net/http, net/url | 295 |
| server-side apply (structured-merge-diff) | 278 |
| k8s.io/client-go | 276 |
| Prometheus metrics | 164 |
| regexp | 102 |
| YAML | 81 |
| klog | 29 |

The Go runtime (GC, allocation, maps, scheduler, stacks, init, other) is
~7.2 s, about half of the Go time.

The top functions by self time: `runtime.tryDeferToSpanScan` 352,
`runtime.scanObject` 291, `runtime.scanObjectsSmall` 282,
`runtime.doInit1` 256, `runtime.selectgo` 165,
`runtime.mallocgcSmallScanHeader` 130, `runtime.nanotime1` (JS) 126,
`runtime.walltime` (JS, `clock.js`) 128,
`k8s.io/kubernetes/pkg/generated/openapi.GetOpenAPIDefinitions` 123,
`runtime.mallocgc` 117, `runtime.growslice` 115, `runtime.step` 113,
`runtime.pcvalue` 112, map iteration 108, `runtime.findObject` 86,
`crypto/internal/fips140/nistec/fiat.p256Mul` 71.

The top packages by self time after `runtime`: `internal/runtime/maps` 426,
`reflect` 338, `net/http` (internal HTTP/2) 250,
`k8s.io/apimachinery/pkg/runtime` 216, the JSON of kube-openapi 214,
`google.golang.org/grpc/internal/transport` 198, `encoding/json/jsontext`
177, `k8s.io/api/core/v1` 164, the generated OpenAPI definitions 152,
Prometheus 130.

JavaScript: cutting the zstd frames and feeding the compiler ~0.8 s (before
`go.run`), the log callback of `terminal.js` (`console.debug`, the log
buffer) ~0.23 s, the clock ~0.25 s (`runtime.walltime` of `clock.js` with
BigInt arithmetic 128 ms, `runtime.nanotime1` 126 ms).

By phase, the largest groups: the package init is mostly init functions
(0.35 s); kube-apiserver to its first answer is GC 0.87 s, OpenAPI 0.47 s,
TLS 0.19 s; kube-apiserver to `/readyz` is gRPC/protobuf/HTTP/2 0.26 s and
the scheduler 0.22 s; the addons are GC 0.76 s, OpenAPI 0.22 s, JSON 0.2 s;
the wait for the pod CIDR is GC 0.32 s and gRPC/protobuf 0.2 s.

The etcd path alone (gRPC, its HTTP/2 framer, Google protobuf, the etcd
client and the shim) is ~0.6 s self time; the shim itself ~0.02 s.

Lazy compilation of Wasm functions on their first call is not visible as a
separate item; it is probably in "(program)" (0.3 s) and in the self time
of the functions.

## Per-controller ServiceAccounts

To measure what `--use-service-account-credentials=true` costs, a build of
the same tree with `--use-service-account-credentials=false` ran against the
unchanged build, 8 first visits each, alternating, `kubectl run` at the
prompt. Without per-controller credentials the controller-manager acts as
`system:kube-controller-manager`, which lacks most of the RBAC rules of the
controllers: the first try never reached the prompt (the EndpointSlice
controller and others were forbidden). So the test build also put the token
of the controller-manager into `system:masters`. That skips the RBAC
authorizer for its requests, which makes the test build a little faster for
a reason of its own. This was a test only; the flag stays.

| | per-controller ServiceAccounts (now) | one identity (test) |
| --- | --- | --- |
| prompt | 17728 (17460-18341, 17276-19382) | 14532 (13899-15039, 13658-17042) |
| controller-manager: start to its first controller start | 5547 (5541-5731) | 302 (215-403) |
| tab node: wait for the pod CIDR | 4331 (3898-4482) | 90 (85-133) |
| addons phase | 3229 (3157-3310) | 4744 (4569-4847) |
| end of `boot()` to the prompt | 1784 (1343-2143) | 871 (664-1085) |
| Go busy, `go.run` to the prompt | 14538 (14268-14795) | 12925 (12365-13397) |
| Go idle, `go.run` to the prompt | 1620 (1538-1679) | 24 (12-44) |
| first pod at the prompt: Ready / HTTP | 714 / 693 | 727 / 707 |

The per-controller ServiceAccounts cost **3.2 s** of the time to the prompt:
~1.6 s of CPU and ~1.6 s of waiting. The whole idle time of the boot goes
away with them. The controllers then start while the addons are applied
(that phase grows by 1.5 s), and nothing moves behind the prompt: the first
pod is as fast as before.

Where the waiting comes from: the controller-manager runs with the client
defaults `--kube-api-qps=20 --kube-api-burst=30`, and
`DynamicControllerClientBuilder` makes all its ServiceAccount GETs, creates
and TokenRequests through the one client of the controller-manager
(`c.Client.CoreV1()`): up to four requests for each of the 45 controllers,
far more than the burst of 30.

A second test build kept `--use-service-account-credentials=true` and only
added `--kube-api-qps=1000 --kube-api-burst=2000` to the controller-manager,
again against the unchanged build, 8 first visits each, alternating:

| | now (QPS 20, burst 30) | QPS 1000, burst 2000 (test) |
| --- | --- | --- |
| prompt | 17353 (17002-17755, 15599-20253) | 14179 (13944-14658, 13199-15812) |
| controller-manager: start to its first controller start | 5789 (5633-6253) | 769 (737-840) |
| node IPAM controller starts (ms after navigation) | 14977 (14486-15874) | 9338 (9078-9552) |
| tab node: wait for the pod CIDR | 4356 (4046-4664) | 93 (76-111) |
| addons phase | 3188 (2958-3365) | 5158 (5055-5304) |
| end of `boot()` to the prompt | 1517 (1110-1832) | 717 (605-858) |
| Go busy, `go.run` to the prompt | 14017 (12681-14348) | 12646 (12383-13090) |
| Go idle, `go.run` to the prompt | 2050 (1818-2592) | 33 (10-65) |
| first pod at the prompt: Ready / HTTP | 808 / 808 | 725 / 778 |

The same 3.2 s, with the per-controller ServiceAccounts kept. So their cost
at the boot is almost all the client rate limit of the controller-manager,
not the ServiceAccounts, tokens and clients themselves (~0.5 s of building
remain).

## Candidates

Ranked by the expected gain in time to the prompt against the cost. The
gains come from the measurements above; where they are an upper bound, it
says so.

1. **Lift the client rate limit of the controller-manager during the
   boot.** Measured: prompt -3.2 s (17.4 -> 14.2 s), first pod unchanged,
   per-controller ServiceAccounts kept. Cost: two flags
   (`--kube-api-qps`, `--kube-api-burst`), or a higher limit only for the
   client that `DynamicControllerClientBuilder` uses (a patch). Risk: low;
   the limit also stops no runaway controller any more, which matters
   little for a single-user cluster in a tab that already runs without API
   priority and fairness. Check the tab nodes in Web Workers and `kubectl
   top` under load before keeping it.
2. **Seed: ship the etcd data of a booted cluster.** Removes the addons
   phase (3.2-3.5 s now, all busy; the controller-manager works in the
   same window, as the test builds show, so the apply itself is at most
   that) and
   the creation of the RBAC roles and priority classes in the `/readyz`
   phase (part of 2.1 s; the two hooks would still wait their first second
   unless they poll at once). Expected gain: 2-3.5 s, an upper bound that
   the marks cannot split further; upstream measured 1.6 s for its seed.
   Cost: high. Snapshot and restore of the etcd shim (persistence needs
   them, too), a build step that boots the binary in Node.js, rewriting
   UIDs, `creationTimestamp` and the times of the managed fields at boot,
   keeping the PKI, the tokens and the ConfigMap
   `extension-apiserver-authentication` out of it, and a check that the
   seed matches the build. Risk: medium (stale or foreign objects in every
   new cluster when it goes wrong).
3. **Staggered start of the controllers**, keeping the per-controller
   ServiceAccounts: build and run the node IPAM controller first, the
   others after it, so the tab node gets its pod CIDR early. Today that
   saves up to the 4.3 s wait for the pod CIDR, but most of the work only
   moves: the build of the other controllers (rate limited) then runs after
   the prompt, where a typed command waits behind it (no preemption). After
   candidate 1 the wait for the pod CIDR is 0.1 s, so the gain left is
   small. Cost: a patch of the controller-manager (`patch.go`). Do 1 first.
4. **Hibernation or a snapshot of the control plane memory** for the return
   visit. A return visit costs as much as a first one (18.9 s against
   18.8 s), because nothing is kept. A snapshot would skip ~16.5 s of Go
   work, but it has to read ~400 MiB of Wasm memory (390-412 MiB at the
   prompt), and the compile (1.6 s) stays. Expected: a return visit of a few
   seconds; the read and restore of 400 MiB were not measured. Cost: very
   high. The JavaScript side of the Go runtime (the value table of
   `syscall/js`, pending timers and promises, `fs.js`, the workers of the
   nodes) must be rebuilt to match, and the clock jumps (Leases, tokens,
   certificates). Risk: high.
5. **GC during the boot.** GC is 1.36 s by `gcCPU` and 2.5 s of self time
   in the profile (plus 0.43 s allocation, 0.56 s stacks). A larger `GOGC`
   until the prompt (the memory limit of 3 GiB stays) trades memory for
   some of that. Expected: a few hundred ms (upstream measured 0.1-0.25 s
   for `GOGC=400`); not measured here. Cost: low. Risk: low, more memory.
6. **OpenAPI on the first request instead of at boot.** OpenAPI is 0.71 s
   of self time: 0.47 s while kube-apiserver starts, 0.22 s while the
   addons are applied. Expected gain: up to ~0.7 s plus its share of the
   GC, and only if the addons stop fetching the spec (`kubectl apply`
   validates against `/openapi/v3` unless told not to, or a seed replaces
   the apply). The cost moves to the first `kubectl apply` and
   `kubectl explain` of a session. Cost: medium (a patch of
   k8s.io/apiserver; some upstream unit tests assume the spec at start).
7. **Keep the unpacked or compiled binary for return visits.** A return
   visit unpacks 31.3 MB of zstd again (~1.1 s left after the last byte on
   4 cores) and compiles from a stream without a code cache. Expected:
   up to ~1.1 s on a return visit (the unpacking); the share of lazy
   compilation is not separable in the profile. Cost: 317 MB more in Cache
   Storage for the unpacked binary, served as a `Response` with a URL so
   that V8 can cache code. Risk: low, storage quota.
8. **Snapshot of the Go heap after the package init**, made at build time.
   The runtime start and the package init take ~1.0 s. Expected: up to
   ~1 s on every visit. Cost: high (a build step in Node.js, the heap in
   the data of the binary, reseeding of random state). Risk: medium.
9. **etcd without gRPC.** The etcd path is ~0.6 s of self time (gRPC 0.37 s,
   its HTTP/2 framer 0.1 s, Google protobuf 0.13 s, the etcd client 0.05 s;
   the shim 0.02 s), plus its share of allocation and GC. The objects stay
   protobuf-encoded either way. Expected: ~0.5-0.8 s. Cost: high, a storage
   backend of kube-apiserver that calls the shim directly, and a new byte
   counter for `cpShares` and `cpBytesRead` (`tabnode_stats.go`), which
   now count the bytes on the etcd listener to split the CPU of `kubectl
   top` between the control plane pods. Risk: high.
10. **The clock of the page.** `runtime.walltime` (BigInt arithmetic in
    `clock.js`) and `runtime.nanotime1` take ~0.25 s of the main thread
    per boot. Expected: ~0.1-0.2 s. Cost: very low (number arithmetic that
    keeps the never-repeating clock). Risk: low.
11. **The first `wait.Poll` second of `rbac/bootstrap-roles` and
    `scheduling/bootstrap-system-priority-classes`.** Hidden behind other
    work today (the phase is 98 % busy). Expected gain now: ~0; it shows
    once the rest of the `/readyz` phase is shorter than a second.

Not candidates: the PKI (60 ms), the etcd shim start (5 ms), the tab node's
own start (0.15 s), the file system of the page (0.25 s, mostly log lines)
and klog (30 ms self).

The 1.2 s from the end of `boot()` to the prompt is the first syncs of the
controllers: Go returns to the event loop only when no goroutine can run.
After candidate 1 it is 0.7 s. It shrinks only with less work at that
point, not by itself.

## How to run

Build the page, then run the script from `tabernetes/`:

```bash
mise run build
```

```bash
mise run site
```

```bash
node docs/boot-time/measure.mjs --rounds 8 --out boot-time.json
```

It prints the tables of this document and writes all raw data (marks, calls
into Go, klog lines of the controller-manager) to `--out`.
`--summary boot-time.json` prints the tables again. More options:

- `--cases first,return,reload`, `--pod-delays 0,1500,3000`,
  `--settle 30000` (ms after the prompt to watch the addons).
- `--no-marks`: boot without `?bootmarks`, only the time to the prompt.
- `--profile --site DIR`: one first visit with a CPU profile; writes
  `<out>.cpuprofile` (opens in Chrome DevTools) and the top functions,
  packages and groups. It needs a binary with names: link
  `k8s/build.sh` without `-s -w`, compress it as `mise run wasm` does and
  put it into a copy of `.build/site`. `--reprofile FILE` analyses a saved
  profile again.
- `--throttle MBIT` limits the download of the page.

A full run of 8 rounds takes ~30 minutes.
