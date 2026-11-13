# Sockets for WebAssembly pods

Status: proposal. No part of this design is in the code.

Language: this document obeys the rules of ASD-STE100 Simplified
Technical English. The names of code, Kubernetes objects, protocols and
standards are technical names. The words "must", "must not" and "can" have
these meanings:

- "must" and "must not" give a requirement.
- "can" gives a possibility, not a requirement.

## 1 Purpose

WebAssembly pods on the tab node cannot open sockets. A server in such a pod
cannot receive connections. A client in such a pod cannot connect to a
Service. This document gives a design that adds TCP and UDP sockets to these
pods.

The same pod manifest must operate the same on the tab node and on a Docker
node. This rule controls most decisions in this document.

## 2 Scope

These items are in the scope of this document:

- WASI preview 2 components on the tab node (the `wasi:cli/command` world).
- Components of the `wasi:http/proxy` world, as runwasi serves them.
- TCP and UDP sockets of `wasi:sockets`, and `wasi:sockets/ip-name-lookup`.
- UDP in the in-process network of the tab: Services, NetworkPolicy and
  cluster DNS.
- An ICMP echo reply for the `ping` built-in command of the terminal.

These items are not in the scope of this document:

- Sockets for WASI preview 1 modules. Section 11 gives the reason.
- Threads, `fork` and `exec` in WebAssembly pods.
- Raw sockets, IP packets, `tcpdump` and `traceroute` on the tab node.
- Connections from a pod to the internet.
- Browsers without JavaScript Promise Integration (JSPI).

## 3 Terms

| Term | Definition |
| --- | --- |
| tab node | The kubelet in the Wasm binary (`k8s/tabernetes/tabnode*.go`). |
| WebAssembly pod | A pod with a runtime class from `wasmRuntimeClasses` (`tabnode_wasi.go`), for example `wasmtime`. |
| module | A WASI preview 1 core module (`wasm32-wasip1`, `GOOS=wasip1`). |
| component | A WASI preview 2 component (`wasm32-wasip2`). |
| pod worker | The Web Worker that runs one container of a WebAssembly pod (`web/src/wasi.worker.js`). |
| Go side | The code of the tab node that starts the pod worker and owns the sockets of the pod in the in-process network. |
| in-process network | The network of Go for `js/wasm`. `net.Listen` and `net.Dial` connect goroutines of the same binary. |
| socket ID | A number that the Go side gives to one socket of one pod worker. |
| flow | The state of one UDP conversation between two address and port pairs. A flow has a timeout. |

## 4 Current condition

- The pod worker runs modules with `@bjorn3/browser_wasi_shim`. It has no
  socket functions. The section "Limits" of `README.md` gives this gap.
- `web/src/wasi.js` refuses components. The pod gets an "unsupported"
  message.
- The pod worker uses JSPI. `fd_read` on stdin and `poll_oneoff` wait for a
  message without a busy loop. The page does not need cross-origin
  isolation.
- The in-process network of Go has UDP. `net_fake.go` in the Go source
  accepts `udp`, `udp4` and `udp6`. The tab node does not use this UDP.
- `tabnode_net.go` (the kube-proxy and the DNS of the tab) forwards TCP only.
  CoreDNS on the tab answers over TCP only.
- NetworkPolicy on the tab node examines TCP connections in `dialCluster`
  (`tabnode_dial.go`). Rules for UDP have no effect.
- On a Docker node, runwasi (`containerd-shim-wasmtime` v0.6.1) runs
  WebAssembly pods. `podlink.go` of the node agent carries TCP from Docker
  pods to tab pods. It does not carry UDP.

## 5 Facts that control the design

The author examined these facts in the sources on 2026-11-13.

1. runwasi v0.6.1 gives a component the full network of the pod.
   `wasi_builder` in `crates/containerd-shim-wasmtime/src/instance.rs` calls
   `inherit_network()`, `allow_tcp(true)`, `allow_udp(true)` and
   `allow_ip_name_lookup(true)`.
2. runwasi gives a module no sockets. It runs a module with `build_p1()` and
   gives no listen socket to the module.
3. The current wasmtime gives no listen sockets to modules. The help text of
   `--tcplisten` says "legacy wasip1 implementation only". `src/common.rs`
   stops with "components do not support --tcplisten".
4. runwasi serves a component of the `wasi:http/proxy` world itself. The
   environment variable `WASMTIME_HTTP_PROXY_SOCKET_ADDR` gives the address.
   The default address is `0.0.0.0:8080`.
5. Go cannot compile a component. Go 1.27.3 (the version in
   `tabernetes/mise.toml`) and the `master` branch know `wasip1` only. Rust
   (`wasm32-wasip2`) and TinyGo can compile components.
