// Unit tests of the proxy path of the pod worker (serveProxy of
// web/src/component.js, docs/wasi-sockets.md section 8.6) with a fake Go
// side: PX7 to PX15, PX20 to PX22, PX24 to PX27, PX34, CR9, CR10 and CO7 for
// each request. The components come from proxy_fixtures.mjs (transpiled with
// jco, in JSPI mode) or are JavaScript objects that use the imports as a
// component does (for client requests and bodies).
import { test } from "node:test";
import assert from "node:assert/strict";
import v8 from "node:v8";
import vm from "node:vm";
import { parse } from "@bytecodealliance/jco";
import { generate } from "@bytecodealliance/jco/component";
import { WASIShim } from "@bytecodealliance/preview2-shim/instantiation";
import { transpile } from "../src/transpile.js";
import { runComponent, createScope, clocksFor, isMemoryError, MAX_INSTANCES, ERROR_EXIT } from "../src/component.js";
import { createPeer } from "../src/wasi_messages.js";
import { proxyComponent } from "./proxy_fixtures.mjs";
// The io.js of the browser shim, which the bundle of the pod worker uses.
import * as io from "../node_modules/@bytecodealliance/preview2-shim/dist/browser/io.js";

v8.setFlagsFromString("--expose-gc");
const gc = vm.runInNewContext("gc");

const enc = new TextEncoder();
const dec = new TextDecoder();
const load = (js) => import("data:text/javascript;base64," + Buffer.from(js).toString("base64"));
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const GO = 0x80000000;

const compiledCache = new Map();
async function compiled(mode, opts = {}) {
  const k = JSON.stringify([mode, opts]);
  if (!compiledCache.has(k)) {
    const r = await transpile(generate, await proxyComponent(mode, opts), "");
    assert.ok(!r.error, r.error);
    assert.equal(r.world, "proxy");
    const modules = {};
    for (const [f, b] of Object.entries(r.modules)) modules[f] = await WebAssembly.compile(b);
    compiledCache.set(k, { js: r.js, modules, imports: r.imports, exports: r.exports, names: r.names, world: r.world, entrypoint: "" });
  }
  return compiledCache.get(k);
}

// serve starts the proxy path with a fake Go side. It records the messages
// of the pod worker and the configuration of each WASIShim.
function serve(component, { env = ["PATH=/bin", "HOSTNAME=p"], shim = WASIShim, ...extra } = {}) {
  const sent = [];
  const drops = [];
  const configs = [];
  const peer = createPeer({ post: (m) => sent.push(m), onDrop: (why) => drops.push(why) });
  class Recording extends shim {
    constructor(config) {
      super(config);
      configs.push(config);
    }
  }
  let exited = null;
  const exit = runComponent({ component, args: ["ignored"], env, cwd: "/" }, { peer, WASIShim: Recording, io: { ...io }, load, ...extra });
  exit.then((code) => (exited = code));
  let next = 0;
  const go = {
    sent,
    drops,
    configs,
    peer,
    get exited() {
      return exited;
    },
    exit,
    // request sends a request message and gives its ID.
    request(fields = {}) {
      const id = GO + ++next;
      peer.dispatch({ type: "request", request: id, method: "GET", url: "http://pod:8080/x", headers: [], body: new ArrayBuffer(0), ...fields });
      return id;
    },
    responses: () => sent.filter((m) => m.type === "response"),
    response: (id) => sent.find((m) => m.type === "response" && m.request === id),
    async until(fn, what = "a condition", ms = 5000) {
      const end = Date.now() + ms;
      for (;;) {
        const v = fn();
        if (v) return v;
        if (Date.now() > end) throw new Error(`timeout: ${what}; sent ${JSON.stringify(sent.map((m) => m.type))}`);
        await sleep(2);
      }
    },
    mem: () => sent.filter((m) => m.type === "mem").map((m) => m.size),
  };
  return go;
}

