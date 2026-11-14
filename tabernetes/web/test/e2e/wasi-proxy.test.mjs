// Proxy components (docs/wasi-sockets.md, section 10.3, rows PX) on the tab
// node, and on a node worker where the column "Nodes" says "worker". The
// image is wasi-test-proxy (wasm-images/rust/proxy): the path of the request
// selects the behavior. The raw HTTP requests go through nc in a busybox pod
// on the tab node, as in the step wasi-proxy of the Docker check
// (docs/docker-node-check/wasi-proxy.sh).
//
// The fixed values are the results of that step on runwasi v0.6.1. Rows
// that only unit tests cover:
//
//   PX23  a request that waits 10 minutes stays open: the synctest of
//         k8s/tabernetes/tabnode_wasi_proxy_test.go (plan rule 5 forbids real
//         waits of 30 seconds or more). Here: a request that waits 20 s.
//   PX24  the number of instances: a hook of the unit test in
//         web/test/proxy.test.mjs. Here: what a client sees. The client
//         request part is in wasi-proxy-cr.test.mjs.
//   PX25  web/test/proxy.test.mjs (PX25, CO7).
//   PX34  web/test/proxy.test.mjs ("Out of memory").
//   PX5   package W3.3 (the loopback table of phase 3).
import { before, test } from "node:test";
import assert from "node:assert/strict";
import { apply, forEachNode, kubectl, logs, namespace, nodeName, pod, podReady, TAB_NODE, waitFor, waitForPod } from "./harness.mjs";
import {
  body, busyboxPod, header, headerNames, inPod, podIP, proxyPod, raw, restarts, running, service, sharedNamespace, status,
} from "./wasi-proxy-lib.mjs";

// The results of runwasi (docs/docker-node-check/wasi-proxy.sh).
const RUNWASI = {
  // PX15: /sockets.
  sockets: [
    "tcp create: ok",
    'tcp start-bind: Err(ErrorCode { code: 1, name: "access-denied", message: "Access denied.\\n\\n            POSIX equivalent: EACCES, EPERM" })',
    'tcp start-connect: Err(ErrorCode { code: 1, name: "access-denied", message: "Access denied.\\n\\n            POSIX equivalent: EACCES, EPERM" })',
    "udp create: ok",
    'udp start-bind: Err(ErrorCode { code: 1, name: "access-denied", message: "Access denied.\\n\\n            POSIX equivalent: EACCES, EPERM" })',
    'resolve: ErrorCode { code: 20, name: "permanent-resolver-failure", message: "A permanent failure in name resolution occurred." }',
  ],
  // PX31: the header names, HTTP/1.1 with Connection: close and HTTP/1.0,
  // each with a host header.
  cookie11: ["connection", "date", "set-cookie", "set-cookie", "transfer-encoding"],
  cookie10: ["date", "set-cookie", "set-cookie"],
  noContent11: ["connection", "date"],
};

const shared = sharedNamespace("px");
const ns = shared.ns;
const client = "client";
// get sends a GET with wget from the client and resolves with {out, rc}.
const get = (url, opts = "") => inPod(ns, client, `wget -qO- -T 30 ${opts} ${url}`);
const req11 = (line, headers = "") => `${line} HTTP/1.1\\r\\nHost: px\\r\\n${headers}Connection: close\\r\\n\\r\\n`;

before(async () => {
  await shared.create();
  await apply([
    proxyPod("px", TAB_NODE),
    service("px", 8080),
    proxyPod("px-bad", TAB_NODE, { addr: "not-an-address" }),
    proxyPod("px6", TAB_NODE, { addr: "[::]:8080" }),
    proxyPod("px-port0", TAB_NODE, { addr: "0.0.0.0:0" }),
    proxyPod("px-foreign", TAB_NODE, { addr: "192.0.2.7:8080" }),
    proxyPod("px-nores", TAB_NODE),
    busyboxPod(client, TAB_NODE),
  ], ns);
  for (const name of ["px", client]) await waitForPod(ns, name, running);
});

