# Sockets for WebAssembly pods

Status: draft design, revision 3. No part of this design is in the code.
Revision 2 and revision 3 contain the findings of two independent reviews.

## Language

This document obeys the rules of ASD-STE100 Simplified Technical English.
These words have special status:

- Technical names: the names of code, files, Kubernetes objects, protocols,
  standards, programs and the terms of section 3.
- Technical verbs: accept, bind, cache, compile, connect, deny, dial, drop,
  export, fail, forward, import, instantiate, listen, load, parse, publish,
  run, serve, suspend, throw, transpile and wrap. Each verb has the same
  definition as in computer networks or in WebAssembly. This document uses
  these words only as verbs.
- "must" and "must not" give a requirement. "can" gives a possibility, not a
  requirement.

Paths are relative to `tabernetes/`. The path `.github/` is at the root of
the repository.

## 1 Purpose

WebAssembly pods on a tab node cannot open sockets. A server in such a pod
cannot receive connections. A client in such a pod cannot connect to a
Service. This document gives a design that adds TCP sockets, UDP sockets,
name lookup and HTTP to these pods.

The same pod manifest must give the same results on a tab node and on a
Docker node. This rule controls most decisions in this document. Section 11
lists the differences that stay.

## 2 Scope

These items are in the scope of this document:

- WASI preview 2 components on tab nodes, in the command world and in the
  proxy world.
- TCP and UDP sockets of `wasi:sockets`, and `wasi:sockets/ip-name-lookup`.
- Client requests of proxy components.
- UDP for all pods of a tab node: a UDP table, Services, flows,
  NetworkPolicy and cluster DNS.
- UDP between tab nodes and Docker nodes.
- An ICMP echo reply for the `ping` built-in command, on one tab node.

These items are not in the scope of this document:

- Sockets for WASI preview 1 modules. Section 13 gives the reason.
- WASI preview 3 components.
- Components that import `tabernetes_webgpu` or `wasi:webgpu`.
- Components that the mirror transpiles before publication.
- Threads, `fork` and `exec` in WebAssembly pods.
- IPv6 in the cluster.
- Raw sockets, IP packets, `tcpdump` and `traceroute` on tab nodes.
- Connections and client requests from a pod to the internet.
- ICMP between nodes.
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
| Go thread | The thread that runs the Wasm binary of one tab node. It also runs `web/src/wasi.js` of that tab node. Each tab node has its own Go thread. |
| Go side | The code of a tab node on its Go thread that starts pod workers and has the sockets of their pods. |
| pod worker | The Web Worker that runs one container of a WebAssembly pod (`web/src/wasi.worker.js`). |
| transpile worker | A new Web Worker that transpiles components with jco. Each Go thread has one transpile worker. |
| module | A WASI preview 1 core module (`wasm32-wasip1`, `GOOS=wasip1`). |
| component | A WASI preview 2 component (`wasm32-wasip2`). |
| world | The set of exports that shows how a runtime runs a component. |
| command component | A component in the command world: it exports `wasi:cli/run`. |
| proxy component | A component in the proxy world: it exports `wasi:http/incoming-handler`. |
| instance | One instantiation of the core modules of a component, with its own memory. |
| client request | An HTTP request that a component sends with `wasi:http/outgoing-handler`. |
| mirror | The OCI mirror of WebAssembly images that the page reads (`wasm-images/`). |
| in-process network | The network of Go for `js/wasm` (`net_fake.go`). `net.Listen` and `net.Dial` connect goroutines of the same Go thread. |
| UDP table | A new table of UDP sockets in each tab node. It is separate from the in-process network. |
| Service translation | The change of a ClusterIP and port to the IP and port of an endpoint, as kube-proxy does it with DNAT. |
| flow | The state of one UDP conversation, with a timeout. |
| strict mode | The part of the tab node that examines open connections again after a NetworkPolicy changes (`tabnode_netpol_strict.go`). |
| source node, destination node | The node that sends a datagram, and the node that receives it. |
| message | A message between two threads (`postMessage`), or a message on a channel between nodes. |
| credit | A number of bytes or items that a receiver can take. The sender stops when its credit is 0. |

## 4 Current condition

### 4.1 Pod workers

- A pod worker runs modules with `@bjorn3/browser_wasi_shim`. It has no
  socket functions. The section "Limits" of `README.md` (line 764) gives
  this limit.
- For a component, `web/src/wasi.js` (lines 48 to 50) gives an
  "unsupported" message.
- A pod worker uses JSPI. `fd_read` on stdin and `poll_oneoff` can suspend
  until a message comes. The page does not use cross-origin isolation.
- Without a subscription to stdin, `poll_oneoff` waits with `Atomics.wait`
  if shared memory is available (`wasi.worker.js`, lines 13 to 17 and 295
  to 296). During this wait, the worker cannot receive messages.
- A pod worker sends `mem` messages (`wasi.worker.js`, lines 191 to 198).
  The tab node uses them for `limits.memory`.
- A pod worker gives the import module `tabernetes_webgpu` to modules
  (`wasi.worker.js`, lines 210 to 214).

### 4.2 The Go thread and the build

- `wasi.js` keeps compiled modules in a cache. The key is the layer digests
  and the path (`wasi.js`, line 19).
- Each node worker installs its own `wasi.js` (`web/src/node.worker.js`,
  lines 10 and 37). So each tab node has its own cache and its own pod
  workers.
- The task `web` of `mise.toml` bundles each `src/*.worker.js` with esbuild
  in the IIFE format (lines 110 to 113). In this format, `import.meta.url`
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
- A connection that NetworkPolicy denies waits for `synTimeout` (127
  seconds, `tabnode_netpol.go`, line 64) and then fails.
- The strict mode closes open connections that a new policy denies
  (`tabnode_netpol_strict.go`, lines 62 to 79).

### 4.4 Other nodes

- Node workers connect to the page through MessagePorts
  (`web/src/channel.js`, `web/src/nodes.js`). Docker nodes connect to the
  page through WebRTC.
- The pod channel between nodes has one ordered channel for each TCP
  connection (`k8s/tabernetes/podlink.go`, lines 7 to 21). The label of the
  channel gives the source node. The first line is `TABP/1 tcp <src> <dst>`.
  The destination node sends `ok` or `refused`.
- The page examines the header of each pod channel that it forwards. A
  Docker node has no informers and trusts the page
  (`docs/networking-policy.md`, lines 532 to 534).
- On a Docker node, `node/agent/podlink.go` catches TCP to tab pods with
  TPROXY. Its nftables rules are for TCP only (lines 100 and 109).
- The rules of kube-network-policies accept every packet of uid 0 on a
  Docker node. So the agent dials through a helper process with a uid that
  is not 0 (`docs/networking-policy.md`, lines 248 to 258).