// jsComponent gives a component of JavaScript: instantiate makes a core
// instance with one memory (CO6) and gives an incoming-handler that calls
// handle(imports, request, outparam). imports are those of the pod worker.
const MEMORY = new WebAssembly.Module(await parse("(module (memory (export \"memory\") 1))"));
function jsComponent(handle, onInstantiate = () => {}) {
  const imports = ["wasi:http/types@0.2.3", "wasi:http/outgoing-handler@0.2.3", "wasi:io/poll@0.2.3", "wasi:clocks/monotonic-clock@0.2.3", "wasi:cli/environment@0.2.3"];
  const component = { js: "", modules: {}, imports, exports: [["wasi:http/incoming-handler@0.2.3", "instance"]], names: { imports, exports: [] }, world: "proxy", entrypoint: "" };
  const fakeLoad = async () => ({
    async instantiate(_getModule, im, instantiateCore) {
      onInstantiate(im);
      await instantiateCore(MEMORY);
      return { "wasi:http/incoming-handler@0.2.3": { handle: (req, out) => handle(im, req, out) } };
    },
  });
  return { component, load: fakeLoad };
}

// respond sets a response with a status, headers and a body, as a component.
function respond(im, out, status, headers = [], body = "") {
  const T = im["wasi:http/types@0.2.3"];
  const r = new T.OutgoingResponse(T.Fields.fromList(headers.map(([k, v]) => [k, enc.encode(v)])));
  r.setStatusCode(status);
  const b = r.body();
  T.ResponseOutparam.set(out, { tag: "ok", val: r });
  if (body) {
    const s = b.write();
    s.checkWrite();
    s.write(typeof body === "string" ? enc.encode(body) : body);
  }
  T.OutgoingBody.finish(b, undefined);
}

test("PX7: each request gets a new instance", async () => {
  const go = serve(await compiled("ok"));
  const a = go.request();
  await go.until(() => go.response(a), "response a");
  const b = go.request();
  await go.until(() => go.response(b), "response b");
  // The global counter of the component is 1 in each response.
  assert.deepEqual(go.responses().map((m) => m.status), [201, 201]);
  assert.deepEqual(go.responses()[0], { type: "response", request: a, status: 201, headers: [], body: go.responses()[0].body });
  assert.equal(go.responses()[0].body.byteLength, 0);
  assert.equal(go.exited, null);
  assert.deepEqual(go.drops, []);
});

test("PX8, PX9, PX10: requests run at the same time, a maximum of 16, the others wait", async () => {
  // For each instance, the number of responses before it.
  const before = [];
  const go = serve(await compiled("wait", { ms: 150 }), { onInstance: () => before.push(go.responses().length) });
  await go.until(() => go.mem().length > 0, "the link check");
  const start = Date.now();
  const ids = [];
  for (let i = 0; i < 20; i++) ids.push(go.request());
  await go.until(() => go.responses().length > 0, "the first response");
  const first = Date.now() - start;
  await go.until(() => go.responses().length === 20, "20 responses");
  const ms = Date.now() - start;
  // Before the first response, 16 instances run and 4 requests wait.
  assert.equal(before.length, 20);
  assert.equal(before.filter((n) => n === 0).length, MAX_INSTANCES);
  assert.ok(first < 16 * 150, `the first response came after ${first} ms: the requests did not run at the same time`);
  assert.ok(ms >= 2 * 150 - 20, `20 requests took ${ms} ms: more than 16 ran at the same time`);
  assert.deepEqual(new Set(go.responses().map((m) => m.request)), new Set(ids));
  assert.ok(go.responses().every((m) => m.status === 201));
});

