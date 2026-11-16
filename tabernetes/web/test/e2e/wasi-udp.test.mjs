// UDP sockets of components on a tab node (docs/wasi-sockets.md, phase 4,
// section 10.3, rows UD, AC1 and the UDP part of TC22), on the tab node, and
// on a node worker for UD2, UD7, UD15 and UD19. The flows (rows FL) are in
// wasi-udp-flows.test.mjs. The image is wasi-test-echo
// (wasm-images/rust/echo): $MODE selects the UDP mode (udp-client,
// udp-errors, udp-options, udp-queue, udp-flood), and each mode prints one
// line for each fact. The clients are busybox pods with nc -u and nslookup,
// and the modes of the echo image. All clients end on their own.
//
// The expected values are the results of the step wasi-udp of the Docker
// check on runwasi (docs/docker-node-check/wasi-udp.sh, PR #91, the object
// RUNWASI). Where the tab node differs from runwasi on purpose (section 11),
// the object TAB has the value of the tab node and the test says so.
//
// Rows that only unit tests cover (no client can see them, or plan rule 5):
//
//   AC1         TestWasiUDPServiceChargesCPU
//               (k8s/tabernetes/tabnode_wasi_udp_acct_test.go), the
//               combined test: 10 MB through a UDP Service. Also
//               TestWasiUDPChargesCPU and TestUDPChargesCPU; the TCP part is
//               TestWasiTCPChargesCPU.
//   AC3         TestCoreDNSChargeUDP (W4.3).
//   MS2 to MS4  TestWasiUDPForeignID, TestWasiUDPNotBindable,
//               TestWasiUDPDatagramAboveCredit (W4.4a).
//   Rule 6      the credits: TestWasiUDPNoCredit (W4.4a) and
//               web/test/udp.test.mjs (W4.4b). The pod worker part of UD14:
//               web/test/udp.test.mjs.
//
// UDP between nodes (phase 5) is in wasi-udp-nodes.test.mjs: no test of
// this file has a client on the tab and a server on the node worker. Each running command component uses one Wasm
// memory of the tab (section 14): the pods share one namespace, the clients
// are containers that exit, and the namespace is deleted after the last
// test.
//
// Model for UDP between nodes (phase 5): one shared namespace with the
// servers, created in before(); the clients get the IPs of the servers as
// literals, so that a lookup does not change the ports; one test for each
// row of 10.3.
import { before, test } from "node:test";
import assert from "node:assert/strict";
import { apply, forEachNode, getJSON, kubectl, namespace, nodeName, podPhase, podReady, TAB_NODE, waitFor, waitForPod } from "./harness.mjs";
import { busyboxPod, inPod, podIP, running, service, sharedNamespace } from "./wasi-proxy-lib.mjs";
import { echoPod, exited, lines, logLine } from "./wasi-lib.mjs";