- runwasi (`containerd-shim-wasmtime` v0.6.1, `node/Dockerfile` line 24)
  runs WebAssembly pods on Docker nodes.
- The NetworkPolicy tests on a Docker node must have nfqueue in a rootless
  node.
  `docs/networking-policy.md` (lines 3 to 5) gives this item as open.

## 5 Facts that control the design

The author and two reviewers examined these facts in the sources on
2026-11-13. Section 15 gives the sources.

1. runwasi v0.6.1 gives a command component the full network of the pod.
   `wasi_builder` (`instance.rs`, lines 415 to 434) calls
   `inherit_network()`, `allow_tcp(true)`, `allow_udp(true)` and
   `allow_ip_name_lookup(true)`.
2. runwasi gives a module no sockets. It runs a module with `build_p1()`
   (`instance.rs`, line 220). In wasmtime 36.0.10, the version of runwasi,
   `sock_accept`, `sock_recv`, `sock_send` and `sock_shutdown` always return
   `Notsock` (`crates/wasi/src/preview1.rs`, lines 2647 to 2693).
3. The CLI of wasmtime gives listen sockets to modules only with its legacy
   implementation of preview 1 (`-S preview2=n`,
   `crates/cli-flags/src/lib.rs`, lines 592 to 600). For components, it
   stops with "components do not support --tcplisten" (`src/common.rs`,
   line 364). runwasi does not use the CLI, so fact 2 is the important
   fact.
4. runwasi serves a proxy component itself (`http_proxy.rs`):
   - It parses `WASMTIME_HTTP_PROXY_SOCKET_ADDR` and binds the full address,
     with its IP and its port (lines 75 to 98).
   - If the environment does not contain this variable, or if the value does
     not parse, the address is `0.0.0.0:8080`. No error occurs.
   - runwasi removes `WASMTIME_HTTP_PROXY_SOCKET_ADDR` and
     `WASMTIME_HTTP_BACKLOG` from the environment of the component.
   - Each request gets a new store and a new instance. Requests run at the
     same time, each in a task of its own (lines 165 to 239).
   - The context of a request contains the environment and `REQUEST_ID`, a
     counter that starts at 0. It has no arguments, no preopened
     directories, no stdio and no network.
   - If the instance traps or sets no response, runwasi gives an error to
     hyper (lines 214 to 233). The container continues to run.
   - runwasi has no timeout for a request.
   - The linker of a proxy component contains `wasi:http`
     (`instance.rs`, lines 263 to 265).
5. runwasi finds the world of a component from its exports (`instance.rs`,
   lines 32 to 51). The first export with the name prefix
   `wasi:http/incoming-handler` gives the proxy world. The first export with
   the prefix `wasi:cli/run` gives the command world. For other components,
   runwasi calls the function that the entrypoint gives (lines 292 to 307).
   The linker of a command component contains no `wasi:http` (lines 402 to
   413).
6. The default context of wasmtime 36.0.10 has `tcp: true`, `udp: true` and
   `ip_name_lookup: false` (`crates/wasi/src/sockets/mod.rs`). The tab node
   must copy the result of each function of `wasi:sockets` in a proxy
   component. Phase 0 records these results.
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
9. The browser shim of jco (`@bytecodealliance/preview2-shim` 0.31.1):
   - All functions of `dist/browser/sockets.js` throw `not-supported`. The
     file also exports `InMemoryTcpSockets` and `InMemoryUdpSockets`.
   - The class `WASIShim` (`dist/common/instantiation.js`) is the point for
     extensions. It takes `tcpSockets`, `udpSockets`, `sandbox.enableNetwork`
     and a `createCli` function for each instance, with environment,
     arguments, stdin, stdout and stderr.
   - `dist/browser/io.js` exports `pollableCreate`, `inputStreamCreate` and
     `outputStreamCreate`.
   - The shim removes the factories of its HTTP resources after it loads
     (`dist/browser/http.js`, lines 573 to 575, 627 to 629, 708 to 710 and
     830). Other code cannot make these resources.
   - `handleIncomingRequest` reads the full request body before it calls the
     component (lines 929 to 934). The response waits for the full body
     (lines 661 to 672).
   - `wasi:http/outgoing-handler` calls `globalThis.fetch` (line 800).
10. The transpiler is in `@bytecodealliance/jco-transpile` 0.24.0, a
    dependency of jco. The browser path uses `dist/component.js`, the
    `-instantiation.js` file (558,290 bytes) and four core modules in
    `vendor/`. The largest core module has a size of 5,669,325 bytes
    (1,935,646 bytes with `gzip -9`). The code loads the core modules with
    `new URL(..., import.meta.url)`.
11. The in-process network of Go has these properties for UDP (`net_fake.go`
    of Go 1.27.3, the same as `master`; tests with Go 1.24.7):
    - The key of a socket is the address family and `ip:port`, without the
      protocol. TCP and UDP cannot use the same port on the same IP.
    - The receive queue of a socket has 65,535 bytes. In a test, 47 of 100
      datagrams of 1,400 bytes arrived. The other sends failed with
      `ENOBUFS`.
    - `WriteTo` to an address without a socket gives no error. `Write` on a
      connected socket gives `ECONNRESET`.
    - `WriteTo` puts the datagram into the queue of the receiver. No
      NetworkPolicy and no Service translation occur.
    - A socket on a wildcard address receives nothing.
    - Ephemeral ports start at 1, for all IPs together. This is also true
      for TCP.
    - A socket can send a datagram of 65,535 bytes.
    - The boundaries of datagrams and the source address do not change.
12. `wasi:sockets` 0.2 does not let a socket use IPv4-mapped IPv6 addresses
    (`tcp.wit`, lines 61 and 103: `invalid-argument`). An IPv6 socket
    cannot connect to IPv4 peers.
13. pion/sctp is the SCTP of the node agent. Its maximum message size is
    65,536 bytes if the configuration does not set a different size
    (`association.go`, line 84).
14. JSPI is in Chrome and Edge 137, Firefox 153 and Safari 27. The dataset
    `web-features` 3.46.0 gives the status "Baseline newly available" since
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

## 6 Decisions

1. WebAssembly pods get sockets through WASI preview 2 only. Facts 1 and 2
   show that only components have sockets on a Docker node. Modules stay
   without sockets.
2. Each Go thread has one transpile worker and one cache of results. The
   key of the cache is the same as for modules, plus the version of jco.
3. The pod worker runs the output of jco in JSPI mode, with `WASIShim`
   (fact 9). Our own code gives `tcpSockets`, `udpSockets`, `ip-name-lookup`
   and `createCli`. The shim gives the other interfaces.
4. The HTTP of proxy components uses the shim. Request bodies and response
   bodies are complete blocks, with a maximum of 16 MiB. Our own code
   replaces `globalThis.fetch` in the pod worker for client requests.
