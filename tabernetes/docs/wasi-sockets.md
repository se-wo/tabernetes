# Sockets for WebAssembly pods

Status: proposal, revision 2. No part of this design is in the code.
Revision 2 contains the findings of an independent review of revision 1.

## Language

This document obeys the rules of ASD-STE100 Simplified Technical English.
These words have special status:

- Technical names: the names of code, files, Kubernetes objects, protocols,
  standards and programs.
- Technical verbs: accept, bind, compile, connect, drop, fail, forward,
  instantiate, listen, load, run, serve, suspend, throw, transpile and wrap.
  Each verb has the same definition as in computer networks or in
  WebAssembly.
- "must" and "must not" give a requirement. "can" gives a possibility, not a
  requirement.

## 1 Purpose

WebAssembly pods on the tab node cannot open sockets. A server in such a pod
cannot receive connections. A client in such a pod cannot connect to a
Service. This document gives a design that adds TCP sockets, UDP sockets and
name lookup to these pods.

The same pod manifest must give the same results on the tab node and on a
Docker node. This rule controls most decisions in this document.

## 2 Scope

These items are in the scope of this document:

- WASI preview 2 components on the tab node, in the `wasi:cli/command` world
  and in the `wasi:http/proxy` world.
- TCP and UDP sockets of `wasi:sockets`, and `wasi:sockets/ip-name-lookup`.
- Client requests of components (`wasi:http/outgoing-handler`).
- UDP in the in-process network of the tab: a UDP table, Services, flows,
  NetworkPolicy and cluster DNS.
- UDP between the tab nodes and the Docker nodes.
- An ICMP echo reply for the `ping` built-in command of the terminal.

These items are not in the scope of this document:

- Sockets for WASI preview 1 modules. Section 12 gives the reason.
- WASI preview 3 components.
- Components with the import module `tabernetes_webgpu`, and `wasi:webgpu`.
- Threads, `fork` and `exec` in WebAssembly pods.
- IPv6 in the cluster.
- Raw sockets, IP packets, `tcpdump` and `traceroute` on the tab node.
- Connections from a pod to the internet.
- The move of CoreDNS from the tab node to a Docker node
  (`tabnode_coredns.go`). This move does not change.

## 3 Terms

| Term | Definition |
| --- | --- |
| tab node | The kubelet in the Wasm binary (`k8s/tabernetes/tabnode*.go`). |
| WebAssembly pod | A pod with a runtime class from `wasmRuntimeClasses` (`tabnode_wasi.go`), for example `wasmtime`. |
| module | A WASI preview 1 core module (`wasm32-wasip1`, `GOOS=wasip1`). |
| component | A WASI preview 2 component (`wasm32-wasip2`). |
| command component | A component with the export `wasi:cli/run`. |
| proxy component | A component with the export `wasi:http/incoming-handler`. |
| client request | An HTTP request that a component sends with `wasi:http/outgoing-handler`. |
| source node, destination node | The node that sends a datagram, and the node that receives it. |
| pod worker | The Web Worker that runs one container of a WebAssembly pod (`web/src/wasi.worker.js`). |
| transpile worker | A new Web Worker that transpiles components with jco. The page has one transpile worker. |
| Go thread | The thread that runs the Wasm binary: the control plane, the tab node and `web/src/wasi.js`. |
| Go side | The code of the tab node on the Go thread that starts the pod worker and owns the sockets of the pod. |
| in-process network | The network of Go for `js/wasm` (`net_fake.go`). `net.Listen` and `net.Dial` connect goroutines of the same binary. |
| UDP table | A new table of UDP sockets in the tab node, apart from the in-process network. |
| flow | The state of one UDP conversation, with a timeout. |
| socket ID | A number that the Go side gives to one socket of one pod worker. |

## 4 Current condition

The pod worker:

- It runs modules with `@bjorn3/browser_wasi_shim`. It has no socket
  functions. The section "Limits" of `README.md` (line 764) gives this limit.
- `web/src/wasi.js` (lines 48 to 50) does not accept components. The pod
  gets an "unsupported" message.
- It uses JSPI. `fd_read` on stdin and `poll_oneoff` can suspend until a
  message comes. The page does not use cross-origin isolation.
- Without a subscription to stdin, `poll_oneoff` waits with `Atomics.wait`
  (`wasi.worker.js`, lines 292 to 296). During this wait, the worker cannot
  receive messages.
- It sends `mem` messages, so the memory counts for `limits.memory`
  (`wasi.worker.js`, lines 207 to 214). It gives the import module
  `tabernetes_webgpu` to modules.
- `wasi.js` runs on the Go thread. It keeps compiled modules in a cache. The
  key is the layer digests and the path (`wasi.js`, line 19).

The network of the tab:

- The tab node uses the in-process network for TCP. `listenPod` and
  `dialCluster` (`tabnode_dial.go`) bind the pod IP. `dialCluster` always
  dials TCP. It does not use its `network` argument (lines 49 to 60).
- `tabnode_net.go` (the kube-proxy and the DNS of the tab) does not forward
  UDP ports (lines 162 and 163). CoreDNS on the tab uses TCP only.
- `netPolicy.allowed` examines TCP only (`tabnode_netpol.go`, line 135). The
  strict mode closes open connections that a new policy denies
  (`tabnode_netpol_strict.go`, lines 62 to 79).