6. jco 1.42.0 changes a component into core modules and JavaScript. It has
   an export for the browser (`@bytecodealliance/jco/component`). It has the
   option `asyncMode: "jspi"`.
7. The browser shim of jco (`@bytecodealliance/preview2-shim` 0.31.1) has no
   real sockets. All functions of `dist/browser/sockets.js` throw
   `not-supported`. The shim has in-memory sockets, but these sockets do not
   connect to our network.
8. The transpiler of jco for the browser is large. The core module
   `js-component-bindgen-component.core.wasm` has 5.7 MB, and 1.9 MB with
   gzip.
9. `@wasmer/sdk` 0.19.1 runs WASIX programs. It needs cross-origin
   isolation. Its network is a WISP connection to a server, and HTTP to
   listeners through its own service worker. It has no public interface for
   a different network.

## 6 Decisions

1. WebAssembly pods get sockets through WASI preview 2 only. Facts 1, 2
   and 3 show that only components have sockets on a Docker node.
2. The pod worker runs components with jco in JSPI mode. Our own code gives
   the `wasi:sockets` interfaces. The shim of jco gives the other
   interfaces.
3. The Go side owns every socket. A socket of a pod is a `net.Listener`,
   `net.Conn` or `net.PacketConn` in the in-process network, with the pod IP
   as the address. Services, cluster DNS, NetworkPolicy, probes,
   `kubectl port-forward` and `podlink` then operate without a change.
4. The tab node serves `wasi:http/proxy` components with `net/http` of Go,
   on the same address as runwasi (fact 4).
5. The in-process network gets UDP for all pods, not only for WebAssembly
   pods.
6. The tab node does not get IP packets. An ICMP echo reply is a function of
   the in-process network, not a socket.

## 7 Requirements

### 7.1 Components

| ID | Requirement |
| --- | --- |
| C1 | The tab node must run a component of the `wasi:cli/command` world in a pod worker. |
| C2 | The arguments, the environment, stdout, stderr, stdin, the exit code and the volumes of a component must agree with the same items of a module. |
| C3 | The pod worker must change a component into JavaScript one time for each layer digest. Other pods and restarts of the same image must use the result again. |
| C4 | If the browser has no JSPI, the pod must fail with `CreateContainerError` and a message that gives the cause. |
| C5 | A module must operate as before. A module gets no socket functions. |

### 7.2 HTTP proxy world

| ID | Requirement |
| --- | --- |
| H1 | The tab node must serve a component of the `wasi:http/proxy` world on the pod IP. |
| H2 | The port must come from `WASMTIME_HTTP_PROXY_SOCKET_ADDR`. If this variable is not in the environment, the port is 8080. The tab node must remove this variable before it gives the environment to the component, as runwasi does. |
| H3 | Each HTTP request must start one call of the `incoming-handler` export. The response must go back to the client with its status, headers and body. |

### 7.3 TCP

| ID | Requirement |
| --- | --- |
| T1 | A component must bind, listen and accept on the pod IP. A bind to `0.0.0.0` or `::` must listen on the pod IP. |
| T2 | A bind to `127.0.0.1` must listen on the pod IP. The tab has no loopback for each pod (see `README.md`). Section 10 gives the risk. |
| T3 | A connect must use `dialCluster`. Service IPs, NetworkPolicy and the connections to other nodes then operate as for other pods of the tab. |
| T4 | A connection that NetworkPolicy denies must fail with `timeout`. A port without a listener must fail with `connection-refused`. |
| T5 | Each connection must have a send window and a receive window of 256 KiB. A full window must stop the sender until the receiver reads. |
| T6 | `shutdown` must close one direction or two directions, as the component asks. |
| T7 | When the container stops, the Go side must close all sockets of the container. |

### 7.4 UDP

| ID | Requirement |
| --- | --- |
| U1 | A component must bind a UDP socket, send datagrams and receive datagrams. The boundaries of a datagram must not change. |
| U2 | The service proxy (`tabnode_net.go`) must forward UDP for Services with `protocol: UDP`. |
| U3 | The service proxy must keep a flow for each UDP conversation. The reply of an endpoint must go back to the client with the Service IP as the source. A flow must close after 30 seconds without a datagram. |
| U4 | `sessionAffinity: ClientIP` and `internalTrafficPolicy: Local` must operate for UDP as for TCP. |
| U5 | NetworkPolicy must examine the first datagram of a flow. A reply in an open flow must pass, as with conntrack on a real node. |
| U6 | CoreDNS on the tab must answer over UDP and over TCP. |
| U7 | A datagram larger than 65507 bytes must fail with `datagram-too-large`. |