5. The Go side has every socket. TCP sockets use the in-process network
   through `listenPod` and `dialCluster`. UDP sockets use the UDP table,
   because the in-process network cannot keep TCP and UDP on one port
   (fact 11).
6. A socket that binds to `127.0.0.1` listens on the pod IP. It accepts only
   connections from the pod IP. Other pods then get the same result as on a
   real node, because the tab node has no loopback for each pod.
7. The tab node serves proxy components the same as runwasi (fact 4). Each
   request gets a new instance. Requests run at the same time, with the same
   context as on runwasi.
8. Client requests of proxy components use the cluster client of
   `tabnode_dial.go`. Client requests to hosts outside the cluster fail,
   because the internet is out of scope. Command components get no
   `wasi:http`, the same as on runwasi (fact 5).
9. The UDP table is for all pods of a tab node. CoreDNS, `nslookup` and
   `nc -u` of the tab node use it.
10. The page examines the source of each datagram between nodes, the same
    as for TCP. Docker nodes trust the page.
11. The tab node does not get IP packets. The ICMP echo reply is a function
    of the tab node. The NetworkPolicy engine examines each echo request.
12. For a failure, the tab node gives the same pod status as runwasi.
    Phase 0 records the status on runwasi for each failure of section 7.1.

## 7 Requirements

Each table has a column "Note" for the reason or for an example. A note is
not a requirement.

### 7.1 Components

| ID | Requirement | Note |
| --- | --- | --- |
| CO1 | The tab node must run a command component in a pod worker. | |
| CO2 | The tab node must find the world of a component in the same sequence as runwasi. | Fact 5. |
| CO3 | For a component without the two exports, the tab node must call the function that the entrypoint gives. | runwasi does this (fact 5). |
| CO4 | If this function is not in the exports, the pod must get the status that runwasi gives. | Phase 0, item 7. |
| CO5 | The arguments, the environment, stdin, stdout, stderr, the exit code and the volumes of a command component must agree with those of a module. | The pod worker gives them through `createCli` of `WASIShim`. |
| CO6 | The memory of each instance must be part of the memory that `limits.memory` controls. | |
| CO7 | When the pod worker discards an instance, this memory must not contain the memory of this instance after that time. | The browser frees the memory later, at garbage collection. |
| CO8 | The transpile worker must transpile a component one time for each key of the cache. | Other pods and restarts use the cached result. |
| CO9 | The transpiler must not run on a Go thread. | A transpiler on the Go thread stops the tab node. |
| CO10 | If the browser has no JSPI, a component pod must get `CreateContainerError` with a text that gives the cause. | Fact 14: old browsers only. |
| CO11 | If a component imports an interface that the tab node does not give, the pod must get the status that runwasi gives. | Examples: WASI 0.3, `wasi:keyvalue`. |
| CO12 | The status text must give the name of the import. | |
| CO13 | A module must run with no change. A module gets no socket functions. | |
| CO14 | The generated JavaScript of a component must run only in a pod worker. | Not on a Go thread and not in the page. |
| CO15 | The transpile worker must examine all names of imports and exports before it transpiles. | |
| CO16 | A name with characters that are not in the WIT syntax must fail with `CreateContainerError`. | Section 8.2. |
| CO17 | A component can call 1,000 functions that do not wait and give no data. Then the pod worker must suspend the next call for one task of the event loop. | Without this pause, a component that only polls never receives messages. |

### 7.2 Proxy components

| ID | Requirement | Note |
| --- | --- | --- |
| PX1 | The tab node must serve a proxy component on the address of `WASMTIME_HTTP_PROXY_SOCKET_ADDR`. | Fact 4. |
| PX2 | If the environment does not contain this variable, the address must be `0.0.0.0:8080`. | |
| PX3 | If the value does not parse, the address must be `0.0.0.0:8080`, without an error. | runwasi does the same. |
| PX4 | The IP `0.0.0.0` must listen on the pod IP. | |
| PX5 | The IP `127.0.0.1` must listen on the pod IP and accept only connections from the pod IP. | Decision 6. |
| PX6 | The tab node must remove `WASMTIME_HTTP_PROXY_SOCKET_ADDR` and `WASMTIME_HTTP_BACKLOG` from the environment of the component. | |
| PX7 | Each request must get a new instance of the component. | |
| PX8 | Requests must run at the same time. | |
| PX9 | One container must have a maximum of 16 instances at the same time. | runwasi has no maximum. Section 11. |
| PX10 | When 16 instances run, more requests must wait in a queue. | |
| PX11 | The environment of an instance must contain `REQUEST_ID`. | |
| PX12 | The first request of a container must get the `REQUEST_ID` 0. Each request after it must get the next number. | |
| PX13 | An instance must get no arguments, no preopened directories and an empty stdin. | |
| PX14 | The tab node must discard stdout and stderr of an instance. | runwasi gives no stdio. |
| PX15 | Each function of `wasi:sockets` in an instance must give the same result as on runwasi. | Fact 6. Phase 0, item 7. |
| PX16 | The response must go to the client with its status, its headers and its body. | |
| PX17 | If the response has no `Content-Type`, the tab node must not add one. | `net/http` of Go adds one if the code does not stop it. |
| PX18 | A request body or a response body of more than 16 MiB must fail. | Decision 4. |
| PX19 | For a request body of more than 16 MiB, the client must get the status 413. | |
| PX20 | For a response body of more than 16 MiB, the client must get the status 500. | |
| PX21 | If an instance traps or sets no response, the client must get the same result as on runwasi. | Phase 0, item 7. |
| PX22 | After a trap, the container must continue to run. | Fact 4. |
| PX23 | The tab node must not set a timeout for a request. | runwasi has no timeout. |
| PX24 | If the client closes its connection, the pod worker must discard the instance of this request. | The instance does not continue. Section 11. |
| PX25 | The pod worker must discard an instance after `handle` returns and after the response body is complete. | |

### 7.3 Client requests

| ID | Requirement | Note |
| --- | --- | --- |
| CR1 | A client request of a proxy component must use the cluster client of `tabnode_dial.go`. | Service names, ClusterIPs and NetworkPolicy then give the same results as for other pods. |
| CR2 | A client request to a host outside the cluster must fail with the error code `destination-not-found`. | Decision 8. |
| CR3 | The cluster client must not follow redirects. | wasmtime gives a redirect to the component. |
| CR4 | The cluster client must not add headers. | Examples: `User-Agent`, `Accept-Encoding`. |
| CR5 | The cluster client must not decompress the body. | |
| CR6 | A command component that imports `wasi:http` must get the status that runwasi gives. | Fact 5. Phase 0, item 7. |

### 7.4 TCP

