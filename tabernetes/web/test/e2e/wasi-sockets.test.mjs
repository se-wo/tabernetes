// TCP sockets and name lookup of components (docs/wasi-sockets.md, phase 3,
// section 10.3, rows TC, NL, PX5 and CO17/CO19) on the tab node, and on a
// node worker for TC1/TC8 and NL1/NL2. The image is wasi-test-echo
// (wasm-images/rust/echo): $MODE selects the test mode, and each mode prints
// one line for each fact. The clients are busybox pods with nc and wget, and
// the mode "client" of the echo image.
//
// The expected values are the results of the step wasi-sockets of the
// Docker check on runwasi (docs/docker-node-check/wasi-sockets.sh, PR #84).
// Where the tab node differs from runwasi on purpose (section 11), the test
// says so.
//
// Rows that only unit tests cover (plan rule 5, or no client can see them):
//
//   TC10        TestWasiTCPPolicyTimeout and TestWasiTCPFullQueue
//               (k8s/tabernetes/tabnode_wasi_tcp_test.go): the real wait is
//               127 seconds.
//   TC12        TestWasiTCPCredit, and web/test/tcp.test.mjs (the credit of
//               the pod worker).
//   TC20, TC21  also web/test/sockets.test.mjs (the pod worker).
//   MS2 to MS4  TestWasiTCPForeignID, TestWasiTCPNotBindable,
//               TestWasiTCPDataAboveCredit.
//   AC1         TestWasiTCPChargesCPU.
//
// Each running command component uses one Wasm memory of the tab (section
// 14). The pods of the tab node share one namespace, and the clients are
// containers that exit. The namespace is deleted after the last test.
//
// Model for UDP (phase 4): one shared namespace with the servers, created in
// before(); clients that end on their own; one test for each row of 10.3.
import { before, test } from "node:test";
import assert from "node:assert/strict";
import {
  apply, forEachNode, kubectl, logs, namespace, nodeName, pod, podPhase, podReady, run, TAB_NODE, waitFor, waitForPod,
} from "./harness.mjs";
import { busyboxPod, inPod, podIP, proxyPod, running, service, sharedNamespace } from "./wasi-proxy-lib.mjs";

const ECHO = "ghcr.io/se-wo/tabernetes/wasi-test-echo:latest";
const CMD = "ghcr.io/se-wo/tabernetes/wasi-test-cmd:latest";

// The results of runwasi (wasi-sockets.sh and the report of W3.3a).
const RUNWASI = {
  // TC18 to TC21 (fact 12, rule 3 of section 8.3.2).
  errors: [
    "tc18 bind1 0.0.0.0:7100: ok",
    "tc18 bind2 without listen: ok",
    "tc18 listen1: ok",
    "tc18 bind3 with listen: address-in-use",
    "tc18 listen2: address-in-use",
    "tc18 bind 127.0.0.1 with listen on 0.0.0.0: address-in-use",
    "tc19 bind 192.0.2.7: address-not-bindable",
    "tc20 ipv6 connect 10.96.0.1:443: invalid-argument",
    "tc20 ipv6 connect [::ffff:10.96.0.1]:443: invalid-argument",
    "tc21 backlog 0: invalid-argument",
  ],
  // TC6: the listener on [::] of the mode errors.
  tc6: "tc6 bind [::]:7006: ok listen: ok",
  // TC22. The buffer sizes are the values of runwasi on the GitHub runner;
  // the tab node keeps the value of a set (section 11).
  options: [
    "option keep-alive-enabled default false, set true: ok read true, set false: ok read false",
    "option keep-alive-idle-time default 7200000000000, set 30000000000: ok read 30000000000, set 1500000000: ok read 2000000000, set 0: invalid-argument read 2000000000",
    "option keep-alive-interval default 75000000000, set 5000000000: ok read 5000000000, set 0: invalid-argument read 5000000000",
    "option keep-alive-count default 9, set 3: ok read 3, set 0: invalid-argument read 3",
    "option hop-limit default 64, set 42: ok read 42, set 0: invalid-argument read 42",
    "option receive-buffer-size default 65536, set 65536: ok read 65536, set 1: ok read 1152, set 0: invalid-argument read 1152",
    "option send-buffer-size default 8192, set 65536: ok read 65536, set 1: ok read 2304, set 0: invalid-argument read 2304",
  ],
  // Rule 11: the addresses of localhost, in this sequence.
  localhost: "lookup localhost: [::1, 127.0.0.1]",
};

