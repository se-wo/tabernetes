// UDP between the tab node and a node worker (docs/wasi-sockets.md, phase 5,
// sections 8.7 to 8.9.1, package W5.1b): the rows ND2, ND6, ND9 and FL11 of
// section 10.3, UD7 and UD15 between nodes, host traffic, nslookup to the
// pod IP of CoreDNS on another node, and no refusal between nodes (channel
// rule 7, section 11).
//
// One UDP echo pod (wasi-test-echo) runs on each node. The clients are
// emulated busybox pods (nc -u, nslookup) and containers of the echo image
// in the mode udp-client, which exit: each running component uses one Wasm
// memory of the tab (section 14). The pods share one namespace, and the
// clients get the IPs of the servers as literals, as in wasi-udp.test.mjs.
// This file is also the model for the Docker step between nodes (W5.3).
//
// Unit tests of the same rows (k8s/tabernetes/tabnode_udp_nodes_test.go):
//
//   ND2         TestUDPNodeSendPodIP, TestUDPNodeReceiveNodeIP
//   UD7, UD15   TestUDPNodeServiceRemote
//   ND6         TestUDPNodeEgressDenied
//   ND9, FL7    TestUDPNodeIngressPolicy
//   FL11        TestUDPNodeLocalPolicy
//   Rule 7      TestUDPNodeReceiveDrops
import { before, test } from "node:test";
import assert from "node:assert/strict";
import { apply, forEachNode, getJSON, kubectl, podPhase, podReady, run, TAB_NODE, waitFor, waitForPod, worker } from "./harness.mjs";
import { busyboxPod, inPod, podIP, running, service, sharedNamespace } from "./wasi-proxy-lib.mjs";
import { echoPod, exited, lines } from "./wasi-lib.mjs";

const shared = sharedNamespace("udn");
const ns = shared.ns;
// The pods and IPs of each kind of node: tab and worker.
const POD = { tab: {}, worker: {} };
const IP = {};
const other = { tab: "worker", worker: "tab" };

// clientLines gives the result lines of a udp-client container.
const clientLines = async (name, c) =>
  (await lines(ns, name, c)).filter((l) => /^(answer |no answer$|error )/.test(l));

const clusterIP = async (name) => (await getJSON(`svc -n ${ns} ${name}`)).spec.clusterIP;

const udpClient = (target, env = {}) => ({ MODE: "udp-client", CONNECT: target, ...env });

// nc sends text + "\n" from a busybox pod with nc -u and gives its output:
// "echo <text>\n" from the echo pod, then the exit code.
const nc = async (bb, ip, port, text) => (await inPod(ns, bb, `echo ${text} | nc -u -w 2 ${ip} ${port}; echo rc=$?`)).out;

// received counts the datagrams of n bytes that an echo pod got from ip.
const received = async (echo, n, ip) =>
  (await lines(ns, echo, "c")).filter((l) => l.startsWith(`udp ${n} bytes from ${ip}:`)).length;

