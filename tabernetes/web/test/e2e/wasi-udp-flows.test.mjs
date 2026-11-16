// The UDP flows of the tab node (docs/wasi-sockets.md, phase 4, section 10.3,
// rows FL; section 8.8): the Service translation, sessionAffinity and
// NetworkPolicy for UDP flows with the strict mode. The servers are UDP echo
// pods of wasi-test-echo (PROTO=udp), which log one line for each datagram:
// "udp <n> bytes from <ip>:<port>". The clients are the mode udp-client of
// the echo image (one socket, thus one flow) and busybox nc -u.
//
// Rows that only unit tests cover (k8s/tabernetes/tabnode_flows_test.go,
// tabnode_netpol_udp_test.go):
//
//   FL1         TestUDPFlowTuples (W4.2a).
//   FL2 to FL4  TestUDPFlowTimeoutNoAnswer, TestUDPFlowTimeoutStream,
//               TestUDPFlowTimeoutAnswer (W4.2a) with the clock flowNow:
//               the real waits are 31 to 100 seconds (plan rule 5).
//   FL6, FL7    TestUDPPolicyIngress, TestUDPPolicyProtocol,
//               TestUDPPolicyOneDecision (W4.2b).
//   FL11        TestUDPFlowLocalPolicy (W4.2a) on the tab node. Between
//               nodes (phase 5): TestUDPNodeLocalPolicy and the test
//               "FL11: ..." of wasi-udp-nodes.test.mjs (W5.1b), with
//               endpoints on the tab node and on a node worker.
//   FL13        TestUDPFlowReleaseIP (W4.2a).
//
// The tests run one after the other in one namespace. FL10 runs before FL5,
// which deletes a pod of the Deployment.
import { before, test } from "node:test";
import assert from "node:assert/strict";
import { apply, getJSON, kubectl, podReady, TAB_NODE, waitFor, waitForPod } from "./harness.mjs";
import { busyboxPod, inPod, podIP, running, service, sharedNamespace } from "./wasi-proxy-lib.mjs";
import { echoPod, lines } from "./wasi-lib.mjs";

const shared = sharedNamespace("fl");
const ns = shared.ns;
const bb = "bb";
const IP = {};

// deployment is a Deployment of UDP echo pods with the label app=name.
function deployment(name, replicas) {
  const tpl = echoPod(name, TAB_NODE, [["c", { PROTO: "udp" }]]);
  return {
    apiVersion: "apps/v1",
    kind: "Deployment",
    metadata: { name },
    spec: {
      replicas,
      selector: { matchLabels: { app: name } },
      template: { metadata: { labels: tpl.metadata.labels }, spec: tpl.spec },
    },
  };
}

// repPods gives the names of the running pods of the Deployment rep.
const repPods = async () => (await getJSON(`pods -n ${ns} -l app=rep`)).items
  .filter((p) => !p.metadata.deletionTimestamp && podReady(p)).map((p) => p.metadata.name);

// from gives the source ports of the datagrams that the echo pod name got
// from ip.
async function from(name, ip) {
  const re = new RegExp(`^udp \\d+ bytes from ${ip.replace(/\./g, "\\.")}:(\\d+)$`);
  return (await lines(ns, name, "c")).map((l) => l.match(re)).filter(Boolean).map((m) => m[1]);
}

const policy = (name, spec) => ({
  apiVersion: "networking.k8s.io/v1",
  kind: "NetworkPolicy",
  metadata: { name },
  spec: { podSelector: { matchLabels: { app: "pol" } }, policyTypes: ["Ingress"], ...spec },
});

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

before(async () => {
  await shared.create();
  await apply([
    busyboxPod(bb, TAB_NODE),
    deployment("rep", 2),
    // FL5: the default Service of the Deployment.
    service("rep", 7, "UDP"),
    // FL10: the same pods with sessionAffinity ClientIP.
    service("aff", 7, "UDP", { sessionAffinity: "ClientIP", selector: { app: "rep" } }),
    // FL8, FL9, FL12: the server of the policies.
    echoPod("pol", TAB_NODE, [["c", { PROTO: "udp" }]]),
    service("pol", 7, "UDP"),
  ], ns);
  await waitForPod(ns, bb, running);
  await waitForPod(ns, "pol", podReady);
  await waitFor(async () => (await repPods()).length === 2, { what: "2 ready pods of rep" });
  IP.bb = await podIP(ns, bb);
  for (const s of ["rep", "aff", "pol"]) IP[s] = (await getJSON(`svc -n ${ns} ${s}`)).spec.clusterIP;
});