// echoPod is a pod of the echo image. containers: [name, env object,
// extra fields of the container].
function echoPod(name, node, containers, { restartPolicy, labels = {} } = {}) {
  return {
    apiVersion: "v1",
    kind: "Pod",
    metadata: { name, labels: { app: name, ...labels } },
    spec: {
      runtimeClassName: "wasmtime",
      nodeSelector: { "kubernetes.io/hostname": node },
      terminationGracePeriodSeconds: 1,
      ...(restartPolicy ? { restartPolicy } : {}),
      containers: containers.map(([c, env = {}, extra = {}]) => ({
        name: c,
        image: extra.image ?? ECHO,
        env: Object.entries(env).map(([k, v]) => (typeof v === "string" ? { name: k, value: v } : { name: k, ...v })),
        ...Object.fromEntries(Object.entries(extra).filter(([k]) => k !== "image")),
      })),
    },
  };
}

const tcpProbe = (port) => ({ readinessProbe: { tcpSocket: { port }, periodSeconds: 2 } });

// lines gives the lines of the logs of a container.
const lines = async (ns, name, c) => (await logs(ns, name, c)).replace(/\r/g, "").split("\n");

// logLine waits until a container logs a line that matches re and resolves
// with the line.
const logLine = (ns, name, c, re, timeout = 120) =>
  waitFor(async () => (await lines(ns, name, c)).find((l) => re.test(l)),
    { timeout, what: `a line ${re} in the logs of ${ns}/${name}/${c}` });

// exited waits until a container has ended and resolves with its exit code.
const exited = (ns, name, c, timeout = 180) => waitFor(async () => {
  const p = await pod(ns, name);
  const t = p.status?.containerStatuses?.find((s) => s.name === c)?.state?.terminated;
  return t && { code: t.exitCode };
}, { timeout, what: `the end of ${ns}/${name}/${c}` });

// readyOf gives the ready flag of a container.
const readyOf = (p, c) => p.status?.containerStatuses?.find((s) => s.name === c)?.ready;

// probeFailures counts the events "Readiness probe failed" of a pod.
async function probeFailures(ns, name) {
  const out = await kubectl(`get events -n ${ns} --field-selector involvedObject.name=${name}`);
  return (out.match(/Readiness probe failed/g) ?? []).length;
}

// TC1, TC8, NL1, NL2 on the tab node and on a node worker: the echo server
// through a Service, a client component that finds the Service by its short
// name, and lookups of a full name and of localhost.
forEachNode(["tab", "worker"], (kind) => {
  test(`TC1, TC8, NL1, NL2: the TCP echo pod through a Service and name lookup on ${kind}`, async (t) => {
    const ns = await namespace(t, "tc");
    const node = await nodeName(kind);
    await apply([
      echoPod("echo", node, [["c", {}, tcpProbe(7)]]),
      service("echo", 7),
      busyboxPod("bb", node),
    ], ns);
    // TC1: the tcpSocket probe on the pod IP connects.
    const p = await waitForPod(ns, "echo", podReady);
    assert.equal(p.spec.nodeName, node);
    assert.equal((await lines(ns, "echo", "c"))[0], "tcp echo on 0.0.0.0:7");
    await waitForPod(ns, "bb", running);
    const nc = await inPod(ns, "bb", "echo tcp | nc -w 2 echo 7");
    assert.equal(nc.out.trim(), "echo tcp", nc.out);

    // TC8, NL1: a client component connects to the Service by its short
    // name. NL2: the full name and localhost.
    await apply(echoPod("client", node, [
      ["c", { MODE: "client", CONNECT: "echo:7", LINE: "tc1" }],
      ["fqdn", { LOOKUP: `echo.${ns}.svc.cluster.local` }, { image: CMD }],
      ["localhost", { LOOKUP: "localhost" }, { image: CMD }],
    ], { restartPolicy: "Never" }), ns);
    await waitForPod(ns, "client", podPhase("Succeeded", "Failed"));
    const ip = (await kubectl(`get svc -n ${ns} echo -o jsonpath={.spec.clusterIP}`)).trim();
    const c = await lines(ns, "client", "c");
    assert.ok(c.includes(`lookup echo: [${ip}]`), c.join("\n"));
    // On runwasi, a client to a Service shows the ClusterIP as its remote
    // address (rule 2 of section 8.3.2).
    const m = c.map((l) => l.match(/^local ([\d.]+):(\d+) peer ([\d.]+):7$/)).find(Boolean);
    assert.ok(m, c.join("\n"));
    assert.equal(m[1], await podIP(ns, "client"));
    assert.equal(m[3], ip);
    assert.ok(c.includes("answer echo tc1"), c.join("\n"));
    assert.ok(c.includes("eof"), c.join("\n"));
    assert.ok((await lines(ns, "client", "fqdn")).includes(`lookup echo.${ns}.svc.cluster.local: [${ip}]`));
    assert.ok((await lines(ns, "client", "localhost")).includes(RUNWASI.localhost));
    // NL2: nslookup in a pod of the same node gives the same address.
    const ns1 = await inPod(ns, "bb", "nslookup echo");
    assert.match(ns1.out, new RegExp(`Address:\\s*${ip.replace(/\./g, "\\.")}`), ns1.out);
  });
});