- Tab nodes in Web Workers connect to the page through MessagePorts
  (`web/src/channel.js`, `web/src/nodes.js`). Docker nodes connect through
  WebRTC.
- The pod channel between nodes has one ordered channel for each TCP
  connection. Its first line is `TABP/1 tcp <src> <dst>`, and the node that
  receives the connection sends `ok` or `refused`
  (`k8s/tabernetes/podlink.go`, lines 14 to 21).
- On a Docker node, `node/agent/podlink.go` catches TCP to tab pods with
  TPROXY. Its nftables rules are for TCP only (lines 100 and 109).
- On a Docker node, runwasi (`containerd-shim-wasmtime` v0.6.1,
  `node/Dockerfile` line 24) runs WebAssembly pods.

## 5 Facts that control the design

The author and a second reviewer examined these facts in the sources on
2026-11-13. Section 14 gives the source of each fact.

1. runwasi v0.6.1 gives a command component the full network of the pod.
   `wasi_builder` (`instance.rs`, lines 415 to 434) calls
   `inherit_network()`, `allow_tcp(true)`, `allow_udp(true)` and
   `allow_ip_name_lookup(true)`.
2. runwasi gives a module no sockets. It runs a module with `build_p1()`
   (`instance.rs`, line 220). In wasmtime 36.0.10, the version of runwasi,
   `sock_accept`, `sock_recv`, `sock_send` and `sock_shutdown` always return
   `Notsock`.
3. The CLI of wasmtime gives listen sockets to modules only in its legacy
   mode (`-S preview2=n`). It stops with "components do not support
   --tcplisten" for components. runwasi does not use the CLI, so fact 2 is
   the important fact.
4. runwasi serves a proxy component itself (`http_proxy.rs`):
   - The address comes from `WASMTIME_HTTP_PROXY_SOCKET_ADDR`. If this
     variable is not set, the address is `0.0.0.0:8080`. An address that
     does not parse gives `0.0.0.0:8080` too, without an error.
   - runwasi removes `WASMTIME_HTTP_PROXY_SOCKET_ADDR` and
     `WASMTIME_HTTP_BACKLOG` from the environment of the component.
   - Each request gets a new store and a new instance. Requests run at the
     same time, each in a task of its own.
   - The context of a request has the environment and `REQUEST_ID`, a
     counter that starts at 0. It has no arguments, no preopened
     directories, no stdio and no network.
   - The linker of a proxy component has `wasi:http`, so the component can
     use `wasi:http/outgoing-handler`.
5. runwasi finds the world of a component from its exports (`instance.rs`,
   lines 33 to 50). The first export with the name prefix
   `wasi:http/incoming-handler` gives the proxy world. The first export with
   the prefix `wasi:cli/run` gives the command world. Other components get a
   call of the function that the entrypoint names. The linker of a command
   component has no `wasi:http` (`instance.rs`, lines 402 to 413).
6. Go cannot compile a component with sockets. Go 1.27.3 (the version in
   `tabernetes/mise.toml`) and the `master` branch know `wasip1` only. The
   preview 1 adapter can wrap a module into a component, but this component
   gets no `wasi:sockets`. Rust (`wasm32-wasip2`) can compile components.
7. jco 1.42.0 has an export for the browser:
   `@bytecodealliance/jco/component`, with the function `generate`. In this
   function, the option for JSPI is
   `asyncMode: {tag: "jspi", val: {imports, exports}}`.
8. The browser shim of jco (`@bytecodealliance/preview2-shim` 0.31.1):
   - All functions of `dist/browser/sockets.js` throw `not-supported`.
   - `wasi:http/outgoing-handler` uses `globalThis.fetch`
     (`dist/browser/http.js`, line 800). It cannot get to a ClusterIP.
   - `dist/browser/io.js` exports `pollableCreate`, `inputStreamCreate` and
     `outputStreamCreate`. Our own sockets can use them.
9. The transpiler is in `@bytecodealliance/jco-transpile` 0.24.0, a
   dependency of jco. The core module
   `vendor/js-component-bindgen-component.core.wasm` has a size of
   5,669,325 bytes (1,935,646 bytes with `gzip -9`). The JavaScript glue has
   a size of approximately 530 KB. The code loads the files with
   `new URL(..., import.meta.url)`.
10. The in-process network of Go has these properties for UDP (`net_fake.go`
    of Go 1.27.3, the same as `master`; a test with Go 1.24.7):
    - The key of a socket is the address family and `ip:port`, without the
      protocol. TCP and UDP cannot use the same port on the same IP.
    - The receive queue of a socket has 65,535 bytes. In a test, 47 of 100
      datagrams of 1,400 bytes arrived. The other sends failed with
      `ENOBUFS`.
    - A datagram to an address without a socket gives no error.
    - `WriteTo` puts the datagram in the queue of the receiver. No
      NetworkPolicy and no Service translation occur.
    - A socket on a wildcard address receives nothing.
    - Ephemeral ports start at 1, for all IPs together.
    - The in-process network accepts a datagram of 65,535 bytes.
    - The boundaries of datagrams and the source address do not change.
11. `wasi:sockets` 0.2 does not accept IPv4-mapped IPv6 addresses
    (`tcp.wit`: `invalid-argument`). An IPv6 socket cannot connect to IPv4
    peers.