test("PX11, PX12, PX13, PX14, PX15: the context of an instance", async () => {
  const go = serve(await compiled("wait", { ms: 20 }), { limit: 1, env: ["PATH=/bin", "REQUEST_ID=x", "__proto__=p"] });
  const ids = [go.request(), go.request(), go.request()];
  await go.until(() => go.responses().length === 3, "3 responses");
  // The first config is the link check without a request.
  const [check, ...configs] = go.configs;
  assert.equal(check.environment.REQUEST_ID, "x");
  // REQUEST_ID from 0 in the sequence of arrival, also through the queue.
  assert.deepEqual(configs.map((c) => c.environment.REQUEST_ID), ["0", "1", "2"]);
  assert.deepEqual(go.responses().map((m) => m.request), ids);
  for (const c of configs) {
    assert.equal(c.environment.PATH, "/bin");
    assert.equal(c.environment.__proto__, "p");
    // PX13: no arguments, no preopens, an empty stdin.
    assert.deepEqual(c.arguments, []);
    assert.equal(c.browserFilesystem, undefined);
    assert.equal(c.filesystem, undefined);
    assert.equal(c.initialCwd, undefined);
    assert.throws(() => c.stdin.read(1n), (e) => e.tag === "closed");
    assert.throws(() => c.stdin.blockingRead(1n), (e) => e.tag === "closed");
    // PX14: stdout and stderr go nowhere.
    c.stdout.write(enc.encode("out"));
    c.stderr.write(enc.encode("err"));
    // PX15: wasi:sockets as on runwasi (sockets.js).
    const tcp = c.sockets.tcpCreateSocket.createTcpSocket("ipv4");
    assert.throws(() => tcp.startBind(), (e) => e === "access-denied");
    assert.throws(() => c.sockets.ipNameLookup.resolveAddresses(), (e) => e === "permanent-resolver-failure");
  }
  // Each instance has its own clocks and its own wasi:http.
  assert.notEqual(configs[0].http, configs[1].http);
  assert.notEqual(configs[0].clocks, configs[1].clocks);
  assert.ok(!go.sent.some((m) => m.type === "stdout" || m.type === "stderr"));
  // Go sends stdin after start: the proxy path discards it.
  go.peer.dispatch({ type: "stdin", bytes: new ArrayBuffer(3), end: true });
  assert.deepEqual(go.drops, []);
});

test("PX21, PX22: a trap gives the error trap, and the container continues", async () => {
  const go = serve(await compiled("trap"));
  const a = go.request();
  await go.until(() => go.response(a), "response a");
  const b = go.request();
  await go.until(() => go.response(b), "response b");
  assert.deepEqual(go.responses().map(({ request, error }) => ({ request, error })), [{ request: a, error: "trap" }, { request: b, error: "trap" }]);
  assert.ok(go.responses().every((m) => !("status" in m)));
  await sleep(10);
  assert.equal(go.exited, null);
});

test("PX26: handle without a response gives a response with neither a status nor an error", async () => {
  const go = serve(await compiled("none"));
  const a = go.request();
  await go.until(() => go.response(a), "response");
  assert.deepEqual(go.response(a), { type: "response", request: a });
  assert.equal(go.exited, null);
});

test("PX27: an error code as the response gives that error", async () => {
  const go = serve(await compiled("error"));
  const a = go.request();
  await go.until(() => go.response(a), "response");
  assert.deepEqual(go.response(a), { type: "response", request: a, error: "internal-error" });
});

test("PX16, PX20, PX29: the response of a component, and a body of more than 16 MiB", async () => {
  const seen = [];
  const { component, load: l } = jsComponent((im, req, out) => {
    const stream = req.consume().stream();
    let body = "";
    try {
      body = dec.decode(stream.blockingRead(100n));
    } catch (e) {
      assert.equal(e.tag, "closed");
    }
    seen.push({ method: req.method(), path: req.pathWithQuery(), authority: req.authority(), headers: req.headers().entries().map(([k, v]) => `${k}=${dec.decode(v)}`), body });
    if (req.pathWithQuery() === "/big") {
      const T = im["wasi:http/types@0.2.3"];
      const r = new T.OutgoingResponse(new T.Fields());
      const b = r.body();
      T.ResponseOutparam.set(out, { tag: "ok", val: r });
      const s = b.write();
      // The 17th write of 1 MiB fails, and the component traps.
      for (let i = 0; i < 17; i++) {
        s.checkWrite();
        s.write(new Uint8Array(1 << 20));
      }
      throw new Error("not reached");
    }
    respond(im, out, 299, [["set-cookie", "a=1"], ["set-cookie", "b=2"]], "body");
  });
  const go = serve(component, { load: l });
  const a = go.request({ method: "PURGE", url: "http://pod:8080/a?b", headers: [["cookie", enc.encode("c=1").buffer], ["host", enc.encode("pod:8080").buffer]], body: enc.encode("in").buffer });
  await go.until(() => go.response(a), "response a");
  assert.deepEqual(seen[0], { method: { tag: "other", val: "PURGE" }, path: "/a?b", authority: "pod:8080", headers: ["cookie=c=1"], body: "in" });
  const r = go.response(a);
  assert.equal(r.status, 299);
  assert.deepEqual(r.headers.map(([k, v]) => `${k}=${dec.decode(v)}`), ["set-cookie=a=1", "set-cookie=b=2"]);
  assert.equal(dec.decode(r.body), "body");
  const big = go.request({ url: "http://pod:8080/big" });
  await go.until(() => go.response(big), "response big");
  const rb = go.response(big);
  assert.deepEqual([rb.status, rb.headers, rb.body.byteLength], [500, [], 0]);
});