// The results of runwasi: the "facts:" lines of the step wasi-udp (Docker
// check run 43710146855, the head of PR #91) and the report of W4.5a.
const RUNWASI = {
  // The lines of udp-errors (rules 2 to 7 of section 8.3.3, fact 10). The
  // line "fact port of bind 0.0.0.0:0" has a random port on runwasi; the
  // test examines it separately.
  errors: [
    "rule2 bind 0.0.0.0:7200: ok",
    "rule2 bind 0.0.0.0:7200 again: address-in-use",
    "rule2 bind 127.0.0.1:7200 with 0.0.0.0:7200: address-in-use",
    "rule2 bind 127.0.0.1:7201: ok then 0.0.0.0:7201: address-in-use",
    "rule2 udp bind pod-ip:7202: ok then 127.0.0.1:7202: ok",
    "rule2 udp bind 127.0.0.1:7203: ok then pod-ip:7203: ok",
    "rule2 tcp listen pod-ip:7204: ok ok then 127.0.0.1:7204: ok ok",
    "rule2 udp bind 0.0.0.0:7204 with tcp listeners on 7204: ok",
    "rule2 local before bind: invalid-state",
    "rule4 stream without bind: invalid-state",
    "rule2 bind 0.0.0.0:0: ok local 0.0.0.0:ephemeral",
    "rule3 bind 127.0.0.1:0: ok local 127.0.0.1:ephemeral",
    "rule5 stream 127.0.0.1:7210: ok",
    "rule5 send 65507: ok",
    "rule5 send 65508: datagram-too-large",
    "rule5 check-send after 65508: 16",
    "rule5 send 5 after 65508: ok",
    "rule5 unconnected send 65508: datagram-too-large",
    "rule5 server received: 65507 5",
    "rule6 check-send unconnected: 16",
    "rule6 check-send connected: 16",
    "rule6 at once after stream: check-send 16, send 1, check-send 16",
    "rule6 check-send after a send: 16",
    "rule6 receive 100 after 40 datagrams: 40",
    "rule6 receive 0: 0",
    "rule6 receive max: 0",
    "rule7 connected send, receive, receive, send, send, send, receive: ok, empty, empty, connection-refused, ok, connection-refused, empty",
    "rule7 check-send with a pending error: 16",
    "rule7 connected send ok, receive with a wait of 1 s: empty after 1000 ms, send: connection-refused",
    "rule7 connected send, send, receive, receive, receive: ok, connection-refused, empty, empty, empty",
    "rule7 send 1: ok, a server binds, send 2: connection-refused, send 3: ok, the server receives: 3",
    "rule7 unconnected send, receive, send, send, receive: ok, empty, ok, ok, empty",
    "rule7 connected to pod-ip:7224 with a socket on 127.0.0.1:7224: send ok, send connection-refused",
    "rule4 send without address, not connected: invalid-argument",
    "rule4 send to 0.0.0.0:7: invalid-argument",
    "rule4 stream 0.0.0.0:7: invalid-argument",
    "rule4 send to 127.0.0.1:0: invalid-argument",
    "rule4 stream 127.0.0.1:0: invalid-argument",
    "rule4 send to 224.0.0.1:7: ok",
    "rule4 stream 224.0.0.1:7: ok",
    "rule4 send to 255.255.255.255:7: access-denied",
    "rule4 stream 255.255.255.255:7: access-denied",
    "rule4 send to [2001:db8::1]:7: invalid-argument",
    "rule4 stream [2001:db8::1]:7: invalid-argument",
    "rule4 send to [::ffff:127.0.0.1]:7: invalid-argument",
    "rule4 stream [::ffff:127.0.0.1]:7: invalid-argument",
    "rule4 connected send to another address: invalid-argument",
    "rule4 connected send to its remote address: ok",
    "rule4 stream without address on a connected socket: ok remote invalid-state",
    "rule4 ipv6 bind [::]:0: ok",
    "rule4 ipv6 stream [2001:db8::1]:7: remote-unreachable",
    "rule4 ipv6 send to [2001:db8::1]:7: remote-unreachable",
    "rule4 ipv6 send to 127.0.0.1:7: invalid-argument",
    "rule4 ipv6 send to [::ffff:127.0.0.1]:7: invalid-argument",
    "rule4 ipv6 send to [ff02::1]:7: ok",
    "rule4 ipv6 bind [::1]:7230: ok, bind [::1]:0: ok, send to [::1]:7230: ok, receive: 1 datagrams of 2 bytes from [::1]:ephemeral",
    "rule2 ipv6 bind [::]:7231 with 0.0.0.0:7231: ok",
    "addr local of 0.0.0.0:0 after a send: 0.0.0.0:ephemeral",
    "addr 0.0.0.0:0 sends to 127.0.0.1:7240 (server on 0.0.0.0:7240): ok, the server sees 127.0.0.1:ephemeral, the answer comes from 127.0.0.1:7240",
  ],
  // The UDP part of TC22. The defaults are the values of the GitHub runner;
  // the pod worker has the same values (UDP_DEFAULTS of sockets.js).
  options: [
    "option unicast-hop-limit default 64, set 42: ok read 42, set 255: ok read 255, set 0: invalid-argument read 255",
    "option receive-buffer-size default 524288, set 65536: ok read 65536, set 1: ok read 1152, set 0: invalid-argument read 1152",
    "option send-buffer-size default 106496, set 65536: ok read 65536, set 1: ok read 2304, set 0: invalid-argument read 2304",
    "option after bind unicast-hop-limit 64 receive-buffer-size 524288 send-buffer-size 106496",
    "option ipv6 unicast-hop-limit 64 receive-buffer-size 524288 send-buffer-size 106496",
  ],
  // UD5, UD6: Linux kept 455 of 1,000 datagrams of 1,024 bytes.
  queue: ["received 455 datagrams of 1024 bytes", "received bytes 465920"],
  flood: ["sent 1000 errors 0"],
  // UD8, UD9, UD21: a connected client that sends 3 datagrams, one each
  // 1.5 seconds: only the second send gets the error (rule 7).
  refused: ["no answer", "error send: connection-refused", "no answer"],
  // UD12 on the GitHub runner, also UD10 and UD11 there (fact 17 expects
  // connection-refused; the ICMP error does not reach the pod).
  noAnswer: ["no answer", "no answer", "no answer"],
};