| ID | Requirement | Note |
| --- | --- | --- |
| TC1 | When an IPv4 TCP socket binds to the pod IP or to `0.0.0.0`, it must listen and accept on the pod IP. | |
| TC2 | When a TCP socket binds to `127.0.0.1`, it must listen on the pod IP. | Decision 6. |
| TC3 | This socket must accept only connections from the pod IP. | |
| TC4 | Other connections to this socket must fail with `connection-refused`. | A real node gives this result for a port that is local only. |
| TC5 | After a socket binds to `127.0.0.1`, `local-address` must give `127.0.0.1`. | |
| TC6 | An IPv6 socket must be able to bind to `::`. | |
| TC7 | An IPv6 socket must not listen in the in-process network. | Fact 12. `listenPod` changes `::` to the pod IP, so the Go side must not call it for IPv6. |
| TC8 | A socket that connects must use `dialCluster`. | |
| TC9 | A socket that connects without a bind must get an ephemeral port from 32768 to 60999. | Fact 11: the in-process network starts at 1. |
| TC10 | A connection that NetworkPolicy denies must fail with `timeout`. | The wait is 127 seconds (section 4.3). |
| TC11 | A connection to a port without a listener must fail with `connection-refused`. | |
| TC12 | Each connection must have a credit of 256 KiB in each direction. | Section 8.4. |
| TC13 | `shutdown` must close the receive direction, the send direction or the two directions, as the component asks. | |
| TC14 | When a container stops, the Go side must close all sockets of the container. | |

### 7.5 UDP on a tab node

| ID | Requirement | Note |
| --- | --- | --- |
| UD1 | The key of the UDP table must contain the protocol, the IP and the port. | TCP and UDP can then use the same port on the same IP. |
| UD2 | When a UDP socket binds to the pod IP or to `0.0.0.0`, it must receive on the pod IP. | |
| UD3 | A socket that binds to port 0, or that sends before it binds, must get an ephemeral port from 32768 to 60999. | Fact 16. |
| UD4 | Each IP must have its own range of ephemeral ports. The first ephemeral port of an IP must be 32768. | |
| UD5 | Each UDP socket must have a receive queue of 256 KiB. | Section 8.4. |
| UD6 | If the queue of a socket is full, the tab node must drop the datagram. | The sender gets no error, as on Linux. |
| UD7 | Each datagram must use the Service translation and the flow table of section 8.8 before it arrives. | |
| UD8 | The tab node must drop a datagram to a pod IP and a port without a socket on the same tab node. | |
| UD9 | After such a datagram, a connected socket must get `connection-refused` on its next receive or send. | Linux gives this result after ICMP "port unreachable". |
| UD10 | The tab node must drop a datagram to a ClusterIP without endpoints. | Fact 17. |
| UD11 | After such a datagram, a connected socket must get `connection-refused` on its next receive or send. | |
| UD12 | The tab node must drop a datagram to a ClusterIP and a port that no Service has. The sender must get no error. | No rule of kube-proxy catches it. |
| UD13 | A pod must be able to send a datagram to a Service that has this pod as its endpoint. | Hairpin. |
| UD14 | A datagram of more than 65,507 bytes must fail with `datagram-too-large` in the Go side. | Fact 11. |
| UD15 | The boundaries and the source address of a datagram must not change. | |
| UD16 | `tabnode_net.go` must forward UDP for Services with `protocol: UDP`. | |
| UD17 | One ClusterIP must be able to have the same port for TCP and for UDP. | Example: the Service `kube-dns`. |
| UD18 | CoreDNS on a tab node must give answers over UDP and over TCP on port 53. | |
| UD19 | `nslookup` and `nc -u` of a tab node must use the UDP table. | |

### 7.6 UDP flows

| ID | Requirement | Note |
| --- | --- | --- |
| FL1 | The key of a flow must contain the tuple before the Service translation and the tuple after it. | |
| FL2 | A flow without answers must close after 30 seconds without a datagram. | Fact 16. |
| FL3 | A flow that has answers and is active 2 seconds after its first datagram must close after 120 seconds without a datagram. | Fact 16. |
| FL4 | Other flows with answers must close after 30 seconds without a datagram. | Example: one DNS query and its answer. |
| FL5 | When the EndpointSlices of a Service no longer contain an endpoint, the tab node must remove all flows to this endpoint. | kube-proxy removes these conntrack entries. |
| FL6 | NetworkPolicy must examine the first datagram of a flow. | |
| FL7 | The next datagrams of an open flow, in the two directions, must not get a new examination. | As conntrack on a real node. |
| FL8 | After a NetworkPolicy change, the strict mode must examine all open flows again, with its delay. | Fact 15. |
| FL9 | The strict mode must remove the flows that the policy denies now. | |
| FL10 | `sessionAffinity: ClientIP` must give the same results for UDP as for TCP. | |
| FL11 | `internalTrafficPolicy: Local` must give the same results for UDP as for TCP. | |

### 7.7 Name lookup

| ID | Requirement | Note |
| --- | --- | --- |
| NL1 | `ip-name-lookup` must find names with the search path of the pod. | |
| NL2 | `ip-name-lookup` must use the same resolver as `nslookup` in a pod of the tab node. | |
| NL3 | A name outside the cluster must fail with `name-unresolvable`. | A Docker node finds such names. Section 11. |

### 7.8 UDP between nodes

| ID | Requirement | Note |
| --- | --- | --- |
| ND1 | The test images of each phase must give the same results on a tab node, on a node worker and on a Docker node. | Section 11 lists the differences that stay. |
| ND2 | Between the page and a node worker, UDP must use one channel `udp` for each node worker, on the MessagePort of `web/src/channel.js`. | |
| ND3 | Between the page and a Docker node, UDP must use one WebRTC data channel `udp` for each node, with `ordered: false` and `maxRetransmits: 0`. | |
| ND4 | Each message on a `udp` channel must start with a header of 14 bytes. | Section 8.9. |
| ND5 | The largest message must not be more than 65,536 bytes. | 14 + 65,507 = 65,521 bytes. Fact 13. |
| ND6 | The source node must examine the egress policy of the source pod. | |
| ND7 | For each datagram from a node, the page must make sure that the source IP belongs to that node. | The node IP or the pod CIDR of the node. The page knows the node from the channel. |
| ND8 | The page must drop a datagram with a source IP that does not belong to the node. | |
| ND9 | A tab node that is the destination node must examine the ingress policy of the destination pod. | |
| ND10 | On a Docker node, the nftables rules of kube-network-policies must examine the ingress policy. | |
| ND11 | If more than 1 MiB of datagrams wait for a node worker, the page must drop the next datagram to it. | Section 8.4. |
| ND12 | The page and the node agent must drop a datagram on a WebRTC `udp` channel when `bufferedAmount` is more than 1 MiB. | |
| ND13 | The node agent must catch UDP from Docker pods to tab pods with an nftables `tproxy` rule for UDP and an `IP_TRANSPARENT` socket. | |
| ND14 | The node agent must find the original destination of each datagram with `IP_RECVORIGDSTADDR`. | |
| ND15 | The node agent must send answers to a Docker pod from a socket with the original destination as its address. | The answer then has the tab pod IP as its source. |
| ND16 | The node agent must send a datagram from a tab pod to a Docker pod through an `IP_TRANSPARENT` socket. This socket must have the IP and the port of the tab pod. | |
| ND17 | The node agent must send these datagrams from the helper process with a uid that is not 0. | Section 4.4. Without it, kube-network-policies does not examine the datagram. |
| ND18 | The node agent must forward the answers of the Docker pod back to the page on the same `udp` channel. | |
| ND19 | The node agent must keep flows with the timeouts of FL2 to FL4. | |