### 7.5 Name lookup

| ID | Requirement |
| --- | --- |
| N1 | `ip-name-lookup` must find names with the search path of the pod and with the same resolver as `nslookup` in a pod of the tab. |
| N2 | A name outside the cluster must fail with `name-unresolvable`. The tab cannot ask a DNS server on the internet. |

### 7.6 Docker nodes and other nodes

| ID | Requirement |
| --- | --- |
| D1 | The same manifest must run on a Docker node with runwasi, without a change. |
| D2 | UDP between nodes must use a WebRTC data channel with `ordered: false` and `maxRetransmits: 0`. Loss and a change of sequence can then occur, as with real UDP. |
| D3 | `podlink.go` must carry UDP from Docker pods to tab pods, with TPROXY and `IP_TRANSPARENT`, as it does for TCP. |

### 7.7 ICMP echo

| ID | Requirement |
| --- | --- |
| I1 | The `ping` built-in command must get a reply from a pod IP or a node IP. NetworkPolicy must let the traffic pass. |
| I2 | A ClusterIP must not reply. On a real cluster with the iptables mode of kube-proxy, a ClusterIP does not reply. |
| I3 | The NetworkPolicy API has no field for ICMP. The tab node must use the behavior of Calico: a pod that has a policy for ingress drops ICMP from peers that the policy does not let in. `README.md` must give this behavior. |

## 8 Design

### 8.1 Components of the system

```text
pod worker (one for each container)        Wasm binary (Go side)
+-------------------------------+          +-------------------------------+
| component, changed by jco     |          | tabnode_wasi.go               |
|   wasi:sockets/*  -----+      |          |   socket table of the pod     |
|                        |      | messages |     net.Listener on pod IP    |
| sockets.js (new) <-----+------+--------->|     net.Conn (dialCluster)    |
|   waits with JSPI             |          |     net.PacketConn (UDP)      |
| jco preview2-shim             |          | tabnode_net.go                |
|   cli, clocks, random, io,    |          |   service proxy: TCP and UDP  |
|   filesystem                  |          |   UDP flows, NetworkPolicy    |
+-------------------------------+          +-------------------------------+
```

The pod worker does not open network connections. All sockets are in the Go
side. The pod worker only moves bytes and events.

### 8.2 Messages

The pod worker and the Go side use the current message channel of the pod
worker (`wasi.js`). Each message has a `type` and a socket ID. Bytes go as a
transferable `ArrayBuffer`, not as a copy.

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

### 8.3 Waits

A `wasi:sockets` call that must wait returns a pollable. `wasi:io/poll`
waits for the pollables. In JSPI mode, the pod worker wraps `poll` in
`WebAssembly.Suspending`. The pod worker continues when one of these events
occurs:

- a message from the Go side changes the state of a pollable
- the time of a clock pollable ends

This is the same method as the current stdin of `wasi.worker.js`.

### 8.4 HTTP proxy world

The Go side does these steps for a component of the `wasi:http/proxy` world:

1. It reads the port from `WASMTIME_HTTP_PROXY_SOCKET_ADDR`, or it uses 8080.
2. It listens on the pod IP and that port with `net/http`.
3. For each request, it sends the method, the URL, the headers and the body
   to the pod worker.
4. The pod worker calls `incoming-handler.handle` and sends the response
   back in parts.
5. The Go side writes the response to the client.

The pod worker must start the next call only after the current call stops.
The component model does not let a second call into an instance while the
first call waits. Phase 0 must examine this behavior with jco. Section 10
gives the risk.

### 8.5 UDP flows

The flow table is in `tabnode_net.go`. The key of a flow is the protocol and
the two address and port pairs. The table keeps these items:

- the endpoint that the service proxy selected
- the result of the NetworkPolicy check
- the time of the last datagram

A flow closes after 30 seconds without a datagram, the same timeout as
`nf_conntrack_udp_timeout` on Linux. The service proxy and NetworkPolicy use
the same table.

### 8.6 Changes to files

| File | Change |
| --- | --- |
| `web/src/wasi.js` | Accept components. Change them with jco and keep the result for each digest. |
| `web/src/wasi.worker.js` | Run components in JSPI mode. Send the messages of section 8.2. |
| `web/src/sockets.js` (new) | The `wasi:sockets` interfaces on the messages of section 8.2. |
| `web/package.json` | Add `@bytecodealliance/jco` and `@bytecodealliance/preview2-shim` with a fixed version. |
| `k8s/tabernetes/tabnode_wasi.go` | The socket table of each container, the HTTP proxy world, the close of all sockets when the container stops. |
| `k8s/tabernetes/tabnode_net.go` | UDP Services, the flow table, CoreDNS over UDP, the ICMP echo reply. |
| `k8s/tabernetes/tabnode_netpol.go` | Decisions for UDP flows and for ICMP. |
| `node/agent/podlink.go` | UDP with TPROXY. |
| `wasm-images/` | Test images: a TCP echo server, a UDP echo server and an HTTP proxy component, in Rust (`wasm32-wasip2`). |
| `README.md` | The new behavior and the new limits. |