// fetching gives a component that sends a client request to url and answers
// with the result: status 200 and the status of the client request, or 502
// and the error code. drop drops the client request before its result.
function fetching({ drop = false } = {}) {
  const got = [];
  const { component, load: l } = jsComponent(async (im, req, out) => {
    const T = im["wasi:http/types@0.2.3"];
    const H = im["wasi:http/outgoing-handler@0.2.3"];
    const r = new T.OutgoingRequest(T.Fields.fromList([["x-a", enc.encode("1")]]));
    r.setScheme({ tag: "HTTP" });
    r.setAuthority("svc.ns:80");
    r.setPathWithQuery(req.pathWithQuery());
    const f = H.handle(r, undefined);
    if (drop) {
      // The fetch message goes after the body promise; then the drop.
      await sleep(0);
      f[Symbol.dispose]();
      respond(im, out, 204);
      return;
    }
    await f.subscribe().block();
    const result = f.get().val;
    got.push(result);
    if (result.tag === "ok") respond(im, out, 200, [["x-status", String(result.val.status())]]);
    else respond(im, out, 502, [["x-code", result.val.tag]]);
  });
  return { component, load: l, got };
}

test("CR9: fetch goes to Go, and the error code of fetched goes to the component without a change", async () => {
  const c = fetching();
  const go = serve(c.component, { load: c.load });
  const a = go.request({ url: "http://pod/one" });
  const fetch = await go.until(() => go.sent.find((m) => m.type === "fetch"), "fetch");
  assert.ok(fetch.fetch >= 1 && fetch.fetch <= 0x7fffffff);
  assert.equal(fetch.method, "GET");
  assert.equal(fetch.url, "http://svc.ns:80/one");
  assert.deepEqual(fetch.headers.map(([k, v]) => `${k}=${dec.decode(v)}`), ["x-a=1"]);
  assert.ok(fetch.body instanceof ArrayBuffer);
  // fetched for an ID that the pod worker does not know: no change.
  go.peer.dispatch({ type: "fetched", fetch: fetch.fetch + 1000, status: 200, headers: [], body: new ArrayBuffer(0) });
  go.peer.dispatch({ type: "fetched", fetch: fetch.fetch, error: "DNS-error" });
  await go.until(() => go.response(a), "response");
  assert.deepEqual(c.got[0], { tag: "err", val: { tag: "DNS-error", val: { rcode: undefined, infoCode: undefined } } });
  assert.equal(dec.decode(go.response(a).headers[0][1]), "DNS-error");
  // A second fetched for the same ID: no change.
  go.peer.dispatch({ type: "fetched", fetch: fetch.fetch, status: 200, headers: [], body: new ArrayBuffer(0) });
  const b = go.request({ url: "http://pod/two" });
  const second = await go.until(() => go.sent.filter((m) => m.type === "fetch")[1], "second fetch");
  assert.notEqual(second.fetch, fetch.fetch);
  go.peer.dispatch({ type: "fetched", fetch: second.fetch, status: 404, headers: [], body: new ArrayBuffer(0) });
  await go.until(() => go.response(b), "response b");
  assert.equal(go.response(b).status, 200);
  assert.equal(dec.decode(go.response(b).headers[0][1]), "404");
  assert.ok(!go.sent.some((m) => m.type === "fetch-cancel"));
  assert.deepEqual(go.drops, []);
});

test("CR10: a client request that the component drops sends fetch-cancel", async () => {
  const c = fetching({ drop: true });
  const go = serve(c.component, { load: c.load });
  const a = go.request();
  await go.until(() => go.response(a), "response");
  const fetch = go.sent.find((m) => m.type === "fetch");
  assert.deepEqual(go.sent.filter((m) => m.type === "fetch-cancel"), [{ type: "fetch-cancel", fetch: fetch.fetch }]);
  // fetched after fetch-cancel crosses it: the pod worker ignores it.
  go.peer.dispatch({ type: "fetched", fetch: fetch.fetch, error: "connection-refused" });
  assert.deepEqual(go.drops, []);
});

