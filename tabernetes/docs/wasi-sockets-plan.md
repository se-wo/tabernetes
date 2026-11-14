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
| W1.4b | 1 | CO2 to CO4, CO11, CO12, CO17 and CO19 (tests in W3.3), CR6, the rest of CO5 and CO18, MS1 and MS6 for new messages | the selection of the world, the statuses of runwasi, stdin and memory of components, `web/src/sockets.js` (a stub that denies, and the count of CO17) | the files of W1.4a | W1.4a, W1.1 | L |
| W1.5 | 1 | FS1 to FS4, CO6, CO7, AC2 | `web/src/files.js` (new), the `cpu` message in `tabnode_acct.go` | `component.js` (start of the adapter), `README.md` (phase 1) | W1.4 | M |
| W2.1a | 2 | PX1 to PX4, PX6, PX16 to PX20, PX23, PX26 to PX33, AC1 (proxy server) | `k8s/tabernetes/tabnode_wasi_proxy.go` (new), the proxy world in `componentStatus`, `startComponent` and `pullWasmOnce` of `tabnode_wasi.go` | `tabnode_wasi_msg.go` | phase 1 | L |
| W2.1b | 2 | CR1 to CR8, CR10, CR11, MS5, AC1 (client requests) | `k8s/tabernetes/tabnode_wasi_fetch.go` (new), a client for components in `tabnode_dial.go`, `synTimeout` as a variable for tests | `tabnode_wasi_msg.go`, `tabnode_netpol.go` (only `synTimeout`) | phase 1 | M |
| W2.2 | 2 | PX7 to PX15, PX21, PX22, PX24 to PX27, PX34, CR9 | `web/src/http.js` (new, from `http-own.js` of phase 0), the proxy path of `component.js` and `component.worker.js` | `wasi_messages.js` | phase 1 | L |
| W2.3a | 2 | All PX and CR tests end-to-end, the CO2 test of `bothrev` | step `wasi-proxy`, the end-to-end tests of PX and CR, `README.md` (phase 2) | none | W2.1a, W2.1b, W2.2 | M |
| W2.3b | 2 | Open question 2 (PX9, PX34) | `web/test/e2e/wasi-memory.test.mjs` (new) | `wasi-sockets.md` (sections 8.6, 11, 12, 14), `component.js` (PX34 in the proxy path), `proxy.test.mjs` (tests of PX34), `README.md` (one sentence after W2.3a) | W2.1a, W2.1b, W2.2 | S |
| W2.4 | 2 | The Wasm memory of the tab (sections 12 and 14) | `web/src/memory.js` (new), see below | `wasi_messages.js`, `tabnode_wasi_msg.go` (rule 4), `component.js`, `wasi.js`, `nodes.js`, `node.worker.js`, `wasi.worker.js` | W2.3b | L |
| W3.1 | 3 | TC1 to TC4, TC7 to TC12, TC14 to TC16, TC18, TC19, NL1 to NL3, MS2 to MS4, AC1 (TCP) | `k8s/tabernetes/tabnode_wasi_tcp.go` (new), `tabnode_loopback.go` (new), the loopback checks in `tabnode_dial.go`, `tabnode_net.go`, `tabnode_netpol.go` and `podlink.go` | `tabnode_wasi_msg.go` | phase 2 | L |
| W3.2 | 3 | TC5, TC6, TC13, TC17, TC20 to TC22, NL in the pod worker | the TCP and lookup part of `web/src/sockets.js` | `wasi_messages.js` | phase 2 | L |
| W3.3 | 3 | All TC and NL tests end-to-end, PX5, CO17, CO19 | step `wasi-sockets`, `README.md` (phase 3) | none | W3.1, W3.2 | M |
| W4.1 | 4 | UD1 to UD6, UD8, UD9, UD14, UD15, UD20, UD21 | `k8s/tabernetes/tabnode_udp.go` (new), `listenPodUDP`, `dialClusterUDP` and ephemeral ports in `tabnode_dial.go` | none | phase 3 | L |
| W4.2 | 4 | UD7, UD10 to UD13, UD16, UD17, FL1 to FL13 | the flow table and UDP Services in `tabnode_net.go`, UDP in `tabnode_netpol.go` and `tabnode_netpol_strict.go` | none | W4.1 | XL |
| W4.3 | 4 | UD18, UD19, UD22, AC3 | UDP in `tabnode_coredns.go`, `nslookup` over UDP and `nc -u` in `coreutils.go` | none | W4.1 | M |
| W4.4 | 4 | UDP in the pod worker, AC1 (UDP) | the UDP part of `sockets.js`, `k8s/tabernetes/tabnode_wasi_udp.go` (new) | `wasi_messages.js`, `tabnode_wasi_msg.go` | W4.1 | L |
| W4.5 | 4 | All UD and FL tests end-to-end | step `wasi-udp` (in one pod and on one node), `README.md` (phase 4) | none | W4.2, W4.3, W4.4 | M |
| W5.1 | 5 | ND2 to ND9, ND11, ND12, ND20 (page and node worker) | the `udp` channel in `podlink.go`, `web/src/channel.js`, `web/src/nodes.js`, `web/src/node.worker.js` | none | phase 4 | L |
| W5.2 | 5 | ND13 to ND19, the agent part of ND3 and ND12 | `node/agent/podlink.go` | none | phase 4 | L |
| W5.3 | 5 | ND1, ND10, the deferred tests | step `wasi-udp` (between nodes), `docs/networking-policy.md`, `README.md` (phase 5) | none | W5.1, W5.2 | M |
| W6.1 | 6 | IC1 to IC8 | ICMP in `tabnode_netpol.go`, `ping` in `coreutils_misc.go`, `README.md` (phase 6) | none | phase 4 | M |

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
- `tabnode_dial.go`: W2.1b, W3.1 and W4.1, in the order of the phases.
- `tabnode_net.go`, `tabnode_netpol.go`: W3.1, W4.2 and W6.1.
- `sockets.js`: W1.4b, W3.2 and W4.4.
- `mise.toml`: W1.1 changes `[tools]`, W1.2 adds a task, W1.3 changes the
  task `web`. The package that merges later rebases.
- `check.sh`, `manifests.yaml`: W1.1 makes one file for each step. Later
  packages change only the file of their step.
- `README.md`: only the last package of each phase.