12. pion/sctp is the SCTP of the node agent. Its maximum message size is
    65,536 bytes, if the configuration does not set a different size
    (`association.go`, line 84).
13. JSPI is in Chrome and Edge since version 137 (2025-05-27). Mozilla
    published an intent to ship JSPI in Firefox 153 (2026-06-10). The
    author did not find the status of Safari.
14. kube-network-policies, the NetworkPolicy engine of all nodes, examines
    ICMP. `ParsePacket` gives no protocol and port 0 for ICMP
    (`pkg/network/packet.go`, lines 159 to 161). The engine examines a
    connection again after a policy changes (`pkg/dataplane/controller.go`,
    lines 265 to 272).
15. Linux has these defaults: `nf_conntrack_udp_timeout` is 30 seconds,
    `nf_conntrack_udp_timeout_stream` is 120 seconds and
    `ip_local_port_range` is 32768 to 60999.
16. `@wasmer/sdk` 0.19.1 runs WASIX programs. It must have cross-origin
    isolation. Its network is a WISP connection to a server, and HTTP to
    listeners through its own service worker. Its type definitions have no
    interface for a different network.

## 6 Decisions

1. WebAssembly pods get sockets through WASI preview 2 only. Facts 1 and 2
   show that only components have sockets on a Docker node. Modules stay
   without sockets.
2. One transpile worker transpiles components. The Go thread keeps the
   result in a cache, with the same key as the cache of modules. The build
   of the mirror (`wasm-images/build.sh`) can transpile its own images
   before publication.
3. The pod worker runs the output of jco in JSPI mode. Our own code gives
   `wasi:sockets` and `wasi:http/outgoing-handler`, with the streams and
   pollables of the shim (fact 8). The shim gives the other interfaces.
4. The Go side owns every socket. TCP sockets use the in-process network
   through `listenPod` and `dialCluster`. UDP sockets use the UDP table,
   because the in-process network cannot keep TCP and UDP on one port
   (fact 10).
5. The tab node serves proxy components the same as runwasi (fact 4). Each
   request gets a new instance. Requests run at the same time, with the
   same context as on runwasi.
6. Client requests of proxy components go through `httpClientFor`
   (`tabnode_dial.go`). Command components get no `wasi:http`, the same as
   on runwasi (fact 5).
7. The UDP table is for all pods of the tab. CoreDNS, `nslookup` and `nc -u`
   of the tab use it.
8. The tab node does not get IP packets. The ICMP echo reply is a function
   of the tab node. The NetworkPolicy engine examines it (fact 14).

## 7 Requirements

### 7.1 Components

| ID | Requirement |
| --- | --- |
| C1 | The tab node must run a command component in a pod worker. |
| C2 | The tab node must find the world of a component in the same sequence as runwasi (fact 5). |
| C3 | A component without `wasi:http/incoming-handler` and without `wasi:cli/run` must fail with `CreateContainerError` and a message that gives the cause. |
| C4 | The arguments, the environment, stdin, stdout, stderr, the exit code and the volumes of a command component must agree with those of a module. |
| C5 | The memory of all instances of a component must count for `limits.memory`, as the `mem` messages of modules do. |
| C6 | The transpile worker must transpile a component one time for each key of layer digests and path. Other pods and restarts must use the cached result. |
| C7 | The transpile must not run on the Go thread. |
| C8 | If the browser has no JSPI, a component pod must fail with `CreateContainerError` and a message that gives the cause. |
| C9 | A component that imports a WASI interface of version 0.3 must fail with `CreateContainerError` and a message that gives the cause. |
| C10 | A module must run with no change. A module gets no socket functions. |
| C11 | The generated JavaScript of a component must run only in a pod worker, not on the Go thread and not in the page. |

### 7.2 Proxy components

| ID | Requirement |
| --- | --- |
| H1 | The tab node must serve a proxy component on the pod IP, with the port from `WASMTIME_HTTP_PROXY_SOCKET_ADDR`. |
| H2 | If `WASMTIME_HTTP_PROXY_SOCKET_ADDR` is not in the environment, the port must be 8080. |
| H3 | If the value of `WASMTIME_HTTP_PROXY_SOCKET_ADDR` does not parse, the port must be 8080, without an error. |
| H4 | The tab node must remove `WASMTIME_HTTP_PROXY_SOCKET_ADDR` and `WASMTIME_HTTP_BACKLOG` from the environment of the component. |
| H5 | Each request must get a new instance of the component. |
| H6 | Requests must run at the same time. One container must have a maximum of 16 instances at the same time. More requests must wait in a queue. |
| H7 | The environment of an instance must contain `REQUEST_ID`. The first request of a container gets 0, and each request adds 1. |
| H8 | An instance must get no arguments, no preopened directories and an empty stdin. |
| H9 | The tab node must discard stdout and stderr of an instance, as runwasi does. |
| H10 | A call of `wasi:sockets` in an instance must fail with the error code that runwasi gives. Phase 0 must record this code. |
| H11 | The response must go to the client with its status, its headers and its body. The body must go in parts, not as one block. |

### 7.3 Client requests

| ID | Requirement |
| --- | --- |
| O1 | `wasi:http/outgoing-handler` of a proxy component must send its requests through `httpClientFor`. Service names, ClusterIPs, NetworkPolicy and other nodes then give the same results as for other pods of the tab. |
| O2 | A command component that imports `wasi:http` must fail to start, as on runwasi. The message must give the name of the import that it cannot find. |