// The other rows run on the tab node, with the pods of one namespace.
const shared = sharedNamespace("so");
const ns = shared.ns;
const bb = "bb";

before(async () => {
  await shared.create();
  // The servers first. The clients start when the servers listen.
  await apply([
    busyboxPod(bb, TAB_NODE),
    // TC2 to TC5, TC15 to TC17: a server on 127.0.0.1:7 with a tcpSocket
    // probe that must fail, and a client in the same pod. DELAY_MS waits
    // until the server listens.
    echoPod("lo", TAB_NODE, [
      ["c", { ADDR: "127.0.0.1:7" }, tcpProbe(7)],
      ["client", { MODE: "client", CONNECT: "127.0.0.1:7", LINE: "lo", DELAY_MS: "5000" }],
    ], { restartPolicy: "Never" }),
    // Section 11: a pod dials its own socket on 127.0.0.1 through its
    // Service and through its pod IP. On the tab node, it connects.
    echoPod("self", TAB_NODE, [
      ["c", { ADDR: "127.0.0.1:7" }],
      ["svc", { MODE: "client", CONNECT: "self:7", LINE: "svc", DELAY_MS: "5000" }],
      ["ip", { POD_IP: { valueFrom: { fieldRef: { fieldPath: "status.podIP" } } }, MODE: "client", CONNECT: "$(POD_IP):7", LINE: "ip", DELAY_MS: "5000" }],
    ], { restartPolicy: "Never" }),
    service("self", 7),
    // TC13, TC14: a server that answers at the end of the stream.
    echoPod("eof", TAB_NODE, [["c", { MODE: "eof" }]]),
    service("eof", 7),
    // TC18 to TC21, then TC6: a listener on [::]:7006 that stays up.
    echoPod("errors", TAB_NODE, [["c", { MODE: "errors" }]]),
    // CO17, CO19.
    echoPod("poll", TAB_NODE, [["c", { MODE: "poll" }]]),
    // PX5: a proxy component on 127.0.0.1:9000 with an httpGet probe that
    // must fail, and a second proxy component on 0.0.0.0:8080.
    (() => {
      const px = proxyPod("pxlo", TAB_NODE, {
        addr: "127.0.0.1:9000",
        extra: { readinessProbe: { httpGet: { path: "/status/200", port: 9000 }, periodSeconds: 2 } },
      });
      px.spec.containers.push({ name: "front", image: px.spec.containers[0].image });
      return px;
    })(),
  ], ns);
  await waitForPod(ns, bb, running);
  await logLine(ns, "eof", "c", /^tcp echo on /);
  // TC9, TC11, TC13, NL1: a client in another pod; TC22; NL2, NL3, rule 11
  // and names outside ASCII (section 11).
  await apply([
    echoPod("client", TAB_NODE, [
      ["c", { MODE: "client", CONNECT: "eof:7", LINE: "tc13" }],
      ["refused", { MODE: "client", CONNECT: "127.0.0.1:9" }],
    ], { restartPolicy: "Never" }),
    echoPod("options", TAB_NODE, [["c", { MODE: "options" }]], { restartPolicy: "Never" }),
    echoPod("lookup", TAB_NODE, [
      ["fqdn", { LOOKUP: "kubernetes.default.svc.cluster.local" }, { image: CMD }],
      ["nl3", { MODE: "client", CONNECT: "example.com:80" }],
      ["nl3cmd", { LOOKUP: "example.com" }, { image: CMD }],
      ["idna", { MODE: "client", CONNECT: "bücher.example:80" }],
    ], { restartPolicy: "Never" }),
  ], ns);
});