before(async () => {
  await shared.create();
  POD.tab.node = TAB_NODE;
  POD.worker.node = await worker();
  for (const kind of ["tab", "worker"]) {
    const p = POD[kind];
    p.echo = `echo-${kind}`;
    p.bb = `bb-${kind}`;
    p.eg = `eg-${kind}`;
    p.client = `client-${kind}`;
  }
  // The servers first: one UDP echo pod on each node, with a Service each
  // and two Services over both (FL11).
  const both = { selector: { echo: "udp" } };
  await apply([
    ...["tab", "worker"].flatMap((k) => [
      echoPod(POD[k].echo, POD[k].node, [["c", { PROTO: "udp" }]], { labels: { echo: "udp" } }),
      service(POD[k].echo, 7, "UDP"),
      busyboxPod(POD[k].bb, POD[k].node),
      // ND6: a client with a policy that denies all its egress.
      busyboxPod(POD[k].eg, POD[k].node),
    ]),
    service("both", 7, "UDP", both),
    service("both-local", 7, "UDP", { ...both, internalTrafficPolicy: "Local" }),
    {
      apiVersion: "networking.k8s.io/v1",
      kind: "NetworkPolicy",
      metadata: { name: "egress-deny" },
      spec: {
        podSelector: { matchExpressions: [{ key: "app", operator: "In", values: [POD.tab.eg, POD.worker.eg] }] },
        policyTypes: ["Egress"],
      },
    },
  ], ns);
  for (const k of ["tab", "worker"]) {
    await waitForPod(ns, POD[k].echo, podReady);
    IP[POD[k].echo] = await podIP(ns, POD[k].echo);
    IP[`svc-${k}`] = await clusterIP(POD[k].echo);
  }
  // The clients of the echo image. The ones of a Service wait 3 seconds
  // for its endpoint on the other node.
  const noAnswer = { CONNECTED: "1", COUNT: "3", INTERVAL_MS: "500", WAIT_MS: "1000" };
  await apply(["tab", "worker"].map((k) => {
    const o = POD[other[k]];
    return echoPod(POD[k].client, POD[k].node, [
      // ND2: the pod IP of the echo pod on the other node.
      ["pod", udpClient(`${IP[o.echo]}:7`, { CONNECTED: "1", COUNT: "2", INTERVAL_MS: "500", LINE: "nd2" })],
      // UD7, UD15: the Service with its only endpoint on the other node.
      ["svc", udpClient(`${IP[`svc-${other[k]}`]}:7`, { CONNECTED: "1", COUNT: "2", INTERVAL_MS: "500", LINE: "service", DELAY_MS: "3000" })],
      ["unc", udpClient(`${IP[`svc-${other[k]}`]}:7`, { LINE: "unconn", DELAY_MS: "3000" })],
      // Rule 7: a port without a socket on the other node.
      ["noport", udpClient(`${IP[o.echo]}:9`, noAnswer)],
    ], { restartPolicy: "Never" });
  }), ns);
  for (const k of ["tab", "worker"]) {
    for (const p of [POD[k].bb, POD[k].eg]) await waitForPod(ns, p, running);
    IP[POD[k].bb] = await podIP(ns, POD[k].bb);
    IP[POD[k].client] = await podIP(ns, POD[k].client);
  }
});

forEachNode(["tab", "worker"], (kind) => {
  const p = POD[kind];
  const o = POD[other[kind]];

  test(`ND2: a pod sends to the pod IP of the echo pod on the other node and gets the answer from that pod IP on ${kind}`, async () => {
    await exited(ns, p.client, "pod");
    const a = await lines(ns, p.client, "pod");
    assert.match(a.find((l) => l.startsWith("local ")) ?? "", new RegExp(`^local ${IP[p.client].replace(/\./g, "\\.")}:\\d+$`), a.join("\n"));
    assert.deepEqual(await clientLines(p.client, "pod"), [`answer echo nd2 from ${IP[o.echo]}:7`, `answer echo nd2 from ${IP[o.echo]}:7`]);
    // The echo pod sees the pod IP of the client.
    assert.equal(await received(o.echo, 3, IP[p.client]), 2);
    // busybox nc -u too.
    assert.equal(await nc(p.bb, IP[o.echo], 7, "busybox"), "echo busybox\nrc=0\n");
  });

  test(`UD7, UD15: a Service with its only endpoint on the other node answers from the ClusterIP on ${kind}`, async () => {
    for (const c of ["svc", "unc"]) await exited(ns, p.client, c);
    const cip = IP[`svc-${other[kind]}`];
    const svc = await clientLines(p.client, "svc");
    assert.ok(svc.includes(`answer echo service from ${cip}:7`), svc.join("\n"));
    assert.ok(svc.every((l) => l === `answer echo service from ${cip}:7` || l === "no answer"), svc.join("\n"));
    assert.deepEqual(await clientLines(p.client, "unc"), [`answer echo unconn from ${cip}:7`]);
    // UD15: the endpoint sees the pod IP of the client.
    assert.ok(await received(o.echo, 7, IP[p.client]) >= 1);
    assert.equal(await received(o.echo, 6, IP[p.client]), 1);
  });

  test(`Rule 7, section 11: a connected client to a port without a socket on the other node gets no connection-refused on ${kind}`, async () => {
    await exited(ns, p.client, "noport");
    assert.deepEqual(await clientLines(p.client, "noport"), ["no answer", "no answer", "no answer"]);
  });

  test(`FL11: with internalTrafficPolicy Local, only the echo pod on the same node answers; without it, both on ${kind}`, async () => {
    const ip = IP[p.bb];
    const ask = async (svc, text, n) => {
      const cip = await clusterIP(svc);
      // The nc run at the same time: a later nc would get the same first
      // free port (UD3) and use the open flow of the one before (FL1).
      const r = await inPod(ns, p.bb, `for i in $(seq ${n}); do echo ${text} | nc -u -w 2 ${cip} 7 & done; wait`);
      // The shell can also print a line for each job.
      assert.equal(r.out.split("\n").filter((l) => l === `echo ${text}`).length, n, `${svc}: ${r.out}`);
    };
    // "fl11-local\n" has 11 bytes, "fl11-cluster\n" 13. Each nc is a new
    // flow, and each new flow gets an endpoint at random, as kube-proxy in
    // iptables mode picks it: 16 flows all on one node is a chance of 2^-15.
    await ask("both-local", "fl11-local", 4);
    assert.equal(await received(p.echo, 11, ip), 4);
    assert.equal(await received(o.echo, 11, ip), 0);
    await ask("both", "fl11-cluster", 16);
    const here = await received(p.echo, 13, ip);
    const there = await received(o.echo, 13, ip);
    assert.equal(here + there, 16);
    assert.ok(here > 0 && there > 0, `${here} on this node, ${there} on the other node`);
  });

  test(`ND6: an egress policy of the client stops the datagrams to the other node on ${kind}`, async () => {
    const ip = await podIP(ns, p.eg);
    assert.equal(await nc(p.eg, IP[o.echo], 7, "egress"), "rc=0\n");
    assert.equal(await received(o.echo, 7, ip), 0);
  });
});