### 7.9 ICMP echo

| ID | Requirement | Note |
| --- | --- | --- |
| IC1 | The `ping` built-in command must get an echo reply from a pod IP on the same tab node. NetworkPolicy must let the echo request go. | IC4 and IC5 give the examination. |
| IC2 | The `ping` built-in command must get an echo reply from the node IP of the tab node. | |
| IC3 | For an IP on another node, `ping` must give no echo reply and must show that ICMP between nodes is not available. | Out of scope. |
| IC4 | The tab node must examine each echo request with the engine of kube-network-policies, with no protocol and with port 0. | Fact 15. |
| IC5 | The tab node must examine the egress policy of the source and the ingress policy of the destination. | |
| IC6 | A ClusterIP must give no echo reply. | Iptables mode of kube-proxy, as on the Docker nodes. |
| IC7 | `README.md` must show that a policy rule with `ports` does not match ICMP. | |
| IC8 | `README.md` must show that a tab node examines each echo request, but a Docker node examines only the first one. | Fact 15. |

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
                             | WASIShim with our sockets and our CLI   |
                             | globalThis.fetch -> message "fetch"     |
                             +-----------------------------------------+
```

The page and each node worker have these parts. The pod worker opens no
network connections. It only moves bytes and events between the component
and the Go side.

### 8.2 Transpiler

The Go side does these steps for a component layer:

1. Make the key from the layer digests, the path and the version of jco.
2. Find the key in the cache of `wasi.js`.
3. If the cache does not contain the key, send the bytes to the transpile
   worker.
4. Compile the core modules of the result with `WebAssembly.compile`.
5. Keep the JavaScript and the compiled core modules in the cache.
6. Send the JavaScript and the core modules to the pod worker.

The transpile worker does these steps:

1. Read the names of all imports and exports.
2. Make sure that each name obeys the WIT syntax (CO15, CO16).
3. Transpile with `instantiation: "async"` and the JSPI lists of fact 8.

The mode `instantiation: "async"` gives a function that takes all imports
as an object. The generated JavaScript then has no bare module names, which
a `Blob` URL cannot find.

The Go thread starts its transpile worker for the first component. The
transpile worker then loads the transpiler. A Go thread without components
does not load it.

The task `web` of `mise.toml` must bundle the transpile worker in the ESM
format and start it as a module worker. The task must also copy the four
core modules of `jco-transpile` next to the bundle.

The pod worker loads the JavaScript from a `Blob` URL. The JavaScript then
runs on the origin of the page, in the pod worker. Phase 0 examines a worker
with an opaque origin as a better location (section 9.1, item 3).

### 8.3 Messages

The pod worker and the Go side use the current message channel of the pod
worker (`wasi.js`). Bytes go as a transferable `ArrayBuffer`. This transfer
makes no copy between the threads, but Go copies the bytes into its memory
and out of it.

Socket messages:

| Type | Direction | Contents |
| --- | --- | --- |
| `bind` | worker to Go | socket ID, protocol, family, address, port |
| `bound` | Go to worker | socket ID, local address and port, or an error code |
| `listen` | worker to Go | socket ID, backlog |
| `listening` | Go to worker | socket ID, or an error code |
| `accepted` | Go to worker | listener socket ID, new socket ID, remote address and port |
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
`connection-refused` for UD9 and UD11.

HTTP messages:

| Type | Direction | Contents |
| --- | --- | --- |
| `request` | Go to worker | request ID, method, URL, headers, body |
| `response` | worker to Go | request ID, status, headers, body, or the result of a trap |
| `cancel` | Go to worker | request ID |
| `fetch` | worker to Go | fetch ID, method, URL, headers, body |
| `fetched` | Go to worker | fetch ID, status, headers, body, or an error code |

### 8.4 Credits and queues

A pod worker reads its messages only while the component waits. So each
direction must have a limit:

| Item | Limit | When the limit is full |
| --- | --- | --- |
| TCP bytes, each direction of a connection | 256 KiB | The sender stops until the receiver sends `credit`. |
| UDP bytes in the queue of a socket in the pod worker | 256 KiB | The Go side drops the datagram (UD6). |
| Accepted connections that the component did not take | the backlog of `listen` | The Go side does not accept more connections. |
| HTTP bodies | 16 MiB | PX18 to PX20. |
| Datagrams on the `udp` channel to a node worker | 1 MiB | The page drops the datagram (ND11). |
| Datagrams on a WebRTC `udp` channel | 1 MiB in `bufferedAmount` | The sender drops the datagram (ND12). |

### 8.5 Calls that wait

These functions can wait. The pod worker gives them in the list
`asyncMode.val.imports` of jco:

- all imports of fact 8
- the functions of our sockets that wait for an address or a connection

The pod worker gives `wasi:cli/run#run` and
`wasi:http/incoming-handler#handle` in the list `asyncMode.val.exports`.

A call that the pod worker suspended continues in these conditions:

- A message from the Go side changes the state of a pollable.
- The time of a clock pollable ends.

The pod worker of a component must not use `Atomics.wait`. During
`Atomics.wait`, the worker cannot receive messages. The current module path
with `Atomics.wait` stays only for modules.

A component can call functions that do not wait in a loop. Then
the pod worker never receives messages. CO17 stops this condition.

### 8.6 Proxy components

The Go side does these steps for a proxy component:

1. Parse the address of PX1 to PX5.
2. Listen on this address with `net/http`.
3. For each request, read the body to a maximum of 16 MiB.
4. Send a `request` message to the pod worker.
5. Wait for the `response` message.
6. Send the response to the client. Do not add a `Content-Type`.
7. If the client closes the connection, send `cancel`.

The pod worker does these steps for each `request` message:

1. Instantiate the core modules again, with a new context (PX11 to PX15).
2. Make a `Request` object from the message.
3. Call `handleIncomingRequest` of the shim with this object.
4. Send the `response` message to the Go side.
5. Discard the instance (PX25).

JSPI can suspend many instances in one pod worker. One suspended instance
does not stop the others. The compiled core modules are the same for all
instances. Only the memory of an instance is new.