## 9 Phases

Each phase must pass its tests before the next phase starts.

| Phase | Contents | Tests |
| --- | --- | --- |
| 0 | Proof of concept in `docs/wasi-sockets-poc/`. Change a Rust TCP echo component with jco in the browser, in JSPI mode, with a stub of `wasi:sockets`. Measure the time to change the component and the size. | The echo component answers. The same image answers on a Docker node with runwasi. |
| 1 | Components without network (C1 to C5). | Current WASI tests pass. A Rust "hello" component writes to the log and stops with exit code 0. |
| 2 | HTTP proxy world (H1 to H3). | `kubectl expose` and `wget -qO- <service>` get the response. A readiness probe with `httpGet` passes. |
| 3 | TCP (T1 to T7) and name lookup (N1, N2). | The TCP echo pod answers through a Service. A NetworkPolicy that denies the client gives a timeout. `kubectl port-forward` operates. |
| 4 | UDP (U1 to U7, D2, D3). | The UDP echo pod answers through a Service. `nslookup` over UDP gets an answer from CoreDNS on the tab. A Docker pod gets a UDP answer from a tab pod. |
| 5 | ICMP echo (I1 to I3). | `ping` to a pod IP gets replies. `ping` to a ClusterIP gets no reply. |

## 10 Risks

| Risk | Effect | Mitigation |
| --- | --- | --- |
| The transpiler of jco is large (fact 8). | The first component pod loads 1.9 MB more. | Load the transpiler only for the first component. Keep the result for each digest. As an alternative, change mirror images in `wasm-images/build.sh` before publication. |
| The output of jco is JavaScript. | The pod worker must load code from a `Blob` URL. A strict Content Security Policy can stop this. | The page sets no Content Security Policy now. Do a check in phase 0. |
| Go cannot compile components (fact 5). | Our current test images in Go cannot use sockets. | Write the test images in Rust. Keep the Go images as modules. |
| A bind to `127.0.0.1` listens on the pod IP (T2). | Other pods can connect to a port that is local on a real node. | Write this gap in `README.md`. A fix needs a loopback for each pod. This fix is out of scope. |
| One call at a time in the HTTP proxy world. | A slow request stops all other requests to the pod. | Write this limit in `README.md`. |
| JSPI is only in Chromium-based browsers by default. | Other browsers cannot run components. | C4 gives a clear message. Modules continue to operate. |
| Each chunk of data is one message between threads. | The throughput is low. | The windows of T5 limit the memory. A ring buffer in shared memory can be a later step. |
| UDP in the tab has no loss. | Programs do not see the behavior of a real network. | Write this gap in `README.md`. D2 gives loss between nodes. |

## 11 Rejected alternatives

| Alternative | Reason |
| --- | --- |
| Listen sockets for modules (WASI preview 1, `sock_accept`). | runwasi and the current wasmtime give no listen sockets to modules (facts 2 and 3). Then the same manifest does not operate on a Docker node. |
| The socket extension of WasmEdge. | Our Docker node runs the wasmtime shim. A second shim makes the node image larger. Few toolchains use this extension. |
| WASIX with `@wasmer/sdk` (fact 9). | It needs cross-origin isolation. Its network does not connect to our in-process network. Servers in a pod cannot get raw TCP connections from Services. runwasi does not run WASIX with the wasmtime shim. |
| A user-space TCP/IP stack (for example the netstack of gVisor) for IP packets. | With this stack, all connections of the tab go through it on one thread, also the connections of the control plane. Then the full cluster is slower, for a small number of tools. A Docker node already has a real kernel. |
| ICMP sockets in WebAssembly pods. | The WASI standard has no ICMP. Real images in WebAssembly pods do not have `ping`. |

## 12 Open questions

1. Can the browser transpiler of jco change a large component (more than
   10 MB) in an acceptable time? Phase 0 must measure this.
2. Does the in-memory UDP of `net_fake.go` keep the boundaries of datagrams
   and the source address? Phase 4 must test this before the design of the
   flow table is final.
3. Must the tab node also serve `wasi:http/proxy` for modules with an
   adapter? runwasi does not do this, so the answer is "no" for now.
