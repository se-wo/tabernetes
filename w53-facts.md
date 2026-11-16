# Facts for W5.3 from the other phase 5 and 6 packages

Collected by the parent from the reports. Read this together with w53.md.

## W5.2a API (branch wasi/w5.2a-agent-udplink, commit adaba81, tabernetes/node/agent/udplink.go) — for W5.2b
- type tabPort interface { WriteTo(b []byte, dst netip.AddrPort) error; Close() error }
- var openTabPort func(tab netip.AddrPort) (tabPort, error)   // nil = counted drop, no panic
- var startUDPNet func(ctx context.Context, l *udpLink) error  // called once via startUDPLinkNet(ctx, t.udp) in servePodLinks after setupPodLinkNet succeeded; nil = skipped
- func (l *udpLink) fromPod(src, dst netip.AddrPort, b []byte)  // never waits, does not keep b, Unmaps addresses
- var udpFlowNow = time.Now
- The flow table opens/closes tabPorts (one per tab address, refcounted by flows, closed after the last flow or at ctx end). A WriteTo error (also an ICMP error from an earlier datagram) is only a counted drop; nothing goes to the page (rule 7). fromPage drops a destination outside flannelSubnet(). tunnel has a new field udp *udpLink (main.go, set after connected; kubectl mode gets none).

## Pion fact (from W5.2a) — for W5.1a item 3
pion webrtc v4.2.26 / sctp v1.12.0: with DetachDataChannels + EnableDataChannelBlockWrite(true), a write on a detached channel CAN wait (sctp Association.sendPayloadData waits on writeNotify while writePending is set, until the association-wide pending queue moved to inflight), so another busy stream (kubectl logs, pod TCP) can make a udp write wait. The agent writes from one goroutine with a queue of 64 messages, drops when full, and reads BufferedAmount() before the enqueue and before each write (> 1 MiB = drop). A message larger than the read buffer gives io.ErrShortBuffer and pion discards it; the agent counts it and reads on.

## W5.0 (PR #103) — nfqueue on the GitHub runner WORKS, for W5.3
- No modprobe, no node image change: runner kernel 7.0.0-1019-azure has nft_queue and nfnetlink_queue loaded; kube-network-policies runs on node-1; ruleset "ct state new queue flags bypass to 98".
- New Docker step `netpol` (docs/docker-node-check/netpol.sh, netpol.yaml; listed in docker-check.yaml after join) is the model for a deny test on node-1: pod_exec + `echo hi | nc -w 2 IP PORT`, server `while true; do echo ok | nc -l -p 8080; done`, namespace netpol-check deleted at the end.
- New policy to denied connection: <= 2 s on the runner; a loop of 10 s with 1 s pauses is enough.
- So ND10, ND17, CR1/CR8 and TC10 are written as PASS/FAIL lines (outcome 1 of w5-plan.md); phase 5 ends when they pass.

## W5.2b (PR #104, draft, based on W5.2a dff4a94) — for W5.3 (networking-policy.md H7/H8)
- Tests: TestUDPLinkNetns (ND13-ND18, 65,507 bytes both directions, non-pod source 10.0.0.1 with socket rule; runs by hand in unshare), TestParseOrigDst (ND14), TestPodLinkRulesUDP (ND13), TestSocketAsType (ND17), TestBindTransparentUDPIPv4.
- Kernel needs TPROXY (nft_tproxy) for UDP and nf_defrag. nft_socket only for answers to the node IP of a worker tab node.
- tproxy finds only CONNECTED UDP sockets; tab sockets are not connected. With nft_socket, datagrams to a tab address with an open socket go to that socket; without it, all go to the TPROXY socket 0.0.0.0:15001 (reads IP_ORIGDSTADDR).
- Tab sockets owned by uid 65534, so kube-network-policies examines the agent's datagrams to pods. Answers leave from the tab address.
- Drop rule iifname tabpods1 stays last. Packets from tabpods1 have notrack.
- Log lines: "udp channel: no socket match, the host of a tab node in a Web Worker gets no UDP answers from pods of this node"; "udp channel: <TPROXY socket|socket of IP>: <err> (the first error of this kind; ...)"; W5.2a: "udp channel: pods of this node cannot send UDP to tab addresses: TPROXY socket: ..." if the bind fails.
- mise in the cloud sandbox cannot install go/rust (proxy blocks mise-versions); go test of the agent runs directly.

