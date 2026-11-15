// ICMP echo of the ping built-in command (docs/wasi-sockets.md, phase 6,
// section 7.9, the rows IC of section 10.3), on the tab node and on a node
// worker. The pods are emulated busybox pods (no Wasm memory) in one shared
// namespace: for each node, "a" pings "b", a Service selects "b", and a
// NetworkPolicy denies the ingress of "b" for IC1.
//
// Rows that only unit tests cover (k8s/tabernetes/tabnode_icmp_test.go):
//
//   IC4, IC5  TestICMPPolicyEgressIngress: no protocol and port 0, the
//             egress of the source and the ingress of the destination.
//   IC7       TestICMPPolicyPorts: a rule with ports does not match ICMP.
//   IC8       TestICMPEachRequest: one decision for each echo request.
//   IC2       TestICMPNodeIP: a pod with a default-deny egress policy gets
//             no reply from the node IP.
//   IC3       TestICMPOtherNode: the node IP of another node and a pod IP of
//             a Docker node (a flannel pod CIDR, no tab node).
//
// The Docker part of IC3 (a ping from a tab pod to a pod of a Docker node)
// is a line of the Docker step of W5.3.
import { before, test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { apply, forEachNode, getJSON, NODES, nodeName, waitFor, waitForPod } from "./harness.mjs";
import { busyboxPod, inPod, podIP, running, service, sharedNamespace } from "./wasi-proxy-lib.mjs";

const shared = sharedNamespace("ic");
const ns = shared.ns;
const KINDS = ["tab", "worker"].filter((k) => NODES.includes(k));
const IP = {};

// received gives the number of echo replies in the statistics of ping.
const received = (out) => Number(/(\d+) packets received/.exec(out)?.[1] ?? -1);

const nodeIP = async (name) =>
  (await getJSON(`node ${name}`)).status.addresses.find((a) => a.type === "InternalIP").address;

before(async () => {
  await shared.create();
  const pods = [];
  for (const kind of KINDS) {
    const node = await nodeName(kind);
    pods.push(busyboxPod(`a-${kind}`, node), busyboxPod(`b-${kind}`, node), service(`b-${kind}`, 80));
  }
  await apply(pods, ns);
  for (const kind of KINDS) {
    for (const p of [`a-${kind}`, `b-${kind}`]) {
      await waitForPod(ns, p, running);
      IP[p] = await podIP(ns, p);
    }
  }
});

forEachNode(["tab", "worker"], (kind) => {
  const a = `a-${kind}`, b = `b-${kind}`;

  // IC1, IC4, IC5: echo replies from a pod of the same node. A policy that
  // denies the ingress of the target stops them.
  test(`IC1, IC4, IC5: ping to a pod IP of the same node on ${kind}`, async () => {
    const r = await inPod(ns, a, `ping -c 2 -i 0.2 -W 1 ${IP[b]}`);
    assert.equal(r.rc, 0, r.out);
    assert.equal(received(r.out), 2, r.out);
    assert.match(r.out, new RegExp(`64 bytes from ${IP[b].replace(/\./g, "\\.")}: seq=1 ttl=64`));
    await apply({
      apiVersion: "networking.k8s.io/v1",
      kind: "NetworkPolicy",
      metadata: { name: `deny-${b}` },
      spec: { podSelector: { matchLabels: { app: b } }, policyTypes: ["Ingress"] },
    }, ns);
    // The tab node of the pods sees the new policy after its informer.
    await waitFor(async () => received((await inPod(ns, a, `ping -c 1 -W 1 ${IP[b]}`)).out) === 0,
      { timeout: 20, interval: 1, what: `the deny policy of ${b} on ${kind}` });
    const d = await inPod(ns, a, `ping -c 2 -i 0.2 -W 1 ${IP[b]}`);
    assert.equal(d.rc, 1, d.out);
    assert.equal(received(d.out), 0, d.out);
  });

  // IC2: an echo reply from the node IP of the same node.
  test(`IC2: ping to the node IP on ${kind}`, async () => {
    const ip = await nodeIP(await nodeName(kind));
    const r = await inPod(ns, a, `ping -c 1 -W 1 ${ip}`);
    assert.equal(r.rc, 0, r.out);
    assert.equal(received(r.out), 1, r.out);
  });

  // IC6: a ClusterIP gives no echo reply.
  test(`IC6: ping to a ClusterIP on ${kind}`, async () => {
    const cip = (await getJSON(`svc -n ${ns} ${b}`)).spec.clusterIP;
    const r = await inPod(ns, a, `ping -c 1 -W 1 ${cip}`);
    assert.equal(r.rc, 1, r.out);
    assert.equal(received(r.out), 0, r.out);
  });
});

// IC3: from a tab pod to a pod and to the node IP of the node worker.
if (KINDS.length === 2) {
  test("IC3: ping from the tab node to the node worker shows that ICMP between nodes is not available", async () => {
    const node = await nodeName("worker");
    for (const ip of [IP["a-worker"], await nodeIP(node)]) {
      const r = await inPod(ns, "a-tab", `ping -c 3 -W 1 ${ip}`);
      assert.equal(r.rc, 1, r.out);
      assert.ok(r.out.includes(`ping: ${ip} is on node ${node}: ICMP between nodes is not available\n`), r.out);
      assert.equal(received(r.out), -1, r.out); // no statistics: ping ends at once
    }
  });
}

// IC7, IC8: README.md contains the two texts.
test("IC7, IC8: README.md shows the ports rule and the examination of each echo request", () => {
  const readme = fs.readFileSync(path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../../README.md"), "utf8");
  assert.ok(readme.includes("A NetworkPolicy rule with `ports` does not match ICMP."), "IC7");
  assert.ok(readme.includes("A tab node examines each echo request"), "IC8 tab");
  assert.ok(readme.includes("A Docker node examines only the first echo request of an ICMP flow"), "IC8 Docker");
});