test("PX24: cancel of a request with a client request sends fetch-cancel and response cancelled", async () => {
  const c = fetching();
  const go = serve(c.component, { load: c.load });
  const a = go.request();
  const fetch = await go.until(() => go.sent.find((m) => m.type === "fetch"), "fetch");
  go.peer.dispatch({ type: "cancel", request: a });
  assert.deepEqual(go.response(a), { type: "response", request: a, error: "cancelled" });
  assert.deepEqual(go.sent.filter((m) => m.type === "fetch-cancel"), [{ type: "fetch-cancel", fetch: fetch.fetch }]);
  // The instance ends: the memory goes back to 0 (CO7).
  await go.until(() => go.mem().at(-1) === 0 && go.mem().length >= 3, "the memory of the instance");
  // A second cancel and a late fetched change nothing.
  go.peer.dispatch({ type: "cancel", request: a });
  go.peer.dispatch({ type: "fetched", fetch: fetch.fetch, status: 200, headers: [], body: new ArrayBuffer(0) });
  await sleep(10);
  assert.equal(go.responses().length, 1);
  assert.equal(c.got.length, 0);
});

test("PX24: cancel continues the suspended calls with an exception, and the browser frees the instances", async () => {
  const registry = new FinalizationRegistry(() => freed++);
  let freed = 0;
  let made = 0;
  const go = serve(await compiled("wait", { ms: 1e9, pages: 2 }), {
    onInstance: (instance) => {
      made++;
      registry.register(instance, null);
    },
  });
  const ids = [];
  for (let i = 0; i < 10; i++) ids.push(go.request());
  await go.until(() => made === 10, "10 instances");
  assert.equal(go.mem().at(-1), 10 * 2 * 65536);
  for (const id of ids) go.peer.dispatch({ type: "cancel", request: id });
  assert.deepEqual(go.responses().map((m) => m.error), Array(10).fill("cancelled"));
  // The stacks unwind: the memory group of each instance goes away (CO7).
  await go.until(() => go.mem().at(-1) === 0, "the memory after the cancel");
  for (let i = 0; i < 50 && freed < 10; i++) {
    gc();
    await sleep(10);
  }
  assert.equal(freed, 10, "the browser frees each cancelled instance");
  // The container serves the next request.
  const next = go.request();
  await go.until(() => made === 11, "the next instance");
  go.peer.dispatch({ type: "cancel", request: next });
  // An unknown request ID: no message.
  go.peer.dispatch({ type: "cancel", request: GO + 999 });
  assert.equal(go.responses().length, 11);
  assert.deepEqual(go.drops, []);
});

test("PX24: cancel of a request in the queue gives cancelled, and it does not run", async () => {
  let made = 0;
  const go = serve(await compiled("wait", { ms: 50 }), { limit: 1, onInstance: () => made++ });
  const a = go.request();
  const b = go.request();
  go.peer.dispatch({ type: "cancel", request: b });
  assert.deepEqual(go.response(b), { type: "response", request: b, error: "cancelled" });
  await go.until(() => go.response(a), "response a");
  await sleep(80);
  assert.equal(made, 1);
  assert.equal(go.responses().length, 2);
  assert.equal(go.response(a).status, 201);
});

test("PX25, CO7: the memory of each instance goes after its response", async () => {
  const go = serve(await compiled("ok", { pages: 3 }));
  await go.until(() => go.mem().length >= 2, "the link check");
  assert.deepEqual(go.mem(), [3 * 65536, 0]);
  for (let i = 0; i < 3; i++) {
    const id = go.request();
    await go.until(() => go.response(id), "response");
    await go.until(() => go.mem().at(-1) === 0, "release");
  }
  assert.deepEqual(go.mem(), [3 * 65536, 0, 3 * 65536, 0, 3 * 65536, 0, 3 * 65536, 0]);
});

// failing gives a load function whose instantiation fails with the error of
// Chrome for a Wasm memory that it cannot make, while fail() is true.
function failing(fail) {
  return async (js) => {
    const real = await load(js);
    return {
      instantiate: (get, imports, core) => real.instantiate(get, imports, async (module, im) => {
        if (fail()) throw new RangeError("WebAssembly.Instance(): Out of memory: Cannot allocate Wasm memory for new instance");
        return core(module, im);
      }),
    };
  };
}