// The values of the tab node where it differs from runwasi on purpose. Each
// item has its row in section 11.
const TAB = {
  errors: {
    // "A bind to 127.0.0.1:P and a bind to the pod IP with the same port P,
    // TCP or UDP": the two IPs are one IP of the table of the ports in use.
    "rule2 udp bind pod-ip:7202: ok then 127.0.0.1:7202: ok": "rule2 udp bind pod-ip:7202: ok then 127.0.0.1:7202: address-in-use",
    "rule2 udp bind 127.0.0.1:7203: ok then pod-ip:7203: ok": "rule2 udp bind 127.0.0.1:7203: ok then pod-ip:7203: address-in-use",
    "rule2 tcp listen pod-ip:7204: ok ok then 127.0.0.1:7204: ok ok": "rule2 tcp listen pod-ip:7204: ok ok then 127.0.0.1:7204: address-in-use invalid-state",
    // "A UDP socket of the family ipv6 on ::1 that sends to ::1 of its own
    // pod": the send gives remote-unreachable, and nothing arrives.
    "rule4 ipv6 bind [::1]:7230: ok, bind [::1]:0: ok, send to [::1]:7230: ok, receive: 1 datagrams of 2 bytes from [::1]:ephemeral":
      "rule4 ipv6 bind [::1]:7230: ok, bind [::1]:0: ok, send to [::1]:7230: remote-unreachable, receive: empty",
    // "A dial from a pod to its own socket on 127.0.0.1 through the pod IP
    // ..., also a UDP datagram": it arrives, so no error.
    "rule7 connected to pod-ip:7224 with a socket on 127.0.0.1:7224: send ok, send connection-refused":
      "rule7 connected to pod-ip:7224 with a socket on 127.0.0.1:7224: send ok, send ok",
    // "A send of a connected UDP socket at once after a send that the tab
    // node refuses": the error message of the Go side usually comes after
    // the second send, so a later send gets the error. If it comes before,
    // the tab node gives the value of runwasi. An array lists each value
    // that passes.
    "rule7 connected send, receive, receive, send, send, send, receive: ok, empty, empty, connection-refused, ok, connection-refused, empty": [
      "rule7 connected send, receive, receive, send, send, send, receive: ok, empty, empty, connection-refused, ok, ok, empty",
      "rule7 connected send, receive, receive, send, send, send, receive: ok, empty, empty, connection-refused, ok, connection-refused, empty",
    ],
    // "A UDP socket on 0.0.0.0 that sends to 127.0.0.1 of its own pod": the
    // answers come from the pod IP. The server on 0.0.0.0 sees the pod IP too.
    "addr 0.0.0.0:0 sends to 127.0.0.1:7240 (server on 0.0.0.0:7240): ok, the server sees 127.0.0.1:ephemeral, the answer comes from 127.0.0.1:7240":
      "addr 0.0.0.0:0 sends to 127.0.0.1:7240 (server on 0.0.0.0:7240): ok, the server sees pod-ip:ephemeral, the answer comes from pod-ip:7240",
  },
  // "The datagrams that a UDP socket keeps before a receive": 256 KiB of
  // payload (UD5).
  queue: ["received 256 datagrams of 1024 bytes", "received bytes 262144"],
  // UD10, UD11: the tab node refuses as fact 17 says.
  noEndpoints: RUNWASI.refused,
};

