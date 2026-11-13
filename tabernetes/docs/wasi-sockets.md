# Sockets for WebAssembly pods

Status: draft design, revision 4. No part of this design is in the code.
Revisions 2, 3 and 4 contain the findings of three independent reviews.
Revision 4 also contains the changes on `main` up to commit 7286cdd.
Phase 0 starts after revision 4, without a fourth review of the text.

## Language

This document obeys the rules of ASD-STE100 Simplified Technical English.
These words have a special function:

- Technical names: the names of code, files, Kubernetes objects, protocols,
  standards, programs and the terms of section 3.
- These words are also technical names: cache, import, export, exit code,
  status code. An import or an export is an item of a WebAssembly module or
  component.
- Technical verbs. Each verb has the same definition as in computer
  networks or in WebAssembly. This document uses these words only as verbs:
  - accept, bind, bundle, compile, connect, decompress, deny, dial, drop
  - exit, fail, forward, instantiate, intercept, listen, load, parse, poll
  - publish, run, serve, suspend, throw, transpile, trap, wrap
- "must" and "must not" give a requirement. "can" gives a possibility, not a
  requirement.
- "stop" means "halt". "prevent" means "make sure that a thing does not occur".
- "Linux node" means a node with a Linux kernel, for example a Docker node.

Paths are relative to `tabernetes/`. The path `.github/` is at the root of
the repository.

## 1 Purpose

WebAssembly pods on a tab node cannot open sockets. A server in such a pod
cannot receive connections. A client in such a pod cannot connect to a
Service. This document gives a design that adds TCP sockets, UDP sockets,
name lookup and HTTP to these pods.

The same pod manifest must have the same results on a tab node and on a
Docker node. This rule controls most decisions in this document. Section 11
lists the differences that stay.

## 2 Scope

These items are in the scope of this document:

- WASI preview 2 components on tab nodes, in the command world and in the
  proxy world.
- TCP and UDP sockets of `wasi:sockets`, and `wasi:sockets/ip-name-lookup`.
- The volumes of components.
- Client requests of proxy components.
- UDP for all pods of a tab node: a UDP table, Services, flows,
  NetworkPolicy and cluster DNS.
- UDP between tab nodes and Docker nodes.
- An ICMP echo reply for the `ping` built-in command, on one tab node.
- The CPU time of the socket work, in `kubectl top`.

These items are not in the scope of this document:

- Sockets for WASI preview 1 modules. Section 13 gives the reason.
- WASI preview 3 components.
- Components with an import of `tabernetes_webgpu` or `wasi:webgpu`.
- Components that the mirror transpiles before publication.
- Threads, `fork` and `exec` in WebAssembly pods.
- IPv6 in the cluster.
- Raw sockets, IP packets, `tcpdump` and `traceroute` on tab nodes.
- Connections and client requests from a pod to the internet.
- ICMP between nodes.
- Multicast and broadcast.
- NodePort and hostPort for UDP with an endpoint on `tab`. The host network
  namespace of a Docker node does not reach tab pods (issue #61, section
  4.4).
- Bodies of HTTP requests and responses in parts (streams). Section 13
  gives the reason.
- The current behavior of CoreDNS: it moves from the tab node to a Docker
  node (`tabnode_coredns.go`). This document does not change this behavior.

## 3 Terms

| Term | Definition |
| --- | --- |
| page | The web page of Tabernetes in the browser tab. It runs the first tab node. |
| tab node | A kubelet in the Wasm binary (`k8s/tabernetes/tabnode*.go`). The page runs one tab node. Each node worker runs one more tab node. |
| node worker | A Web Worker that runs a tab node without the control plane (`web/src/node.worker.js`). |
| Docker node | A node in a Docker container, with containerd, runwasi and the node agent (`node/`). |
| Go thread | The thread that runs the Wasm binary of one tab node. It also runs `web/src/wasi.js` of that tab node. Each tab node has its own Go thread. |
| Go side | The code of a tab node on its Go thread that starts pod workers and has the sockets of their pods. |
| pod worker | The Web Worker that runs one container of a WebAssembly pod (`web/src/wasi.worker.js`). |
| transpile worker | A new Web Worker that transpiles components with jco. Each Go thread has one transpile worker. |
| module | A WASI preview 1 core module (`wasm32-wasip1`, `GOOS=wasip1`). |
| component | A WASI preview 2 component (`wasm32-wasip2`). |
| world | The set of exports that shows how a runtime runs a component. |
| command component | A component in the command world: it has the export `wasi:cli/run`. |
| proxy component | A component in the proxy world: it has the export `wasi:http/incoming-handler`. |
| instance | One instantiation of the core modules of a component, with its own memory. |
| client request | An HTTP request that a component sends with `wasi:http/outgoing-handler`. |
| HTTP path | The code in the pod worker that moves requests, responses and client requests between the component and the Go side. Phase 0 selects it (section 9.1, item 12). |
| mirror | The OCI mirror of WebAssembly images that the page reads (`wasm-images/`). |
| in-process network | The network of Go for `js/wasm` (`net_fake.go`). `net.Listen` and `net.Dial` connect goroutines of the same Go thread. |
| UDP table | A new table of UDP sockets in each tab node. It is separate from the in-process network. |
| loopback table | A new table in each tab node of the sockets that bind to `127.0.0.1` (section 8.11). |
| Service translation | The change of a ClusterIP and port to the IP and port of an endpoint, as kube-proxy does it with DNAT. |
| flow | The state of one UDP conversation, with a timeout. |
| strict mode | The part of the tab node that examines open connections again after a NetworkPolicy changes (`tabnode_netpol_strict.go`). |
| source node, destination node | The node that sends a datagram, and the node that receives it. |
| message | A message between two threads (`postMessage`), or a message on a channel between nodes. |
| credit | A number of bytes or items that a receiver can take. The sender stops when its credit is 0. |
| Docker check | The script `docs/docker-node-check/check.sh`. It joins a Docker node in a headless Chromium and runs steps on it (`docs/docker-node-check.md`). |
| deferred test | A test with NetworkPolicy on a Docker node. It runs only after the nfqueue item of section 4.4 closes (section 9). |

## 4 Current condition

### 4.1 Pod workers

- A pod worker runs modules with `@bjorn3/browser_wasi_shim`. It has no
  socket functions. The section "Limits" of `README.md` (line 782) gives
  this limit.
- For a component, `web/src/wasi.js` (lines 48 to 50) gives an
  "unsupported" text.
- A pod worker uses JSPI. `fd_read` on stdin and `poll_oneoff` can suspend
  until a message arrives. The page does not use cross-origin isolation.
- Without a subscription to stdin, `poll_oneoff` waits with `Atomics.wait`
  if shared memory is available (`wasi.worker.js`, lines 13 to 17 and 295
  to 296). During this wait, the worker cannot receive messages.
- A pod worker sends `mem` messages (`wasi.worker.js`, lines 191 to 198).
  The tab node uses them for `limits.memory`.
- A pod worker gives the import module `tabernetes_webgpu` to modules
  (`wasi.worker.js`, lines 210 to 214).
- A module gets a copy of the files of its container (`wasmPreopens`,
  `tabnode_wasi.go`, lines 664 to 671). The root and each directory volume
  are preopened directories. The copy has a limit of 64 MiB. The writes of
  the module stay in the pod worker and end with it.
- The pod worker makes the preopened directories with `PreopenDirectory` of
  `browser_wasi_shim` (`wasi.worker.js`, lines 58 and 176). Each file of a
  read-only volume has the flag `readonly`.

### 4.2 The Go thread and the build

- `wasi.js` keeps compiled modules in a cache. The key is the layer digests
  and the path (`wasi.js`, line 19).
- Each node worker installs its own `wasi.js` (`web/src/node.worker.js`,
  lines 11 and 38). So each tab node has its own cache and its own pod
  workers.
- The task `web` of `mise.toml` bundles each `src/*.worker.js` with esbuild
  in the IIFE format (lines 109 to 112). In this format, `import.meta.url`
  is empty.

### 4.3 The network of a tab node

- The tab node uses the in-process network for TCP. `listenPod` and
  `dialCluster` (`tabnode_dial.go`) bind the pod IP.
- `listenPod` changes `0.0.0.0`, `::`, `127.0.0.1` and `::1` to the pod IP
  (lines 27 to 29). The tab node has no loopback for each pod.
- `dialCluster` always dials TCP. It does not use its `network` argument
  (lines 49 to 60).
- `httpClientFor` gives a cluster client for hosts in the cluster. For other
  hosts, it gives `http.DefaultClient`, which uses `fetch()` of the browser
  (lines 109 to 118).
- `tabnode_net.go` (the kube-proxy and the DNS of the tab node) does not
  forward UDP ports (lines 162 and 163). CoreDNS on a tab node uses TCP only.
- `netPolicy.allowed` examines TCP only (`tabnode_netpol.go`, line 135).
- `dialFromPod` decides NetworkPolicy before the connection exists
  (`tabnode_netpol.go`, lines 201 to 244). `policyListener.Accept` decides
  ingress after `Accept` and then closes the connection (lines 282 to 297).
- A connection that NetworkPolicy denies waits for `synTimeout` (127
  seconds, `tabnode_netpol.go`, line 64) and then fails.
- The strict mode closes open connections that a new policy denies
  (`tabnode_netpol_strict.go`, lines 62 to 79).
- CoreDNS answers with `lookupName` (`tabnode_coredns.go`, line 342).
  `lookupName` calls `chargeDNS` one time for each query. Commit 84da797
  removed a second charge for queries over TCP.

### 4.4 Other nodes

- Node workers connect to the page through MessagePorts
  (`web/src/channel.js`, `web/src/nodes.js`). Docker nodes connect to the
  page through WebRTC.
- A MessagePort has no signal for back pressure. `channel.js` sets
  `bufferedAmount` to 0 (line 10).
- The pod channel between nodes has one ordered channel for each TCP
  connection (`k8s/tabernetes/podlink.go`, lines 7 to 21). The label of the
  channel gives the source node. The first line is `TABP/1 tcp <src> <dst>`.
  The destination node sends `ok` or `refused`.
- The page examines the header of each pod channel that it forwards. A
  Docker node has no informers and does not examine the header again
  (`docs/networking-policy.md`, lines 531 to 533).
- On a Docker node, `node/agent/podlink.go` intercepts TCP to tab pods with
  TPROXY. Its nftables rules are for TCP only (lines 100 and 109).
- The rules of kube-network-policies accept every packet of uid 0 on a
  Docker node. So the agent makes each socket to a pod on a thread with the
  file system uid 65534. `podLinkUID` (line 70) and `socketAs` (lines 419
  to 443) do this. `docs/networking-policy.md` (lines 534 to 536) gives the
  reason.
  `socketAs` makes TCP sockets only.
- runwasi (`containerd-shim-wasmtime` v0.6.1, `node/Dockerfile` line 24)
  runs WebAssembly pods on Docker nodes.
- The NetworkPolicy tests on a Docker node must have nfqueue in a rootless
  node. `docs/networking-policy.md` (lines 3 to 5) gives this item as not
  complete.