// PX12 and PX7 first: the first 3 requests of the container px.
test("PX12, PX7: REQUEST_ID 0, 1, 2 and a new instance for each request on tab", async () => {
  const ids = [], counters = [];
  for (let i = 0; i < 3; i++) {
    const { out, rc } = await get("px:8080/context");
    assert.equal(rc, 0, out);
    ids.push(out.match(/^env REQUEST_ID=(.*)$/m)?.[1]);
    counters.push(out.match(/^counter (.*)$/m)?.[1]);
  }
  assert.deepEqual(ids, ["0", "1", "2"]);
  assert.deepEqual(counters, ["1", "1", "1"]);
});

// PX1, PX4, PX16, and the context of PX6, PX11 and PX13: a pod with the two
// variables of runwasi, a Service and an httpGet readiness probe on 9090.
forEachNode(["tab", "worker"], (kind) => {
  test(`PX1, PX4, PX16, PX6, PX11, PX13: a Service and a readiness probe on the address of the variable on ${kind}`, async (t) => {
    const ns1 = await namespace(t);
    const node = await nodeName(kind);
    const probe = proxyPod("probe", node, {
      addr: "0.0.0.0:9090",
      extra: { readinessProbe: { httpGet: { path: "/status/200", port: 9090 }, periodSeconds: 2 } },
    });
    probe.spec.containers[0].env.push({ name: "WASMTIME_HTTP_BACKLOG", value: "64" });
    await apply([probe, service("probe", 9090), busyboxPod("client", node)], ns1);
    const p = await waitForPod(ns1, "probe", podReady);
    assert.equal(p.spec.nodeName, node);
    await waitForPod(ns1, "client", running);
    const { out, rc } = await inPod(ns1, "client", "wget -qO- -T 30 probe:9090/context");
    assert.equal(rc, 0, out);
    // PX6, PX11, PX13, with the values of runwasi.
    assert.match(out, /^args \[\]$/m);
    assert.match(out, /^preopens 0$/m);
    assert.match(out, /^env REQUEST_ID=\d+$/m);
    assert.match(out, /^env HOSTNAME=probe$/m);
    assert.doesNotMatch(out, /^env WASMTIME_HTTP/m);
  });
});

test("PX2, PX3: no variable and a value that does not parse listen on port 8080 on tab", async () => {
  for (const name of ["px", "px-bad"]) {
    await waitForPod(ns, name, running);
    const { out, rc } = await get(`${await podIP(ns, name)}:8080/status/200`);
    assert.equal(rc, 0, `${name}: ${out}`);
  }
});

test("PX4, PX32, PX33: [::]:8080, port 0 and a foreign IP on tab", async () => {
  // PX4: [::] listens on the IPv4 pod IP, without a restart.
  await waitForPod(ns, "px6", running);
  const r6 = await get(`${await podIP(ns, "px6")}:8080/status/200`);
  assert.equal(r6.rc, 0, r6.out);
  assert.equal(restarts(await pod(ns, "px6")).split(" ")[0], "0");

  // PX32: an ephemeral port of 32768 to 60999. runwasi gets one from the
  // kernel; the tab node tries them in sequence, so the scan is short.
  await waitForPod(ns, "px-port0", running);
  const ip = await podIP(ns, "px-port0");
  const scan = await inPod(ns, client, `for p in $(seq 32768 32800); do nc -z -w 2 ${ip} $p && echo port $p; done`);
  const ports = [...scan.out.matchAll(/^port (\d+)$/gm)].map((m) => Number(m[1]));
  assert.equal(ports.length, 1, scan.out);
  assert.ok(ports[0] >= 32768 && ports[0] <= 60999, scan.out);
  const r0 = await get(`${ip}:${ports[0]}/status/200`);
  assert.equal(r0.rc, 0, r0.out);

  // PX33: as runwasi, exit code 137 and a restart.
  const f = await waitForPod(ns, "px-foreign", (p) => p.status?.containerStatuses?.[0]?.restartCount >= 1);
  assert.match(restarts(f), /^\d+ 137 Error$/);
  const text = await kubectl(`logs -n ${ns} px-foreign --previous`);
  assert.match(text, /Address not available \(os error 99\)/);
});

