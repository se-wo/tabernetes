// Client requests of proxy components (docs/wasi-sockets.md, section 10.3,
// rows CR), the client request part of PX24, and the proxy world of
// wasi-test-bothrev (CO2), on the tab node and on a node worker where the
// column "Nodes" says "worker". The fixed values are the results of the step
// wasi-proxy of the Docker check (docs/docker-node-check/wasi-proxy.sh) on
// runwasi v0.6.1.
//
// Two servers in busybox pods on the tab node, with nc:
//
//   cr    the test Service of CR3 to CR5, as wasi-proxy-cr of
//         docs/docker-node-check/wasi-proxy.yaml: for each connection, a
//         redirect with a gzip body and Connection: close. Its log has the
//         requests.
//   sink  a server that does not answer. Its log has the requests, and
//         "--- closed" when the other side closes the connection.
//
// Rows that other tests cover:
//
//   CR6       wasi-worlds.test.mjs (tab, worker).
//   CR8       the time of 127 s and CR11: k8s/tabernetes/tabnode_wasi_fetch_test.go
//             (plan rule 5 forbids real waits of 30 seconds or more).
//   CR1, CR8  NetworkPolicy on a Docker node: a deferred test (decision 16).
//   CR9       web/test/http.test.mjs and web/test/proxy.test.mjs.
import { before, test } from "node:test";
import assert from "node:assert/strict";
import { apply, forEachNode, logs, nodeName, TAB_NODE, waitFor, waitForPod } from "./harness.mjs";
import { busyboxPod, inPod, podIP, proxyPod, running, service, sharedNamespace } from "./wasi-proxy-lib.mjs";

// The gzip bytes of "compressed body\n" (gzip -n -9), in base64 for the
// server and in hex as /fetchfull writes them.
const GZIP_BASE64 = "H4sIAAAAAAACA0vOzy0oSi0uTk1RSMpPqeQCAAjb6mcQAAAA";
const GZIP_HEX = "1f8b08000000000002034bcecf2d284a2d2e4e4d5148ca4fa9e4020008dbea6710000000";

const CR_SERVER = [
  "while true; do",
  "{ printf 'HTTP/1.1 302 Found\\r\\nLocation: /elsewhere\\r\\nContent-Encoding: gzip\\r\\nContent-Length: 36\\r\\nConnection: close\\r\\n\\r\\n';",
  'echo "$BODY" | base64 -d; } | nc -l -p 8000;',
  "echo '--- end';",
  "done",
].join(" ");

// <&-: nc gets no stdin, so it does not close its side of the connection.
const SINK = "while true; do nc -l -p 9000 <&-; echo '--- closed'; done";

const shared = sharedNamespace("cr");
const ns = shared.ns;
const client = "client";
const get = (url) => inPod(ns, client, `wget -qO- -T 60 ${url}`);

before(async () => {
  await shared.create();
  await apply([
    proxyPod("px", TAB_NODE),
    service("px", 8080),
    busyboxPod("cr", TAB_NODE, CR_SERVER, [{ name: "BODY", value: GZIP_BASE64 }]),
    service("cr", 8000),
    busyboxPod("sink", TAB_NODE, SINK),
    service("sink", 9000),
    busyboxPod(client, TAB_NODE),
  ], ns);
  for (const name of ["px", "cr", "sink", client]) await waitForPod(ns, name, running);
});

// sinkSaw waits until the log of sink has the request line of path and,
// after it, "--- closed".
async function sinkSaw(path) {
  return waitFor(async () => {
    const lines = (await logs(ns, "sink")).replace(/\r/g, "").split("\n");
    const at = lines.findIndex((l) => l.startsWith(`GET ${path} `));
    return at >= 0 && lines.indexOf("--- closed", at) > at && lines;
  }, { timeout: 30, what: `the request ${path} and its close in the log of sink` });
}

forEachNode(["tab", "worker"], (kind) => {
  test(`CR1: a client request to a Service by its name on ${kind}`, async () => {
    let target = "px";
    if (kind === "worker") {
      const node = await nodeName(kind);
      await apply([proxyPod("pxw", node), service("pxw", 8080)], ns);
      await waitForPod(ns, "pxw", running);
      target = "pxw";
    }
    const { out, rc } = await get(`${target}:8080/fetch/cr:8000/cr1-${kind}`);
    assert.equal(rc, 0, out);
    assert.match(out, /^status 302 after \d+ ms$/m);
  });
});

test("CR2, CR7: a name and an IP outside the cluster on tab", async () => {
  const name = await get("px:8080/fetch/example.com/");
  assert.match(name.out, /^error ErrorCode::DnsError\(/m, name.out);
  const ip = await get("px:8080/fetch/192.0.2.1/");
  assert.match(ip.out, /^error ErrorCode::ConnectionRefused after \d+ ms$/m, ip.out);
});

test("CR3, CR4, CR5: no redirects, no added headers, no decompression on tab", async () => {
  const { out, rc } = await get("px:8080/fetchfull/cr:8000/cr345");
  assert.equal(rc, 0, out);
  const lines = out.split("\n");
  // CR3: the redirect arrives at the component.
  assert.ok(lines.includes("status 302"), out);
  assert.ok(lines.map((l) => l.toLowerCase()).includes("h location: /elsewhere"), out);
  assert.ok(lines.map((l) => l.toLowerCase()).includes("h content-encoding: gzip"), out);
  // CR5: the compressed bytes arrive without a change.
  assert.ok(lines.includes("body-len 36"), out);
  assert.ok(lines.includes(`body-hex ${GZIP_HEX}`), out);
  // CR4: the server gets only Host, as from wasmtime.
  const req = (await logs(ns, "cr")).replace(/\r/g, "").split("\n");
  const at = req.findIndex((l) => l.startsWith("GET /cr345 "));
  assert.ok(at >= 0, req.join("\n"));
  const end = req.indexOf("", at);
  const names = req.slice(at + 1, end).map((l) => l.split(":")[0].toLowerCase());
  assert.deepEqual(names, ["host"], req.slice(at, end).join("\n"));
});

test("CR10: the component drops a client request, and the Go side closes the connection on tab", async () => {
  const { out, rc } = await get("px:8080/fetchdrop/2000/sink:9000/cr10");
  assert.equal(rc, 0, out);
  assert.match(out, /^dropped after 2000 ms, the response was pending$/m);
  await sinkSaw("/cr10");
});

test("PX24: a client that closes its connection closes the client request of the instance on tab", async () => {
  // /fetch waits for a response that sink never sends. wget stops after 3
  // seconds and closes its connection: the pod worker discards the
  // instance and its client request (fetch-cancel).
  const { out, rc } = await inPod(ns, client, "wget -qO- -T 3 px:8080/fetch/sink:9000/px24");
  assert.notEqual(rc, 0, out);
  await sinkSaw("/px24");
  // The container continues to serve.
  const next = await get("px:8080/status/200");
  assert.equal(next.rc, 0, next.out);
});

test("CO2: wasi-test-bothrev answers requests on tab", async () => {
  await apply(proxyPod("bothrev", TAB_NODE, { extra: { image: "ghcr.io/se-wo/tabernetes/wasi-test-bothrev:latest" } }), ns);
  await waitForPod(ns, "bothrev", running);
  const ip = await podIP(ns, "bothrev");
  const r = await waitFor(async () => {
    const x = await get(`${ip}:8080/`);
    return x.rc === 0 && x;
  }, { timeout: 60, what: "a response of bothrev" });
  assert.match(r.out, /^handle$/m);
});