For a `cancel` message, the pod worker does not continue the instance. It
removes all references to it.

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
(FL2 to FL4). The tab node removes flows when an endpoint is not in the
EndpointSlices (FL5) and when a policy changes (FL8, FL9).

### 8.9 UDP between nodes

```text
tab pod -> UDP table -> page -> udp channel -> node agent
        -> IP_TRANSPARENT socket (helper, uid not 0) -> Docker pod
Docker pod -> tproxy -> node agent -> udp channel -> page
        -> UDP table -> tab pod
```

The page is the center. The page forwards each datagram between two
nodes, as for TCP. Each message has this header:

| Byte | Contents |
| --- | --- |
| 0 | Version: 1 |
| 1 | Type: 1 for a datagram. Other values are for a later design. |
| 2 to 5 | Source IPv4 address |
| 6 and 7 | Source port |
| 8 to 11 | Destination IPv4 address |
| 12 and 13 | Destination port |

The datagram follows the header. A Docker node has no informers, so the
page examines the source (ND7, ND8). Between the page and a node worker,
the channel has no loss. Between the page and a Docker node, loss can occur
(ND3).

### 8.10 Changes to files

| File | Change |
| --- | --- |
| `web/src/transpile.worker.js` (new) | Transpile components with jco (section 8.2). |
| `web/src/wasi.js` | Start components. Keep the results of the transpile worker in the cache. |
| `web/src/wasi.worker.js` | Run components in JSPI mode with `WASIShim`. Instantiate proxy components for each request. Send the messages of section 8.3. |
| `web/src/sockets.js` (new) | `tcpSockets`, `udpSockets` and `ip-name-lookup` for `WASIShim`. |
| `web/src/fetch.js` (new) | The replacement of `globalThis.fetch` in the pod worker. |
| `web/src/channel.js`, `web/src/nodes.js` | The `udp` channel to node workers. |
| `web/package.json` | Add `@bytecodealliance/jco` and `@bytecodealliance/preview2-shim` with a fixed version. |
| `mise.toml` | The task `web` bundles the transpile worker as ESM and copies the core modules of `jco-transpile`. Add Rust with the target `wasm32-wasip2`. |
| `site/build.mjs` | Copy the core modules of `jco-transpile` to the site. |
| `k8s/tabernetes/tabnode_wasi.go` | The sockets of each container, the proxy server and client requests. Close all sockets when a container stops. |
| `k8s/tabernetes/tabnode_udp.go` (new) | The UDP table (section 8.7). |
| `k8s/tabernetes/tabnode_dial.go` | Add `listenPodUDP`, `dialClusterUDP` and ephemeral ports. Change the cluster client for CR3 to CR5. |
| `k8s/tabernetes/tabnode_net.go` | UDP Services and the flow table. |
| `k8s/tabernetes/tabnode_netpol.go`, `tabnode_netpol_strict.go` | NetworkPolicy for UDP flows and for ICMP. The strict mode for flows. |
| `k8s/tabernetes/tabnode_coredns.go` | Answers over UDP. |
| `k8s/tabernetes/coreutils.go`, `coreutils_misc.go` | `nslookup` over UDP, `nc -u`, the ICMP echo of `ping`. |
| `k8s/tabernetes/podlink.go` | The `udp` channel and the examination of the source in the page. |
| `node/agent/podlink.go` | nftables rules for UDP, transparent UDP sockets in the helper process, flows. |
| `wasm-images/images.txt`, `wasm-images/build.sh` | Test images in Rust (section 10.1). |
| `.github/workflows/ci.yaml` | Rust for the test images. |
| `README.md` | The new behavior and the differences of section 11. Remove "Not covered: UDP" (line 558). |
| `docs/networking-policy.md` | UDP flows, the `udp` channel and ICMP. |

## 9 Phases

Each phase starts only after all tests of the phase before it give correct
results. Section 10 gives the tests.

| Phase | Contents |
| --- | --- |
| 0 | Proof of concept in `docs/wasi-sockets-poc/`. Section 9.1 gives its items. |
| 1 | Components without network: CO1 to CO17. |
| 2 | Proxy components and client requests: PX1 to PX25, CR1 to CR6. |
| 3 | TCP and name lookup: TC1 to TC14, NL1 to NL3. |
| 4 | UDP on a tab node: UD1 to UD19, FL1 to FL11. |
| 5 | UDP between nodes: ND1 to ND19. |
| 6 | ICMP echo: IC1 to IC8. |

### 9.1 Items of phase 0

The proof of concept must give a result for each item:

1. Transpile a command component and a proxy component in a transpile
   worker. Do this in Chrome, Firefox and Safari.
2. Measure the time and the size for a component of 1 MB. Do the same
   for a component of more than 10 MB.
3. Load the output from a `Blob` URL in a pod worker. Then try a worker with
   an opaque origin.
4. Make sure that the page has no Content Security Policy that stops the
   load from a `Blob` URL.
5. Run 16 instances of a proxy component in one pod worker with JSPI. Make
   sure that they continue in a different sequence.
6. Send 10,000 requests in a sequence to a proxy component. Record the
   memory of the browser tab.
7. On a Docker node with runwasi v0.6.1, record these results:
   - the result of each function of `wasi:sockets` in a proxy component
     (PX15)
   - what the client gets after a trap and without a response (PX21)
   - the pod status for a component without the necessary export (CO4)
   - the pod status for an import that the tab node does not give (CO11)
   - the pod status for a command component that imports `wasi:http` (CR6)
   - the name lookup of a Service by its short name (NL1)
8. Run the echo image for TCP, the echo image for UDP and the proxy image on
   the Docker node.
9. Bind TCP and UDP on the same port in a prototype of the UDP table.
10. Send a message of 65,521 bytes on an unordered WebRTC data channel from
    the browser to pion.
11. Bundle the transpile worker as ESM with the task `web`. Load the four
    core modules from the bundle.
12. Make sure that the replacement of `globalThis.fetch` gets the client
    requests of the shim.

## 10 Tests

### 10.1 Test images

The mirror gets these images, in Rust:

- a command component that writes its arguments, its environment and a
  file of a volume
- an echo server for TCP, and an echo server for UDP
- a proxy component that shows its context
- a proxy component that sends client requests

### 10.2 Tests for each requirement

The column "Nodes" gives the nodes for the test:

- "tab" is the tab node of the page.
- "worker" is a node worker.
- "Docker" is a Docker node with runwasi.

Tests with NetworkPolicy on a Docker node must wait for the open item of
section 4.4.