test("Host traffic: nc -u in the terminal of the tab node gets the answer of the echo pod on the worker, from the node IP", async () => {
  const r = await run(`echo host | nc -u -w 2 ${IP[POD.worker.echo]} 7`);
  assert.equal(r.out, "echo host\n");
  const node = await getJSON(`node ${TAB_NODE}`);
  const nodeIP = node.status.addresses.find((a) => a.type === "InternalIP").address;
  assert.ok(await received(POD.worker.echo, 5, nodeIP) >= 1, `no datagram from the node IP ${nodeIP}`);
});

test("nslookup in a pod on the worker asks the pod IP of CoreDNS on the tab node over UDP", async () => {
  const cd = (await getJSON("pods -n kube-system -l k8s-app=kube-dns")).items.filter(podReady);
  const far = cd.find((p) => p.spec.nodeName !== POD.worker.node);
  assert.ok(far, `CoreDNS pods: ${cd.map((p) => p.spec.nodeName).join(", ")}`);
  const k8s = (await getJSON("svc -n default kubernetes")).spec.clusterIP;
  const r = await inPod(ns, POD.worker.bb, `nslookup kubernetes.default.svc.cluster.local ${far.status.podIP}`);
  assert.equal(r.rc, 0, r.out);
  assert.ok(r.out.startsWith(`Server:\t\t${far.status.podIP}\nAddress:\t${far.status.podIP}:53\n\n`), r.out);
  assert.ok(r.out.includes(`Name:\tkubernetes.default.svc.cluster.local\nAddress: ${k8s}\n`), r.out);
});

// The last test: the policy changes the flows of the echo pods.
test("ND9: an ingress policy of the echo pods stops the datagrams from the other node, on tab and worker", async () => {
  const pairs = ["tab", "worker"].filter((k) => POD[k].node).map((k) => [POD[k].bb, POD[other[k]].echo]);
  for (const [bb, echo] of pairs) assert.equal(await nc(bb, IP[echo], 7, "before"), "echo before\nrc=0\n", `${bb} to ${echo}`);
  await apply({
    apiVersion: "networking.k8s.io/v1",
    kind: "NetworkPolicy",
    metadata: { name: "ingress-deny" },
    spec: { podSelector: { matchLabels: { echo: "udp" } }, policyTypes: ["Ingress"] },
  }, ns);
  // The informers of both nodes need a moment for the policy. A denied
  // datagram does not reach the echo pod.
  for (const [bb, echo] of pairs) {
    await waitFor(async () => {
      const n = await received(echo, 7, IP[bb]);
      return (await nc(bb, IP[echo], 7, "denied")) === "rc=0\n" && (await received(echo, 7, IP[bb])) === n;
    }, { timeout: 20, interval: 1, what: `the ingress policy of ${echo} for ${bb}` });
  }
  await kubectl(`delete networkpolicy -n ${ns} ingress-deny`);
  for (const [bb, echo] of pairs) {
    await waitFor(async () => (await nc(bb, IP[echo], 7, "after")) === "echo after\nrc=0\n",
      { timeout: 20, interval: 1, what: `an answer of ${echo} to ${bb} after the policy` });
  }
});
