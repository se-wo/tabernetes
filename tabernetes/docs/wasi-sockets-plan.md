# Work packages for WASI sockets

This file cuts phases 1 to 6 of `wasi-sockets.md` into work packages. One
coding session does one package. Section 9 of `wasi-sockets.md` stays the
rule: a phase starts only after the tests of the phase before it give
correct results. Inside a phase, the packages in the column "After" give the
order. Packages with the same predecessors can run at the same time.

## Rules for each package

1. Start from the latest `main`. Read `wasi-sockets.md` (sections 7 to 10
   and the rows of your requirements) and this file.
2. Change only the files that your package owns. A file of another package
   gets a change only where the column "Shared" of your row says so.
3. For each requirement of your package, write the test of section 10.3
   for the nodes that your package can reach. A test that needs a later
   package stays for that package. Name it in your pull request.
4. Use the message names and fields of `web/src/wasi_messages.js` and
   `k8s/tabernetes/tabnode_wasi_msg.go` (package W1.4a). A new message or
   field goes into both files in the same pull request.
5. Tests that wait 30 seconds or more of real time use a clock that the
   test controls (CR8, CR11, TC10, PX23, FL2, FL3).
6. Before each push: `mise run test`, the linters of `.github/workflows/ci.yaml`
   (super-linter: markdownlint with a limit of 400 characters for each
   line, textlint, shellcheck, shfmt, actionlint, zizmor) on the files you
   changed.
7. If the design has a gap or a contradiction, make the smallest decision
   that works, write it into `wasi-sockets.md` in ASD-STE100, as the
   rest of that file, and name it in the pull request.
8. Open a pull request against `main`. When all checks are green and the
   tests of your package pass, merge it with a merge commit. Do not disable
   or skip a test to get a green check.
9. Do not write names or IDs of AI models into the repository.

## Packages

