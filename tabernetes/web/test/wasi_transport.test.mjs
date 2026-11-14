// Unit tests of the transport of wasi.js for the pod workers of components
// (docs/wasi-sockets.md, section 8.3 and decision 13): the pod worker starts
// from a data: URL with the text of component.worker.js, with a Blob URL as
// the fallback. Messages go through without a change.
import { test, beforeEach } from "node:test";
import assert from "node:assert/strict";
import { createRuntime } from "../src/wasi.js";

const SCRIPT = "/* the bundle of component.worker.js */ postMessage({type: 'ready'});";

class FakeWorker {
  static all = [];
  static failData = false; // a data: worker fires error before ready
  constructor(url, opts) {
    this.url = url;
    this.opts = opts;
    this.posted = [];
    this.terminated = false;
    FakeWorker.all.push(this);
    if (FakeWorker.failData && url.startsWith("data:")) setTimeout(() => this.onerror({ message: "data: workers are not allowed", preventDefault() {} }), 1);
  }
  postMessage(msg, transfer) { this.posted.push([msg, transfer]); }
  terminate() { this.terminated = true; }
  // from the side of the worker
  emit(data) { this.onmessage({ data }); }
}

let fetches;
const fetchScript = async (url) => {
  fetches.push(url);
  if (url.includes("missing")) return new Response("no", { status: 404, statusText: "Not Found" });
  return new Response(SCRIPT);
};
const runtime = (componentWorkerURL = "http://x/component.worker.js") =>
  createRuntime({ workerURL: "http://x/wasi.worker.js", mirror: "", transpileURL: "http://x/transpile/t.js", componentWorkerURL, Worker: FakeWorker, fetch: fetchScript });
const tick = () => new Promise((r) => setTimeout(r, 5));

beforeEach(() => {
  FakeWorker.all = [];
  FakeWorker.failData = false;
  fetches = [];
});

test("the pod worker of a component starts from a data: URL with the text of component.worker.js", async () => {
  const rt = runtime();
  const h = rt.start({ component: true, name: "ns/pod/c" });
  const got = [];
  h.onMessage((m) => got.push(m));
  // A message before the pod worker is ready waits for it.
  const start = { type: "start", component: { js: "export const GENERATED = 1;", modules: {} }, args: ["a"], env: [], cwd: "/" };
  h.post(start, []);
  await tick();
  assert.equal(FakeWorker.all.length, 1);
  const w = FakeWorker.all[0];
  assert.equal(w.url, "data:text/javascript," + encodeURIComponent(SCRIPT));
  assert.equal(w.opts.name, "wasm ns/pod/c");
  assert.equal(w.posted.length, 0);
  // ready: the transport consumes it and sends the messages that wait.
  w.emit({ type: "ready" });
  assert.deepEqual(w.posted, [[start, []]]);
  assert.equal(got.length, 0);
  // CO14: the generated JavaScript goes only in a message, never in a URL.
  assert.ok(!decodeURIComponent(w.url).includes("GENERATED"));
  // Messages go through without a change, also those that Go must drop.
  const buf = new ArrayBuffer(2);
  const out = { type: "stdout", bytes: buf };
  w.emit(out);
  w.emit({ type: "format-disk" });
  assert.equal(got[0], out);
  assert.deepEqual(got[1], { type: "format-disk" });
  const tr = [new ArrayBuffer(1)];
  h.post({ type: "stdin", bytes: tr[0], end: false }, tr);
  assert.equal(w.posted[1][1], tr);
});

test("the text of component.worker.js is fetched one time", async () => {
  const rt = runtime();
  rt.start({ component: true }).onMessage(() => {});
  rt.start({ component: true }).onMessage(() => {});
  await tick();
  assert.deepEqual(fetches, ["http://x/component.worker.js"]);
  assert.equal(FakeWorker.all.length, 2);
});

test("messages before onMessage are kept", async () => {
  const rt = runtime();
  const h = rt.start({ component: true });
  await tick();
  FakeWorker.all[0].emit({ type: "ready" });
  FakeWorker.all[0].emit({ type: "exit", code: 0 });
  const got = [];
  h.onMessage((m) => got.push(m));
  assert.deepEqual(got, [{ type: "exit", code: 0 }]);
});

test("decision 13: if a data: worker does not start, the pod worker starts from a Blob URL", async () => {
  FakeWorker.failData = true;
  const rt = runtime();
  const h = rt.start({ component: true });
  const got = [];
  h.onMessage((m) => got.push(m));
  h.post({ type: "start" }, []);
  await tick();
  assert.equal(FakeWorker.all.length, 2);
  const [d, b] = FakeWorker.all;
  assert.ok(d.url.startsWith("data:") && d.terminated);
  assert.match(b.url, /^blob:/);
  b.emit({ type: "ready" });
  assert.deepEqual(b.posted, [[{ type: "start" }, []]]);
  assert.deepEqual(got, []);
  // The next pod worker starts from the Blob URL at once.
  rt.start({ component: true }).onMessage(() => {});
  await tick();
  assert.equal(FakeWorker.all.length, 3);
  assert.equal(FakeWorker.all[2].url, b.url);
});

test("a pod worker that fails after ready gives exit 128", async () => {
  const rt = runtime();
  const h = rt.start({ component: true });
  const got = [];
  h.onMessage((m) => got.push(m));
  await tick();
  const w = FakeWorker.all[0];
  w.emit({ type: "ready" });
  w.onerror({ message: "out of memory", preventDefault() {} });
  assert.deepEqual(got, [{ type: "exit", code: 128, error: "the pod worker failed: out of memory" }]);
  assert.ok(w.terminated);
  assert.equal(FakeWorker.all.length, 1);
});

test("kill gives exit 137, close gives no message, and later messages are dropped", async () => {
  const rt = runtime();
  const h1 = rt.start({ component: true });
  const h2 = rt.start({ component: true });
  const got1 = [], got2 = [];
  h1.onMessage((m) => got1.push(m));
  h2.onMessage((m) => got2.push(m));
  await tick();
  const [w1, w2] = FakeWorker.all;
  w1.emit({ type: "ready" });
  w2.emit({ type: "ready" });
  h1.kill();
  h1.kill();
  h2.close();
  h2.kill();
  w1.emit({ type: "stdout", bytes: new ArrayBuffer(1) });
  h1.post({ type: "stdin" }, []);
  assert.deepEqual(got1, [{ type: "exit", code: 137 }]);
  assert.deepEqual(got2, []);
  assert.ok(w1.terminated && w2.terminated);
  assert.equal(w1.posted.length, 0);
});

test("a component worker script that cannot be fetched gives exit 128", async () => {
  const rt = runtime("http://x/missing.worker.js");
  const h = rt.start({ component: true });
  const got = [];
  h.onMessage((m) => got.push(m));
  await tick();
  assert.equal(FakeWorker.all.length, 0);
  assert.equal(got.length, 1);
  assert.equal(got[0].code, 128);
  assert.match(got[0].error, /cannot start: GET http:\/\/x\/missing.worker.js: 404/);
});