| Requirements | Test | Nodes |
| --- | --- | --- |
| CO1, CO5 | The command component writes its arguments, its environment and a file of a volume to stdout. It reads stdin and stops with exit code 3. | tab, worker, Docker |
| CO2 | A component with the two exports runs in the world of its first export. | tab, Docker |
| CO3, CO4 | A component without the two exports runs the function of the entrypoint. A second component without this function gets the status of phase 0. | tab, Docker |
| CO6, CO7 | A component that uses more memory than `limits.memory` gets `OOMKilled`. After 100 requests to a proxy component, the counted memory is the memory of the instances that run. | tab |
| CO8, CO9 | Two pods of the same image cause one transpile step. During the transpile step of a large component, the terminal shows each typed character at once. | tab, worker |
| CO10 | The test removes `WebAssembly.Suspending`. A component pod then gets `CreateContainerError` with the cause. | tab |
| CO11, CO12 | A component with a WASI 0.3 import gets the status of phase 0 with the name of the import. | tab, Docker |
| CO13 | All current tests of `tabnode_wasi_test.go` give correct results. | tab |
| CO14 | A unit test makes sure that the page and the Go thread load no generated JavaScript. | tab |
| CO15, CO16 | A component with a name that contains a quotation mark gets `CreateContainerError`. | tab |
| CO17 | A component polls a socket without a wait. A datagram to it arrives in less than 1 second. | tab |
| PX1, PX4, PX16 | `kubectl expose` and `wget -qO- <service>` get the response. A readiness probe with `httpGet` gives a correct result. | tab, worker, Docker |
| PX2, PX3 | A pod without the variable and a pod with an incorrect value listen on port 8080. | tab, Docker |
| PX5 | With `127.0.0.1:9000`, another pod gets `connection-refused`. A probe of the pod itself gets the response. | tab, Docker |
| PX6, PX11, PX13 | The context component writes its environment, its arguments and its preopened directories into the response. | tab, Docker |
| PX7 | The component increments a global counter and writes it into the response. Each response shows 1. | tab, Docker |
| PX8, PX9, PX10 | 20 requests at the same time go to a component that waits 1 second. On the tab node, all responses come in approximately 2 seconds. On a Docker node, they come in approximately 1 second. | tab, Docker |
| PX12 | 3 requests in a sequence get the `REQUEST_ID` values 0, 1 and 2. | tab, Docker |
| PX14 | The component writes to stdout and to stderr. `kubectl logs` shows no text from the instance. | tab, Docker |
| PX15 | The component calls each function of `wasi:sockets` and writes the results into the response. The results agree with phase 0. | tab, Docker |
| PX17 | A response without `Content-Type` arrives without `Content-Type`. | tab, Docker |
| PX18, PX19, PX20 | A request body of 17 MiB gets the status 413. A response body of 17 MiB gets the status 500. A body of 1 MiB arrives without a change. | tab |
| PX21, PX22 | A request that causes a trap gives the result of phase 0. The next request gets a response. | tab, Docker |
| PX23, PX24, PX25 | A request that waits for 10 minutes stays open. A client that closes the connection sets the number of instances to 0. | tab |
| CR1 | The component gets a response from another Service by its name. A NetworkPolicy that denies it gives an error. | tab, worker, Docker |
| CR2 | A client request to `example.com` fails with `destination-not-found`. | tab |
| CR3, CR4, CR5 | A test Service sends a redirect and a compressed body, and writes the headers that it receives. The component gets the redirect, the compressed body and no added headers. | tab, Docker |
| CR6 | A command component that imports `wasi:http` gets the status of phase 0. | tab, Docker |
| TC1, TC8 | The TCP echo pod gives answers through a Service. | tab, worker, Docker |
| TC2, TC3, TC4, TC5 | A server on `127.0.0.1` gives `127.0.0.1` as `local-address`. A client in the same pod connects. A client in another pod gets `connection-refused`. | tab, Docker |
| TC6, TC7 | A bind to `::` gives no error. A connection to the pod IP gets `connection-refused`. | tab, Docker |
| TC9 | A client that connects without a bind has a local port from 32768 to 60999. | tab, Docker |
| TC10 | A denied client gets `timeout` after approximately 127 seconds. | tab, Docker |
| TC11 | A client to a closed port gets `connection-refused`. | tab, Docker |
| TC12 | A unit test sends 10 MB to a server that does not read. The buffer of the Go side for this connection stays at 256 KiB or less. | tab |
| TC13 | The client closes its send direction. The server reads the end of the stream and sends its answer. | tab, Docker |
| TC14 | After `kubectl delete pod` of the server, the client gets the end of its connection. | tab |
| NL1, NL2 | A component finds a Service by its short name. | tab, worker, Docker |
| NL3 | The name `example.com` fails with `name-unresolvable`. | tab |
| UD1, UD16, UD17, UD18 | The Service `kube-dns` has 53/UDP and 53/TCP. `nslookup` over UDP and over TCP get answers from CoreDNS on the tab node. | tab |
| UD2, UD7, UD15 | The UDP echo pod gives answers through a Service. The answers have the correct source address. | tab, worker, Docker |
| UD3, UD4 | Two pods bind to port 0 at the same time. Each pod gets port 32768. | tab |
| UD5, UD6 | A client sends 1,000 datagrams of 1,024 bytes to a server that does not read. The sender gets no error. The server then reads 256 datagrams. | tab |
| UD8, UD9 | A connected client sends to a port without a socket. The next receive gives `connection-refused`. | tab |
| UD10, UD11 | A connected client sends to a Service without endpoints. The next receive gives `connection-refused`. | tab |
| UD12 | A client sends to a ClusterIP port without a Service. The client gets no error and no answer. | tab |
| UD13 | The UDP echo pod sends to its own Service and gets its answer. | tab |
| UD14 | A datagram of 65,508 bytes fails with `datagram-too-large`. | tab, Docker |
| UD19 | `nc -u` sends a line to the UDP echo pod and receives it. | tab |
| FL1, FL6, FL7 | A client sends to a ClusterIP. The answers come with the ClusterIP as their source. | tab |
| FL2 | A flow without answers is not in the flow table after 31 seconds. | tab |
| FL3 | A flow with answers for 5 seconds is in the flow table after 100 seconds without datagrams. | tab |
| FL4 | One query and one answer: the flow is not in the flow table after 31 seconds. | tab |
| FL5 | Scale the UDP Deployment from 2 to 1. A client that sends one datagram each second gets answers from the other endpoint. | tab |
| FL8, FL9 | A new policy denies an open flow. The next datagram does not arrive. | tab |
| FL10 | With `sessionAffinity: ClientIP`, 10 datagrams from one client go to one endpoint. | tab |
| FL11 | With `internalTrafficPolicy: Local`, datagrams from a tab pod go only to endpoints on the same tab node. | tab, worker |
| ND1 | The tests with "worker" or "Docker" in this table. | worker, Docker |
| ND2 | UDP between pods on the page node and on a node worker. | tab, worker |
| ND3, ND4, ND5 | A tab pod sends a datagram of 65,507 bytes to a Docker pod. The Docker pod gets it. | tab, Docker |
| ND6, ND9, ND10 | A denied tab pod and a denied Docker pod get no answers. | tab, Docker |
| ND7, ND8 | A unit test of the page sends a datagram with a false source IP on the channel of a node. The page drops it. | tab |
| ND11, ND12 | A unit test sends 2 MiB of datagrams to a channel that does not move. The page drops the datagrams after 1 MiB. | tab |
| ND13, ND14, ND15 | A Docker pod sends to a tab pod and gets an answer from the tab pod IP. | Docker |
| ND16, ND17, ND18 | A tab pod sends to a Docker pod and gets an answer. A policy on the Docker node denies a second tab pod. | tab, Docker |
| ND19 | A unit test of the agent makes sure that a flow without answers closes after 30 seconds. | Docker |
| IC1, IC4, IC5 | `ping` to a pod IP gets echo replies. A policy without ICMP for the pod stops them. | tab |
| IC2 | `ping` to the node IP gets echo replies. | tab |
| IC3 | `ping` to a pod on a Docker node shows that ICMP between nodes is not available. | tab, Docker |
| IC6 | `ping` to a ClusterIP gets no echo reply. | tab |
| IC7, IC8 | `README.md` contains the two texts. | none |