## W4.5b (PR #100, merged) — for W5.1b, W5.3, W6.1
- Model: wasi-udp.test.mjs (sharedNamespace in before(), servers first, clients get IP literals so no lookup moves ports, clientLines() helper, RUNWASI/TAB objects). wasi-lib.mjs has echoPod/lines/logLine/exited; service(name, port, "UDP", spec) in wasi-proxy-lib.mjs. wasi-udp-flows.test.mjs for FL rows.
- Rows waiting for phase 5: ND rows, FL11 (worker), UD19 phase 5 (nslookup default server over UDP to CoreDNS on another node), Docker pods to tab CoreDNS over UDP, NetworkPolicy for UDP of components on Docker nodes.
- nc -u now gathers writes for 5 ms (ncUDPGather) into one datagram (<= 8,192 bytes). nc -u ignores -p.
- E2E suite takes ~14-16 min in CI.
- W5.0 final (PR #103 merged f5240ac): when kube-network-policies does not run, pod/ruleset/deny write SKIP with the reason. Stale text for W5.3 to fix: networking-policy.md "Tested with a Docker node" ("Not tested: the DaemonSet deciding on the Docker node ... no nft_queue") and wasi-sockets.md section 4.4 bullet (~line 198, "networking-policy.md lines 3 to 5 gives this item as not complete"). The netpol step needs only the join before it.

## W5.1a (commit 910fc55 on wasi/w5.1a-udp-channel) — relayed to W5.1b and W5.2a at 22:12
- udpToNode set in init() of podlink_udp.go (udpLinkToNode); udpFromNode called from one goroutine per channel. Node worker opens tabernetesOpen("udp") after startTabNode (runWorkerNode). udpLinkNodeOf(ip) on parsed table (pod CIDRs, then InternalIP), rebuilt <= 1/s; tests override udpLinkLister; udpChanDropCount(reason).
- Page requires Docker-node channel ordered=false, maxRetransmits=0, else closes ("udp channel of NODE: ordered=X maxRetransmits=Y, want false and 0: closed"). Unknown node -> closed. Drop log each 10 s: "udp channel: dropped N messages (reason=count ...)". Node worker credits dropped messages too.

## W5.2a final (PR #102 merged 924ae47) — for W5.3
- Tests (node/agent/udplink_test.go): ND3 TestUDPLinkChannelSettings, TestUDPLinkChannelPeers; ND4/ND5 TestUDPLinkTestVectors, TestUDPLinkExamination, TestUDPLinkFromPageDrops; ND12 TestUDPLinkBufferedAmount, TestUDPLinkWriteWaitsQueueDrops; ND19 TestUDPLinkFlowTimeouts, TestUDPLinkSharedSocketLastFlow; ND15/16/18 logic TestUDPLinkFlowBothDirections, TestUDPLinkNodeIPSource; rule 7 TestUDPLinkSocketErrors, TestUDPLinkNoOpener; rule 11 TestUDPLinkFlowLimit, TestUDPLinkPortLimit; TestUDPLinkChannelClosed, TestUDPLinkReadLoop, TestUDPLinkNoChannel.
- Agent drop log (<= every 10 s): "udp channel: dropped N datagrams (T in total; <reason>: <count>, ...)". Reasons: invalid message, credit, destination not a pod of this node, no flannel subnet, flow limit, socket limit, no socket for tab addresses, socket open failed, write to pod failed, address not usable, no channel, bufferedAmount above 1 MiB, send queue full, channel write failed, message longer than 65,536 bytes. startUDPNet error: "udp channel: pods of this node cannot send UDP to tab addresses: <err>". Channel close: "udp channel: closed (<err>); the node agent does not open it again".
- Expired-but-unswept flow restarts as new flow; sweep every 10 s.

## W5.1b interim (branch wasi/w5.1b-udp-nodes de04abc) — for W5.3
- New e2e wasi-udp-nodes.test.mjs (13 tests, tab+worker, ~100 s).
- busybox nc -u rebinds 32768 after the previous nc exits, so sequential nc runs reuse one flow (FL1) and hit the same endpoint. For round robin run nc in parallel (`... & done; wait`); the tab terminal prints "[n] ..." job lines — filter them.
- Echo pod log "udp N bytes from IP:port" counts datagrams per endpoint.
- translateUDP picks all ready endpoints. Endpoint on a Docker node IP is lost (new section 11 row).

## W6.1 final (PR #106 merged 1be7e29) — for W5.3
- README already changed by W6.1: lines ~73-77, new "### ICMP echo" under "## NetworkPolicy", ICMP item in the differences list, limits line that replaced "ping has no ICMP". W5.3 merges origin/main first.
- IC3 Docker part is open: W5.3 adds one line to its Docker step: ping from a tab pod to a Docker pod shows "ping: <ip> is on node <name>: ICMP between nodes is not available" (stderr, exit 1, no echo request).
- ICMP facts for networking-policy.md: allowedICMP(ctx, src, dst) calls decide(ctx, "", src, 0, dst, 0) -> EvaluatePacket with Proto "" and ports 0 (as ParsePacket for ICMP). One call: egress of source + ingress of destination. Exempt: source in 127/8, node IP (terminal, hostNetwork), src == dst; ping also skips the engine for 127/8 or own pod IP destination. Allows until informers sync. A rule with ports never matches ICMP; without ports it does. Each echo request is decided (hook icmpPolicy); a Docker node examines only the first echo request of a flow. ClusterIPs give no reply; ICMP between nodes is not available. A lost echo request waits -W seconds (default 2).

## W5.1a final (PR #105 merged 895eeae) — for W5.3
- Tests (k8s/tabernetes/podlink_udp_test.go): ND4/ND5 TestUDPChanHeader; ND3 page part + rule 1 TestUDPLinkAttach; ND7/ND8 + rule 4 TestUDPLinkRoute; rule 5 TestUDPLinkToNodePage; ND11 TestUDPLinkCredits; ND20, ND2 worker side TestUDPLinkWorker; ND12 page part TestUDPLinkBuffered; TestUDPLinkSendFails.
- Page drop log (<= every 10 s): "udp channel: dropped N messages (reason=count ...)"; reasons: short, version, type, size, port, credit, queue, source, destination, no channel, table, address, state, buffered, send, closed.
- Relaying between other nodes is control-plane CPU, not charged to a pod.
- Local test-web on Node 22 has 27 failures (no WebAssembly.promising), same on main; CI is green.