| ID | Phase | Requirements | Owns | Shared | After | Size |
| --- | --- | --- | --- | --- | --- | --- |
| W1.1 | 1 | Test images of section 10.1, the Docker check in CI | `wasm-images/` (Rust images for phases 1 to 4, from the components of phase 0), `images.txt`, `docs/docker-node-check/` (image import, one script and one manifest file for each `wasi-*` step), `.github/workflows/docker-check.yaml` (new) | `mise.toml`: Rust in `[tools]`. `ci.yaml`: Rust. | none | L |
| W1.2 | 1 | Test harness for "tab" and "worker" | `web/test/` (new): unit tests of the JavaScript with `node --test`. An end-to-end job in CI that starts the page with `browser.mjs` and runs commands with `tab.mjs`, on the tab node and on a node worker. | `mise.toml`: a task `test-web`. `ci.yaml`: the new jobs. | none | M |
| W1.3 | 1 | CO8 to CO10, CO14 to CO16 | `web/src/transpile.worker.js` (new), the cache in `web/src/wasi.js` | `web/package.json`, `mise.toml` (task `web`), `site/build.mjs` | none | L |
| W1.4a | 1 | CO1, CO5 (arguments, environment, stdout, stderr), the exit codes of CO18, CO13, MS1, MS6 | `web/src/wasi_messages.js`, `k8s/tabernetes/tabnode_wasi_msg.go` (new, with dispatchers), the transport, `component.worker.js` (from `data:`), `wasi:cli/run` | `wasi.js`, `wasi.worker.js`, `tabnode_wasi.go`, `mise.toml` (task `web`), `site/build.mjs` | W1.2, W1.3 (end-to-end: W1.1) | L |
| W1.4b | 1 | CO2 to CO4, CO11, CO12, CO17 and CO19 (tests in W3.3b), CR6, the rest of CO5 and CO18, MS1 and MS6 for new messages | the selection of the world, the statuses of runwasi, stdin and memory of components, `web/src/sockets.js` (a stub that denies, and the count of CO17) | the files of W1.4a | W1.4a, W1.1 | L |
| W1.5 | 1 | FS1 to FS4, CO6, CO7, AC2 | `web/src/files.js` (new), the `cpu` message in `tabnode_acct.go` | `component.js` (start of the adapter), `README.md` (phase 1) | W1.4 | M |
| W2.1a | 2 | PX1 to PX4, PX6, PX16 to PX20, PX23, PX26 to PX33, AC1 (proxy server) | `k8s/tabernetes/tabnode_wasi_proxy.go` (new), the proxy world in `componentStatus`, `startComponent` and `pullWasmOnce` of `tabnode_wasi.go` | `tabnode_wasi_msg.go` | phase 1 | L |
| W2.1b | 2 | CR1 to CR8, CR10, CR11, MS5, AC1 (client requests) | `k8s/tabernetes/tabnode_wasi_fetch.go` (new), a client for components in `tabnode_dial.go`, `synTimeout` as a variable for tests | `tabnode_wasi_msg.go`, `tabnode_netpol.go` (only `synTimeout`) | phase 1 | M |
| W2.2 | 2 | PX7 to PX15, PX21, PX22, PX24 to PX27, PX34, CR9 | `web/src/http.js` (new, from `http-own.js` of phase 0), the proxy path of `component.js` and `component.worker.js` | `wasi_messages.js` | phase 1 | L |
| W2.3a | 2 | All PX and CR tests end-to-end, the CO2 test of `bothrev` | step `wasi-proxy`, the end-to-end tests of PX and CR, `README.md` (phase 2) | none | W2.1a, W2.1b, W2.2 | M |
| W2.3b | 2 | Open question 2 (PX9, PX34) | `web/test/e2e/wasi-memory.test.mjs` (new) | `wasi-sockets.md` (sections 8.6, 11, 12, 14), `component.js` (PX34 in the proxy path), `proxy.test.mjs` (tests of PX34), `README.md` (one sentence after W2.3a) | W2.1a, W2.1b, W2.2 | S |
| W2.4 | 2 | The Wasm memory of the tab (sections 12 and 14) | `web/src/memory.js` (new), see below | `wasi_messages.js`, `tabnode_wasi_msg.go` (rule 4), `component.js`, `wasi.js`, `nodes.js`, `node.worker.js`, `wasi.worker.js` | W2.3b | L |
| W3.1a | 3 | TC2 to TC4, TC9, TC15, TC16, PX5 (Go) | `tabnode_loopback.go` (new); in `tabnode_dial.go` the table of ports in use, the ephemeral TCP ports and the loopback checks; the loopback checks in `tabnode_net.go`, `tabnode_netpol.go`, `podlink.go` | `tabnode_wasi_proxy.go` (PX5), `tabnode_probe.go`, `tabnode_lifecycle.go`, `tabnode_grpc.go` (a loopback check) | phase 2 | M |
| W3.1b | 3 | TC1, TC7, TC8, TC10 to TC12, TC14, TC18, TC19, NL1 to NL3, MS2 to MS4, AC1 (TCP) | `k8s/tabernetes/tabnode_wasi_tcp.go` (new) | `tabnode_wasi_msg.go` | phase 2, merges after W3.1a | L |
| W3.2 | 3 | TC5, TC6, TC13, TC17, TC20 to TC22, NL in the pod worker | the TCP and lookup part of `web/src/sockets.js` | `wasi_messages.js`, `component.js` (the calls of `createSockets`) | phase 2 | L |
| W3.3a | 3 | The results of runwasi for phase 3, the Docker tests of TC, NL and PX5 | test modes in `wasm-images/rust/echo`, step `wasi-sockets` | the PX5 part of step `wasi-proxy` | phase 2 | M |
| W3.3b | 3 | All TC and NL tests end-to-end, PX5, CO17, CO19 | the end-to-end tests of phase 3, `README.md` (phase 3) | `sockets.js` (`TCP_DEFAULTS`, with the values of W3.3a) | W3.1a, W3.1b, W3.2, W3.3a | M |
| W4.1 | 4 | UD1 to UD6, UD8, UD9, UD14, UD15, UD20, UD21, AC1 (UDP table) | `k8s/tabernetes/tabnode_udp.go` (new): the UDP table, `listenPodUDP`, `dialClusterUDP`, the hook `udpRoute` (section 8.7), with the table of ports in use of W3.1a | `tabnode_dial.go` (a helper for an exclusive port), `tabnode_loopback.go` (UDP rows), `tabnode_wasi_proxy.go` (`listenProxy`, PX32) | phase 3 | M |
| W4.2a | 4 | UD7, UD10 to UD13, UD16, UD17, FL1 to FL5, FL10, FL11, FL13 | `k8s/tabernetes/tabnode_flows.go` (new): the flow table and the hook `udpPolicy` (section 8.8); UDP Services and the events of EndpointSlices in `tabnode_net.go` | `tabnode.go` (`releaseIP`, FL13) | W4.1 | L |
| W4.2b | 4 | FL6 to FL9, FL12 | UDP in `tabnode_netpol.go` and `tabnode_netpol_strict.go` | none | W4.2a | M |
| W4.3 | 4 | UD18, UD19, UD22, AC3 | UDP in `tabnode_coredns.go`, `nslookup` over UDP in `coreutils.go`, `nc -u` in `coreutils_misc.go` | none | W4.1, W4.2a | M |
| W4.4a | 4 | UDP of components on the Go side, MS2 to MS4 (UDP), AC1 (UDP messages) | `k8s/tabernetes/tabnode_wasi_udp.go` (new) | `tabnode_wasi_tcp.go` (the registration in `init()`, `tcpData`) | W4.1 | M |
| W4.4b | 4 | UDP in the pod worker | the UDP part of `web/src/sockets.js` | `sockets.js` (the list of types in `createSocketTable`) | phase 3 | M |
| W4.5a | 4 | The results of runwasi for phase 4, the Docker tests of UD | UDP test modes in `wasm-images/rust/echo`, step `wasi-udp` (in one pod and on one node) | none | phase 3 | M |
| W4.5b | 4 | All UD and FL tests end-to-end, AC1 (UDP, combined) | the end-to-end tests of phase 4, `README.md` (phase 4) | `wasi-proxy-lib.mjs` | W4.1, W4.2a, W4.2b, W4.3, W4.4a, W4.4b, W4.5a | M |
| W5.0 | 5 | The nfqueue item of `docs/networking-policy.md` (phase 0, decision 16) | step `netpol` of the Docker check (new: `netpol.sh`, `netpol.yaml`), phase 0 and the status line of `docs/networking-policy.md` | `check.sh` (the list of steps), `docker-check.yaml` (kernel modules), `wasi-sockets.md` (the risk of nfqueue in section 12) | phase 4 | S |
| W5.1a | 5 | ND2 (the channel), ND3 to ND5, ND7, ND8, ND11, ND12 (the page), ND20 | `k8s/tabernetes/podlink_udp.go` (new): the `udp` channel of the page and of a node worker | `tunnel.go` (`attachChannel`, `joinedNode`), `nodeworker.go` (`prepareWorkerNode`, `forgetNode`, the channel of a node worker) | phase 4 | M |
| W5.1b | 5 | ND2 (end-to-end), ND6, ND9, FL11 (worker) | UDP to and from other nodes in `tabnode_udp.go` and `tabnode_flows.go`, `web/test/e2e/wasi-udp-nodes.test.mjs` (new) | `wasi-proxy-lib.mjs` | phase 4, merges after W5.1a | M |
| W5.2a | 5 | ND3 to ND5, ND12 (the agent), ND19 | `node/agent/udplink.go` (new): the `udp` channel and the flows of the node agent | `node/agent/main.go` (the start of the channel), `node/agent/podlink.go` (one call in `servePodLinks`) | phase 4 | M |
| W5.2b | 5 | ND13 to ND18 | `node/agent/udplink_net.go` (new): the nftables rules and the sockets for UDP | `node/agent/podlink.go` (`podLinkRules`, `socketAs`), `node/agent/podlink_test.go` (the network namespace test) | W5.2a | M |
| W5.3 | 5 | ND1, ND10, the deferred tests, UD19 (phase 5) | step `wasi-udp` (between nodes), `kubeDNSProto` (`tabnode_coredns.go`), the default server of `nslookup` (`coreutils.go`), `docs/networking-policy.md`, `README.md` (phase 5) | the deferred lines of `wasi-proxy.sh` and `wasi-sockets.sh`, the echo image (UDP modes), `docker-check.yaml` (`paths`) | W5.0, W5.1a, W5.1b, W5.2a, W5.2b | L |
| W6.1 | 6 | IC1 to IC8 | ICMP in `tabnode_netpol.go`, `ping` in `coreutils_misc.go`, `web/test/e2e/icmp.test.mjs` (new), `README.md` (phase 6) | `coreutils_misc_test.go` | phase 4 | M |