// clientLines gives the result lines of a udp-client container.
const clientLines = async (ns, name, c) =>
  (await lines(ns, name, c)).filter((l) => /^(answer |no answer$|error )/.test(l));

// clusterIP gives the ClusterIP of a Service.
const clusterIP = async (ns, name) => (await getJSON(`svc -n ${ns} ${name}`)).spec.clusterIP;

// The udp-client mode: one container that sends to target.
const udpClient = (target, env = {}) => ({ MODE: "udp-client", CONNECT: target, ...env });

// UD2, UD7, UD15, UD19 on the tab node and on a node worker: the UDP echo
// pod through a Service. The answers have the ClusterIP and the port of the
// Service as their source, for a connected socket and for a socket without
// stream to an address. The server sees the pod IP of the client.
forEachNode(["tab", "worker"], (kind) => {
  test(`UD2, UD7, UD15, UD19: the UDP echo pod through a Service on ${kind}`, async (t) => {
    const ns = await namespace(t, "ud");
    const node = await nodeName(kind);
    await apply([
      echoPod("echo", node, [["c", { PROTO: "udp" }]]),
      service("echo", 7, "UDP"),
      busyboxPod("bb", node),
    ], ns);
    await waitForPod(ns, "echo", podReady);
    assert.equal(await logLine(ns, "echo", "c", /^udp echo on /), "udp echo on 0.0.0.0:7");
    await waitForPod(ns, "bb", running);
    // UD19: nc -u in a pod of the same node. The Service can need a moment
    // for its endpoint, so the test sends again.
    const nc = await waitFor(async () => {
      const r = await inPod(ns, "bb", "echo udp | nc -u -w 2 echo 7; echo rc=$?");
      return r.out.startsWith("echo udp\n") && r.out;
    }, { timeout: 60, what: "the answer of the echo pod to nc -u" });
    assert.equal(nc, "echo udp\nrc=0\n");
    const cip = await clusterIP(ns, "echo");
    await apply(echoPod("client", node, [
      ["svc", udpClient("echo:7", { CONNECTED: "1", COUNT: "2", INTERVAL_MS: "500", LINE: "svc" })],
      ["unc", udpClient("echo:7", { LINE: "unc" })],
    ], { restartPolicy: "Never" }), ns);
    await waitForPod(ns, "client", podPhase("Succeeded", "Failed"));
    const ip = await podIP(ns, "client");
    const a = await lines(ns, "client", "svc");
    assert.ok(a.includes(`lookup echo: [${cip}]`), a.join("\n"));
    assert.match(a.find((l) => l.startsWith("local ")) ?? "", new RegExp(`^local ${ip.replace(/\./g, "\\.")}:\\d+$`), a.join("\n"));
    assert.ok(a.includes(`peer ${cip}:7`), a.join("\n"));
    assert.deepEqual(await clientLines(ns, "client", "svc"), [`answer echo svc from ${cip}:7`, `answer echo svc from ${cip}:7`]);
    const b = await lines(ns, "client", "unc");
    assert.match(b.find((l) => l.startsWith("local ")) ?? "", /^local 0\.0\.0\.0:\d+$/, b.join("\n"));
    assert.deepEqual(await clientLines(ns, "client", "unc"), [`answer echo unc from ${cip}:7`]);
    // UD15: the server sees the pod IP of the client, and the boundaries of
    // each datagram ("svc" and "unc" without a newline: 3 bytes).
    const s = await lines(ns, "echo", "c");
    const from = s.filter((l) => l.startsWith(`udp 3 bytes from ${ip}:`));
    assert.equal(from.length, 3, s.join("\n"));
  });
});

// The other rows run on the tab node, with the pods of one namespace.
const shared = sharedNamespace("ud");
const ns = shared.ns;
const bb = "bb";
const IP = {};