### 7.4 TCP

| ID | Requirement |
| --- | --- |
| T1 | An IPv4 TCP socket must bind, listen and accept on the pod IP. A bind to `0.0.0.0` must listen on the pod IP. |
| T2 | An IPv6 socket must accept a bind to `::`. It must receive no IPv4 connections (fact 11). |
| T3 | A bind to `127.0.0.1` must listen on the pod IP. The tab has no loopback for each pod. |
| T4 | After a bind to `127.0.0.1`, `local-address` must give `127.0.0.1`. |
| T5 | A connect must use `dialCluster`. |
| T6 | A connection that NetworkPolicy denies must fail with `timeout`. |
| T7 | A connection to a port without a listener must fail with `connection-refused`. |
| T8 | Each connection must have a send window and a receive window of 256 KiB. A full window must stop the sender until the receiver reads. |
| T9 | `shutdown` must close the receive direction, the send direction or the two directions, as the component asks. |
| T10 | When a container stops, the Go side must close all sockets of the container. |

### 7.5 UDP in the tab

| ID | Requirement |
| --- | --- |
| U1 | The UDP table must use the protocol, the IP and the port as the key. TCP and UDP must be able to use the same port on the same IP. |
| U2 | A UDP socket must bind to the pod IP. A bind to `0.0.0.0` must bind to the pod IP. |
| U3 | A bind to port 0, or a send without a bind, must get an ephemeral port from 32768 to 60999. Each IP has its own range. |
| U4 | Each UDP socket must have a receive queue of 256 KiB. If the queue is full, the tab node must drop the datagram. The sender must get no error. |
| U5 | Each datagram must go through the Service translation and the flow table of section 8.7 before it gets to the receiver. |
| U6 | The tab node must drop a datagram to a pod IP and a port without a socket on the same node. |
| U7 | After such a drop, a connected socket must get `connection-refused` on its next receive or send. Linux gives this result after an ICMP "port unreachable". |
| U8 | A datagram of more than 65,507 bytes must fail with `datagram-too-large` in the Go side. |
| U9 | The boundaries and the source address of a datagram must not change. |
| U10 | `tabnode_net.go` must forward UDP for Services with `protocol: UDP`. One ClusterIP must be able to have the same port for TCP and for UDP. |
| U11 | CoreDNS on the tab must give answers over UDP and over TCP on port 53. |
| U12 | `nslookup` and `nc -u` of the tab must use the UDP table. |

### 7.6 UDP flows

| ID | Requirement |
| --- | --- |
| UF1 | The key of a flow must contain the tuple before the Service translation and the tuple after it. |
| UF2 | A flow with datagrams in one direction only must close after 30 seconds without a datagram. |
| UF3 | A flow with datagrams in the two directions must close after 120 seconds without a datagram. |
| UF4 | When an endpoint goes out of the EndpointSlices of a Service, the tab node must remove all flows to this endpoint. |
| UF5 | NetworkPolicy must examine the first datagram of a flow. Datagrams in both directions of an open flow then go through without a new check. |
| UF6 | After a NetworkPolicy change, the tab node must examine all open flows again, with the delay of the strict mode. It must remove flows that the policy denies now. |
| UF7 | `sessionAffinity: ClientIP` and `internalTrafficPolicy: Local` must give the same results for UDP as for TCP. |

### 7.7 Name lookup

| ID | Requirement |
| --- | --- |
| N1 | `ip-name-lookup` must find names with the search path of the pod and with the same resolver as `nslookup` in a pod of the tab. |
| N2 | A name outside the cluster must fail with `name-unresolvable`. The tab cannot ask a DNS server on the internet. |

### 7.8 UDP between nodes

| ID | Requirement |
| --- | --- |
| D1 | The test images of each phase must give the same results on the tab node and on a Docker node with runwasi. |
| D2 | Between the page and a tab node in a Web Worker, UDP must use one channel `udp` for each node, on the MessagePort of `web/src/channel.js`. |
| D3 | Between the page and a Docker node, UDP must use one WebRTC data channel `udp` for each node, with `ordered: false` and `maxRetransmits: 0`. |
| D4 | Each message on a `udp` channel must start with a binary header of 12 bytes. The header contains the source IPv4 address and port, and the destination IPv4 address and port. The datagram follows the header. |
| D5 | The largest message is then 65,519 bytes. This size must be less than the maximum message size of pion and of the browser (fact 12). Phase 0 must make sure of this for the browser. |
| D6 | The source node must examine the egress policy of the source. |
| D7 | The destination node must make sure that the source IP belongs to the source node (its node IP or its pod CIDR). |
| D8 | The destination node must examine the ingress policy of the destination with its own engine. |
| D9 | The node agent must catch UDP from Docker pods to tab pods with an nftables `tproxy` rule for UDP and an `IP_TRANSPARENT` socket. |
| D10 | The node agent must find the original destination of each datagram with `IP_RECVORIGDSTADDR`. |
| D11 | The node agent must send the answers to a Docker pod from a socket bound to the original destination. The answer then has the tab pod IP as its source. |
| D12 | The node agent must send a datagram from a tab pod to a Docker pod through an `IP_TRANSPARENT` socket. This socket must have the IP and the port of the tab pod. |
| D13 | The node agent must keep flows with the timeouts of UF2 and UF3. |