test("TC2, TC3, TC4, TC5, TC17: a server on 127.0.0.1 and a client of the same pod on tab", async () => {
  // TC5: local-address gives 127.0.0.1.
  assert.equal(await logLine(ns, "lo", "c", /^tcp echo on /), "tcp echo on 127.0.0.1:7");
  // TC3, TC17: the client of the same pod sees 127.0.0.1 at the two ends.
  await exited(ns, "lo", "client");
  const a = await lines(ns, "lo", "client");
  const local = a.map((l) => l.match(/^local 127\.0\.0\.1:(\d+) peer 127\.0\.0\.1:7$/)).find(Boolean);
  assert.ok(local, a.join("\n"));
  assert.ok(a.includes("answer echo lo"), a.join("\n"));
  // The server sees 127.0.0.1 at the two ends too.
  assert.ok((await lines(ns, "lo", "c")).includes(`tcp from Ok(127.0.0.1:${local[1]}) to Ok(127.0.0.1:7)`));
  // TC9 for a client of 127.0.0.1: an ephemeral port.
  assert.ok(Number(local[1]) >= 32768 && Number(local[1]) <= 60999, local[0]);
  // TC2, TC4: another pod gets "Connection refused" from the connect.
  const ip = await podIP(ns, "lo");
  const b = await inPod(ns, bb, `wget -qO- -T 3 ${ip}:7; echo rc=$?`);
  assert.match(b.out, /Connection refused/, b.out);
  const c = await inPod(ns, bb, `echo x | nc -w 2 ${ip} 7`);
  assert.notEqual(c.rc, 0, c.out);
});

test("TC15, TC16: a tcpSocket probe of a server on 127.0.0.1 fails, kubectl port-forward connects on tab", async () => {
  // TC15: the probe fails, and an event shows the failure.
  await waitFor(async () => (await probeFailures(ns, "lo")) > 0, { what: "a probe failure event of lo" });
  assert.equal(readyOf(await pod(ns, "lo"), "c"), false);
  // TC16: port-forward from the terminal reaches the server. kill ends the
  // job.
  const pf = await run(`kubectl port-forward -n ${ns} pod/lo 17007:7 & sleep 3; echo pf | nc -w 3 127.0.0.1 17007; kill %%`);
  assert.match(pf.out.replace(/\r/g, ""), /^echo pf$/m, pf.out);
  const jobs = await run("jobs");
  assert.doesNotMatch(jobs.out, /port-forward/, jobs.out);
});

test("Section 11: a pod reaches its own socket on 127.0.0.1 through its Service and its pod IP on tab", async () => {
  await waitForPod(ns, "self", podPhase("Succeeded", "Failed", "Running"));
  for (const [c, line] of [["svc", "answer echo svc"], ["ip", "answer echo ip"]]) {
    await exited(ns, "self", c);
    const a = await lines(ns, "self", c);
    assert.ok(a.includes(line), `${c}:\n${a.join("\n")}`);
  }
  // runwasi gives connection-refused here (section 11).
});

test("TC9, TC11, TC13, NL1: a client in another pod on tab", async () => {
  await waitForPod(ns, "client", podPhase("Succeeded", "Failed"));
  const svcIP = (await kubectl(`get svc -n ${ns} eof -o jsonpath={.spec.clusterIP}`)).trim();
  const a = await lines(ns, "client", "c");
  // NL1: the short name of the Service.
  assert.ok(a.includes(`lookup eof: [${svcIP}]`), a.join("\n"));
  // TC9: an ephemeral port; the remote address is the ClusterIP (rule 2).
  const m = a.map((l) => l.match(/^local ([\d.]+):(\d+) peer ([\d.]+):7$/)).find(Boolean);
  assert.ok(m, a.join("\n"));
  assert.equal(m[1], await podIP(ns, "client"));
  assert.ok(Number(m[2]) >= 32768 && Number(m[2]) <= 60999, m[0]);
  assert.equal(m[3], svcIP);
  // TC13: the client closes its send direction, the server reads the end of
  // the stream and answers.
  for (const l of ["shutdown send: ok", "answer echo tc13", "eof"]) assert.ok(a.includes(l), `missing ${l} in\n${a.join("\n")}`);
  assert.ok((await lines(ns, "eof", "c")).includes("tcp eof after 5 bytes: Ok(5)"));
  // TC11: a port without a listener.
  assert.deepEqual((await lines(ns, "client", "refused")).filter(Boolean), ["error connect: connection-refused"]);
});