test("FL10: with sessionAffinity ClientIP, 10 sockets of one client get answers from one endpoint on tab", async (t) => {
  // 10 nc -u at the same time: 10 sockets with different source ports.
  const r = await waitFor(async () => {
    const o = await inPod(ns, bb, "for i in 0 1 2 3 4 5 6 7 8 9; do echo aff$i | nc -u -w 2 aff 7 & done; wait");
    return o.out.split("\n").filter((l) => /^echo aff\d$/.test(l)).length === 10 && o;
  }, { timeout: 60, what: "10 answers through the Service aff" });
  t.diagnostic(r.out.replace(/\n/g, " "));
  const ports = {};
  for (const p of await repPods()) ports[p] = new Set(await from(p, IP.bb));
  t.diagnostic(JSON.stringify(Object.fromEntries(Object.entries(ports).map(([k, v]) => [k, v.size]))));
  const used = Object.values(ports).filter((s) => s.size > 0);
  assert.equal(used.length, 1, "the datagrams of the client went to more than one endpoint");
  assert.ok(used[0].size >= 10, `${used[0].size} source ports`);
});

test("FL5: after the delete of its endpoint, a flow gets answers from the other endpoint on tab", async () => {
  await apply(echoPod("fl5", TAB_NODE, [["c", {
    MODE: "udp-client", CONNECT: `${IP.rep}:7`, COUNT: "60", INTERVAL_MS: "1000", WAIT_MS: "900", LINE: "fl5", DELAY_MS: "2000",
  }]], { restartPolicy: "Never" }), ns);
  const ip = await podIP(ns, "fl5");
  // The endpoint of the flow: the pod that got the datagrams of the client.
  const first = await waitFor(async () => {
    for (const p of await repPods()) if ((await from(p, ip)).length >= 2) return p;
  }, { timeout: 60, what: "the endpoint of the flow of fl5" });
  const port = (await from(first, ip))[0];
  const seen = (await lines(ns, "fl5", "c")).filter((l) => l === `answer echo fl5 from ${IP.rep}:7`).length;
  await kubectl(`delete pod -n ${ns} ${first} --grace-period=0 --force --wait=false`);
  // The same socket (the same source port) now reaches another endpoint.
  const other = await waitFor(async () => {
    for (const p of await repPods()) if (p !== first && (await from(p, ip)).includes(port)) return p;
  }, { timeout: 60, what: "another endpoint for the flow of fl5" });
  assert.notEqual(other, first);
  await waitFor(async () => (await lines(ns, "fl5", "c")).filter((l) => l === `answer echo fl5 from ${IP.rep}:7`).length > seen,
    { timeout: 30, what: "answers to fl5 after the delete" });
  const a = await lines(ns, "fl5", "c");
  assert.ok(a.slice(-5).some((l) => l === `answer echo fl5 from ${IP.rep}:7`), a.join("\n"));
  await kubectl(`delete pod -n ${ns} fl5 --wait=false`);
});

test("FL8, FL9, FL12: a new policy removes an open flow; a later policy that allows it works in less than 5 seconds on tab", async (t) => {
  await apply(echoPod("polc", TAB_NODE, [["c", {
    MODE: "udp-client", CONNECT: `${IP.pol}:7`, COUNT: "120", INTERVAL_MS: "1000", WAIT_MS: "500", LINE: "polc", DELAY_MS: "2000",
  }]], { restartPolicy: "Never" }), ns);
  const ip = await podIP(ns, "polc");
  const count = async () => (await from("pol", ip)).length;
  // An open flow: the server got datagrams and the client got answers.
  await waitFor(async () => (await count()) >= 3, { timeout: 60, what: "an open flow of polc" });
  // FL8, FL9: a policy that denies all ingress of pol. The strict mode
  // removes the flow (informer plus 200 ms, W4.2b): wait up to 5 seconds.
  await apply(policy("deny", {}), ns);
  await sleep(5000);
  const denied = await count();
  const sentBefore = (await lines(ns, "polc", "c")).length;
  await sleep(4000);
  assert.equal(await count(), denied, "datagrams arrived after the deny policy");
  assert.ok((await lines(ns, "polc", "c")).length > sentBefore, "the client stopped");
  // FL12: a policy that allows 7/UDP. The denied datagrams made no flow, so
  // the next datagram arrives.
  const t0 = Date.now();
  await apply(policy("allow", { ingress: [{ ports: [{ protocol: "UDP", port: 7 }] }] }), ns);
  await waitFor(async () => (await count()) > denied, { timeout: 5, interval: 0.5, what: "a datagram after the allow policy" });
  t.diagnostic(`the first datagram after the allow policy: ${Date.now() - t0} ms`);
  await kubectl(`delete networkpolicy -n ${ns} deny allow --wait=false`);
  await kubectl(`delete pod -n ${ns} polc --wait=false`);
});