W3.1a, W3.1b, W3.2 and W3.3a run at the same time. W3.1a merges before
W3.1b. W3.3a records the results of runwasi first, and the parent session
gives them to the other packages. W3.3b starts after these four packages.
Section 8.3.2 of `wasi-sockets.md` gives the rules of the TCP and lookup
messages. Each package of phase 3 changes only the rows of its requirements
in sections 10.3 and 11 of `wasi-sockets.md`.

Phase 4 has these rows in place of W4.2, W4.4 and W4.5 of the first
plan. W4.2 (XL) is W4.2a and W4.2b, W4.4 is W4.4a (Go) and W4.4b (pod
worker), and W4.5 is W4.5a (the results of runwasi, first) and W4.5b (the
end-to-end tests, last). W3.1a already made the table of ports in use and
the ephemeral ports of each IP, so W4.1 has the size M. Section 8.3.3 of
`wasi-sockets.md` gives the rules of the UDP messages. The order:

1. The docs PR of the parent (section 8.3.3, the decisions of phase 4)
   merges first.
2. W4.5a, W4.1, W4.4b and W2.4 start. W4.5a records the results of runwasi
   first, and the parent session gives them to the other packages.
3. W4.1 publishes its API on its branch early and reports it. W4.2a, W4.3
   and W4.4a then start from this branch. They rebase onto `main` after W4.1
   merges. W4.3 merges after W4.2a.