before(async () => {
  await shared.create();
  const fqdn = (svc) => `${svc}.${ns}.svc.cluster.local`;
  // The servers first.
  await apply([
    busyboxPod(bb, TAB_NODE),
    echoPod("echo", TAB_NODE, [["c", { PROTO: "udp" }]]),
    service("echo", 7, "UDP"),
    // UD10, UD11: a Service without endpoints.
    service("none", 7, "UDP"),
    // UD13: the container "self" sends to the Service of its own pod.
    echoPod("self", TAB_NODE, [
      ["c", { PROTO: "udp" }],
      ["self", udpClient(`${fqdn("self")}:7`, { CONNECTED: "1", COUNT: "3", LINE: "self", DELAY_MS: "8000" })],
    ], { restartPolicy: "Never" }),
    service("self", 7, "UDP"),
    // UD20: a server on 127.0.0.1:7 and a client of the same pod. Section
    // 11: the pod reaches its own socket on 127.0.0.1 through its pod IP and
    // through its Service. A client that exits makes the pod not ready, so
    // the client of the Service sends first.
    echoPod("lo", TAB_NODE, [
      ["c", { PROTO: "udp", ADDR: "127.0.0.1:7" }],
      ["client", udpClient("127.0.0.1:7", { LINE: "lo", DELAY_MS: "12000" })],
      ["ip", { POD_IP: { valueFrom: { fieldRef: { fieldPath: "status.podIP" } } }, ...udpClient("$(POD_IP):7", { LINE: "ip", DELAY_MS: "12000" })}],
      ["svc", udpClient(`${fqdn("lo")}:7`, { COUNT: "2", INTERVAL_MS: "500", LINE: "svc", DELAY_MS: "8000" })],
    ], { restartPolicy: "Never" }),
    service("lo", 7, "UDP"),
    // UD14, rules 2 to 7, fact 10 and the UDP part of TC22.
    echoPod("facts", TAB_NODE, [
      ["errors", { MODE: "udp-errors", POD_IP: { valueFrom: { fieldRef: { fieldPath: "status.podIP" } } } }],
      ["options", { MODE: "udp-options" }],
    ], { restartPolicy: "Never" }),
    // UD5, UD6: 1,000 datagrams of 1,024 bytes to a server that does not
    // read until the flood is over.
    echoPod("queue", TAB_NODE, [
      ["server", { MODE: "udp-queue", ADDR: "0.0.0.0:7300", READ_DELAY_MS: "8000" }],
      ["flood", { MODE: "udp-flood", CONNECT: "127.0.0.1:7300", DELAY_MS: "3000" }],
    ], { restartPolicy: "Never" }),
  ], ns);
  await waitForPod(ns, "echo", podReady);
  IP.echo = await podIP(ns, "echo");
  IP.lo = await podIP(ns, "lo");
  IP.echoSvc = await clusterIP(ns, "echo");
  IP.none = await clusterIP(ns, "none");
  // The clients get the IPs as literals: no lookup before the bind.
  const refused = { CONNECTED: "1", COUNT: "3", INTERVAL_MS: "500", WAIT_MS: "1000", DELAY_MS: "3000" };
  await apply([
    // UD3, UD4: two pods bind to port 0 at the same time. Also UD2, UD7,
    // UD15 with the IP of the Service.
    echoPod("eph1", TAB_NODE, [["c", udpClient(`${IP.echoSvc}:7`, { CONNECTED: "1", LINE: "eph1", DELAY_MS: "3000" })]], { restartPolicy: "Never" }),
    echoPod("eph2", TAB_NODE, [["c", udpClient(`${IP.echoSvc}:7`, { LINE: "eph2", DELAY_MS: "3000" })]], { restartPolicy: "Never" }),
    echoPod("refused", TAB_NODE, [
      // UD8, UD9: the pod IP of the echo pod, port 9 without a socket. A
      // client without connect gets no error.
      ["noport", udpClient(`${IP.echo}:9`, refused)],
      ["noport-unc", udpClient(`${IP.echo}:9`, { ...refused, CONNECTED: "" })],
      // UD10, UD11: a Service without endpoints.
      ["noep", udpClient(`${IP.none}:7`, refused)],
      // UD12: the ClusterIP of echo with port 9, which no Service has.
      ["nosvc", udpClient(`${IP.echoSvc}:9`, refused)],
      // UD21: the pod IP of lo, where only 127.0.0.1:7 has a socket.
      ["lo", udpClient(`${IP.lo}:7`, refused)],
    ], { restartPolicy: "Never" }),
  ], ns);
  await waitForPod(ns, bb, running);
});