### 7.9 ICMP echo

| ID | Requirement |
| --- | --- |
| I1 | The `ping` built-in command must get an echo reply from a pod IP or a node IP if NetworkPolicy lets the echo request go through. |
| I2 | The tab node must examine an echo request with the engine of kube-network-policies, with no protocol and with port 0 (fact 14). |
| I3 | The tab node must examine the egress policy of the source and the ingress policy of the destination. |
| I4 | A ClusterIP must give no echo reply. On a real cluster with the iptables mode of kube-proxy, a ClusterIP gives no echo reply. |
| I5 | `README.md` must show that a policy rule with `ports` does not match ICMP. |

## 8 Design

### 8.1 Parts of the system

```text
+------------------+         +-----------------------------------------+
| transpile worker |  result | Go thread                               |
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
                             | wasi:sockets -> sockets.js              |
                             | outgoing-handler -> http.js             |
                             | preview2-shim: cli, clocks, random, io, |
                             |   filesystem                            |
                             +-----------------------------------------+
```

The pod worker opens no network connections. It only moves bytes and
events between the component and the Go side.

### 8.2 Transpile

The Go side does these steps for a component layer:

1. Make the key from the layer digests and the path.
2. Find the key in the cache of `wasi.js`.
3. If the key is not in the cache, send the bytes to the transpile worker.
4. Compile the core modules of the result with `WebAssembly.compile`.
5. Keep the JavaScript and the compiled core modules in the cache.
6. Send the JavaScript and the core modules to the pod worker.

The page starts the transpile worker for the first component. The transpile
worker loads the transpiler of jco at that time. The page does not load it
for pods without components.

The pod worker loads the JavaScript from a `Blob` URL. The JavaScript then
runs on the origin of the page, in the pod worker.

### 8.3 Messages

The pod worker and the Go side use the current message channel of the pod
worker (`wasi.js`). Each message has a `type` and a socket ID. Bytes go as a
transferable `ArrayBuffer`. This transfer makes no copy between the
threads, but Go copies the bytes into its memory and out of it.

| Type | Direction | Contents |
| --- | --- | --- |
| `listen` | worker to Go | socket ID, protocol, address, port, backlog |
| `listening` | Go to worker | socket ID, bound address and port, or an error code |
| `accepted` | Go to worker | listener socket ID, new socket ID, remote address and port |
| `connect` | worker to Go | socket ID, address, port |
| `connected` | Go to worker | socket ID, local address and port, or an error code |
| `data` | both | socket ID, bytes |
| `window` | both | socket ID, number of bytes that the receiver read |
| `datagram` | both | socket ID, remote address and port, bytes |
| `shutdown` | worker to Go | socket ID, receive or send or both |
| `closed` | both | socket ID, error code or none |
| `lookup` | worker to Go | request ID, name |
| `addresses` | Go to worker | request ID, addresses or an error code |
| `request` | Go to worker | request ID, method, URL, headers |
| `response` | worker to Go | request ID, status, headers |
| `body` | both | request ID, bytes, or the end of the body |
| `fetch` | worker to Go | request ID, method, URL, headers of a client request |

### 8.4 Calls that wait

A `wasi:sockets` call that must wait gives a pollable. These functions wait
and must be in the list `asyncMode.val.imports` of jco:

- `wasi:io/poll.poll`
- `pollable.block`
- `input-stream.blocking-read`
- `output-stream.blocking-write-and-flush`
- `output-stream.blocking-flush`
- the read of stdin

A suspended call continues when a message from the Go side changes the state
of a pollable. It also continues when the time of a clock pollable ends.

The pod worker of a component must not use `Atomics.wait`. During
`Atomics.wait`, the worker cannot receive messages. The current module path
with `Atomics.wait` stays only for modules.

### 8.5 Proxy components

The Go side does these steps for a proxy component:

1. Read the port from `WASMTIME_HTTP_PROXY_SOCKET_ADDR` (H1 to H3).
2. Listen on the pod IP and that port with `net/http`.
3. For each request, send a `request` message and the body to the pod
   worker.
4. Send the `response` and the body to the client.

The pod worker does these steps for each `request` message:

1. Instantiate the core modules again, with a new context (H5, H7 to H10).
2. Call `incoming-handler.handle`.
3. Send the `response` message and the body to the Go side.
4. Discard the instance after the response.

JSPI can suspend many instances in one pod worker. One suspended instance
does not stop the others. The compiled core modules are the same for all
instances, so only the memory of an instance is new.

### 8.6 UDP table

`tabnode_udp.go` (new) keeps the UDP sockets of all pods of one tab node.
It gives the same functions to all code of the tab:

- `listenPodUDP(ctx, addr)` gives a `net.PacketConn` on the pod IP.
- `dialClusterUDP(ctx, addr)` gives a connected `net.PacketConn`, with the
  Service translation and the flows of section 8.7.

A send to an IP on another node goes on the `udp` channel of that node
(section 8.8). The UDP table does not use the in-process network of Go.

### 8.7 UDP flows

The flow table is in `tabnode_net.go`, next to the service proxy. A flow
has these items:

- the tuple before the Service translation and the tuple after it
- the endpoint that the service proxy selected
- the result of the NetworkPolicy check
- the time of the last datagram in each direction

