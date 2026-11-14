// Unit tests of the transpile cache in wasi.js (docs/wasi-sockets.md,
// section 8.2): CO8, CO10 and CO14, with a transpile worker that runs in
// this thread. Run: node --test test/ in web/.
import { test, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { generate } from "@bytecodealliance/jco/component";
import { createRuntime } from "../src/wasi.js";
import { transpile } from "../src/transpile.js";
import { component } from "./component_fixtures.mjs";

// FakeWorker does what transpile.worker.js does, in this thread.
class FakeWorker {
  static started = [];
  static mode = "real"; // "real", "evil" (a result that sets a global) or "crash"
  constructor(url, opts) {
    this.url = url;
    this.opts = opts;
    FakeWorker.started.push(this);
  }
  postMessage({ id, bytes, entrypoint }) {
    setTimeout(async () => {
      if (FakeWorker.mode === "crash") return this.onerror({ message: "boom" });
      const r = FakeWorker.mode === "evil"
        ? { js: "globalThis.__tabernetesEvaluated = true; export function instantiate() {}", modules: {}, imports: [], exports: [] }
        : await transpile(generate, new Uint8Array(bytes), entrypoint);
      if (r.error) return this.onmessage({ data: { id, error: r.error } });
      const modules = Object.fromEntries(Object.entries(r.modules).map(([f, b]) => [f, b.slice().buffer]));
      this.onmessage({ data: { id, js: r.js, modules, imports: r.imports, exports: r.exports } });
    }, 1);
  }
  terminate() {}
}

let bytes;
const blobs = new Map();
const fakeFetch = async (url) => {
  const b = blobs.get(url);
  return b ? new Response(b) : new Response("not found", { status: 404, statusText: "Not Found" });
};
const layer = (url) => ({ url, digest: `x:${url}`, kind: "wasm" });
const runtime = () => createRuntime({ workerURL: "http://x/wasi.worker.js", mirror: "", transpileURL: "http://x/transpile/transpile.worker.js", Worker: FakeWorker, fetch: fakeFetch });

let saved;
beforeEach(async () => {
  bytes ??= await component(["go", "other"]);
  blobs.set("http://x/c", bytes);
  blobs.set("http://x/m", new Uint8Array([0, 97, 115, 109, 1, 0, 0, 0]));
  FakeWorker.started = [];
  FakeWorker.mode = "real";
  // Node.js has no JSPI: the tests give the two names.
  saved = { Suspending: WebAssembly.Suspending, promising: WebAssembly.promising };
  WebAssembly.Suspending ??= function Suspending() {};
  WebAssembly.promising ??= (f) => f;
});
afterEach(() => {
  for (const [k, v] of Object.entries(saved)) {
    if (v === undefined) delete WebAssembly[k];
    else WebAssembly[k] = v;
  }
});

test("a module starts no transpile worker", async () => {
  const rt = runtime();
  const r = await rt.load({ layers: [layer("http://x/m")], headers: {} });
  assert.ok(r.module instanceof WebAssembly.Module);
  assert.deepEqual(rt.stats(), { transpiles: 0, transpileWorker: false });
  assert.equal(FakeWorker.started.length, 0);
});

test("a component gives JavaScript and compiled core modules", async () => {
  const rt = runtime();
  const r = await rt.load({ layers: [layer("http://x/c")], headers: {}, components: true });
  assert.equal(r.unsupported, undefined, r.unsupported);
  const c = r.component;
  assert.equal(typeof c.js, "string");
  assert.ok(Object.keys(c.modules).length >= 1);
  for (const m of Object.values(c.modules)) assert.ok(m instanceof WebAssembly.Module);
  assert.deepEqual(c.exports, [["go", "function"], ["other", "function"]]);
  assert.equal(c.entrypoint, "");
  // One module worker of the transpile URL.
  assert.equal(FakeWorker.started.length, 1);
  assert.equal(FakeWorker.started[0].url, "http://x/transpile/transpile.worker.js");
  assert.equal(FakeWorker.started[0].opts.type, "module");
});

test("a caller without components gets unsupported", async () => {
  const r = await runtime().load({ layers: [layer("http://x/c")], headers: {} });
  assert.match(r.unsupported, /WASI preview 2 component.*did not ask for components/);
});

test("CO8: one transpiler run for each key; a second entrypoint gives a second run", async () => {
  const rt = runtime();
  const src = (path) => ({ layers: [layer("http://x/c")], headers: {}, path, components: true });
  // Two pods of the same image at the same time, then a restart.
  await Promise.all([rt.load(src("")), rt.load(src(""))]);
  await rt.load(src(""));
  assert.equal(rt.stats().transpiles, 1);
  const a = await rt.load(src("#go"));
  assert.equal(a.component.entrypoint, "go");
  assert.equal(rt.stats().transpiles, 2);
  await rt.load(src("#go"));
  assert.equal(rt.stats().transpiles, 2);
  await rt.load(src("#other"));
  assert.equal(rt.stats().transpiles, 3);
  // One transpile worker for the Go thread.
  assert.equal(FakeWorker.started.length, 1);
});

test("CO8: another image is another key", async () => {
  const rt = runtime();
  blobs.set("http://x/c2", bytes);
  await rt.load({ layers: [layer("http://x/c")], headers: {}, components: true });
  await rt.load({ layers: [layer("http://x/c2")], headers: {}, components: true });
  assert.equal(rt.stats().transpiles, 2);
});

test("CO10: without JSPI, a component gets a text with the cause", async () => {
  delete WebAssembly.Suspending;
  const rt = runtime();
  const r = await rt.load({ layers: [layer("http://x/c")], headers: {}, components: true });
  assert.match(r.unsupported, /JavaScript Promise Integration/);
  assert.equal(rt.stats().transpiles, 0);
});

test("CO16: an error of the transpile worker gives unsupported, and it is not kept", async () => {
  const rt = runtime();
  const s = Buffer.from(bytes).toString("latin1");
  const i = s.lastIndexOf("other");
  blobs.set("http://x/bad", Uint8Array.from(Buffer.from(s.slice(0, i) + 'othe"' + s.slice(i + 5), "latin1")));
  const src = { layers: [layer("http://x/bad")], headers: {}, components: true };
  const r = await rt.load(src);
  assert.match(r.unsupported, /cannot run: jco cannot transpile the component/);
  await rt.load(src);
  assert.equal(rt.stats().transpiles, 2);
});

test("a failed transpile worker rejects the load, and the next load starts a new worker", async () => {
  const rt = runtime();
  FakeWorker.mode = "crash";
  await assert.rejects(rt.load({ layers: [layer("http://x/c")], headers: {}, components: true }), /transpile worker failed: boom/);
  FakeWorker.mode = "real";
  const r = await rt.load({ layers: [layer("http://x/c")], headers: {}, components: true });
  assert.ok(r.component);
  assert.equal(FakeWorker.started.length, 2);
});

test("CO14: wasi.js does not run the generated JavaScript", async () => {
  FakeWorker.mode = "evil";
  const r = await runtime().load({ layers: [layer("http://x/c")], headers: {}, components: true });
  assert.match(r.component.js, /__tabernetesEvaluated/);
  assert.equal(globalThis.__tabernetesEvaluated, undefined);
  // No code of the page or of the Go thread can run a text as code.
  for (const f of ["wasi.js", "transpile.js", "transpile.worker.js", "async_lists.js", "wasi_messages.js"]) {
    let src = readFileSync(new URL(`../src/${f}`, import.meta.url), "utf8").replace(/^\s*\/\/.*$/gm, "");
    if (f === "wasi.js") {
      // The one exception: newPodWorkers starts the pod worker of components
      // from a data: URL or a Blob URL with the text of component.worker.js
      // (decision 13). wasi_transport.test.mjs makes sure that the generated
      // JavaScript never goes into these URLs.
      const start = src.indexOf("function newPodWorkers(");
      const end = start + src.slice(start + 1).search(/\n(async )?function /) + 1;
      assert.ok(start >= 0 && end > start, "wasi.js has no newPodWorkers");
      const pod = src.slice(start, end);
      assert.deepEqual([...pod.matchAll(/new Blob\(\[(\w+)\]|"data:text\/javascript," \+ encodeURIComponent\((\w+)\)/g)].map((m) => m[1] ?? m[2]), ["textValue", "textValue"]);
      src = src.slice(0, start) + src.slice(end);
    }
    for (const re of [/\beval\s*\(/, /\bFunction\s*\(/, /\bimport\s*\(/, /createObjectURL/, /importScripts/, /["'`]data:/]) {
      assert.doesNotMatch(src, re, `${f} contains ${re}`);
    }
  }
});