test("isMemoryError finds the error of a Wasm memory that the browser cannot make", () => {
  assert.ok(isMemoryError(new RangeError("WebAssembly.Instance(): Out of memory: Cannot allocate Wasm memory for new instance")));
  assert.ok(isMemoryError(new RangeError("WebAssembly.Memory(): could not allocate memory")));
  assert.ok(!isMemoryError(new Error("unreachable")));
});

test("PX34: a request without memory for its instance waits until another instance ends", async () => {
  let failures = 0;
  const go = serve(await compiled("wait", { ms: 60 }), { load: failing(() => failures > 0 && failures-- > 0), retryMs: 60_000 });
  await go.until(() => go.mem().length >= 2, "the link check");
  const a = go.request();
  await sleep(10);
  failures = 1;
  const b = go.request();
  await go.until(() => failures === 0, "the failed instantiation of b");
  await sleep(5);
  // c comes after b in the queue.
  const c = go.request();
  await sleep(5);
  assert.equal(go.responses().length, 0);
  await go.until(() => go.responses().length === 3, "3 responses");
  // b and c wait for the end of a, in their sequence.
  assert.deepEqual(go.responses().map((m) => [m.request, m.status]), [[a, 201], [b, 201], [c, 201]]);
});

test("PX34: without another instance, the request tries again after a time", async () => {
  let failures = 0;
  const go = serve(await compiled("ok"), { load: failing(() => failures > 0 && failures-- > 0), retryMs: 30 });
  await go.until(() => go.mem().length >= 2, "the link check");
  failures = 2;
  const start = Date.now();
  const a = go.request();
  await go.until(() => go.response(a), "response");
  assert.equal(go.response(a).status, 201);
  assert.ok(Date.now() - start >= 50, "two retries");
});

test("a proxy component that cannot link exits with 137 and the name of the function", async () => {
  class NoSet extends WASIShim {
    getImportObject(opts) {
      const o = super.getImportObject(opts);
      for (const k of Object.keys(o)) {
        if (k.startsWith("wasi:http/types")) {
          const { ResponseOutparam: _, ...rest } = o[k];
          o[k] = rest;
        }
      }
      return o;
    }
  }
  const go = serve(await compiled("ok"), { shim: NoSet });
  assert.equal(await go.exit, ERROR_EXIT);
  const text = go.sent.filter((m) => m.type === "stderr").map((m) => dec.decode(m.bytes)).join("");
  assert.match(text, /^component imports instance `wasi:http\/types@0\.2\.3`, but a matching implementation was not found in the linker/);
});

test("a request before the generated JavaScript loads waits for it", async () => {
  const component = await compiled("ok");
  let release;
  const gate = new Promise((r) => (release = r));
  const go = serve(component, { load: async (js) => (await gate, load(js)) });
  const a = go.request();
  await sleep(10);
  assert.equal(go.responses().length, 0);
  release();
  await go.until(() => go.response(a), "response");
  assert.equal(go.response(a).status, 201);
});

test("createScope and clocksFor: a cancel fails each wait and stops the timers", async () => {
  const scope = createScope();
  const clocks = clocksFor(scope, io.pollableCreate);
  const ready = clocks.monotonicClock.subscribeDuration(0n);
  assert.equal(ready.ready(), true);
  const short = clocks.monotonicClock.subscribeDuration(1_000_000n);
  await short.block();
  assert.equal(short.ready(), true);
  const long = clocks.monotonicClock.subscribeInstant(clocks.monotonicClock.now() + 3_600_000_000_000n);
  const waits = [long.block(), scope.wait(new Promise(() => {}))];
  let stepped = 0;
  scope.onCancel(() => stepped++);
  scope.cancel();
  for (const w of waits) await assert.rejects(w, /cancelled/);
  assert.equal(stepped, 1);
  assert.equal(long.ready(), false);
  await assert.rejects(scope.wait(Promise.resolve(1)), /cancelled/);
  const wall = clocks.wallClock.now();
  assert.equal(typeof wall.seconds, "bigint");
});