The service proxy, NetworkPolicy and the node agent use the same timeouts
(UF2, UF3). The tab node removes flows when an endpoint goes out of the
EndpointSlices (UF4) and when a policy changes (UF6).

### 8.8 UDP between nodes

```text
tab pod -> UDP table -> page -> udp channel -> node agent
        -> IP_TRANSPARENT socket -> Docker pod
Docker pod -> tproxy -> node agent -> udp channel -> page
        -> UDP table -> tab pod
```

The `udp` channel has one message for each datagram (D4). The source node
examines the egress policy (D6). The destination node examines the source and
the ingress policy (D7, D8). Between tab nodes in Web Workers, the channel is
a MessagePort and has no loss (D2). To a Docker node, the channel is a WebRTC
data channel without retransmission (D3). Loss can then occur, as with real
UDP.

### 8.9 Changes to files

| File | Change |
| --- | --- |
| `web/src/transpile.worker.js` (new) | Transpile components with jco (section 8.2). |
| `web/src/wasi.js` | Accept components. Keep the results of the transpile worker in the cache. |
| `web/src/wasi.worker.js` | Run components in JSPI mode. Instantiate proxy components for each request. Send the messages of section 8.3. |
| `web/src/sockets.js` (new) | `wasi:sockets` on the messages of section 8.3. |
| `web/src/http.js` (new) | `wasi:http/outgoing-handler` on the `fetch` message. |
| `web/src/channel.js`, `web/src/nodes.js` | The `udp` channel to tab nodes in Web Workers. |
| `web/package.json` | Add `@bytecodealliance/jco` and `@bytecodealliance/preview2-shim` with a fixed version. |
| `site/build.mjs` | Copy the `.wasm` files of `jco-transpile` next to the bundle, because esbuild does not copy files from `new URL(..., import.meta.url)`. |
| `k8s/tabernetes/tabnode_wasi.go` | The sockets of each container, the proxy server, client requests. Close all sockets when a container stops. |
| `k8s/tabernetes/tabnode_udp.go` (new) | The UDP table (section 8.6). |
| `k8s/tabernetes/tabnode_dial.go` | Add `listenPodUDP` and `dialClusterUDP`. |
| `k8s/tabernetes/tabnode_net.go` | UDP Services and the flow table. |
| `k8s/tabernetes/tabnode_netpol.go`, `tabnode_netpol_strict.go` | Decisions for UDP flows and for ICMP. A check of all flows after a policy change. |
| `k8s/tabernetes/tabnode_coredns.go` | Answers over UDP. |
| `k8s/tabernetes/coreutils.go`, `coreutils_misc.go` | `nslookup` over UDP, `nc -u`, the ICMP echo of `ping`. |
| `k8s/tabernetes/podlink.go` | The `udp` channel and its checks (D4 to D8). |
| `node/agent/podlink.go` | nftables rules for UDP, transparent UDP sockets, flows (D9 to D13). |
| `wasm-images/images.txt`, `wasm-images/build.sh` | Test images in Rust: a command component, an echo server for TCP and one for UDP, and two proxy components. One proxy component sends client requests. |
| `tabernetes/mise.toml`, `.github/workflows/ci.yaml` | Rust with the target `wasm32-wasip2`, for the test images. |
| `README.md` | The new behavior and the new limits. Remove "Not covered: UDP" (line 558). |
| `docs/networking-policy.md` | UDP flows, the `udp` channel and ICMP. |

## 9 Phases

Each phase starts only after all tests of the phase before it give correct
results. The tests of phases 1 to 6 run on the tab node and on a Docker node
(D1). Section 10 gives the tests.

| Phase | Contents |
| --- | --- |
| 0 | Proof of concept in `docs/wasi-sockets-poc/`. Section 9.1 gives its items. |
| 1 | Components without network: C1 to C11. |
| 2 | Proxy components and client requests: H1 to H11, O1, O2. |
| 3 | TCP and name lookup: T1 to T10, N1, N2. |
| 4 | UDP in the tab: U1 to U12, UF1 to UF7. |
| 5 | UDP between nodes: D2 to D13. |
| 6 | ICMP echo: I1 to I5. |

### 9.1 Items of phase 0

The proof of concept must give a result for each of these items:

1. Transpile a command component and a proxy component in a transpile worker
   in Chrome. Measure the time and the size for a component of 1 MB and for
   a component of more than 10 MB.
2. Load the output from a `Blob` URL in a pod worker. Make sure that the
   page has no Content Security Policy that stops this.
3. Find how jco writes the names from the component into the JavaScript.
   Make sure that a name cannot add code.
4. Suspend 16 instances of a proxy component in one pod worker with JSPI.
   Make sure that the instances continue in a different sequence.
5. Bind TCP and UDP on the same port in a prototype of the UDP table.
6. Run the TCP echo image, the UDP echo image and the proxy image on a
   Docker node with runwasi v0.6.1. Record the results.
7. On the Docker node, record the error code of `wasi:sockets` in a proxy
   component (H10). Record the status of a command component that imports
   `wasi:http` (O2).
8. Send a message of 65,519 bytes on an unordered WebRTC data channel from
   the browser to pion.

## 10 Tests