## 11 Differences from a Docker node

These differences stay after this design. `README.md` must show them.

| Item | Tab node | Docker node with runwasi |
| --- | --- | --- |
| Instances of a proxy component at the same time | A maximum of 16 | No maximum |
| Bodies of HTTP requests and responses | Complete blocks, a maximum of 16 MiB | Streams, no maximum |
| An instance after the client closes the connection | The pod worker discards it | It runs until `handle` returns |
| Client requests and name lookup to the internet | They fail | They work |
| UDP in the tab node | No loss before a queue is full | Loss is possible |
| `connection-refused` for UDP between nodes | Not available | Available |
| ICMP | Only on one tab node, each echo request examined | Between all nodes, only the first echo request examined |

## 12 Risks

| Risk | Effect | Action |
| --- | --- | --- |
| The transpiler is large (fact 10). | The first component pod of each Go thread loads approximately 2.5 MB more. | The transpile worker loads it only for the first component. The browser keeps the files in its HTTP cache for the other Go threads. |
| The transpile step of a large component is slow. | The pod starts late. | The transpile worker keeps the Go thread free (CO9). Phase 0 measures the time. |
| The generated JavaScript contains names from the component. | A bad name can put code into the origin of the page. A pod worker can use IndexedDB, Cache Storage and same-origin requests. | CO14 to CO16. Phase 0 tries a worker with an opaque origin (item 3). |
| A Content Security Policy can stop code from a `Blob` URL. | Components cannot run. | The page sets no Content Security Policy now. Phase 0, item 4. |
| esbuild in the IIFE format makes `import.meta.url` empty. | The transpile worker cannot load the transpiler. | Section 8.2: ESM and a copy of the core modules. Phase 0, item 11. |
| Go cannot compile components (fact 7). | Our current test images in Go cannot use sockets. | The new test images are in Rust. `mise.toml` and CI get Rust. |
| Each request makes a new instance with a new memory. | The browser frees the memory only at garbage collection. Many requests can use too much memory. | PX9, CO7. Phase 0, item 6. |
| The Go thread moves the data of all sockets of pods. | Much network traffic of pods makes the control plane slower. This is the same reason as for the rejection of a TCP/IP stack in section 13, but only for traffic of pods. | The credits of section 8.4 set a limit to the memory. A later design can move bytes between workers with shared memory. |
| The tests of NetworkPolicy on a Docker node must have nfqueue in a rootless node. | Some tests cannot run. | The item is open in `docs/networking-policy.md`. Phase 5 starts after it. |
| WASI 0.3 comes. jco-transpile already uses `preview3-shim`. | New components do not run on a tab node. | CO11 and CO12 give a clear text. A later design can add WASI 0.3. |
| Old browsers have no JSPI (fact 14). | These browsers cannot run components. | CO10. Modules continue to run. |

## 13 Alternatives

| Alternative | Result | Reason |
| --- | --- | --- |
| Listen sockets for modules (WASI preview 1, `sock_accept`). | Rejected | runwasi gives no sockets to modules (fact 2). The same manifest then does not give the same results on a Docker node. |
| The socket extension of WasmEdge. | Rejected | Our Docker node runs the wasmtime shim. A second shim makes the node image larger. Few toolchains use this extension. |
| WASIX with `@wasmer/sdk` (fact 18). | Rejected | It must have cross-origin isolation. Its network does not connect to our in-process network. runwasi does not run WASIX with the wasmtime shim. |
| A TCP/IP stack in user space (for example the netstack of gVisor) for IP packets. | Rejected | With this stack, the Go thread moves the data of all connections of a tab node through it. This also includes the connections of the control plane. A Docker node already has a real kernel. |
| ICMP sockets in WebAssembly pods. | Rejected | WASI has no ICMP. Real images in WebAssembly pods do not have `ping`. |
| UDP through the in-process network of Go. | Rejected | TCP and UDP cannot use the same port (fact 11). The queue drops datagrams at the sender. No policy examines them. |
| A change to the `net` package of Go. | Rejected | The project then must keep a patch of the standard library for each Go version. |
| One instance of a proxy component for all requests. | Rejected | runwasi makes a new instance for each request (fact 4). A component with state in global variables then gives different results. |
| A transpiler on the Go thread. | Rejected | It stops the tab node for the full time of the transpile step. |
| One transpile worker in the page for all tab nodes. | Rejected | Node workers then must have a new channel to the page. The browser HTTP cache already stops a second download. |
| Our own `wasi:http`, with bodies in parts. | Later | It replaces a large part of the shim (fact 9). Bodies in parts are a later design. |
| Components that the mirror transpiles before publication. | Later | Images from other registries come when a pod starts, so the transpile worker is necessary in all conditions. |

## 14 Open questions

1. How much time does the transpiler use for a component of more than
   10 MB?
   Phase 0 measures the time. The target is less than 10 seconds in Chrome
   on a laptop.
2. Is 16 the correct maximum of instances for PX9? Phase 0 (item 6) and
   phase 2 measure the memory for each instance.
3. After phase 5, Docker pods can get UDP answers from CoreDNS on a tab
   node. Can CoreDNS then stay on the tab node? A later design must decide.

## 15 Sources

The author read these sources on 2026-11-13.

- runwasi v0.6.1, `crates/containerd-shim-wasmtime/src/instance.rs` and
  `http_proxy.rs`:
  <https://github.com/containerd/runwasi/tree/containerd-shim-wasmtime/v0.6.1/crates/containerd-shim-wasmtime/src>
- wasmtime `main`, `src/common.rs` and `crates/cli-flags/src/lib.rs`, and
  wasmtime 36.0.10, `crates/wasi/src/preview1.rs` and
  `crates/wasi/src/sockets/mod.rs`:
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