test("UD1, UD16, UD17, UD18: kube-dns has 53/UDP and 53/TCP, nslookup gets answers over UDP and TCP on tab", async () => {
  const dns = await getJSON("svc -n kube-system kube-dns");
  const ports = dns.spec.ports.filter((p) => p.port === 53).map((p) => p.protocol ?? "TCP").sort();
  assert.deepEqual(ports, ["TCP", "UDP"]);
  // CoreDNS runs on the tab node: else the datagrams to kube-dns go to
  // another node (phase 5), and this test is about the tab node.
  const cd = (await getJSON("pods -n kube-system -l k8s-app=kube-dns")).items;
  assert.ok(cd.some((p) => p.spec.nodeName === TAB_NODE && podReady(p)), `CoreDNS pods: ${cd.map((p) => p.spec.nodeName).join(", ")}`);
  const k8s = await clusterIP("default", "kubernetes");
  const dnsIP = dns.spec.clusterIP;
  const want = `Name:\tkubernetes.default.svc.cluster.local\nAddress: ${k8s}\n`;
  // The default server: over UDP, as kube-dns has a ready endpoint for
  // 53/UDP on this tab node (UD19, kubeDNSProto).
  for (const line of ["nslookup kubernetes.default", `nslookup kubernetes.default.svc.cluster.local ${dnsIP}`, `nslookup -vc kubernetes.default.svc.cluster.local ${dnsIP}`]) {
    const r = await inPod(ns, bb, line);
    assert.equal(r.rc, 0, `${line}: ${r.out}`);
    assert.ok(r.out.startsWith(`Server:\t\t${dnsIP}\nAddress:\t${dnsIP}:53\n\n`), `${line}: ${r.out}`);
    assert.ok(r.out.includes(want), `${line}: ${r.out}`);
  }
  // UD18: a raw query over UDP with nc -u to the ClusterIP of kube-dns
  // gets one answer from CoreDNS.
  const m = await dnsUDP(dnsIP, "kubernetes.default.svc.cluster.local");
  assert.equal(m.rcode, 0);
  assert.equal(m.tc, false);
  assert.equal(m.answers, 1);
});

test("UD22: an answer over UDP with 40 addresses has the TC bit, and nslookup gets it over TCP on tab", async () => {
  // A Service without a selector, with a manual EndpointSlice of 40
  // addresses. The Corefile does not change.
  const addrs = Array.from({ length: 40 }, (_, i) => `10.244.250.${i + 1}`);
  await apply([
    { apiVersion: "v1", kind: "Service", metadata: { name: "many" }, spec: { clusterIP: "None", ports: [{ name: "x", port: 80, protocol: "TCP" }] } },
    {
      apiVersion: "discovery.k8s.io/v1",
      kind: "EndpointSlice",
      metadata: { name: "many-1", labels: { "kubernetes.io/service-name": "many" } },
      addressType: "IPv4",
      ports: [{ name: "x", port: 80, protocol: "TCP" }],
      endpoints: addrs.map((a) => ({ addresses: [a], conditions: { ready: true } })),
    },
  ], ns);
  const dnsIP = await clusterIP("kube-system", "kube-dns");
  const name = `many.${ns}.svc.cluster.local`;
  // nslookup asks over UDP, gets the TC bit and asks again over TCP.
  const r = await waitFor(async () => {
    const o = await inPod(ns, bb, `nslookup ${name} ${dnsIP}`);
    return o.out.split("\n").filter((l) => l.startsWith("Address: 10.244.250.")).length === 40 && o;
  }, { timeout: 60, what: `40 addresses of ${name}` });
  assert.equal(r.rc, 0, r.out);
  // The answer over UDP: the TC bit, the header and the question, no
  // records (section 11).
  const m = await dnsUDP(dnsIP, name);
  assert.equal(m.tc, true);
  assert.equal(m.rcode, 0);
  assert.equal(m.answers, 0);
});