| Requirement | Test | Nodes |
| --- | --- | --- |
| C1, C4 | A Rust command component writes arguments, environment and a file of a volume to stdout, reads stdin and stops with exit code 3. | tab, Docker |
| C2, C3 | A component without the two exports fails with `CreateContainerError`. | tab |
| C5 | A component that uses more memory than `limits.memory` gets `OOMKilled`. | tab |
| C6, C7 | Two pods of the same image cause one transpile. The terminal accepts keys during the transpile of a large component. | tab |
| C8 | A browser without JSPI (a test with JSPI removed) gives `CreateContainerError` with the cause. | tab |
| C9 | A component with a WASI 0.3 import fails with the cause. | tab |
| C10 | All current tests of `tabnode_wasi_test.go` give correct results. | tab |
| H1, H2, H11 | `kubectl expose` and `wget -qO- <service>` get the response. A readiness probe with `httpGet` gives a correct result. | tab, Docker |
| H3, H4 | A pod with an incorrect address listens on 8080. The component writes its environment into the response, without the two variables. | tab, Docker |
| H5, H6, H7 | 20 requests at the same time to a component with a delay of 1 second. All responses come in approximately 2 seconds, each with a different `REQUEST_ID`. | tab, Docker |
| H8, H9, H10 | The component writes its arguments and the result of a `wasi:sockets` call into the response. stdout is empty in `kubectl logs`. | tab, Docker |
| O1 | A proxy component gets a response from another Service by its name. A NetworkPolicy that denies it gives an error. | tab, Docker |
| O2 | A command component that imports `wasi:http` fails to start. | tab, Docker |
| T1, T5 | The TCP echo pod gives answers through a Service. | tab, Docker |
| T2 | A bind to `::` gives no error. A connection to the pod IP gets `connection-refused`. | tab, Docker |
| T3, T4 | A bind to `127.0.0.1` gives `127.0.0.1` as `local-address`. | tab |
| T6, T7 | A denied client gets `timeout`. A client to a closed port gets `connection-refused`. | tab, Docker |
| T8 | A client sends 10 MB to a server that reads slowly. The memory of the Go side stays below 1 MB for this connection. | tab |
| T9 | The client closes its send direction. The server reads the end of the stream and sends its answer. | tab, Docker |
| T10 | `kubectl delete pod` of the server. The client gets the end of its connection. | tab |
| N1, N2 | A component finds a Service by its short name. `example.com` gives `name-unresolvable`. | tab |
| U1, U10, U11 | The kube-dns Service has 53/UDP and 53/TCP. `nslookup` over UDP and over TCP get answers from CoreDNS on the tab. | tab |
| U2, U3, U9 | The UDP echo pod gives answers through a Service, with the correct source address. A client without a bind gets a port of 32768 or more. | tab, Docker |
| U4 | A burst of 1,000 datagrams to a server that does not read. The sender gets no error. The server receives the first 256 KiB. | tab |
| U6, U7 | A connected client sends to a port without a socket. The next receive gives `connection-refused`. | tab |
| U8 | A datagram of 65,508 bytes fails with `datagram-too-large`. | tab, Docker |
| U12 | `nc -u` sends a line to the UDP echo pod and receives it. | tab |
| UF1, UF5 | A client sends to a Service IP. The answers come with the Service IP as their source. | tab |
| UF2, UF3 | A flow without answers is gone after 30 seconds. A flow with answers stays for 120 seconds. | tab |
| UF4 | Scale the UDP Deployment from 2 to 1. A client that sends one datagram each second moves to the other endpoint. | tab |
| UF6 | A new policy denies an open flow. The next datagram does not arrive. | tab |
| UF7 | With `sessionAffinity: ClientIP`, 10 datagrams from one client go to one endpoint. | tab |
| D2 | UDP between pods on two tab nodes in Web Workers. | tab |
| D3, D4, D5 | A tab pod sends a datagram of 65,507 bytes to a Docker pod. | tab, Docker |
| D6, D7, D8 | A denied tab pod and a denied Docker pod get no answers. The destination node drops a datagram with a false source IP. | tab, Docker |
| D9, D10, D11 | A Docker pod sends to a tab pod and gets an answer from the tab pod IP. | Docker |
| D12, D13 | A tab pod sends to a Docker pod and gets an answer. | tab, Docker |
| I1, I2, I3 | `ping` to a pod IP gets echo replies. A policy without ICMP for the pod stops them. | tab |
| I4 | `ping` to a ClusterIP gets no echo reply. | tab |
| I5 | The text is in `README.md`. | none |

## 11 Risks