test("PX8, PX9, PX10: 20 requests that wait 1 second on tab", async () => {
  const start = Date.now();
  const { out } = await inPod(ns, client, "for i in $(seq 20); do wget -qO- -T 30 px:8080/wait/1000 & done; wait");
  const ms = Date.now() - start;
  assert.equal((out.match(/waited 1000/g) ?? []).length, 20, out);
  // 16 instances at the same time (PX9): the last 4 requests wait in the
  // queue (PX10), so all responses take 2 seconds or more. runwasi has no
  // maximum: approximately 1 second. The upper limit is generous for slow
  // runners.
  assert.ok(ms >= 1900, `${ms} ms`);
  assert.ok(ms < 8000, `${ms} ms`);
});

test("PX14: the tab node discards stdout and stderr of an instance on tab", async () => {
  const { out, rc } = await get("px:8080/stdio");
  assert.equal(rc, 0, out);
  assert.match(out, /^wrote to stdout and stderr$/m);
  assert.doesNotMatch(await logs(ns, "px"), /from the instance/);
});

test("PX15: the results of wasi:sockets agree with runwasi on tab", async () => {
  const { out, rc } = await get("px:8080/sockets");
  assert.equal(rc, 0, out);
  for (const line of RUNWASI.sockets) assert.ok(out.split("\n").includes(line), `missing "${line}" in\n${out}`);
});

test("PX16, PX28, PX17: request headers, two Set-Cookie headers, no Content-Type on tab", async () => {
  const h = await raw(ns, client, "px:8080", req11("GET /headers", "Cookie: c=1\\r\\nUser-Agent: px28\\r\\nOrigin: http://example.org\\r\\n"));
  assert.equal(status(h), "200", h);
  for (const line of ["cookie: c=1", "user-agent: px28", "origin: http://example.org"]) {
    assert.ok(body(h).toLowerCase().includes(`\n${line}\n`), `missing ${line} in\n${h}`);
  }
  // PX17: /headers sets no headers.
  assert.ok(!headerNames(h).includes("content-type"), h);
  const c = await raw(ns, client, "px:8080", req11("GET /cookie"));
  assert.deepEqual(header(c, "set-cookie"), ["a=1", "b=2"], c);
});

test("PX18, PX19, PX20: bodies of 17 MiB fail, a body of 1 MiB arrives without a change on tab", async () => {
  const big = 17 << 20;
  // PX19: a request body of 17 MiB gets 413.
  const r413 = await inPod(ns, client,
    `{ printf 'POST /echo HTTP/1.0\\r\\nHost: px\\r\\nContent-Length: ${big}\\r\\n\\r\\n'; seq 3000000 | head -c ${big}; sleep 3; } | nc px 8080 | head -1`,
    { timeout: 300 });
  assert.match(r413.out, /^HTTP\/1\.[01] 413 /m, r413.out);
  // PX20: a response body of 17 MiB gets 500.
  const r500 = await raw(ns, client, "px:8080", `GET /big/${big} HTTP/1.0\\r\\nHost: px\\r\\n\\r\\n`, 5);
  assert.equal(status(r500), "500", r500.slice(0, 300));
  // PX18: 1 MiB through /echo.
  const mib = 1 << 20;
  const r = await inPod(ns, client, [
    `seq 300000 | head -c ${mib} >/tmp/b`,
    `{ printf 'POST /echo HTTP/1.0\\r\\nHost: px\\r\\nContent-Length: ${mib}\\r\\n\\r\\n'; cat /tmp/b; sleep 2; } | nc px 8080 >/tmp/r`,
    "head -c 300 /tmp/r",
    "echo",
    "echo sent $(sha256sum </tmp/b | cut -c1-64)",
    "echo got $(sed '1,/^\\r*$/d' /tmp/r | sha256sum | cut -c1-64)",
  ].join("; "), { timeout: 300 });
  assert.match(r.out, /^x-body-length: 1048576$/mi, r.out);
  const sent = r.out.match(/^sent (\w{64})$/m)?.[1];
  assert.ok(sent, r.out);
  assert.match(r.out, new RegExp(`^got ${sent}$`, "m"));
});

test("PX21, PX22, PX27: a trap and an error code give no response, and the container continues on tab", async () => {
  for (const path of ["/trap", "/error"]) {
    const r = await raw(ns, client, "px:8080", req11(`GET ${path}`));
    assert.equal(r, "", `${path}: ${r}`);
    const next = await get("px:8080/status/200");
    assert.equal(next.rc, 0, `after ${path}: ${next.out}`);
  }
  assert.equal(restarts(await pod(ns, "px")).split(" ")[0], "0");
});