test("UD2, UD3, UD4, UD7, UD15: two pods bind to port 0 at the same time and each gets port 32768 on tab", async () => {
  for (const p of ["eph1", "eph2"]) await exited(ns, p, "c");
  const a = await lines(ns, "eph1", "c");
  // A connected socket shows the pod IP after stream (rule 3).
  assert.ok(a.includes(`local ${await podIP(ns, "eph1")}:32768`), a.join("\n"));
  assert.ok(a.includes(`peer ${IP.echoSvc}:7`), a.join("\n"));
  assert.deepEqual(await clientLines(ns, "eph1", "c"), [`answer echo eph1 from ${IP.echoSvc}:7`]);
  const b = await lines(ns, "eph2", "c");
  assert.ok(b.includes("local 0.0.0.0:32768"), b.join("\n"));
  assert.deepEqual(await clientLines(ns, "eph2", "c"), [`answer echo eph2 from ${IP.echoSvc}:7`]);
});

test("UD5, UD6: a server that does not read keeps 256 KiB of datagrams, the sender gets no error on tab", async (t) => {
  for (const c of ["server", "flood"]) await exited(ns, "queue", c);
  assert.deepEqual((await lines(ns, "queue", "flood")).filter(Boolean), RUNWASI.flood);
  const s = (await lines(ns, "queue", "server")).filter(Boolean);
  assert.equal(s[0], "udp queue on 0.0.0.0:7300 receive-buffer-size 524288");
  // runwasi: RUNWASI.queue (section 11).
  assert.deepEqual(s.slice(1), TAB.queue);
  t.diagnostic(`runwasi: ${RUNWASI.queue.join("; ")}`);
});

test("UD8, UD9: a connected client to a port without a socket gets connection-refused on tab", async () => {
  for (const c of ["noport", "noport-unc"]) await exited(ns, "refused", c);
  assert.ok((await lines(ns, "refused", "noport")).includes(`peer ${IP.echo}:9`));
  assert.deepEqual(await clientLines(ns, "refused", "noport"), RUNWASI.refused);
  // A socket without stream to an address gets no error.
  assert.deepEqual(await clientLines(ns, "refused", "noport-unc"), RUNWASI.noAnswer);
});

test("UD10, UD11: a connected client to a Service without endpoints gets connection-refused on tab", async (t) => {
  await exited(ns, "refused", "noep");
  // On the GitHub runner, runwasi gives no answer and no error (10.3).
  assert.deepEqual(await clientLines(ns, "refused", "noep"), TAB.noEndpoints);
  t.diagnostic(`runwasi on the GitHub runner: ${RUNWASI.noAnswer.join("; ")}`);
});

test("UD12: a client to a ClusterIP port without a Service gets no error and no answer on tab", async () => {
  await exited(ns, "refused", "nosvc");
  assert.deepEqual(await clientLines(ns, "refused", "nosvc"), RUNWASI.noAnswer);
});

test("UD13: the echo pod sends to its own Service and gets its answer on tab", async () => {
  await exited(ns, "self", "self");
  const cip = await clusterIP(ns, "self");
  const a = await clientLines(ns, "self", "self");
  // The first datagram can come before the endpoint of the Service.
  assert.ok(a.includes(`answer echo self from ${cip}:7`), a.join("\n"));
  assert.ok(a.every((l) => l === `answer echo self from ${cip}:7` || l === "no answer"), a.join("\n"));
});

test("UD14, rules 2 to 7: the facts of udp-errors are the values of runwasi on tab", async (t) => {
  await exited(ns, "facts", "errors");
  const ip = await podIP(ns, "facts");
  const out = (await lines(ns, "facts", "errors")).filter(Boolean)
    .map((l) => l.replaceAll(`${ip}:ephemeral`, "pod-ip:ephemeral").replaceAll(`${ip}:`, "pod-ip:"));
  const missing = [];
  for (const l of RUNWASI.errors) {
    const want = [TAB.errors[l] ?? l].flat();
    if (!want.some((w) => out.includes(w))) missing.push(want.join("\n  or "));
    if (TAB.errors[l]) t.diagnostic(`section 11: runwasi "${l}"`);
  }
  assert.deepEqual(missing, [], `the lines of the tab node:\n${out.join("\n")}`);
  // Rule 2: port 0 gives an ephemeral port.
  const port = out.map((l) => l.match(/^fact port of bind 0\.0\.0\.0:0: 0\.0\.0\.0:(\d+)$/)).find(Boolean);
  assert.ok(port && Number(port[1]) >= 32768 && Number(port[1]) <= 60999, out.join("\n"));
});