test("TC6, TC7, TC18, TC19, TC20, TC21: the error codes of fact 12 and an IPv6 listener on tab", async (t) => {
  const tc6 = await logLine(ns, "errors", "c", /^tc6 /);
  const out = await lines(ns, "errors", "c");
  for (const l of RUNWASI.errors) assert.ok(out.includes(l), `missing "${l}" in\n${out.join("\n")}`);
  // Rule 5: recorded, the unit tests of W3.1b examine it.
  t.diagnostic(out.filter((l) => /^(rule5|backlog) /.test(l)).join("; "));
  // TC6: the bind to :: gives no error. TC7: the IPv4 pod IP is refused.
  assert.equal(tc6, RUNWASI.tc6);
  const b = await inPod(ns, bb, `wget -qO- -T 3 ${await podIP(ns, "errors")}:7006; echo rc=$?`);
  assert.match(b.out, /Connection refused/, b.out);
});

test("TC22: the socket options keep their values on tab", async () => {
  await waitForPod(ns, "options", podPhase("Succeeded", "Failed"));
  assert.deepEqual((await lines(ns, "options", "c")).filter(Boolean), RUNWASI.options);
});

test("NL2, NL3: a full name, a name outside the cluster and a name outside ASCII on tab", async (t) => {
  await waitForPod(ns, "lookup", podPhase("Succeeded", "Failed"));
  const k8s = (await kubectl("get svc -n default kubernetes -o jsonpath={.spec.clusterIP}")).trim();
  assert.ok((await lines(ns, "lookup", "fqdn")).includes(`lookup kubernetes.default.svc.cluster.local: [${k8s}]`));
  // NL3: the error code of the client mode of W3.3a.
  assert.deepEqual((await lines(ns, "lookup", "nl3")).filter(Boolean), ["error lookup: name-unresolvable"]);
  // The std of Rust gives an error for the same name.
  const std = (await lines(ns, "lookup", "nl3cmd")).find((l) => l.startsWith("lookup "));
  assert.match(std, /^lookup example\.com: error /);
  t.diagnostic(std);
  // Section 11: runwasi changes the name with IDNA; the tab node gives
  // invalid-argument.
  assert.deepEqual((await lines(ns, "lookup", "idna")).filter(Boolean), ["error lookup: invalid-argument"]);
});

test("PX5: a proxy component on 127.0.0.1:9000 accepts only its own pod on tab", async () => {
  await waitForPod(ns, "pxlo", running);
  const ip = await podIP(ns, "pxlo");
  // The second proxy component of the pod gets the response.
  const a = await waitFor(async () => {
    const r = await inPod(ns, bb, `wget -qO- -T 10 ${ip}:8080/fetch/127.0.0.1:9000/status/200`);
    return /^status 200/m.test(r.out) && r.out;
  }, { timeout: 120, what: "the response of 127.0.0.1:9000 through the second component" });
  assert.match(a, /^status 200/m);
  // Another pod is refused.
  const b = await inPod(ns, bb, `wget -qO- -T 5 ${ip}:9000/status/200; echo rc=$?`);
  assert.match(b.out, /Connection refused/, b.out);
  // The httpGet probe on 9000 fails: the pod is not Ready, and an event
  // shows the failure.
  await waitFor(async () => (await probeFailures(ns, "pxlo")) > 0, { what: "a probe failure event of pxlo" });
  const p = await pod(ns, "pxlo");
  assert.equal(readyOf(p, "c"), false);
  assert.ok(!podReady(p));
});

test("CO17, CO19: a component that reads in a loop without a wait gets the data of a late client on tab", async (t) => {
  await logLine(ns, "poll", "c", /^tcp poll on /);
  const ip = await podIP(ns, "poll");
  const delay = 1000;
  await inPod(ns, bb, `{ sleep ${delay / 1000}; echo x; } | nc -w 5 ${ip} 7`);
  const l = await logLine(ns, "poll", "c", /^poll /);
  t.diagnostic(l);
  const m = l.match(/^poll (\d+) bytes after (\d+) ms$/);
  assert.ok(m, l);
  assert.equal(m[1], "2");
  // The time from the send: less than 1,000 ms (runwasi: 999 ms for the
  // whole wait of 1 s).
  assert.ok(Number(m[2]) < delay + 1000, l);
});

// TC14 last: it deletes the server eof. kubectl makes --grace-period=0 a
// grace period of 1 second without --force.
test("TC14: the client gets the end of its connection after the delete of the server on tab", async () => {
  await apply(echoPod("reader", TAB_NODE, [["c", { MODE: "client", CONNECT: "eof:7", READ_FOREVER: "1" }]], { restartPolicy: "Never" }), ns);
  await logLine(ns, "reader", "c", /^local /);
  await kubectl(`delete pod -n ${ns} eof --grace-period=0 --force --wait=false`);
  await exited(ns, "reader", "c", 60);
  const a = await lines(ns, "reader", "c");
  assert.ok(a.includes("eof"), a.join("\n"));
});