test("PX23: a request that waits 20 seconds gets its response on tab", async () => {
  const start = Date.now();
  const { out, rc } = await get("px:8080/wait/20000");
  assert.equal(rc, 0, out);
  assert.match(out, /^waited 20000$/m);
  assert.ok(Date.now() - start >= 20000);
});

test("PX24: clients that close their connections free the instances on tab", async () => {
  // 16 clients close their connections after 2 seconds. Each instance waits
  // 10 minutes. If the pod worker kept them, the next 16 requests would wait
  // in the queue of PX10 for 10 minutes.
  const start = Date.now();
  const { out } = await inPod(ns, client,
    "m=closed; for i in $(seq 16); do (wget -qO- -T 2 px:8080/wait/600000 || echo $m-by-client) & done; wait");
  // The shell of the tab writes the 16 lines at the same time, and a line
  // for each job: count the words of the output, not the lines.
  assert.equal((out.match(/closed-by-client/g) ?? []).length, 16, out);
  assert.doesNotMatch(out, /waited/);
  assert.ok(Date.now() - start < 60000);
  const again = await inPod(ns, client, "for i in $(seq 16); do wget -qO- -T 30 px:8080/wait/1000 & done; wait");
  assert.equal((again.out.match(/waited 1000/g) ?? []).length, 16, again.out);
});

test("PX26: no response gives the state and the restart of runwasi on tab", async () => {
  await waitForPod(ns, "px-nores", running);
  const ip = await podIP(ns, "px-nores");
  assert.equal((await get(`${ip}:8080/context`)).rc, 0);
  const r = await raw(ns, client, `${ip}:8080`, req11("GET /noresponse"));
  assert.equal(r, "");
  const p = await waitForPod(ns, "px-nores", (x) => running(x) && x.status.containerStatuses[0].restartCount >= 1);
  assert.equal(restarts(p), "1 139 Error");
  const ctx = await waitFor(async () => {
    const c = await get(`${await podIP(ns, "px-nores")}:8080/context`);
    return c.rc === 0 && c.out;
  }, { timeout: 60, what: "px-nores after its restart" });
  assert.match(ctx, /^env REQUEST_ID=0$/m);
});

test("PX29: PURGE, TRACE and a GET with a body arrive without a change on tab", async () => {
  const purge = await raw(ns, client, "px:8080", "PURGE /echo HTTP/1.0\\r\\nHost: px\\r\\n\\r\\n");
  assert.deepEqual(header(purge, "x-method"), ["other:PURGE"], purge);
  const trace = await raw(ns, client, "px:8080", "TRACE /echo HTTP/1.0\\r\\nHost: px\\r\\n\\r\\n");
  assert.deepEqual(header(trace, "x-method"), ["TRACE"], trace);
  const g = await raw(ns, client, "px:8080", "GET /echo HTTP/1.0\\r\\nHost: px\\r\\nContent-Length: 9\\r\\n\\r\\nget body\\n");
  assert.deepEqual(header(g, "x-method"), ["GET"], g);
  assert.deepEqual(header(g, "x-body-length"), ["9"], g);
  assert.equal(body(g), "get body\n");
});

test("PX30: the status codes 204, 304 and 599 on tab", async () => {
  for (const code of ["204", "304", "599"]) {
    assert.equal(status(await raw(ns, client, "px:8080", req11(`GET /status/${code}`))), code);
  }
});

test("PX31: the header names of runwasi for HTTP/1.1 and HTTP/1.0 on tab", async () => {
  assert.deepEqual(headerNames(await raw(ns, client, "px:8080", req11("GET /cookie"))), RUNWASI.cookie11);
  assert.deepEqual(headerNames(await raw(ns, client, "px:8080", "GET /cookie HTTP/1.0\\r\\nHost: px\\r\\n\\r\\n")), RUNWASI.cookie10);
  // runwasi gives no response to a request without a host header.
  assert.equal(await raw(ns, client, "px:8080", "GET /cookie HTTP/1.0\\r\\n\\r\\n"), "");
  assert.deepEqual(headerNames(await raw(ns, client, "px:8080", req11("GET /status/204"))), RUNWASI.noContent11);
});