| Risk | Effect | Mitigation |
| --- | --- | --- |
| The transpiler is large (fact 9). | The first component pod loads approximately 2.5 MB more. | The transpile worker loads it only for the first component. The mirror can transpile its images before publication. |
| The transpile of a large component is slow. | The pod starts late. | The transpile worker keeps the Go thread free (C7). Phase 0 measures the time. |
| The output of jco is JavaScript with names from the component. | A bad name can put code into the page origin. | C11 keeps this code in the pod worker. Phase 0 examines the names (item 3). |
| A Content Security Policy can stop code from a `Blob` URL. | Components cannot run. | The page sets no Content Security Policy now. Phase 0 examines this (item 2). |
| esbuild does not copy the `.wasm` files of `jco-transpile`. | The transpile worker cannot load the transpiler. | `site/build.mjs` copies the files. |
| Go cannot compile components (fact 6). | Our current test images in Go cannot use sockets. | The new test images are in Rust. `mise.toml` and CI get Rust. |
| A bind to `127.0.0.1` listens on the pod IP (T3). | Other pods can connect to a port that is local on a real node. | `README.md` shows this limit. A fix is a loopback for each pod. This fix is out of scope. |
| H6 sets a maximum of 16 instances. runwasi has no maximum. | Under a high load, the tab gives slower responses than a Docker node. | `README.md` shows this limit. |
| Each instance of a proxy component has its own memory. | Many requests at the same time use much memory. | H6 and C5. |
| H9 discards the logs of proxy components, as runwasi does. | Users do not see the output of their component. | `README.md` shows this behavior. |
| JSPI is not in all browsers (fact 13). | Some browsers cannot run components. | C8 gives a clear message. Modules continue to run. |
| All sockets of pods go through the Go thread. | Much network traffic of pods makes the control plane slower. This is the same argument as in the rejection of a TCP/IP stack in section 12, but only for traffic of pods. | The windows of T8 and the queues of U4 set a limit to the memory. A later design can move bytes between workers with shared memory. |
| UDP in the tab has no loss before a queue is full. | Programs do not see the loss of a real network in the tab. | `README.md` shows this behavior. Between the page and a Docker node, loss can occur (D3). |
| WASI 0.3 comes. jco-transpile already uses `preview3-shim`. | New components do not run on the tab. | C9 gives a clear message. A later design can add WASI 0.3. |

## 12 Alternatives

| Alternative | Result | Reason |
| --- | --- | --- |
| Listen sockets for modules (WASI preview 1, `sock_accept`). | Rejected | runwasi gives no sockets to modules (fact 2). The same manifest then does not give the same results on a Docker node. |
| The socket extension of WasmEdge. | Rejected | Our Docker node runs the wasmtime shim. A second shim makes the node image larger. Few toolchains use this extension. |
| WASIX with `@wasmer/sdk` (fact 16). | Rejected | It must have cross-origin isolation. Its network does not connect to our in-process network. Services cannot connect to servers in a pod. runwasi does not run WASIX with the wasmtime shim. |
| A user-space TCP/IP stack (for example the netstack of gVisor) for IP packets. | Rejected | With this stack, all connections of the tab go through it on the Go thread. This also includes the connections of the control plane. The full cluster is then slower, for a small number of tools. A Docker node already has a real kernel. |
| ICMP sockets in WebAssembly pods. | Rejected | WASI has no ICMP. Real images in WebAssembly pods do not have `ping`. |
| UDP through the in-process network of Go. | Rejected | TCP and UDP cannot use the same port (fact 10). The queue drops datagrams at the sender, and no policy examines them. |
| A change to the `net` package of Go. | Rejected | The project then must keep a patch of the standard library for each Go version. The UDP table is in our own code. |
| One instance of a proxy component for all requests. | Rejected | runwasi makes a new instance for each request (fact 4). A component with state in global variables then gives different results. |
| A transpile on the Go thread. | Rejected | A transpile of a large component stops the control plane for the full time. |
| A transpile at build time only. | Partly accepted | It makes the start faster for the images of our mirror. Images from other registries come when a pod starts, so the transpile worker is necessary. |

## 13 Open questions

1. How long is the transpile of a component of more than 10 MB? Phase 0
   measures the time. The target is less than 10 seconds in Chrome on a
   laptop.
2. Is 16 the correct maximum of instances for H6? Phase 2 measures the
   memory for each instance.
3. Is JSPI in Safari? The author did not find a primary source.
4. After phase 5, Docker pods can get UDP answers from CoreDNS on the tab.
   Can the move of CoreDNS to a Docker node then stop? A later design must
   decide this.

## 14 Sources

All sources were read on 2026-11-13.

- runwasi v0.6.1, `crates/containerd-shim-wasmtime/src/instance.rs` and
  `http_proxy.rs`:
  <https://github.com/containerd/runwasi/tree/containerd-shim-wasmtime/v0.6.1/crates/containerd-shim-wasmtime/src>
- wasmtime `main`, `src/common.rs` and `crates/cli-flags/src/lib.rs`:
  <https://github.com/bytecodealliance/wasmtime>
- Go `src/internal/syslist/syslist.go` and `src/net/net_fake.go`:
  <https://github.com/golang/go/tree/go1.27.3/src>
- jco 1.42.0, jco-transpile 0.24.0 and preview2-shim 0.31.1 on npm:
  <https://www.npmjs.com/package/@bytecodealliance/jco>
- `wasi:sockets` `wit/tcp.wit`:
  <https://github.com/WebAssembly/wasi-sockets/blob/main/wit/tcp.wit>
- pion/sctp `association.go`:
  <https://github.com/pion/sctp/blob/master/association.go>
- JSPI in browsers:
  <https://web-platform-dx.github.io/web-features-explorer/features/wasm-jspi>
  and <https://groups.google.com/a/mozilla.org/g/dev-platform/c/8cOxM-dWaNs>
- kube-network-policies, `pkg/network/packet.go` and
  `pkg/dataplane/controller.go`:
  <https://github.com/kubernetes-sigs/kube-network-policies>
- Linux `Documentation/networking/nf_conntrack-sysctl.rst` and
  `ip-sysctl.rst`:
  <https://docs.kernel.org/networking/nf_conntrack-sysctl.html>
- [`@wasmer/sdk` 0.19.1 on npm](https://www.npmjs.com/package/@wasmer/sdk)
