# NetworkPolicy on Tabernetes nodes

Status: design. Nothing of it is implemented yet. The proofs of concept in
`networking-policy-poc/` answer the open questions that code could settle.

Today a NetworkPolicy is stored and nothing enforces it. This design makes
the nodes enforce it with the decision engine of
[kube-network-policies](https://github.com/kubernetes-sigs/kube-network-policies),
the NetworkPolicy implementation of SIG Network. The engine is the same Go
code that decides on real nodes. Only its dataplane is replaced: on the tab
nodes, the engine is asked when a connection opens, not when a packet passes.
Docker nodes run the upstream DaemonSet unchanged, next to flannel.

## Summary

- The tab nodes (`tab` and `tab-worker-N`) link the engine into the Wasm
  binary. The pods there dial through `dialCluster`, which binds the pod IP as
  the source address. Before the connection exists, the engine sees source and
  destination, like the first packet of a connection on a real node.
- A denied connection behaves like a dropped SYN: the dial hangs until its
  deadline and fails with a timeout. A port nobody listens on still fails at
  once with "connection refused".
- Service IPs are resolved in `dialCluster` (in-process DNAT). The engine sees
  the endpoint pod, like the nftables dataplane sees the packet after
  kube-proxy's DNAT.
- Connections between nodes leave the apiserver's port-forward. A new channel
  `pod` carries them, node to node. Its first message names the source and the
  destination. The receiving node checks that the source belongs to the
  sending node, and asks its own engine again.
- Docker nodes run the upstream kube-network-policies DaemonSet with nftables
  and nfqueue. It hooks next to flannel and kube-proxy, as on any cluster that
  pairs flannel with a policy agent. The agent gets TPROXY rules, so pods on
  Docker nodes can reach tab pods, with their own pod IP as the source.

## What the proofs of concept show

`networking-policy-poc/run.sh` runs each one in a temporary directory.

| Question | Result |
| --- | --- |
| Do the engine packages build for `GOOS=js GOARCH=wasm`? (`run.sh engine`) | Yes, with one change to `pkg/network/packet.go`: js has no `syscall.IPPROTO_{DSTOPTS,HOPOPTS,ROUTING,FRAGMENT,SCTP}`, the patch writes their IANA numbers. `pkg/network/netlink.go` builds (mdlayher/netlink has a stub). The other packages need nothing. |
| Does it decide correctly in Wasm? (`run.sh engine`) | Yes. With a fake clientset, informers and a policy "web only from role=client on port 80" it allows client→web:80, denies client→web:8080, another pod→web and the node IP→web. One decision takes 0.1 ms, the first 1 ms. |
| Does the source IP survive the fake network of Go? (`run.sh fakenet`) | Yes. A `net.Dialer` with `LocalAddr` set to the pod IP gives the listener exactly that address as `RemoteAddr`. A dial to an address without a listener fails at once with "connection refused". |
| Can a rootless Docker node catch traffic to tab pods? (`run.sh tproxy`) | Yes, on this kernel. In a new user and network namespace (the agent's setup), an nftables `tproxy` rule hands a pod's connection to a listener with `IP_TRANSPARENT`. The listener sees the source pod IP and the original destination. |
| Is `net.bridge.bridge-nf-call-iptables` settable there? | Yes. It is per network namespace and writable by root of the user namespace (kernel 6.18, built-in `br_netfilter`). |

Not verified: nfqueue in the rootless node. The sandbox kernel lacks
`nft_queue` (`CONFIG_NFT_QUEUE` is not set), so no rule with `queue` loads
there. nftables rules, conntrack labels and `NETFILTER_NETLINK_QUEUE` work.
The netlink calls of nfqueue check `CAP_NET_ADMIN` in the user namespace that
owns the network namespace, so it should work. It must be tried on Docker
Desktop and on a Linux host first (phase 0).

A correction to earlier notes: kube-network-policies v1.1.3 (current) ships
`plugins/npa-v1alpha1` (AdminNetworkPolicy, BaselineAdminNetworkPolicy) and
`plugins/npa-v1alpha2` (ClusterNetworkPolicy), not a v1beta1 plugin. The
ClusterNetworkPolicy plugin follows the network-policy-api release the module
pins (v0.2.1).

## Connection path and hook points

```text
 node "tab" (main thread)                              node "tab-worker-1" (Web Worker)
+------------------------------------------------+    +---------------------------------+
| pod A (10.244.0.3)                             |    | pod C (10.244.1.4)              |
|   wget web / connect 10.244.1.4:80             |    |   listenPod 10.244.1.4:80       |
|        |                                       |    |        ^                        |
|        v                                       |    |        | [H4] accept: ingress   |
|   dialCluster ---------------------------+     |    |        |      (defense in depth)|
|   [H1] src = pod IP (LocalAddr)          |     |    |   dial from 10.244.0.3          |
|   [H2] cluster IP? pick endpoint (DNAT)  |     |    |        ^                        |
|   [H3] engine: egress + ingress          |     |    |   [H6] engine again,            |
|        |                                 |     |    |        src must be in the       |
|        | local pod IP       remote pod IP|     |    |        sender's pod CIDR        |
|        v                                 v     |    |        |                        |
|   listenPod 10.244.0.2:80     "pod" channel ======== MessageChannel ==> attachChannel |
|   [H4] accept: ingress        [H5] header:     |    |                                 |
|                               src, dst, proto |    +---------------------------------+
|                                     ||        |
| service listeners (cluster IPs,      ||        |    docker run tabernetes (rootless)
| node ports): host traffic only       ||        |   +----------------------------------+
| (terminal, apiserver, proxy panel)   ++====== WebRTC data channel "pod" ==> agent      |
|                                                |   |   [H7] transparent dial from the |
| kubelet port-forward: no policy (loopback)     |   |   tab pod IP, as uid != 0        |
+------------------------------------------------+   |        v                         |
                                                     |   nftables of kube-network-      |
                                                     |   policies (postrouting, nfqueue)|
                                                     |        v           ^             |
                                                     |   cni0 / flannel   | [H8] TPROXY |
                                                     |   pods on Docker --+ to tab pods |
                                                     +----------------------------------+
```

- **H1** `dialCluster` binds `LocalAddr` to the pod IP of `procOf(ctx)`. The
  fake network keeps it, so every listener sees which pod dials.
- **H2** A cluster IP is resolved in `dialCluster`: the service proxy's
  endpoint list and round robin pick one endpoint. The engine then sees the
  endpoint pod, as on a real node after DNAT.
- **H3** The engine evaluates egress and ingress (`PolicyEngine.EvaluatePacket`
  runs both, as upstream). Deny: no connection is made.
- **H4** `listenPod` wraps its listener. `Accept` asks the engine for the
  ingress of each connection, by `RemoteAddr`. It catches dials that do not
  use `dialCluster`. A denied connection is closed at once (a reset, the
  dialer already got a connection).
- **H5** For a pod IP of another node, `dialCluster` opens a `pod` channel to
  that node and sends a header first.
- **H6** The receiving node checks the header against the sending node and
  asks its own engine. Then it dials the pod from the source IP of the header,
  so H4 sees the real source there too.
- **H7, H8** Docker nodes, see below.

### Denied connections

NetworkPolicy drops: the client of a real cluster sees a timeout, not a
refusal. The tab does the same where it can:

- H3 and H6 deny before a connection exists. `dialCluster` then waits until
  its context ends, or 127 seconds (the SYN retries of Linux), and returns an
  `i/o timeout` error. A sender across a `pod` channel gets no answer and
  times out the same way.
- No listener on the destination: "connection refused" at once, as today,
  and from the receiving node as a `refused` answer on the channel.
- H4 can only close a connection that already exists. It is a safety net. A
  test that sees "connected, then closed" there points at a dial that misses
  `dialCluster`.

### Services

The service listeners stay, for host traffic: the terminal, the apiserver's
service proxy, the proxy panel and port-forwards to services. Pods no longer
use them: `dialCluster` handles cluster IPs itself (H2). The current
`forward` tries the next endpoint when one fails. With policy, it must not
skip a denied endpoint: kube-proxy picks one endpoint and the SYN is dropped.
Only transport errors (a node that is gone) try the next one.

NodePorts on the tab answer only on the node IP and 127.0.0.1, so their
callers are the host as well.

## Source identity between nodes

### The pod channel

Today a pod on another node is reached through the apiserver's port-forward.
The kubelet there dials from the node: the source pod is lost. Port-forward
also enters the pod like loopback, which NetworkPolicy never sees on a real
node either. So pod traffic moves to a channel of its own, next to `api` and
`kubelet:10250`:

- Label `pod`. The page creates it and stamps the label with the sending node,
  `pod;from=tab-worker-1`. The sender cannot choose that part. Worker to
  worker: the page creates a `MessageChannel` and gives one port to each
  worker, so the bytes do not pass the main thread. To and from a Docker node:
  an RTCDataChannel. A worker reaches a Docker node through the page, which
  splices the port and the data channel in JavaScript.
- The first message is one line: `TABP/1 tcp <src ip>:<port> <dst ip>:<port>`.
  The receiver answers one line: `ok`, `refused`, or nothing (denied). Then
  the channel is the byte stream.
- The target node of a pod IP comes from `node.spec.podCIDR`, which the
  controller-manager hands out from 10.244.0.0/16 for every node.

The receiving node accepts the header only if the source IP is the node IP of
the sending node or lies in its `podCIDR`. A node can speak for its own pods,
not for others. Then it asks its own engine (H6). Both nodes deciding is what
upstream does as well: each node evaluates the whole pipeline for the packets
it sees.

### Why the receiving node may trust it

All nodes are one user's: the page and its Web Workers run one Wasm module of
one origin, and a Docker node joined with a bootstrap token the user pasted.
Only the page creates channels. A pod has no handle on them: native containers
are goroutines with a context, WASI pods have no sockets, and pods on Docker
nodes live in network namespaces of their own. WebRTC data channels run over
DTLS, with the fingerprints exchanged by copy and paste. So the channel is
an authenticated link between two nodes. The header is as trustworthy as the
node that sends it.

### Compared with Cilium

Cilium puts a numeric security identity of the source (derived from its
labels) into the tunnel header: the VNI of VXLAN, or a Geneve option. The
receiving node trusts it because the tunnel runs over the node network; with
WireGuard or IPsec the nodes are authenticated too. A node that lies can claim
any identity.

The `pod` header carries the source IP, not an identity:

- The engine looks pods up by IP (`PodInfoProvider.GetPodInfoByIP`) from its
  own informers. Labels from the sender could be stale or forged; the IP is
  checked against the sender's pod CIDR.
- Pod IPs are unique in the cluster: tab nodes and flannel take them from the
  same `podCIDR` of each node.
- The DTLS data channel plays the part of WireGuard: an encrypted link between
  authenticated nodes.

Like Cilium, a compromised node can lie about its own pods. Unlike Cilium's
default, it cannot claim the pods of other nodes.

## Docker nodes and flannel

Docker nodes run real containers. flannel connects them (`addons/flannel.yaml`:
the bridge `cni0` per node, VXLAN between nodes, `--ip-masq`, iptables mode),
and kube-proxy runs in iptables mode. flannel enforces no policy. The plan
keeps flannel and adds the upstream kube-network-policies DaemonSet, like
Canal pairs flannel with Calico's policy. kube-network-policies is made to run
next to any CNI.

Why the two fit:

- **Own nftables table.** kube-network-policies keeps its rules in a table of
  its own. Its main chain hooks `postrouting` at `srcnat - 5`, before the SNAT
  of flannel's `--ip-masq` and of kube-proxy. It sees pod IPs, after the DNAT
  of kube-proxy. flannel and kube-proxy write iptables rules in tables of
  their own. With the nft or the legacy backend, they coexist with a separate
  nftables table.
- **Unique pod IPs.** flannel's `--kube-subnet-mgr` takes the subnet of each
  node from `node.spec.podCIDR`, the same field the tab nodes use. flannel
  does not masquerade inside 10.244.0.0/16. A packet from a pod on another
  Docker node keeps its pod IP across VXLAN, and the engine finds the pod.
- **Pods on the same node.** They talk over the `cni0` bridge. nftables `inet`
  hooks see bridged packets only with `br_netfilter` and
  `net.bridge.bridge-nf-call-iptables=1`. The sysctl is per network
  namespace and the agent can set it in its rootless namespace (checked).
  The module must be loaded in the host kernel. kube-proxy's hairpin needs it
  already. The agent sets the sysctl and warns when `/proc/sys/net/bridge` is
  missing.
- **Tab nodes stay out of flannel.** flannel and the new DaemonSet keep the
  node affinity `tabernetes.io/tab DoesNotExist`. Tab nodes have no flannel
  annotations (`backend-data`, `public-ip`), so flannel adds no VXLAN routes
  or FDB entries for their pod CIDRs. The TPROXY rules for those CIDRs below
  conflict with nothing.
- **hairpinMode and portmap.** The packet a hairpin or hostPort produces goes
  through `postrouting` after DNAT like any other. Upstream behaviour, nothing
  to do.
- **Image.** The node image's `images` stage reads the images from the addon
  manifests. A new `addons/kube-network-policies.yaml` gets its image mirrored
  without extra work.

The DaemonSet asks for `privileged: true`. In the rootless node that means all
capabilities over the node's own user namespace, as for kube-proxy today.

### Tab pod to Docker pod (H7)

The agent receives the `pod` channel. It must not decide itself, and must not
dial as root: the upstream rules accept every packet of uid 0 on the node
(`meta skuid 0 ... accept`, so that kubelet probes always work). So the agent
dials through a small helper process with a non-zero uid and `CAP_NET_RAW` as
an ambient capability. The helper binds the tab pod's IP with
`IP_TRANSPARENT`. The packet then enters `cni0` from 10.244.0.3, and the
nftables of kube-network-policies decide on it like on any other packet. Its
answers come back through the same TPROXY rule and routing table as H8. If the
packet is dropped, the helper's connect times out and the agent sends no
answer on the channel. The tab side times out too.

### Docker pod to tab pod (H8)

Today a Docker pod cannot reach a tab pod at all: nothing routes into the
browser. The agent adds:

```text
table inet tabernetes {
  set tabpods { type ipv4_addr; flags interval; }   # podCIDRs of the tab nodes
  chain prerouting {
    type filter hook prerouting priority mangle;
    ip daddr @tabpods meta l4proto tcp tproxy ip to :15001 meta mark set 1
  }
}
ip rule add fwmark 1 lookup 100
ip route add local 0.0.0.0/0 dev lo table 100
```

The agent listens on :15001 with `IP_TRANSPARENT`. TPROXY keeps the original
destination, unlike `REDIRECT`. That matters: kube-network-policies evaluates
traffic from pods to the node in its `input` chain, and must see the tab pod as
the destination, not the node. So the egress policy of the Docker pod is
decided by the kernel on the Docker node. The agent then opens a `pod` channel
to the tab node of that pod CIDR, with the Docker pod as the source. The tab
node checks the source against the Docker node's `podCIDR` and decides
ingress (H6). This lifts the limit "Pods on Docker nodes cannot reach pods on
the tab nodes" for TCP.

The service IPs of tab pods need nothing more: kube-proxy on the Docker node
DNATs a cluster IP to the endpoint's pod IP, and the TPROXY rule sees the
pod IP of a tab node.

## Node and host traffic

A real node has a host: the kubelet, system daemons and hostNetwork pods.
NetworkPolicy cannot block a pod's own loopback or traffic from its own node
(Kubernetes docs, "What you can't do"). kube-network-policies skips both:
`iifname lo accept` and uid 0 on the node.

In the tab, the host of a node is everything of the Wasm instance that is not
a pod:

- the terminal and its commands (kubectl, `wget`, `proxy`),
- the control plane on `tab`: the apiserver, its service and pod proxy, the
  admission webhooks it calls,
- the kubelet: probes, lifecycle hooks, port-forward, exec,
- hostNetwork pods (they have the node IP).

Their dials have no `procOf(ctx)` pod and come from 127.0.0.1 or the node IP.

- To a pod on the same node: always allowed.
- To a pod on another node: the `pod` header carries the node IP as the
  source. The receiving node evaluates it, so `ipBlock` rules apply, as for
  host traffic between real nodes.
- A pod to a node IP, its own included: an egress to that IP, evaluated like
  upstream's `input` chain.
- A pod to itself (its pod IP, which is also its localhost): always allowed.
  On Linux this is loopback. The proof of concept shows that the engine
  would deny it, so the hooks skip the engine when source and destination
  IPs are the same pod.
- `kubectl port-forward` to a pod: never evaluated. CRI port-forward enters
  the pod like loopback on a real node too.

## Gaps

- **UDP.** The tab nodes have no UDP sockets for pods; services skip UDP
  ports. UDP rules are evaluated correctly but cannot be tested there. SCTP
  never.
- **Egress to the internet.** `wget` and `curl` to other hosts go through the
  browser's `fetch()`. There is no IP. For a pod with egress policy, the
  engine is asked with the destination 0.0.0.0 on port 80 or 443: a rule with
  `ipBlock: 0.0.0.0/0` allows it, other rules do not. This is close, not
  exact (`except` lists cannot match).
- **Strict mode.** Upstream (`--strict-mode`, the default) also cuts open
  connections when a policy changes. Phase 6: each node tracks open
  connections per pod and evaluates them again on a sync callback.
- **hostNetwork pods** on the tab share the node IP with the control plane.
  They are host traffic; policies do not select them (`PodOwnsIPs` is false
  for them upstream as well).
- **Named ports** work: the engine reads the container ports from the pod
  spec.
- **WASI pods** have no sockets. Nothing to enforce.

## Integration

### Vendoring

`k8s/patch.go` already vendors mvdan.cc/sh with `vendorModule`. The engine
goes in the same way:

- `KUBE_NETWORK_POLICIES_VERSION = "v1.1.3"` in `mise.toml`, with a renovate
  comment. Its `k8s.io/*` requirements are v0.37.2, the same as
  `KUBERNETES_VERSION`; renovate PRs show a mismatch at build time.
- `vendorModule("sigs.k8s.io/kube-network-policies", ..., "pkg/api",
  "pkg/network", "pkg/networkpolicy", "pkg/podinfo")`. `pkg/network/netlink.go`
  is not copied: only the dataplane uses `IsNetlinkRejection`, and the
  mdlayher/netlink module is then not needed. `vendorModule` gets an exclude
  list for that.
- One `edit` in `packet.go`: the five `syscall.IPPROTO_*` become numbers. It
  fails like every patch when upstream changes the line. Better: an upstream PR
  that uses `golang.org/x/net/ipv4`-style constants of its own, then the edit
  goes.
- Kubernetes vendors everything else already: prometheus, protobuf, klog,
  client-go. The ClusterNetworkPolicy plugin (phase 5) adds
  `sigs.k8s.io/network-policy-api` (types, clientset, informers).

### Size

Measured on a small Wasm binary with client-go informers, with and without
the engine (`-s -w`):

| | raw | zstd -19 |
| --- | --- | --- |
| engine | +0.4 MB | about +0.1 MB |
| engine and the ClusterNetworkPolicy plugin with its clientset | +0.55 MB | about +0.1 MB |

Against 290 MB and 28 MB that is noise.

### Memory

The engine needs informers on all pods, namespaces, NetworkPolicies and nodes
of the cluster. Today the kubelet of a tab node watches only its own pods. A
pod costs a few KB in the cache; a transform that drops `managedFields` halves
that. 500 pods are a few MB per node. A worker is 100 MB and more already.
Each node runs its own engine: a worker must not ask the main thread about
every connection.

### Code

- `tabnode_netpol.go`: the engine, its informers (with the transform) and
  `allow(src, dst, port)` with the host and loopback rules. It starts with the
  node, in the main thread and in workers.
- `tabnode_dial.go`: H1 to H5.
- `tabnode_net.go`: endpoint selection for `dialCluster`, no fall-through on
  deny.
- `podlink.go`: the `pod` channel, sending and receiving (H5, H6).
- `web/src/nodes.js`, `join.js`, `node.worker.js`: route `pod` channels.
- `node/agent/`: the channel, the transparent helper, TPROXY rules, the
  bridge sysctl.

## Proof that it is real

The NetworkPolicy end-to-end tests of Kubernetes (`test/e2e/network/netpol`,
`[sig-network] Netpol`) create namespaces x, y, z with pods a, b, c and probe
every pair with `kubectl exec ... /agnhost connect <host:port> --timeout=<t>
--protocol=<p>`. [Cyclonus](https://github.com/mattfenwick/cyclonus) does
the same over many more generated policies.

What it takes:

1. **agnhost connect** in `tabnode_images.go`: dial with `dialCluster`
   within `--timeout`; on success exit 0; else print `TIMEOUT`, `REFUSED` or
   `OTHER: <err>` to stderr and exit 1, like upstream. `--protocol=udp`
   and `sctp` print `OTHER` (unsupported).
2. **Server containers.** The test pods run one agnhost server container
   per port and protocol (`serve-hostname` or `porter`, see
   `test/e2e/network/netpol/kubemanager.go` of the Kubernetes version). The
   built-in agnhost must accept their flags and listen on TCP. Accepting a
   connection is all `connect` needs.
3. **A driver.** `e2e.test` is a Linux binary and needs an apiserver. The only
   way in from outside is a Docker node: its container has the apiserver
   tunnel on 127.0.0.1:6443. Join one, cordon it, so all test pods land on
   `tab` and the workers, copy `kubectl config view --raw` into it, and run
   `e2e.test --provider=skeleton --ginkgo.focus='\[sig-network\] Netpol'
   --ginkgo.skip='SCTP|UDP'` there. Cyclonus runs as a Job on the same node.
4. **Mixed runs.** Uncordon the Docker node and let pods spread
   (`addnode 2` as well). This exercises the `pod` channel, H7 and H8.

Expected failures:

- every UDP case (no UDP on tab nodes), SCTP everywhere;
- cases that expect a probe to the internet through an `ipBlock` with
  `except` (the `fetch()` approximation);
- timing: the tab runs one thread. Many pods and the exec probes of 3x3x3
  pairs can exceed the probe timeouts. Workers spread the pods; the test's
  `--timeout` may need raising, which says nothing about the policy;
- before phase 6: cases that change a policy while a connection stays open
  (few, mostly in Cyclonus).

Anything else that fails is a bug of this design.

## Phases

Each phase is one small PR.

| # | What | Effort |
| --- | --- | --- |
| 0 | Try nfqueue and the upstream DaemonSet in the rootless node, on Docker Desktop and a Linux host. Also a non-root helper with ambient `CAP_NET_RAW` and `IP_TRANSPARENT` (H7). If it fails, stop before phase 4 and decide again. | 1 day |
| 1 | Vendor the engine (`patch.go`, `mise.toml`), `tabnode_netpol.go` with informers, nothing hooked. `kubectl get` shows nothing new; the build proves the vendoring. | 1 day |
| 2 | Tab nodes: H1 to H4, deny as timeout, host and loopback rules, `dialCluster` DNAT. Pods on one tab node obey NetworkPolicy. `agnhost connect`. | 3 days |
| 3 | The `pod` channel between tab nodes (tab, workers), H5 and H6; the service proxy stops using port-forward for pods. | 3 days |
| 4 | Docker nodes: the DaemonSet addon, the bridge sysctl, the `pod` channel in the agent with the transparent helper (H7) and TPROXY (H8). Docker pods reach tab pods. | 4 days |
| 5 | ClusterNetworkPolicy: the network-policy-api CRDs as an addon, `plugins/npa-v1alpha2` in the engine on tab nodes, the matching flag on the DaemonSet. | 2 days |
| 6 | Strict mode: cut open connections on policy changes. | 2 days |
| 7 | End-to-end and Cyclonus runs, documented in `README.md`, maybe in CI with a Docker node. | 2 days |

## Risks

- **nfqueue in the rootless node** is unverified (phase 0). Without it, Docker
  nodes keep no policy: the DaemonSet does not become ready, or lets traffic
  pass (`--fail-open`, the default). The agent should report it as a node
  condition.
- **Host kernels.** TPROXY, `nft_queue`, conntrack labels and `br_netfilter`
  must exist in the kernel of Docker Desktop (LinuxKit) and of Linux hosts.
  The agent checks each at start and says which is missing.
- **Engine API.** `PolicyEngine`, `PolicyEvaluator` and `podinfo` are internal
  packages of kube-network-policies, not a stable library. Updates may break
  the build; the version is pinned and renovate PRs show it.
- **The fake network** keeping `LocalAddr` is behaviour of Go's
  `syscall/net_fake.go`, not a documented promise. A Go update could change
  it; a test in `mise run test` (`fakenet` of the proof of concept) catches
  that.
- **Dials around `dialCluster`.** A program that calls `net.Dial` directly
  bypasses H1 to H3. H4 still checks ingress, with the source 127.0.0.1 (host).
  A test greps the image programs for raw dials.
- **One thread.** A decision costs about 0.1 ms per connection, nothing next to
  the rest. Informer updates of all pods cost more on big clusters.
- **Trust** rests on the page's JavaScript. Who controls the page controls the
  cluster anyway.

## Rejected alternatives

- **An unmodified CNI** (Cilium, Calico, kube-router, Antrea): they all
  enforce in the Linux kernel (eBPF, iptables/nftables, OVS), which the tab
  does not have.
- **eunomia-bpf / wasm-bpf**: only the loader runs in Wasm; the eBPF programs
  still need a kernel.
- **bpftime** (eBPF in userspace): would also need a packet layer, kernel
  helpers, hooks and emulated `bpf()` and netlink.
- **A Linux VM in the tab** (container2wasm, Bochs, TinyEMU, Linux on Wasm):
  could run a real CNI, but as a separate, very slow node with gigabytes of
  memory.
- **A NetworkPolicy evaluator of our own**: not "real"; it would drift from
  upstream semantics. The upstream engine is small and builds for Wasm.
- **Identity in the port-forward stream** (a preamble on the data of the
  apiserver's port-forward): anyone allowed to port-forward could forge it,
  and the path stays a detour through the apiserver.
- **Carrying labels or a Cilium-style numeric identity**: needs a cluster-wide
  identity allocator; the IP plus the sender's pod CIDR check is enough, and
  the engine works by IP.
- **REDIRECT instead of TPROXY on Docker nodes**: rewrites the destination to
  the node, so the kernel would evaluate a Docker pod's egress against the
  wrong address.
- **The agent deciding with its own engine for tab traffic**: would duplicate
  the DaemonSet and need cluster-wide read credentials on every Docker node;
  the transparent helper lets the kernel rules decide.