4. W4.2b starts after W4.2a merges.
5. W4.5b starts after all other packages of phase 4.

In phase 4, only W4.5a (the results of runwasi) and W4.5b (the final
results) change the rows of section 10.3. The other packages write their
decisions into sections 8 and 11 and into their pull requests.

Phase 5 has these rows in place of W5.1 and W5.2 of the first plan. W5.1
is W5.1a (the `udp` channel) and W5.1b (the UDP table and the flows
between nodes). W5.2 is W5.2a (the channel and the flows of the node
agent) and W5.2b (the kernel part of the node agent). W5.0 examines
nfqueue on the Docker node first. Section 8.9.1 of `wasi-sockets.md` gives
the rules of the `udp` channel. At most four sessions run at the same
time. The order:

1. The pull request of the parent (section 8.9.1, the hooks in
   `udplink_hooks.go`, these rows) merges first.
2. W5.0, W5.1a, W5.1b and W5.2a start. W5.0 reports the result of nfqueue
   at once.
3. W5.2a publishes its API on its branch early and reports it. W5.2b starts
   from this branch when a session ends (W5.0 first). It merges
   `origin/main` into its branch after W5.2a merges.
4. W5.1b merges after W5.1a. W6.1 starts when W5.1a or W5.2a merged.
5. W5.3 starts after W5.0, W5.1a, W5.1b, W5.2a and W5.2b.

In phase 5, only W5.3 changes the rows of section 10.3. W6.1 changes the
rows of IC. The other packages write their decisions into sections 8 and
11 and into their pull requests.

W2.4 does not stop phase 3. It can run at the same time as phases 3 to 6.
It does these items:

- A broker in the page (`web/src/memory.js`). It counts the Wasm memories
  of all Go threads and pod workers and shares a limit for the tab between
  the containers.
- A request from the page that makes idle pod workers collect their
  garbage. To examine: one new memory in each pod worker. A memory that the
  browser cannot make starts a garbage collection in that worker.
- A status in the terminal and on the Node for a node worker whose Go
  binary gets no memory (`nodes.js`, `node.worker.js`).
- The text of section 8.6 for modules (`wasi.worker.js`).

Phase 5 has only deferred tests. So W6.1 can run at the same time as phase
5. The deferred tests of W5.3 write SKIP until the nfqueue item of
`docs/networking-policy.md` closes.

## Files that more than one package changes

- `tabnode_wasi.go`, `component.js` and `component.worker.js`: W1.4a puts
  the message handling into the dispatchers. Later packages add handlers in files of their own.
- `wasi_messages.js`, `tabnode_wasi_msg.go`: the parent changes the limit of
  `datagram` in the docs PR of phase 4. During phase 4, only W2.4 adds
  messages to them.
- `tabnode_dial.go`: W2.1b, W3.1a and W4.1, in the order of the phases.
- `tabnode_net.go`: W3.1a, W4.2a and W6.1.
- `tabnode_netpol.go`: W3.1a, W4.2b and W6.1.
- `tabnode_wasi_tcp.go`: W3.1b. W4.4a changes only the registration in
  `init()` and `tcpData`.
- `tabnode_wasi_proxy.go`: W2.1a, W3.1a (PX5) and W4.1 (`listenProxy`).
- `tabnode.go`: W4.2a changes only `releaseIP`.
- `sockets.js`: W1.4b, W3.2, W3.3b (`TCP_DEFAULTS`) and W4.4b.
- `mise.toml`: W1.1 changes `[tools]`, W1.2 adds a task, W1.3 changes the
  task `web`. The package that merges later rebases.
- `check.sh`, `manifests.yaml`: W1.1 makes one file for each step. Later
  packages change only the file of their step.
- `README.md`: only the last package of each phase.
- `udplink_hooks.go`: the parent, in the pull request of the contract of
  phase 5.
- `tabnode_udp.go`, `tabnode_flows.go`: W4.1 and W4.2a, then W5.1b.
- `tabnode_coredns.go`, `coreutils.go`: W4.3, then W5.3.
- `tunnel.go`, `nodeworker.go`: W5.1a changes only the lines of the `udp`
  channel.
- `node/agent/podlink.go`: W5.2a adds one call in `servePodLinks`. W5.2b
  changes only `podLinkRules` and `socketAs`.
- `docker-check.yaml`: W5.0 (kernel modules), then W5.3 (`paths`).
- `README.md` in phases 5 and 6: W5.3 and W6.1. The package that merges
  later merges `origin/main` first.