test("TC22 (UDP): the UDP options of runwasi on tab", async () => {
  await exited(ns, "facts", "options");
  assert.deepEqual((await lines(ns, "facts", "options")).filter(Boolean), RUNWASI.options);
});

test("UD19: nc -u gets the answer of the UDP echo pod, and no text for a refused datagram on tab", async () => {
  const a = await inPod(ns, bb, "echo udp | nc -u -w 2 echo 7; echo rc=$?");
  assert.equal(a.out, "echo udp\nrc=0\n");
  // As busybox on runwasi (fact 9 of W4.5a): no text, exit 0.
  const b = await inPod(ns, bb, `echo x | nc -u -w 2 ${IP.echo} 9; echo rc=$?`);
  assert.equal(b.out, "rc=0\n");
});

test("UD20, UD21: a UDP server on 127.0.0.1 receives from its own pod, another pod gets connection-refused on tab", async () => {
  assert.equal(await logLine(ns, "lo", "c", /^udp echo on /), "udp echo on 127.0.0.1:7");
  await exited(ns, "lo", "client");
  const a = await lines(ns, "lo", "client");
  const local = a.map((l) => l.match(/^local 0\.0\.0\.0:(\d+)$/)).find(Boolean);
  assert.ok(local, a.join("\n"));
  // "A UDP socket on 0.0.0.0 that sends to 127.0.0.1 of its own pod": the
  // answer comes from the pod IP; runwasi gives 127.0.0.1:7 (section 11).
  assert.deepEqual(await clientLines(ns, "lo", "client"), [`answer echo lo from ${IP.lo}:7`]);
  // The server on 127.0.0.1 sees 127.0.0.1 as the source, as on runwasi.
  assert.ok((await lines(ns, "lo", "c")).includes(`udp 2 bytes from 127.0.0.1:${local[1]}`));
  // UD21: the second send gets the error, as on runwasi.
  await exited(ns, "refused", "lo");
  assert.deepEqual(await clientLines(ns, "refused", "lo"), RUNWASI.refused);
});

test("Section 11: a pod reaches its own UDP socket on 127.0.0.1 through its pod IP and through its Service on tab", async (t) => {
  for (const c of ["ip", "svc"]) await exited(ns, "lo", c);
  const ip = await clientLines(ns, "lo", "ip");
  assert.match(ip[0] ?? "", /^answer echo ip from [\d.]+:7$/, ip.join("\n"));
  const cip = await clusterIP(ns, "lo");
  const svc = await clientLines(ns, "lo", "svc");
  assert.ok(svc.includes(`answer echo svc from ${cip}:7`), svc.join("\n"));
  t.diagnostic(`pod IP: ${ip.join("; ")}; Service: ${svc.join("; ")}`);
  // runwasi: nothing arrives, and the socket on 127.0.0.1 gets no datagram.
});

// dnsUDP sends a query of the type A for name with nc -u from the busybox
// pod to server:53 and gives the flags of the answer: rcode, tc and the
// number of answers.
async function dnsUDP(server, name) {
  const q = [0x12, 0x34, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0];
  for (const label of name.split(".")) q.push(label.length, ...Buffer.from(label));
  q.push(0, 0, 1, 0, 1);
  const r = await inPod(ns, bb, `echo ${Buffer.from(q).toString("base64")} | base64 -d | nc -u -w 2 ${server} 53 | base64 -w 0`);
  const m = Buffer.from(r.out.trim(), "base64");
  assert.ok(m.length >= 12 && m[0] === 0x12 && m[1] === 0x34, `the answer of ${server}: ${r.out}`);
  return { rcode: m[3] & 0x0f, tc: (m[2] & 0x02) !== 0, answers: m.readUInt16BE(6) };
}