- The host network namespace of a Docker node does not reach tab pods
  (issue #61, `docs/docker-node-check.md`, lines 78 to 83). The kernel drops
  packets on `tabpods1` with a node address as their source. Pods of the
  Docker node reach tab pods.
- Without a proxy, a Docker node has no internet. It pulls only from the
  OCI mirror of its node image (`node/agent/imagemirror.go`). This mirror
  has the pause image and the images of the add-ons, for example
  kube-proxy, flannel and CoreDNS.
- The Docker check drives a Docker node from headless Chromium
  (`docs/docker-node-check/`: `check.sh`, `tab.mjs`, `browser.mjs`,
  `manifests.yaml`). Each step writes PASS, FAIL or SKIP. `IMPORT=1`
  imports images such as busybox into the node.

### 4.5 CPU time on a tab node

- `countBusy` (`web/src/clock.js`, line 24) measures the busy time of the
  Go thread.
- `nodeSysModel.sample` (`k8s/tabernetes/tabnode_stats.go`, line 263) takes
  the time that containers got with `ChargeCPU` off this busy time.
- `cpShares` (line 179) divides the time that remains between the control
  plane pods by the bytes of their REST requests.
- `tabnode_images_ingress.go` (lines 1103 to 1106) charges its work to the
  container with `acctFrom(ctx).ChargeCPU(time.Since(start))`.
- A WebAssembly container has the profile `wasmProfile` with 0 CPU
  (`tabnode_acct.go`, line 95). The pod worker runs on its own thread, so
  `countBusy` does not see its time.

## 5 Facts that control the design

The author and three reviewers examined these facts in the sources on
2026-11-13. Section 15 gives the sources.

1. runwasi v0.6.1 gives a command component the complete network of the
   pod. `wasi_builder` (`instance.rs`, lines 415 to 434) calls
   `inherit_network()`, `allow_tcp(true)`, `allow_udp(true)` and
   `allow_ip_name_lookup(true)`.
2. runwasi gives a module no sockets. It runs a module with `build_p1()`
   (`instance.rs`, line 220). In wasmtime 36.0.10, the version of runwasi,
   `sock_accept`, `sock_recv`, `sock_send` and `sock_shutdown` always return
   `Notsock` (`crates/wasi/src/preview1.rs`, lines 2647 to 2693).
3. The CLI of wasmtime gives sockets that listen to modules only with its
   legacy implementation of preview 1 (`-S preview2=n`,
   `crates/cli-flags/src/lib.rs`, lines 592 to 600). For components, the
   CLI fails with the text "components do not support --tcplisten" (line
   364 of `src/common.rs`). runwasi does not use the CLI, so fact 2 is the important
   fact.
4. runwasi serves a proxy component itself (`http_proxy.rs`):
   - It parses `WASMTIME_HTTP_PROXY_SOCKET_ADDR` and binds the complete
     address, with its IP and its port (lines 75 to 98).
   - If the environment does not contain this variable, or if the value does
     not parse, the address is `0.0.0.0:8080`. No error occurs.
   - The value `[::]:8080` parses and gives an IPv6 socket (lines 84 to
     87). Linux then also serves IPv4 on it.
   - runwasi removes `WASMTIME_HTTP_PROXY_SOCKET_ADDR` and
     `WASMTIME_HTTP_BACKLOG` from the environment of the component.
   - Each request gets a new store and a new instance. Requests run at the
     same time, each in a task of its own (lines 165 to 239). Requests on
     one HTTP/1.1 connection run in sequence.
   - The context of a request contains the environment and `REQUEST_ID`, a
     counter that starts at 0. It has no arguments, no preopened
     directories, no stdio and no network.
   - If the instance traps, runwasi gives an error to hyper (lines 214 to
     233). The container continues to run.
   - If the instance sets an error code as its response, runwasi also gives
     an error to hyper (line 216).
   - If `handle` returns normally and sets no response, runwasi calls
     `expect_err` on a result without an error (lines 225 to 228). This
     causes a panic. The release profile of runwasi sets `panic = "abort"`
     (`Cargo.toml`, lines 60 and 61). Phase 0 (item 14) examines the
     published binary.
   - runwasi has no timeout for a request.
   - The linker of a proxy component contains `wasi:http`
     (`instance.rs`, lines 263 to 265).
5. runwasi finds the world of a component from its exports (`instance.rs`,
   lines 32 to 51). The first export with the name prefix
   `wasi:http/incoming-handler` sets the proxy world. The first export with
   the prefix `wasi:cli/run` sets the command world. For other components,
   runwasi calls the function that the entrypoint names (lines 292 to 307).
   The linker of a command component contains no `wasi:http` (lines 402 to
   413).
6. The default context of wasmtime 36.0.10 has `tcp: true`, `udp: true` and
   `ip_name_lookup: false` (`crates/wasi/src/sockets/mod.rs`, lines 42 to
   50). `SocketAddrCheck` denies all addresses (lines 122 to 126). For each
   function of `wasi:sockets` in a proxy component, the tab node must have
   the same result. Phase 0 records these results.
7. Go cannot compile a component with sockets. Go 1.27.3 (the version in
   `mise.toml`) and the `master` branch have the port `wasip1` only
   (`src/internal/syslist/syslist.go`). The preview 1 adapter can wrap a
   module into a component, but this component gets no `wasi:sockets`. Rust
   (`wasm32-wasip2`) and TinyGo (target `wasip2`) can compile components.
8. jco 1.42.0 has an export for the browser,
   `@bytecodealliance/jco/component`, with the function `generate`. The
   option for JSPI is `asyncMode: {tag: "jspi", val: {imports, exports}}`.
   The list of jco itself (`dist/common.js`, lines 9 to 19) contains:
   - imports: `wasi:io/poll#poll`, `pollable.block`,
     `input-stream.blocking-read`, `input-stream.blocking-skip`,
     `output-stream.blocking-flush`, `output-stream.blocking-write-and-flush`,
     `output-stream.blocking-write-zeroes-and-flush` and
     `output-stream.blocking-splice`;
   - exports: `wasi:cli/run#run` and `wasi:http/incoming-handler#handle`.

   Only a function in `asyncMode.val.imports` can suspend. The option for
   the browser is `instantiation: {tag: "async"}`
   (`js-component-bindgen-component-instantiation.d.ts`, lines 33 to 36).
9. The browser shim of jco (`@bytecodealliance/preview2-shim` 0.31.1):
   - All functions of `dist/browser/sockets.js` throw `not-supported`. The
     file also exports `InMemoryTcpSockets` and `InMemoryUdpSockets`.
   - The class `WASIShim` (`dist/common/instantiation.js`) is the point for
     extensions. Its comment marks it "(EXPERIMENTAL)" (line 3).
   - `WASIShim` takes `environment`, `arguments`, `initialCwd`, `stdin`,
     `stdout` and `stderr`, and calls `createCli` itself (lines 110 to 126).
   - It takes a complete `sockets` object, or `tcpSockets` and `udpSockets`
     (lines 160 to 181). It has no option for `ip-name-lookup` alone.
   - It takes a complete `http` object (line 182).
   - It takes `filesystem`, or `browserFilesystem` with an adapter and
     preopens (lines 128 to 156). `createCli` gives no files.
   - `dist/browser/io.js` exports `pollableCreate`, `inputStreamCreate` and
     `outputStreamCreate`.
   - `poll` with a ready pollable continues after `setTimeout(0)`
     (`dist/browser/io.js`, lines 331 to 343).
   - The shim removes the factories of its HTTP resources after it loads
     (`dist/browser/http.js`, lines 573 to 575, 627 to 629, 708 to 710 and
     830). Other code cannot make these resources.
   - The shim reads `clone()`, `headers.entries()`, `body`, `method`, `url`
     and `arrayBuffer()` of a request (lines 612 to 621). It makes the
     response with `new Response(body, {status, headers})` (lines 661 to
     672). It keeps the complete response body without a limit (lines 250
     to 270).
   - `method()` gives `{tag: method.toLowerCase()}` (line 581). A method
     that is not standard then gives a variant that is not correct.
   - For a response with an error code, the shim sends status code 500 with
     the text "WASI HTTP handler error" (line 691).
   - `wasi:http/outgoing-handler` calls `globalThis.fetch` (line 800). The
     shim changes each failure of `fetch` to `internal-error`, or to
     `connection-timeout` for an `AbortError` (`mapFetchError`, lines 739
     to 744). It stops a `fetch` after its default of 600 seconds or when
     the component discards the future.
   - `cli.js` (lines 13 to 25) gives only the exit codes 0 and 1. Other
     exit codes need the unstable function `exit-with-code`.
10. The transpiler is in `@bytecodealliance/jco-transpile` 0.24.0, a
    dependency of jco. The browser path uses `dist/component.js`, the
    `-instantiation.js` file (558,290 bytes) and four core modules in
    `vendor/`. The largest core module has a size of 5,669,325 bytes
    (1,935,646 bytes with `gzip -9`). The code loads the core modules from
    `new URL('../vendor/…', import.meta.url)`. A parser for components
    (wasm-tools) is one more core module of 2.4 MB.
11. The in-process network of Go has these properties for UDP (`net_fake.go`
    of Go 1.27.3, the same as `master`; tests with Go 1.24.7):
    - The key of a socket is the address family and `ip:port`, without the
      protocol. TCP and UDP cannot use the same port on the same IP.
    - The receive queue of a socket has 65,535 bytes. In a test, 47 of 100
      datagrams of 1,400 bytes arrived. The other sends failed with
      `ENOBUFS`. `SetReadBuffer` can change the size of the queue.
    - `WriteTo` to an address without a socket gives no error. `Write` on a
      connected socket gives `ECONNRESET`.
    - `WriteTo` puts the datagram into the queue of the receiver. No
      NetworkPolicy and no Service translation occur.
    - A socket on a wildcard address receives nothing.
    - Ephemeral ports start at 1, for all IPs together. This is also true
      for TCP.
    - A socket can send a datagram of 65,535 bytes. This is more than the
      IPv4 limit of 65,507 bytes.
    - The boundaries of datagrams and the source address do not change.
12. `wasi:sockets` 0.2 (`tcp.wit`):
    - A socket cannot use IPv4-mapped IPv6 addresses (lines 61 and 103:
      `invalid-argument`). An IPv6 socket cannot connect to IPv4 peers.
    - A remote address of the wrong family gives `invalid-argument` (line
      101).
    - A second bind to an address gives `address-in-use` (line 64). An
      address that the network cannot bind gives `address-not-bindable`
      (line 65).
    - `set-listen-backlog-size` with the value 0 gives `invalid-argument`
      (line 247).
13. pion/sctp is the SCTP of the node agent. Its maximum message size is
    65,536 bytes if the configuration does not set a different size
    (`association.go`, line 84).
14. JSPI is in Chrome and Edge 137, Firefox 153 and Safari 27. The dataset
    `web-features` 3.46.0 gives the value "Baseline newly available" since
    2026-09-14.
15. kube-network-policies v1.1.3 (the version in `mise.toml`) examines
    ICMP. `ParsePacket` gives no protocol and port 0 for ICMP
    (`pkg/network/packet.go`, lines 160 to 162). The engine examines a
    connection again after a policy changes
    (`pkg/dataplane/controller.go`, lines 265 to 272). It does not examine
    ICMP flows again (lines 448 to 453).
16. Linux has these defaults:
    - `nf_conntrack_udp_timeout` is 30 seconds.
    - `nf_conntrack_udp_timeout_stream` is 120 seconds.
    - A flow gets the timeout of 120 seconds only if it has answers. It must
      also be active 2 seconds after its first datagram
      (`nf_conntrack_udp_packet` in `net/netfilter/nf_conntrack_proto_udp.c`).
    - `ip_local_port_range` is 32768 to 60999.
17. In the iptables mode of kube-proxy, a ClusterIP without endpoints
    rejects a datagram with ICMP "port unreachable". A connected socket then
    gets `ECONNREFUSED`.
18. `@wasmer/sdk` 0.19.1 runs WASIX programs. It must have cross-origin
    isolation. Its network is a WISP connection to a server, HTTP to
    listeners through its own service worker, and TCP between its sandboxes.
    Its type definitions export no interface for a different network.
19. In a worker of Chrome 153, a browser `Request` and `Response` change the
    data:
    - `new Request` with 17 headers kept only `authorization`,
      `content-type` and one custom header. It removed, for example, `Host`,
      `Cookie`, `Origin`, `Referer`, `User-Agent` and `Accept-Encoding`.
    - `new Request` throws for `TRACE`, `CONNECT`, a GET with a body and a
      URL with credentials.
    - `new Response` removes `Set-Cookie`. It throws `RangeError` for a
      status code outside 200 to 599. It throws `TypeError` for 204 or 304
      with a body.
20. wasmtime 36.0.10 `crates/wasi-http/src/types.rs`:
    - It removes 9 headers from a request that arrives (lines 324 to 334 and
      588): `connection`, `keep-alive`, `proxy-authenticate`,
      `proxy-authorization`, `proxy-connection`, `transfer-encoding`,
      `upgrade`, `host` and `http2-settings`.
    - For a client request, a name that does not resolve gives `DNS-error`.
      The connect timeout of 600 seconds gives `connection-timeout`. Each
      other connect error gives `connection-refused` (lines 402 to 419).
21. In a worker of Chrome 153, 200 chained calls of `setTimeout(0)` took
    807 ms, because of a minimum of 4 ms. A message on a `MessageChannel`
    has no such minimum.
22. In Chrome 153, a worker from a `data:` URL has an opaque origin.
    IndexedDB and `caches` throw `SecurityError` in it. It has JSPI, and it
    can `import()` a `Blob` URL that it makes.

## 6 Decisions

1. WebAssembly pods get sockets through WASI preview 2 only. Facts 1 and 2
   show that only components have sockets on a Docker node. Modules stay
   without sockets.
2. Each Go thread has one transpile worker and one cache of results. The
   key of the cache contains the layer digests, the path, the version of
   jco, the entrypoint and the lists of `asyncMode`.
3. The pod worker runs the output of jco in JSPI mode, with `WASIShim`
   (fact 9). The pod worker gives these objects to `WASIShim`:
   - a complete `sockets` object, with `ip-name-lookup`;
   - `environment`, `arguments`, `stdin`, `stdout` and `stderr`;
   - a `browserFilesystem` adapter over the copy of the files of section
     4.1 (section 8.10).

   The shim gives the other interfaces.
4. A browser `Request` or `Response` object must not hold the data of a
   request, a response or a client request. Fact 19 shows the data that it
   removes. Phase 0 (item 12) selects the HTTP path from two candidates:
   - objects with the same functions as `Request` and `Response`, which the
     shim reads (fact 9), without the rules of the browser;
   - a `wasi:http` of Tabernetes, through the option `http` of `WASIShim`.

   Request bodies and response bodies are complete blocks, with a maximum
   of 16 MiB.
5. The Go side has every socket. TCP sockets use the in-process network
   through `listenPod` and `dialCluster`. UDP sockets use the UDP table,
   because the in-process network cannot keep TCP and UDP on one port
   (fact 11).
6. A socket that binds to `127.0.0.1` listens on the pod IP. The loopback
   table (section 8.11) records it. Each path that dials such a socket
   examines the loopback table before it connects. So a client from outside
   the pod gets `connection-refused`, as on a Linux node.
7. The tab node serves proxy components the same as runwasi (fact 4). Each
   request gets a new instance. Requests run at the same time, with the same
   context as on runwasi.
8. Client requests of proxy components use the cluster client of
   `tabnode_dial.go`. Client requests to hosts outside the cluster fail,
   because the internet is out of scope. The error codes are those of
   wasmtime (fact 20). Command components get no `wasi:http`, the same as
   on runwasi (fact 5).
9. The UDP table is for all pods of a tab node. CoreDNS, `nslookup` and
   `nc -u` of the tab node use it.
10. The page examines the source of each datagram between nodes, the same
    as for TCP. Docker nodes do not examine the source again.
11. The tab node does not get IP packets. The ICMP echo reply is a function
    of the tab node. The NetworkPolicy engine examines each echo request.
12. For a failure, the tab node gives the same pod status as runwasi.
    Phase 0 records the status on runwasi for each failure of section 7.1.
13. The pod worker of a component runs in a worker with an opaque origin,
    from a `data:` URL (fact 22). Phase 0 (item 16) examines it in Firefox
    and Safari. If it fails there, the pod worker runs from a `Blob` URL on
    the origin of the page.
14. The Go side examines each message from a pod worker before it does the
    step of the message. The generated JavaScript runs next to the message
    port, so it can send each message (section 7.12).
15. The Go side charges the CPU time of its socket work to the container
    with `ChargeCPU` (section 4.5). Without this, `kubectl top` shows this
    time as CPU of the control plane pods.
16. A test with NetworkPolicy on a Docker node is a deferred test. A phase
    can end without its deferred tests. Phase 5 cannot end before all
    deferred tests give correct results.

## 7 Requirements

Each table has a column "Note" for the reason or for an example. A note is
not a requirement.

### 7.1 Components

| ID | Requirement | Note |
| --- | --- | --- |
| CO1 | The tab node must run a command component in a pod worker. | |
| CO2 | The tab node must find the world of a component in the same sequence as runwasi. | Fact 5. |
| CO3 | For a component without the two exports, the tab node must call the function that the entrypoint names. | runwasi does this (fact 5). The pod worker puts this function into `asyncMode.val.exports`. |
| CO4 | If this function is not in the exports, the pod must get the status that runwasi gives. | Phase 0, item 7. |
| CO5 | The arguments, the environment, stdin, stdout and stderr of a command component must agree with those of a module. | `WASIShim` gives them through `createCli` (fact 9). Section 7.10 gives the files. |
| CO6 | The memory of each instance must be part of the memory that `limits.memory` controls. | |
| CO7 | When the pod worker discards an instance, the counted memory must decrease by the memory of this instance. | The browser frees the memory later, at garbage collection. |
| CO8 | The transpile worker must transpile a component one time for each key of the cache. | Decision 2. Other pods and restarts use the result in the cache. |
| CO9 | The transpiler must not run on a Go thread. | A transpiler on the Go thread stops all other work of the tab node. |
| CO10 | If the browser has no JSPI, a component pod must get `CreateContainerError` with a text that gives the cause. | Fact 14: old browsers only. |
| CO11 | If a component has an import that the tab node does not give, the pod must get the status that runwasi gives. | Examples: WASI 0.3, `wasi:keyvalue`, a function of a newer `wasi:*@0.2.x` version. On runwasi, the container exits with status 137 (section 9.2). |
| CO12 | The status text must give the name of the import. | |
| CO13 | A module must run with no change. A module gets no socket functions. | |
| CO14 | The generated JavaScript of a component must run only in a pod worker. | Not on a Go thread and not in the page. |
| CO15 | The transpile worker must examine all names of imports and exports in the result of `generate`. It must do this before the result goes to the Go side. | A parser before `generate` is 2.4 MB more (fact 10). |
| CO16 | A name with characters that are not in the WIT syntax must fail with `CreateContainerError`. | Section 8.2. |
| CO17 | The pod worker must count the calls in sequence that do not wait and give no data. After 1,000 such calls, it must suspend the next call for one task of the event loop. | Section 8.5 gives the calls. Without this pause, a component that polls never receives messages. |
| CO18 | A command component must exit with the exit code 0 or 1, as on runwasi. A component with an import of `exit-with-code` must get the status of CO11. | runwasi does not link `exit-with-code` (section 9.2, item 13). The shim has it (fact 9), so the pod worker must not give it. |
| CO19 | The pod worker must continue a suspended call with a message on a `MessageChannel`, not with `setTimeout`. | Fact 21. |

### 7.2 Proxy components

| ID | Requirement | Note |
| --- | --- | --- |
| PX1 | The tab node must serve a proxy component on the address of `WASMTIME_HTTP_PROXY_SOCKET_ADDR`. | Fact 4. |
| PX2 | If the environment does not contain this variable, the address must be `0.0.0.0:8080`. | |
| PX3 | If the value does not parse, the address must be `0.0.0.0:8080`, without an error. | runwasi does the same. |
| PX4 | The IPs `0.0.0.0` and `::` must listen on the pod IP. | Linux serves IPv4 on `[::]` (fact 4). |
| PX5 | The IP `127.0.0.1` must listen on the pod IP and accept only connections from the pod IP. | Decision 6. |
| PX6 | The tab node must remove `WASMTIME_HTTP_PROXY_SOCKET_ADDR` and `WASMTIME_HTTP_BACKLOG` from the environment of the component. | |
| PX7 | Each request must get a new instance of the component. | |
| PX8 | Requests must run at the same time. | The instances use one thread. Section 11. |
| PX9 | One container must have a maximum of 16 instances at the same time. | runwasi has no maximum. Section 11. |
| PX10 | When 16 instances run, more requests must wait in a queue. | |
| PX11 | The environment of an instance must contain `REQUEST_ID`. | |
| PX12 | The first request of a container must get the `REQUEST_ID` 0. Each request after it must get the next number. | |
| PX13 | An instance must get no arguments, no preopened directories and an empty stdin. | |
| PX14 | The tab node must discard stdout and stderr of an instance. | runwasi gives no stdio. |
| PX15 | Each function of `wasi:sockets` in an instance must give the same result as on runwasi. | Fact 6. Phase 0, item 7. |
| PX16 | The response must go to the client with its status code, all its headers and its body. | `Set-Cookie` is an example (fact 19). |
| PX17 | If the response has no `Content-Type`, the tab node must not add one. | `net/http` of Go adds one if the code does not prevent it. |
| PX18 | A request body or a response body of more than 16 MiB must fail. | Decision 4. |
| PX19 | For a request body of more than 16 MiB, the client must get the status code 413. | |
| PX20 | For a response body of more than 16 MiB, the client must get the status code 500. | The shim keeps the complete body without a limit (fact 9). The HTTP path must examine the size. Phase 0, item 12. |
| PX21 | If an instance traps, the client must get the same result as on runwasi. | Section 9.2: the client gets no response, and the connection closes. |
| PX22 | After an instance traps, the container must continue to run. | Fact 4. |
| PX23 | The tab node must not set a timeout for a request. | runwasi has no timeout. |
| PX24 | If the client closes its connection, the pod worker must discard the instance of this request. It must also cancel the timers, the client requests and the pollables of this instance. | The instance does not continue. Section 11. Phase 0, item 15. |
| PX25 | The pod worker must discard an instance after `handle` returns and after the response body is complete. | |
| PX26 | If `handle` returns and sets no response, the client, the container state and the restart count must be the same as on runwasi. | Section 9.2: the client gets no response, and the container exits with status 139. |
| PX27 | If the instance sets an error code as its response, the client must get the same result as on runwasi. | Section 9.2: the client gets no response. The shim sends 500 (fact 9), so the HTTP path must not use this response of the shim. |
| PX28 | The component must get all headers of the request, except the 9 headers of fact 20. | `Cookie`, `User-Agent` and `Origin` are examples. |
| PX29 | The component must get each method and each request body. | Examples: a method that is not standard, `TRACE`, a GET with a body (fact 19). |
| PX30 | The tab node must send each status code that runwasi sends. | Examples: 204, 304, a status code outside 200 to 599. |
| PX31 | For the same response, the client must get the same header names as from runwasi. | Examples: `Date`, `Content-Length` or `Transfer-Encoding: chunked`, a client with HTTP/1.0. |
| PX32 | With port 0 in the address, the tab node must listen on an ephemeral port from 32768 to 60999. | |
| PX33 | With an IP that the pod cannot bind, the container must get the same result as on runwasi. | Phase 0, item 7. |

### 7.3 Client requests

| ID | Requirement | Note |
| --- | --- | --- |
| CR1 | A client request of a proxy component must use the cluster client of `tabnode_dial.go`. | Service names, ClusterIPs and NetworkPolicy then have the same results as for other pods. |
| CR2 | A client request to a hostname outside the cluster must fail with the error code `DNS-error`. | wasmtime gives this code for a name that does not resolve (fact 20). Decision 8. |
| CR3 | The cluster client must not follow redirects. | wasmtime gives a redirect to the component. |
| CR4 | The cluster client must not add headers other than `Host`. | wasmtime adds `Host`. Other examples: `User-Agent`, `Accept-Encoding`. |
| CR5 | The cluster client must not decompress the body. | |
| CR6 | A command component with an import of `wasi:http` must get the status that runwasi gives. | Fact 5. Phase 0, item 7. |
| CR7 | A client request to an IP outside the cluster must fail with `connection-refused`. | wasmtime gives this code for a connect error that is not a timeout (fact 20). |
| CR8 | A client request that NetworkPolicy denies must fail after the same time and with the same error code as on a Docker node. | Phase 0, item 17. |
| CR9 | The pod worker must give the error code of the Go side to the component without a change. | The shim changes each failure of `fetch` (fact 9). Section 8.6. |
| CR10 | If the component discards a client request, the pod worker must send `fetch-cancel`. The Go side must then close the connection. | |
| CR11 | A client request must fail with `connection-timeout` if the connection does not occur in 600 seconds. | The default of wasmtime (fact 20). |

### 7.4 TCP

| ID | Requirement | Note |
| --- | --- | --- |
| TC1 | When an IPv4 TCP socket binds to the pod IP or to `0.0.0.0`, it must listen and accept on the pod IP. | |
| TC2 | When a TCP socket binds to `127.0.0.1`, it must listen on the pod IP. | Decision 6. |
| TC3 | This socket must accept only connections from the pod IP. | |
| TC4 | Other connections to this socket must fail with `connection-refused` when the client dials. | A Linux node gives this result. An examination after `Accept` gives a connection that then closes. Section 8.11. |
| TC5 | After a socket binds to `127.0.0.1`, `local-address` must give `127.0.0.1`. | |
| TC6 | An IPv6 socket must be able to bind to `::`. | |
| TC7 | An IPv6 socket must not listen in the in-process network. | Fact 12. `listenPod` changes `::` to the pod IP, so the Go side must not call it for IPv6. |
| TC8 | A socket that connects must use `dialCluster`. | |
| TC9 | A socket that connects before it binds must get an ephemeral port from 32768 to 60999. | Fact 11: the in-process network starts at 1. |
| TC10 | A connection that NetworkPolicy denies must fail with `timeout`. | The wait is 127 seconds (section 4.3). |
| TC11 | A connection to a port without a listener must fail with `connection-refused`. | |
| TC12 | Each connection must have a credit of 256 KiB in each direction. | Section 8.4. |
| TC13 | `shutdown` must close the receive direction, the send direction or the two directions, as the component asks. | |
| TC14 | When a container stops, the Go side must close all sockets of the container. | |
| TC15 | A kubelet probe, the Service proxy and the pod channel must get `connection-refused` from a socket of TC2. | Their source is not the pod IP. Section 8.11. |
| TC16 | `kubectl port-forward` must reach a socket of TC2. | On a Linux node, port-forward connects in the network namespace of the pod. |
| TC17 | For a connection from the same pod to `127.0.0.1`, the two sockets must show `127.0.0.1` as their local address and as their remote address. | |
| TC18 | A second bind to the same IP, port and protocol must fail with `address-in-use`. | Fact 12. |
| TC19 | A bind to an IP other than the pod IP, `0.0.0.0` or `127.0.0.1` must fail with `address-not-bindable`. | Fact 12. |
| TC20 | An IPv6 socket that connects to an IPv4 address must fail with `invalid-argument`. | Fact 12. |
| TC21 | `set-listen-backlog-size` with the value 0 must fail with `invalid-argument`. | Fact 12. |
| TC22 | Each socket option must keep its value and give it back. It must have no other effect on a tab node. | Examples: keep-alive, buffer sizes, hop limit. Section 11. |

### 7.5 UDP on a tab node

| ID | Requirement | Note |
| --- | --- | --- |
| UD1 | The key of the UDP table must contain the protocol, the IP and the port. | TCP and UDP can then use the same port on the same IP. |
| UD2 | When a UDP socket binds to the pod IP or to `0.0.0.0`, it must receive on the pod IP. | |
| UD3 | A socket that binds to port 0, or that sends before it binds, must get an ephemeral port from 32768 to 60999. | Fact 16. |
| UD4 | Each IP must have its own range of ephemeral ports. The first ephemeral port of an IP must be 32768. | |
| UD5 | Each UDP socket must have a receive queue of 256 KiB. | Section 8.4. |
| UD6 | If the queue of a socket has no space, the tab node must drop the datagram. | The sender gets no error, as on Linux. |
| UD7 | Each datagram must go through the Service translation and the flow table of section 8.8 before it arrives. | |
| UD8 | The tab node must drop a datagram to a pod IP and a port without a socket on the same tab node. | |
| UD9 | After such a datagram, a connected socket must get `connection-refused` on its next receive or send. | Linux gives this result after ICMP "port unreachable". |
| UD10 | The tab node must drop a datagram to a ClusterIP without endpoints. | Fact 17. |
| UD11 | After such a datagram, a connected socket must get `connection-refused` on its next receive or send. | |
| UD12 | The tab node must drop a datagram to a ClusterIP and a port that no Service has. The sender must get no error. | No rule of kube-proxy matches it. |
| UD13 | A pod must be able to send a datagram to a Service that has this pod as its endpoint. | The datagram goes back to its source pod. |
| UD14 | A datagram of more than 65,507 bytes must fail with `datagram-too-large` in the Go side. | 65,507 bytes is the IPv4 limit. The in-process network accepts 65,535 bytes (fact 11). |
| UD15 | The boundaries and the source address of a datagram must not change. | |
| UD16 | `tabnode_net.go` must forward UDP for Services with `protocol: UDP`. | |
| UD17 | One ClusterIP must be able to have the same port for TCP and for UDP. | Example: the Service `kube-dns`. |
| UD18 | CoreDNS on a tab node must give answers over UDP and over TCP on port 53. | |
| UD19 | `nslookup` and `nc -u` of a tab node must use the UDP table. | |
| UD20 | A UDP socket that binds to `127.0.0.1` must receive only datagrams from the pod IP. | Decision 6. |
| UD21 | The tab node must drop other datagrams to this socket. UD9 then applies. | A Linux node gives ICMP "port unreachable". |
| UD22 | An answer of CoreDNS over UDP that is larger than the query allows must have the TC bit. | 512 bytes without EDNS0. The client then asks again over TCP. |

### 7.6 UDP flows

| ID | Requirement | Note |
| --- | --- | --- |
| FL1 | The key of a flow must contain the tuple before the Service translation and the tuple after it. | |
| FL2 | A flow without answers must close after 30 seconds without a datagram. | Fact 16. |
| FL3 | A flow that has answers and is active 2 seconds after its first datagram must close after 120 seconds without a datagram. | Fact 16. |
| FL4 | Other flows with answers must close after 30 seconds without a datagram. | Example: one DNS query and its answer. |
| FL5 | When the EndpointSlices of a Service no longer contain an endpoint, the tab node must remove all flows to this endpoint. | kube-proxy removes these conntrack entries. |
| FL6 | NetworkPolicy must examine the first datagram of a flow. | |
| FL7 | The next datagrams of an open flow, in the two directions, must not get a new examination. | As conntrack on a Linux node. |
| FL8 | After a NetworkPolicy change, the strict mode must examine all open flows again, with its delay. | Fact 15. |
| FL9 | The strict mode must remove the flows that the policy denies now. | |
| FL10 | `sessionAffinity: ClientIP` must have the same results for UDP as for TCP. | |
| FL11 | `internalTrafficPolicy: Local` must have the same results for UDP as for TCP. | |
| FL12 | A datagram that NetworkPolicy denies must not make a flow. | Else a later policy that allows it has no effect for 30 seconds. |
| FL13 | When a pod stops, the tab node must remove all flows of its pod IP. | A new pod can get the same pod IP and the same first port 32768 (UD4). |

### 7.7 Name lookup

| ID | Requirement | Note |
| --- | --- | --- |
| NL1 | `ip-name-lookup` must find names with the search path of the pod. | |
| NL2 | `ip-name-lookup` must use the same resolver as `nslookup` in a pod of the tab node. | |
| NL3 | A name outside the cluster must fail with `name-unresolvable`. | A Docker node finds such names. Section 11. |

### 7.8 UDP between nodes

| ID | Requirement | Note |
| --- | --- | --- |
| ND1 | The test images of each phase must have the same results on a tab node, on a node worker and on a Docker node. | Section 11 lists the differences that stay. |
| ND2 | Between the page and a node worker, UDP must use one channel `udp` for each node worker, on the MessagePort of `web/src/channel.js`. | |
| ND3 | Between the page and a Docker node, UDP must use one WebRTC data channel `udp` for each node, with `ordered: false` and `maxRetransmits: 0`. | Phase 0, item 10. |
| ND4 | Each message on a `udp` channel must start with a header of 14 bytes. | Section 8.9. |
| ND5 | The largest message must not be more than 65,536 bytes. | 14 + 65,507 = 65,521 bytes. Fact 13. |
| ND6 | The source node must examine the egress policy of the source pod. | |
| ND7 | For each datagram from a node, the page must make sure that the source IP belongs to that node. | The node IP or the pod CIDR of the node. The page knows the node from the channel. |
| ND8 | The page must drop a datagram with a source IP that does not belong to the node. | |
| ND9 | A tab node that is the destination node must examine the ingress policy of the destination pod. | |
| ND10 | On a Docker node, the nftables rules of kube-network-policies must examine the ingress policy. | |
| ND11 | If the node worker did not acknowledge more than 1 MiB of datagrams, the page must drop the next datagram to it. | A MessagePort has no `bufferedAmount` (section 4.4). ND20 gives the acknowledgement. |
| ND12 | The page and the node agent must drop a datagram on a WebRTC `udp` channel when `bufferedAmount` is more than 1 MiB. | |
| ND13 | The node agent must intercept UDP from Docker pods to tab pods with an nftables `tproxy` rule for UDP and an `IP_TRANSPARENT` socket. | |
| ND14 | The node agent must find the original destination of each datagram with `IP_RECVORIGDSTADDR`. | |
| ND15 | The node agent must send answers to a Docker pod from a socket with the original destination as its address. | The answer then has the tab pod IP as its source. |
| ND16 | The node agent must send a datagram from a tab pod to a Docker pod through an `IP_TRANSPARENT` socket. This socket must have the IP and the port of the tab pod. | |
| ND17 | A thread with the file system uid 65534 must make this socket. | Section 4.4. Without it, kube-network-policies does not examine the datagram. |
| ND18 | The node agent must forward the answers of the Docker pod back to the page on the same `udp` channel. | |
| ND19 | The node agent must keep flows with the timeouts of FL2 to FL4. | |
| ND20 | A node worker must send a `credit` message on the `udp` channel for the bytes of the datagrams that it took. | Section 8.9. |

### 7.9 ICMP echo

| ID | Requirement | Note |
| --- | --- | --- |
| IC1 | The `ping` built-in command must get an echo reply from a pod IP on the same tab node. NetworkPolicy must allow the echo request. | IC4 and IC5 give the examination. |
| IC2 | The `ping` built-in command must get an echo reply from the node IP of the tab node. | |
| IC3 | For an IP on another node, `ping` must give no echo reply and must show that ICMP between nodes is not available. | Out of scope. |
| IC4 | The tab node must examine each echo request with the engine of kube-network-policies, with no protocol and with port 0. | Fact 15. |
| IC5 | The tab node must examine the egress policy of the source and the ingress policy of the destination. | |
| IC6 | A ClusterIP must give no echo reply. | The iptables mode of kube-proxy has the same result. The Docker nodes use it. |
| IC7 | `README.md` must show that a policy rule with `ports` does not match ICMP. | |
| IC8 | `README.md` must show that a tab node examines each echo request, but a Docker node examines only the first one. | Fact 15. |

### 7.10 Files of components

| ID | Requirement | Note |
| --- | --- | --- |
| FS1 | A command component must get the same preopened directories as a module of the same container. | The copy of section 4.1. |
| FS2 | A file of a read-only volume must be read-only for a component. | |
| FS3 | The writes of a component must stay in the pod worker and end with it, as for a module. | |
| FS4 | A copy of more than 64 MiB must fail with the same text as for a module. | `wasmSnapshotLimit`. |

### 7.11 CPU time

| ID | Requirement | Note |
| --- | --- | --- |
| AC1 | The Go side must charge the CPU time of its work for a pod to the container with `acctFrom(ctx).ChargeCPU`. | The work: Service translation, the UDP table, flows, the steps of the proxy server, the messages of the pod worker. Section 4.5. |
| AC2 | The pod worker can send a `cpu` message with the busy time of its instances. If it does, the tab node must charge this time to the container. | `wasmProfile` has 0 CPU (section 4.5). The `cpu` message is next to the `mem` message. |
| AC3 | An answer of CoreDNS over UDP must call `lookupName` only. It must not call `chargeDNS` a second time. | Commit 84da797 (section 4.3). |

### 7.12 Messages of the pod worker

| ID | Requirement | Note |
| --- | --- | --- |
| MS1 | The Go side must examine each message from a pod worker before it does the step of the message. | Decision 14. |
| MS2 | Each socket ID in a message must belong to the container of the pod worker. | |
| MS3 | The IP of a `bind` must obey TC19. | |
| MS4 | Sizes and credits in a message must not be more than the limits of section 8.4. | |
| MS5 | The URL of each `fetch` must obey CR1, CR2 and CR7. | |
| MS6 | If a message fails the examination, the Go side must drop the message and close the socket or the request of the message. | |

## 8 Design

### 8.1 Parts of one tab node

```text
+------------------+         +-----------------------------------------+
| transpile worker |  result | Go thread of the tab node               |
| jco-transpile    |<------->| wasi.js: cache of transpile results     |
| generate()       |         | tabnode_wasi.go: sockets, proxy server  |
+------------------+         | tabnode_udp.go: UDP table               |
                             | tabnode_net.go: Services, flows, policy |
                             +-----------------------------------------+
                                   ^ messages of section 8.3
                                   v
                             +-----------------------------------------+
                             | pod worker (one for each container)     |
                             | output of jco, in JSPI mode             |
                             | WASIShim with Tabernetes sockets, CLI   |
                             | and files; the HTTP path                |
                             +-----------------------------------------+
```

The page and each node worker have these parts. The pod worker opens no
network connections. It only moves bytes and events between the component
and the Go side.

### 8.2 Transpiler

The Go side does these steps for a component layer:

1. Make the key of decision 2.
2. Find the key in the cache of `wasi.js`.
3. If the cache does not contain the key, send the bytes to the transpile
   worker.
4. Compile the core modules of the result with `WebAssembly.compile`.
5. Keep the JavaScript and the compiled core modules in the cache.
6. Send the JavaScript and the core modules to the pod worker.

The transpile worker does these steps:

1. Transpile with `instantiation: {tag: "async"}` and the lists of section
   8.5.
2. Read the names of all imports and exports in the result of `generate`.
3. Make sure that each name obeys the WIT syntax (CO15, CO16).
4. If a name does not obey it, discard the result and send an error.

The mode `instantiation: {tag: "async"}` gives a function that takes all
imports as an object. The generated JavaScript then has no bare module
names, which a `Blob` URL cannot find. Phase 0 (item 3) makes sure of this.

The Go thread starts its transpile worker for the first component. The
transpile worker then loads the transpiler. A Go thread without components
does not load it.

The task `web` of `mise.toml` must bundle the transpile worker in the ESM
format, with `--external:node:*`, and start it as a module worker. The task
must copy the four core modules of `jco-transpile` to `../vendor/`, relative
to the bundle (fact 10). The server must send them with
`Content-Type: application/wasm` for `WebAssembly.compileStreaming`.

The pod worker of a component starts from a `data:` URL (decision 13). It
loads the generated JavaScript from a `Blob` URL that it makes.

### 8.3 Messages

The pod worker and the Go side use the current message channel of the pod
worker (`wasi.js`). Bytes go as a transferable `ArrayBuffer`. This transfer
makes no copy between the threads, but Go copies the bytes into its memory
and out of it. The Go side examines each message (section 7.12).

Socket messages:

| Type | Direction | Contents |
| --- | --- | --- |
| `bind` | worker to Go | socket ID, protocol, family, address, port |
| `bound` | Go to worker | socket ID, local address and port, or an error code |
| `listen` | worker to Go | socket ID, backlog |
| `listening` | Go to worker | socket ID, or an error code |
| `accepted` | Go to worker | listener socket ID, new socket ID, local and remote address and port |
| `connect` | worker to Go | socket ID, protocol, remote address and port |
| `connected` | Go to worker | socket ID, local and remote address and port, or an error code |
| `data` | the two directions | socket ID, bytes (TCP) |
| `datagram` | the two directions | socket ID, remote address and port, bytes (UDP) |
| `credit` | the two directions | socket ID, kind (bytes, datagram bytes or connections), number |
| `error` | Go to worker | socket ID, error code for the next receive or send |
| `shutdown` | worker to Go | socket ID, receive or send or the two |
| `closed` | the two directions | socket ID, error code or none |
| `lookup` | worker to Go | request ID, name |
| `addresses` | Go to worker | request ID, addresses or an error code |

For UDP, `connect` sets the remote address of the socket. `error` gives
`connection-refused` for UD9 and UD11. The pod worker keeps the socket
options (TC22) and sends no message for them.

HTTP messages:

| Type | Direction | Contents |
| --- | --- | --- |
| `request` | Go to worker | request ID, method, URL, headers, body |
| `response` | worker to Go | request ID, status code, headers, body, or the result of the instance |
| `cancel` | Go to worker | request ID |
| `fetch` | worker to Go | fetch ID, method, URL, headers, body |
| `fetched` | Go to worker | fetch ID, status code, headers, body, or an error code |
| `fetch-cancel` | worker to Go | fetch ID |

Other messages:

| Type | Direction | Contents |
| --- | --- | --- |
| `cpu` | worker to Go | busy time of the instances since the last `cpu` message (AC2) |

### 8.4 Credits and queues

A pod worker reads its messages only while the component waits. So each
direction must have a limit:

| Item | Limit | At the limit |
| --- | --- | --- |
| TCP bytes, each direction of a connection | 256 KiB | The sender stops until the receiver sends `credit`. |
| UDP bytes in the queue of a socket in the pod worker | 256 KiB | The Go side drops the datagram (UD6). |
| Accepted connections that the component did not take | the backlog of `listen` | The Go side does not accept more connections. |
| HTTP bodies | 16 MiB | PX18 to PX20. |
| Datagram bytes to a node worker without a `credit` | 1 MiB | The page drops the datagram (ND11). |
| Datagrams on a WebRTC `udp` channel | 1 MiB in `bufferedAmount` | The sender drops the datagram (ND12). |

### 8.5 Calls that wait

The pod worker gives these functions in the list `asyncMode.val.imports`
of jco:

- all imports of fact 8;
- all functions of `wasi:sockets/tcp`, `wasi:sockets/udp` and
  `wasi:sockets/ip-name-lookup`;
- the functions of `wasi:io/streams` that do not block: `read`, `skip`,
  `check-write`, `write`, `flush`, `write-zeroes` and `splice`.

In `wasi:sockets` 0.2, no function waits. All waits go through `poll`,
`pollable.block` and the functions of streams that block. A function in the
list suspends only when it returns a promise. The functions of the second
and the third item return a promise only for CO17. The pod worker counts
the calls in sequence that give no data. After 1,000 such calls, the next
call returns a promise that continues after one message on a
`MessageChannel` (CO19).

The pod worker gives these functions in the list `asyncMode.val.exports`:

- `wasi:cli/run#run`
- `wasi:http/incoming-handler#handle`
- the function that the entrypoint names (CO3)

A call that the pod worker suspended continues in these conditions:

- A message from the Go side changes the state of a pollable.
- The time of a clock pollable ends.
- The pause of CO17 ends.

The pod worker of a component must not use `Atomics.wait`. During
`Atomics.wait`, the worker cannot receive messages. The current module path
with `Atomics.wait` stays only for modules.

A component in a loop of CPU work without calls of section 8.5 does not
suspend. The pod worker then receives no messages, also no `cancel`. The
tab node can only stop the pod worker. Section 11 gives this difference.

### 8.6 Proxy components

The Go side does these steps for a proxy component:

1. Parse the address of PX1 to PX5, PX32 and PX33.
2. Listen on this address with `net/http`.
3. For each request, read the body to a maximum of 16 MiB.
4. Send a `request` message to the pod worker.
5. Wait for the `response` message.
6. Send the response to the client. Do not add a `Content-Type`.
7. If the client closes the connection, send `cancel`.

The pod worker does these steps for each `request` message:

1. Instantiate the core modules again, with a new context (PX11 to PX15).
2. Give the request to the HTTP path of phase 0, item 12.
3. Call `handle` of the component.
4. Send the `response` message to the Go side.
5. Discard the instance (PX25).

JSPI can suspend many instances in one pod worker. One suspended instance
does not stop the others. The compiled core modules are the same for all
instances. Only the memory of an instance is new.

For a `cancel` message, the pod worker does not continue the instance. It
cancels the timers, the client requests and the pollables of the instance.
It then removes all references to the instance (PX24).

For client requests, the HTTP path sends a `fetch` message. The Go side
does the client request with the cluster client and sends `fetched`. The
error code in `fetched` goes to the component without a change (CR9). With
the shim, the HTTP path wraps `outgoingHandler.handle` or
`FutureIncomingResponse.prototype.get` to keep this code.

### 8.7 UDP table

`tabnode_udp.go` (new) keeps the UDP sockets of all pods of one tab node.
It gives the same functions to all code of the tab node:

- `listenPodUDP(ctx, addr)` gives a `net.PacketConn` on the pod IP.
- `dialClusterUDP(ctx, addr)` gives a connected `net.PacketConn`, with the
  Service translation and the flows of section 8.8.

A datagram to an IP on another node goes on the `udp` channel (section
8.9). The UDP table does not use the in-process network of Go.

### 8.8 UDP flows

The flow table is in `tabnode_net.go`, next to the service proxy. A flow
has these items:

- the tuple before the Service translation and the tuple after it
- the endpoint that the service proxy selected
- the result of the NetworkPolicy examination
- the time of the first datagram
- the time of the last datagram in each direction

The service proxy, NetworkPolicy and the node agent use the same timeouts
(FL2 to FL4). The tab node removes flows in these conditions:

- An endpoint is not in the EndpointSlices (FL5).
- A policy changes (FL8, FL9).
- A pod stops (FL13).

A denied datagram makes no flow (FL12).

### 8.9 UDP between nodes

```text
tab pod -> UDP table -> page -> udp channel -> node agent
        -> IP_TRANSPARENT socket (thread with fsuid 65534) -> Docker pod
Docker pod -> tproxy -> node agent -> udp channel -> page
        -> UDP table -> tab pod
```

The page is the center. The page forwards each datagram between two
nodes, as for TCP. Each message has this header:

| Byte | Contents |
| --- | --- |
| 0 | Version: 1 |
| 1 | Type: 1 for a datagram, 2 for a credit. Other values are for a later design. |
| 2 to 5 | Source IPv4 address. For a credit: the number of bytes. |
| 6 and 7 | Source port. For a credit: 0. |
| 8 to 11 | Destination IPv4 address. For a credit: 0. |
| 12 and 13 | Destination port. For a credit: 0. |

The datagram follows the header. A Docker node has no informers, so the
page examines the source (ND7, ND8). Between the page and a node worker,
the channel has no loss. Between the page and a Docker node, loss can occur
(ND3).

A node worker sends a credit after it takes datagrams from the channel
(ND20). The page subtracts the credit from the bytes that it sent (ND11).
Only the channel to a node worker has credits. A WebRTC channel has
`bufferedAmount`.

### 8.10 Files of components

The pod worker gives `WASIShim` the option `browserFilesystem`. Its adapter
implements `BrowserFilesystemAdapter` (`dist/browser/filesystem.d.ts`, line
32) over the copy that `wasmPreopens` makes (section 4.1). The preopens
have the same guest paths as for a module. The adapter obeys the flag
`readOnly` of each preopen (FS2). Writes change only the copy in the pod
worker (FS3).

### 8.11 Loopback table

Each tab node keeps a loopback table. A row has the pod IP, the protocol and
the port of a socket that binds to `127.0.0.1`. The Go side adds a row at
`bind` and removes it at `closed`.

Each path that dials a pod IP examines the loopback table before it
connects:

| Path | Source | Result for a row in the loopback table |
| --- | --- | --- |
| `dialFromPod`, the same pod | the pod IP | Connect (TC3). |
| `dialFromPod`, another pod | another pod IP | `connection-refused` after the NetworkPolicy examination (TC4). |
| `dialCluster` without a pod: kubelet probes, the terminal | the node IP | `connection-refused` (TC15). |
| The Service proxy of `tabnode_net.go` | the node IP | `connection-refused` (TC15). |
| The destination side of the pod channel | another node | The answer `refused` (TC15). |
| `kubectl port-forward` | the pod network namespace | Connect (TC16). |
| The UDP table | another pod IP | Drop, then UD9 (UD21). |

The loopback table does not examine at `Accept`. `policyListener.Accept`
(section 4.3) closes a connection after it exists, so the client sees a
connection and then its end.

### 8.12 Changes to files

| File | Change |
| --- | --- |
| `web/src/transpile.worker.js` (new) | Transpile components with jco (section 8.2). |
| `web/src/wasi.js` | Start components. Keep the results of the transpile worker in the cache. |
| `web/src/wasi.worker.js` | Run components in JSPI mode with `WASIShim`. Instantiate proxy components for each request. Send the messages of section 8.3. |
| `web/src/sockets.js` (new) | The `sockets` object for `WASIShim`, with `ip-name-lookup`. The count of CO17. |
| `web/src/http.js` (new) | The HTTP path of phase 0, item 12, with `fetch` and `fetch-cancel`. |
| `web/src/files.js` (new) | The `browserFilesystem` adapter of section 8.10. |
| `web/src/channel.js`, `web/src/nodes.js` | The `udp` channel to node workers, with credits. |
| `web/package.json` | Add `@bytecodealliance/jco` and `@bytecodealliance/preview2-shim` with a fixed version. |
| `mise.toml` | The task `web` bundles the transpile worker as ESM and copies the core modules of `jco-transpile` to `../vendor/`. Add Rust with the target `wasm32-wasip2`. |
| `site/build.mjs` | Copy the core modules of `jco-transpile` to the site. Send them with `Content-Type: application/wasm`. |
| `k8s/tabernetes/tabnode_wasi.go` | The sockets of each container, the proxy server, client requests and the examination of messages (section 7.12). Close all sockets when a container stops. Charge the CPU time (AC1, AC2). |
| `k8s/tabernetes/tabnode_udp.go` (new) | The UDP table (section 8.7). |
| `k8s/tabernetes/tabnode_loopback.go` (new) | The loopback table (section 8.11). |
| `k8s/tabernetes/tabnode_dial.go` | Add `listenPodUDP`, `dialClusterUDP` and ephemeral ports. Examine the loopback table. Change the cluster client for CR3 to CR5. |
| `k8s/tabernetes/tabnode_net.go` | UDP Services and the flow table. Examine the loopback table. |
| `k8s/tabernetes/tabnode_netpol.go`, `tabnode_netpol_strict.go` | NetworkPolicy for UDP flows and for ICMP. The strict mode for flows. Examine the loopback table in `dialFromPod`. |
| `k8s/tabernetes/tabnode_coredns.go` | Answers over UDP with `lookupName` only (AC3). The TC bit (UD22). |
| `k8s/tabernetes/tabnode_acct.go` | Charge the `cpu` message (AC2). |
| `k8s/tabernetes/coreutils.go`, `coreutils_misc.go` | `nslookup` over UDP, `nc -u`, the ICMP echo of `ping`. |
| `k8s/tabernetes/podlink.go` | The `udp` channel and the examination of the source in the page. The loopback table for the pod channel. |
| `node/agent/podlink.go` | nftables rules for UDP, transparent UDP sockets from `socketAs`, flows. |
| `wasm-images/images.txt`, `wasm-images/build.sh` | Test images in Rust (section 10.1). |
| `docs/docker-node-check/check.sh`, `manifests.yaml` | The steps of section 10.2. An import of the test images. |
| `.github/workflows/ci.yaml` | Rust for the test images. |
| `README.md` | The new behavior and the differences of section 11. Remove "Not covered: UDP" (line 597). |
| `docs/networking-policy.md` | UDP flows, the `udp` channel and ICMP. |

## 9 Phases

Each phase starts only after all tests of the phase before it give correct
results. Deferred tests are not part of this rule (decision 16). Section 10
gives the tests.

| Phase | Contents |
| --- | --- |
| 0 | Proof of concept in `docs/wasi-sockets-poc/`. Section 9.1 gives its items. |
| 1 | Components without network: CO1 to CO19, FS1 to FS4, MS1, MS6, AC2. |
| 2 | Proxy components and client requests: PX1 to PX33, CR1 to CR11, MS5, AC1. |
| 3 | TCP and name lookup: TC1 to TC22, NL1 to NL3, MS2 to MS4. |
| 4 | UDP on a tab node: UD1 to UD22, FL1 to FL13, AC3. |
| 5 | UDP between nodes: ND1 to ND20. All deferred tests. |
| 6 | ICMP echo: IC1 to IC8. |

### 9.1 Items of phase 0

The proof of concept must give a result for each item. Phase 0 writes each
result into this document. If a result changes a decision, phase 0 changes
the decision and its requirements.

1. Transpile a command component and a proxy component in a transpile
   worker. Do this in Chrome, Firefox and Safari.
2. Measure the time and the size for a component of 1 MB. Do the same
   for a component of more than 10 MB.
3. Load the output from a `Blob` URL in a pod worker. Make sure that it has
   no bare module names.
4. Make sure that the page has no Content Security Policy that prevents
   code from a `Blob` URL.
5. Run 16 instances of a proxy component in one pod worker with JSPI. Make
   sure that they continue in a different sequence.
6. Send 10,000 requests in a sequence to a proxy component. Record the
   memory of the browser tab.
7. On a Docker node with runwasi v0.6.1, record these results:
   - the result of each function of `wasi:sockets` in a proxy component
     (PX15)
   - what the client gets after an instance traps (PX21)
   - what the client gets for an error code as the response (PX27)
   - the pod status for a component without the necessary export (CO4)
   - the pod status for an import that the tab node does not give (CO11)
   - the pod status for a command component with an import of `wasi:http`
     (CR6)
   - the name lookup of a Service by its short name (NL1)
   - the addresses `[::]:8080`, port 0 and an IP that the pod cannot bind
     (PX4, PX32, PX33)
8. Run the echo image for TCP, the echo image for UDP and the proxy image on
   the Docker node with the Docker check.
9. Bind TCP and UDP on the same port in a prototype of the UDP table.
10. Send a message of 65,521 bytes on an unordered WebRTC data channel from
    the browser to pion. Send the same message from pion to the browser.
    Record the settings of the channel `udp` that each side sees
    (ND3).
11. Bundle the transpile worker as ESM with the task `web`. Load the four
    core modules from `../vendor/` with `compileStreaming`.
12. Select the HTTP path of decision 4. Make a prototype of each candidate.
    For each candidate, record these items:
    - the results of PX16, PX20, PX28 to PX30 and CR9
    - the time of 1,000 requests
    - the memory after 1,000 requests
    - the size of the code
13. Record the exit codes 0, 1 and 3 of a Rust `wasm32-wasip2` command
    component on runwasi and through the shim. Record if
    `exit-with-code` works on the two (CO18).
14. Examine if the published binary of runwasi v0.6.1 has `panic = "abort"`.
    For a proxy component that sets no response, record what the client
    gets, the container state and the restart count (PX26).
15. Cancel 1,000 instances that JSPI suspended. Record the memory before and
    after garbage collection (PX24).
16. Run the pod worker in a worker with an opaque origin in Firefox and
    Safari. Send a `WebAssembly.Module` to it with `postMessage`
    (decision 13).
17. On a Docker node, send a client request that NetworkPolicy denies.
    Record the error code and the time until it fails (CR8). This item is a
    deferred test.
18. Get the Rust test images onto a Docker node without internet. Select
    access to ghcr.io or an import step in the Docker check, as `IMPORT=1`
    does for busybox (section 10.2).

### 9.2 Results of phase 0

`docs/wasi-sockets-poc/run.sh` gives these results. The command `run.sh
browser` runs the browser items in headless Chrome 153. The command `run.sh
runwasi` runs the test components of `docs/wasi-sockets-poc/components/`
with the published `containerd-shim-wasmtime` v0.6.1 and containerd 2.4.2.
It runs without kubelet. So it records the exit status of the task, not the
pod status and not the restart count. The Docker check records these
(item 8).

| Item | Result | Effect on the design |
| --- | --- | --- |
| 7, PX15 | `create-tcp-socket` and `create-udp-socket` give a socket. `start-bind` and `start-connect` give `access-denied`. `resolve-addresses` gives `permanent-resolver-failure`. | PX15 uses these results. |
| 7, PX21 | After an instance traps, the client gets no response. The connection closes. The container continues to run. | PX21. |
| 7, PX27 | After an error code as the response, the client gets no response. The connection closes. | PX27. |
| 7, PX16, PX28 | `Cookie`, `User-Agent` and `Origin` arrive at the component. Two `Set-Cookie` headers arrive at the client. | PX16 and PX28 stay. |
| 7, PX30, PX31 | hyper sends `date`. For a body without `Content-Length`, it sends `transfer-encoding: chunked`. A response with 204 has only `date`. The status code 599 arrives. | The tab node sends the same header names (PX31). |
| 7, PX11 | The environment contains the variables of the container spec, for example `PATH` and `HOSTNAME`, and `REQUEST_ID`. | PX11 stays. |
| 7, PX3, PX32 | A value that does not parse gives `0.0.0.0:8080`. Port 0 gives an ephemeral port of the kernel. | PX3 and PX32 stay. |
| 7, PX33 | The IP 192.0.2.7 gives "Address not available (os error 99)". The task exits with status 137. | The tab node gives the same status. |
| 7, PX4 | Not examined. The kernel of the test host has no IPv6. | The Docker check examines `[::]:8080`. |
| 7, CO11, CR6 | A component with an import that the linker does not have does not start. The log names the interface, for example `wasi:http/types@0.2.12`. The task exits with status 137. A command component with `wasi:http` gets this result. | CO11, CR6. |
| 7, CO4, NL1 | Not examined yet. | |
| 13 | `std::process::exit` of Rust gives 0 for 0, 1 for 1 and 1 for 3. Rust 1.97 imports only `exit`. A component with an import of `exit-with-code` does not link (status 137). | CO18: exit codes 0 and 1 only. |
| 14 | If `handle` sets no response, the client gets no response. The task then exits with status 139. The published binary does not continue after the panic. | PX26. The Docker check records the restart count. |
| 1 | In Chrome 153, a module worker transpiles the command component and the proxy component. Firefox and Safari: not examined. The test host has only Chromium. | |
| 2 | The first transpiler run takes 393 to 427 ms, with the load of the transpiler from a local server. A component of 3.4 MB takes 79 to 83 ms. A component of 4.4 MB takes 135 to 165 ms. A component of 12.0 MB from componentize-js takes 593 to 614 ms. | Open question 1: the time in Chrome is far below 10 seconds. |
| 2 | The generated JavaScript of the proxy component has 785,828 bytes. It contains the runtime of jco for JSPI. | The cache of decision 2 keeps it one time for each key. |
| 3 | The pod worker loads the generated JavaScript from a `Blob` URL. It runs the command component with `WASIShim`. The JavaScript has no bare module names. | Section 8.2 stays. |
| 4 | `site/page.html` and `site/terminal.html` have no Content Security Policy. The service worker `web/proxy-sw.js` has only the scope `tabernetes-proxy/`. The headers of the published site: not examined. | |
| 11 | esbuild bundles the transpile worker as ESM only with `--external:node:*`. jco-transpile has dynamic imports of `node:fs/promises` for Node.js. The bundle has 652,619 bytes. It loads the four core modules from `../vendor/` with `Content-Type: application/wasm`. | Section 8.2 gets the flag. |
| 13 | In the shim, `exit(3)` of Rust gives 1. A component that calls `exit-with-code(3)` gives 3. | CO18: the pod worker must not give `exit-with-code`. |

## 10 Tests

### 10.1 Test images

The mirror gets these images, in Rust:

- a command component that writes its arguments, its environment and a
  file of a volume
- an echo server for TCP, and an echo server for UDP
- a proxy component that shows its context
- a proxy component that sends client requests

The images are on ghcr.io and in the mirror of the page. A Docker node
without a proxy cannot pull them (section 4.4). Phase 0, item 18 selects
the method for the Docker check.

### 10.2 Tests on a Docker node

The Docker check runs the tests of the column "Docker". Each phase adds
steps to `check.sh` and manifests to `manifests.yaml`:

| Phase | Step | Contents |
| --- | --- | --- |
| 1 | `wasi-command` | The command components and the files. |
| 2 | `wasi-proxy` | The proxy components and the client requests. |
| 3 | `wasi-sockets` | TCP and name lookup. |
| 4, 5 | `wasi-udp` | UDP in a pod of the Docker node, and UDP between nodes. |

A step that needs NetworkPolicy writes SKIP until the nfqueue item of
section 4.4 closes. The Docker check records the result of each step in
`$OUT/<step>.txt`.

### 10.3 Tests for each requirement

The column "Nodes" gives the nodes for the test:

- "tab" is the tab node of the page.
- "worker" is a node worker.
- "Docker" is a Docker node with runwasi, through the Docker check.
- "Docker (deferred)" is a deferred test.

| Requirements | Test | Nodes |
| --- | --- | --- |
| CO1, CO5, FS1 | The command component writes its arguments, its environment and a file of a volume to stdout. It reads stdin. | tab, worker, Docker |
| CO18 | The command component exits with 0, 1 and 3. The pod shows the exit codes 0, 1 and 1. A component with `exit-with-code` gets the status of CO11. | tab, Docker |
| CO2 | A component with the two exports runs in the world of its first export. | tab, Docker |
| CO3, CO4 | A component without the two exports runs the function of the entrypoint. A second component without this function gets the status of phase 0. | tab, Docker |
| CO6, CO7 | A component that uses more memory than `limits.memory` gets `OOMKilled`. During one request, the counted memory increases by the memory of the instance. After the response, it decreases by the same number. | tab |
| CO8 | Two pods of the same image cause one transpiler run. A second entrypoint causes a second run. | tab, worker |
| CO9 | During the transpiler run of a large component on the page, the terminal shows each typed character immediately. On a node worker, a readiness probe of a pod of that node continues to give correct results. | tab, worker |
| CO10 | The test removes `WebAssembly.Suspending`. A component pod then gets `CreateContainerError` with the cause. | tab |
| CO11, CO12 | A component with a WASI 0.3 import gets the status of phase 0 with the name of the import. | tab, Docker |
| CO13 | All current tests of `tabnode_wasi_test.go` give correct results. | tab |
| CO14 | A unit test makes sure that the page and the Go thread load no generated JavaScript. | tab |
| CO15, CO16 | A component with a name that contains a quotation mark gets `CreateContainerError`. | tab |
| CO17, CO19 | A component polls a socket without a wait. A datagram to it arrives in less than 1 second. | tab |
| FS2, FS3 | The command component writes to a read-only volume and gets an error. It writes to another volume, and a restart shows the old file. | tab, Docker |
| FS4 | A volume of 65 MiB gives the same text for a component and for a module. | tab |
| PX1, PX4, PX16 | `kubectl expose` and `wget -qO- <service>` get the response. A readiness probe with `httpGet` gives a correct result. | tab, worker, Docker |
| PX2, PX3 | A pod without the variable and a pod with an incorrect value listen on port 8080. | tab, Docker |
| PX4, PX32, PX33 | Pods with `[::]:8080`, with port 0 and with a foreign IP get the results of phase 0. | tab, Docker |
| PX5 | With `127.0.0.1:9000`, another pod gets `connection-refused`. A second container of the same pod gets the response. A kubelet `httpGet` probe on port 9000 fails. | tab, Docker |
| PX6, PX11, PX13 | The context component writes its environment, its arguments and its preopened directories into the response. | tab, Docker |
| PX7 | The component increases a global counter and writes it into the response. Each response shows 1. | tab, Docker |
| PX8, PX9, PX10 | 20 requests at the same time go to a component that waits 1 second. On the tab node, all responses arrive in approximately 2 seconds. On a Docker node, they arrive in approximately 1 second. | tab, Docker |
| PX12 | 3 requests in a sequence get the `REQUEST_ID` values 0, 1 and 2. | tab, Docker |
| PX14 | The component writes to stdout and to stderr. `kubectl logs` shows no text from the instance. | tab, Docker |
| PX15 | The component calls each function of `wasi:sockets` and writes the results into the response. The results agree with phase 0. | tab, Docker |
| PX16, PX28 | A request with `Cookie`, `User-Agent` and `Origin` arrives with these headers. A response with two `Set-Cookie` headers arrives with them. | tab, Docker |
| PX17 | A response without `Content-Type` arrives without `Content-Type`. | tab, Docker |
| PX18, PX19, PX20 | A request body of 17 MiB gets the status code 413. A response body of 17 MiB gets the status code 500. A body of 1 MiB arrives without a change. | tab |
| PX21, PX22 | A request that causes a trap gives the result of phase 0. The next request gets a response. | tab, Docker |
| PX23, PX24, PX25 | A request that waits for 10 minutes stays open. A client that closes the connection sets the number of instances to 0. A client request of the instance then closes on the Go side. | tab |
| PX26 | A component that sets no response gives the client result, the container state and the restart count of phase 0. | tab, Docker |
| PX27 | A component that sets an error code gives the result of phase 0. | tab, Docker |
| PX29 | Requests with the method `PURGE`, with `TRACE` and a GET with a body arrive at the component without a change. | tab, Docker |
| PX30 | Responses with the status codes 204, 304 and 599 arrive with the same status code as on a Docker node. | tab, Docker |
| PX31 | A test compares the header names of the same response from a tab node and from a Docker node, for HTTP/1.1 and HTTP/1.0. Only the values of `Date` are different. | tab, Docker |
| CR1 | The component gets a response from another Service by its name. | tab, worker, Docker |
| CR1, CR8 | A NetworkPolicy that denies the client request gives the error code and the time of phase 0. | tab, Docker (deferred) |
| CR2, CR7 | A client request to `example.com` fails with `DNS-error`. A client request to `192.0.2.1` fails with `connection-refused`. | tab |
| CR3, CR4, CR5 | A test Service sends a redirect and a compressed body, and writes the headers that it receives. The component gets the redirect, the compressed body and no added headers other than `Host`. | tab, Docker |
| CR6 | A command component with an import of `wasi:http` gets the status of phase 0. | tab, Docker |
| CR9 | A unit test of the pod worker gives each error code in `fetched`. The component gets the same code. | tab |
| CR10 | The component discards a client request to a Service that does not answer. The Go side closes the connection. | tab |
| CR11 | A unit test with a short timer makes sure that the Go side gives `connection-timeout`. | tab |
| TC1, TC8 | The TCP echo pod gives answers through a Service. | tab, worker, Docker |
| TC2, TC3, TC4, TC5, TC17 | A server on `127.0.0.1` gives `127.0.0.1` as `local-address`. A client in the same pod connects and sees `127.0.0.1` at the two ends. A client in another pod gets `connection-refused` from `connect`. | tab, Docker |
| TC15, TC16 | A kubelet `tcpSocket` probe of a server on `127.0.0.1` fails. `kubectl port-forward` to this server connects. | tab, Docker |
| TC6, TC7 | A bind to `::` gives no error. A connection to the pod IP gets `connection-refused`. | tab, Docker |
| TC9 | A client that connects before it binds has a local port from 32768 to 60999. | tab, Docker |
| TC10 | A denied client gets `timeout` after approximately 127 seconds. | tab, Docker (deferred) |
| TC11 | A client to a closed port gets `connection-refused`. | tab, Docker |
| TC12 | A unit test sends 10 MB to a server that does not read. The buffer of the Go side for this connection stays at 256 KiB or less. | tab |
| TC13 | The client closes its send direction. The server reads the end of the stream and sends its answer. | tab, Docker |
| TC14 | After `kubectl delete pod` of the server, the client gets the end of its connection. | tab |
| TC18, TC19, TC20, TC21 | The component does a second bind, a bind to a foreign IP, an IPv6 connect to an IPv4 address and a backlog of 0. Each call gives the error code of fact 12. | tab, Docker |
| TC22 | The component sets and reads each socket option. The values agree. | tab, Docker |
| NL1, NL2 | A component finds a Service by its short name. | tab, worker, Docker |
| NL3 | The name `example.com` fails with `name-unresolvable`. | tab |
| UD1, UD16, UD17, UD18 | The Service `kube-dns` has 53/UDP and 53/TCP. `nslookup` over UDP and over TCP get answers from CoreDNS on the tab node. | tab |
| UD22 | A query over UDP for a name with 40 addresses gets an answer with the TC bit. | tab |
| AC3 | A unit test sends one query over UDP. `chargeDNS` charges one time. | tab |
| UD2, UD7, UD15 | The UDP echo pod gives answers through a Service. The answers have the correct source address. | tab, worker, Docker |
| UD3, UD4 | Two pods bind to port 0 at the same time. Each pod gets port 32768. | tab |
| UD5, UD6 | A client sends 1,000 datagrams of 1,024 bytes to a server that does not read. The sender gets no error. The server then reads 256 datagrams. | tab |
| UD8, UD9 | A connected client sends to a port without a socket. The next receive gives `connection-refused`. | tab, Docker |
| UD10, UD11 | A connected client sends to a Service without endpoints. The next receive gives `connection-refused`. | tab, Docker |
| UD12 | A client sends to a ClusterIP port without a Service. The client gets no error and no answer. | tab, Docker |
| UD13 | The UDP echo pod sends to its own Service and gets its answer. | tab, Docker |
| UD14 | A datagram of 65,508 bytes fails with `datagram-too-large`. | tab, Docker |
| UD19 | `nc -u` sends a line to the UDP echo pod and receives it. | tab |
| UD20, UD21 | A UDP server on `127.0.0.1` receives from its own pod. A connected client in another pod gets `connection-refused`. | tab, Docker |
| FL1 | A unit test sends from one client to two ClusterIPs with the same endpoint. The flow table has two flows. | tab |
| FL6, FL7 | A unit test counts the calls of the NetworkPolicy engine. 10 datagrams and 10 answers of one flow cause one call. | tab |
| FL2 | A flow without answers is not in the flow table after 31 seconds. | tab |
| FL3 | A flow with answers for 5 seconds is in the flow table after 100 seconds without datagrams. | tab |
| FL4 | One query and one answer: the flow is not in the flow table after 31 seconds. | tab |
| FL5 | A client sends one datagram each second to a UDP Deployment with 2 replicas. The test deletes the pod that answered. The client then gets answers from the other endpoint. | tab |
| FL8, FL9 | A new policy denies an open flow. The next datagram does not arrive. | tab |
| FL10 | With `sessionAffinity: ClientIP`, 10 sockets with different source ports in one client pod get answers from one endpoint. | tab |
| FL11 | With `internalTrafficPolicy: Local`, datagrams from a tab pod go only to endpoints on the same tab node. | tab, worker |
| FL12 | A policy denies a datagram. A new policy then allows it. The next datagram arrives in less than 5 seconds. | tab |
| FL13 | A unit test removes a pod. The flow table has no flow of its pod IP. | tab |
| ND1 | The tests with "worker" or "Docker" in this table. | worker, Docker |
| ND2 | UDP between pods on the page node and on a node worker. | tab, worker |
| ND3 | A unit test of the page and a unit test of the agent read the settings of the channel `udp`. | tab, Docker |
| ND3, ND4, ND5 | A tab pod sends a datagram of 65,507 bytes to a Docker pod. The Docker pod gets it. | tab, Docker |
| ND6, ND9, ND10 | A denied tab pod and a denied Docker pod get no answers. | tab, Docker (deferred) |
| ND7, ND8 | A unit test of the page sends a datagram with a false source IP on the channel of a node. The page drops it. | tab |
| ND11, ND20 | A unit test sends 2 MiB of datagrams to a node worker that sends no credit. The page drops the datagrams after 1 MiB. After a credit, datagrams go again. | tab, worker |
| ND12 | A unit test of the page and a unit test of the agent send to a WebRTC channel with a `bufferedAmount` of 2 MiB. Each drops the datagram. | tab, Docker |
| ND13, ND14, ND15 | A Docker pod sends to a tab pod and gets an answer from the tab pod IP. | Docker |
| ND16, ND18 | A tab pod sends to a Docker pod and gets an answer. | tab, Docker |
| ND17 | A policy on the Docker node denies a second tab pod. | tab, Docker (deferred) |
| ND19 | A unit test of the agent makes sure that a flow without answers closes after 30 seconds. | Docker |
| IC1, IC4, IC5 | `ping` to a pod IP gets echo replies. A policy without ICMP for the pod prevents them. | tab |
| IC2 | `ping` to the node IP gets echo replies. | tab |
| IC3 | `ping` to a pod on a Docker node shows that ICMP between nodes is not available. | tab, Docker |
| IC6 | `ping` to a ClusterIP gets no echo reply. | tab |
| IC7, IC8 | `README.md` contains the two texts. | none |
| MS1 to MS6 | A unit test sends messages with a foreign socket ID, a foreign bind IP, a size above the limit or a URL outside the cluster. The Go side drops each message and closes the socket or the request. | tab |
| AC1 | A unit test sends 10 MB through a UDP Service and a TCP Service. The CPU of the container increases. The CPU of the control plane pods does not increase. | tab |
| AC2 | If the pod worker sends `cpu`, a component in a loop of CPU work shows CPU in `kubectl top pod`. | tab |

## 11 Differences from a Docker node

These differences stay after this design. `README.md` must show them.

| Item | Tab node | Docker node with runwasi |
| --- | --- | --- |
| Instances of a proxy component at the same time | A maximum of 16 | No maximum |
| Instances of a proxy component on threads | One thread. An instance in a loop of CPU work stops the other instances and `cancel`. | Parallel tasks of tokio |
| A component in a loop of CPU work | It receives no messages. Only a stop of the container ends it. | The kernel stops it at the end of its time |
| Bodies of HTTP requests and responses | Complete blocks, a maximum of 16 MiB | Streams, no maximum |
| An instance after the client closes the connection | The pod worker discards it | It runs until `handle` returns |
| Client requests and name lookup to the internet | They fail | They work |
| UDP in the tab node | No loss before a queue has no space | Loss is possible |
| `connection-refused` for UDP between nodes | Not available | Available |
| Socket options | They keep their values and have no effect | The kernel uses them |
| ICMP | Only on one tab node, each echo request examined | Between all nodes, only the first echo request examined |
| NodePort and hostPort for UDP with an endpoint on `tab` | Out of scope | The host network namespace does not reach tab pods (issue #61) |

If phase 0 (item 12) selects an HTTP path with differences, phase 0 adds
them to this table.

## 12 Risks

| Risk | Effect | Action |
| --- | --- | --- |
| The transpiler is large (fact 10). | The first component pod of each Go thread loads approximately 2.5 MB more. | The transpile worker loads it only for the first component. The browser keeps the files in its HTTP cache for the other Go threads. |
| The transpiler run of a large component is slow. | The pod starts late. | The transpile worker keeps the Go thread free (CO9). Phase 0 measures the time. |
| The generated JavaScript contains names from the component. | A bad name can put code into the pod worker. With the origin of the page, the pod worker can use IndexedDB, Cache Storage and same-origin requests. | CO14 to CO16. The opaque origin of decision 13. The examination of messages (section 7.12). |
| A Content Security Policy can prevent code from a `Blob` URL. | Components cannot run. | The page sets no Content Security Policy now. Phase 0, item 4. |
| esbuild in the IIFE format makes `import.meta.url` empty. | The transpile worker cannot load the transpiler. | Section 8.2: ESM and a copy of the core modules to `../vendor/`. Phase 0, item 11. |
| `WASIShim` has the mark "(EXPERIMENTAL)" (fact 9). | A new version of the shim can change its options. | `web/package.json` fixes the version. Phase 1 tests the options. |
| The `poll` of the shim continues a ready pollable after `setTimeout(0)` (fact 9, fact 21). | A component that polls ready sockets does approximately 250 rounds each second. | Phase 1 measures it with the CO17 test. A later change can give the pod worker its own `poll`. |
| Go cannot compile components (fact 7). | The current test images in Go cannot use sockets. | The new test images are in Rust. `mise.toml` and CI get Rust. |
| Each request makes a new instance with a new memory. | The browser frees the memory only at garbage collection. Many requests can use too much memory. | PX9, CO7. Phase 0, items 6 and 15. |
| The Go thread moves the data of all sockets of pods. | Much network traffic of pods makes the control plane slower. This is the same reason as for the rejection of a TCP/IP stack in section 13, but only for traffic of pods. | The credits of section 8.4 set a limit to the memory. AC1 shows the time in the container. A later design can move bytes between workers with shared memory. |
| The tests of NetworkPolicy on a Docker node must have nfqueue in a rootless node. | Deferred tests cannot run. | The item is not complete in `docs/networking-policy.md`. Phase 5 cannot end before it (decision 16). |
| A Docker node without a proxy has no internet (section 4.4). | The Docker check cannot pull the Rust test images. | Phase 0, item 18. |
| The host network namespace of a Docker node does not reach tab pods (issue #61). | NodePort and hostPort for UDP cannot work with an endpoint on `tab`. | Out of scope. Section 11. |
| WASI 0.3 is near. jco-transpile already uses `preview3-shim`. | New components do not run on a tab node. | CO11 and CO12 give a clear text. A later design can add WASI 0.3. |
| The shim and wasmtime 36.0.10 have different versions of the `wasi:*@0.2.x` interfaces. | A component can link on one node and fail on the other. | CO11 gives the status of runwasi and the name of the import. |
| Old browsers have no JSPI (fact 14). | These browsers cannot run components. | CO10. Modules continue to run. |

## 13 Alternatives

| Alternative | Result | Reason |
| --- | --- | --- |
| Listen sockets for modules (WASI preview 1, `sock_accept`). | Rejected | runwasi gives no sockets to modules (fact 2). The same manifest then does not have the same results on a Docker node. |
| The socket extension of WasmEdge. | Rejected | The Docker node runs the wasmtime shim. A second shim makes the node image larger. Few toolchains use this extension. |
| WASIX with `@wasmer/sdk` (fact 18). | Rejected | It must have cross-origin isolation. Its network does not connect to the in-process network. runwasi does not run WASIX with the wasmtime shim. |
| A TCP/IP stack in user space (for example the netstack of gVisor) for IP packets. | Rejected | With this stack, the Go thread moves the data of all connections of a tab node through it. This also includes the connections of the control plane. A Docker node already has a Linux kernel. |
| ICMP sockets in WebAssembly pods. | Rejected | WASI has no ICMP. Images from registries for WebAssembly pods do not have `ping`. |
| UDP through the in-process network of Go. | Rejected | TCP and UDP cannot use the same port (fact 11). No policy examines the datagrams. |
| A change to the `net` package of Go. | Rejected | The project then must keep a patch of the standard library for each Go version. |
| One instance of a proxy component for all requests. | Rejected | runwasi makes a new instance for each request (fact 4). A component with state in global variables then has different results. |
| A transpiler on the Go thread. | Rejected | It stops the tab node for the complete time of the transpiler run. |
| One transpile worker in the page for all tab nodes. | Rejected | Node workers then must have a new channel to the page. The browser HTTP cache already prevents a second download. |
| Browser `Request` and `Response` objects for the shim. | Rejected | They remove headers, methods and status codes (fact 19). |
| A `wasi:http` of Tabernetes, with bodies in parts. | Phase 0 | Phase 0, item 12 compares it with objects for the shim. Bodies in parts are a later design. |
| A parser of components before `generate`. | Rejected | It is 2.4 MB more (fact 10). CO15 examines the result of `generate`. |
| An examination of `127.0.0.1` sockets at `Accept`. | Rejected | The client sees a connection and then its end, not `connection-refused` (section 8.11). |
| Components that the mirror transpiles before publication. | Later | Pods can use images from other registries, so the transpile worker is necessary in all conditions. |

## 14 Open questions

1. How much time does the transpiler use for a component of more than
   10 MB?
   Phase 0 measures the time. The target is less than 10 seconds in Chrome
   on a laptop.
2. Is 16 the correct maximum of instances for PX9? Phase 0 (items 6 and 15)
   and phase 2 measure the memory for each instance.
3. After phase 5, Docker pods can get UDP answers from CoreDNS on a tab
   node. Can CoreDNS then stay on the tab node? A later design must decide.

## 15 Sources

The author and the reviewers read these sources on 2026-11-13.

- runwasi v0.6.1, `crates/containerd-shim-wasmtime/src/instance.rs` and
  `http_proxy.rs`, and `Cargo.toml`:
  <https://github.com/containerd/runwasi/tree/containerd-shim-wasmtime/v0.6.1>
- wasmtime `main`, `src/common.rs` and `crates/cli-flags/src/lib.rs`, and
  wasmtime 36.0.10, `crates/wasi/src/preview1.rs`,
  `crates/wasi/src/sockets/mod.rs` and `crates/wasi-http/src/types.rs`:
  <https://github.com/bytecodealliance/wasmtime>
- Go `src/internal/syslist/syslist.go` and `src/net/net_fake.go`:
  <https://github.com/golang/go/tree/go1.27.3/src>
- TinyGo `targets/wasip2.json`:
  <https://github.com/tinygo-org/tinygo/blob/release/targets/wasip2.json>
- jco 1.42.0, jco-transpile 0.24.0 and preview2-shim 0.31.1 on npm:
  <https://www.npmjs.com/package/@bytecodealliance/jco>
- `wasi:sockets` `wit/tcp.wit`:
  <https://github.com/WebAssembly/wasi-sockets/blob/main/wit/tcp.wit>
- pion/sctp `association.go`:
  <https://github.com/pion/sctp/blob/master/association.go>
- `web-features` 3.46.0, feature `wasm-jspi`:
  <https://web-platform-dx.github.io/web-features-explorer/features/wasm-jspi>
- kube-network-policies v1.1.3, `pkg/network/packet.go` and
  `pkg/dataplane/controller.go`:
  <https://github.com/kubernetes-sigs/kube-network-policies/tree/v1.1.3>
- Linux `Documentation/networking/nf_conntrack-sysctl.rst`,
  `Documentation/networking/ip-sysctl.rst` and
  `net/netfilter/nf_conntrack_proto_udp.c` in the
  [repository of Linux](https://github.com/torvalds/linux)
- [`@wasmer/sdk` 0.19.1 on npm](https://www.npmjs.com/package/@wasmer/sdk)
- Tests in a worker of Chrome 153 for facts 19, 21 and 22, by the third
  reviewer.
