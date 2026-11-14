// End-to-end tests of the transpile worker and its cache
// (docs/wasi-sockets.md, section 8.2): CO8, CO9, CO10, CO15 and CO16.
//
// Until the pod worker can run components (package W1.4), a component pod
// gets CreateContainerError with an "unsupported" text after the transpiler
// run. These tests examine what is observable now: the count of transpiler
// runs of each Go thread (tabernetesWasi.stats() of wasi.js), the event loop
// of the Go thread during a large transpile, and the error texts.
import { test, after } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { randomBytes } from "node:crypto";
import { apply, forEachNode, getJSON, namespace, nodeName, onNode, pod, waitFor } from "./harness.mjs";
import { component, withName } from "../component_fixtures.mjs";

// require() also finds a global install through NODE_PATH; import does not.
const { chromium } = createRequire(import.meta.url)("playwright");

const IMAGE = "ghcr.io/se-wo/tabernetes/wasi-test-cmd:latest";

// The page of the terminal, over the DevTools port of browser.mjs.
let pagePromise = null;
function page() {
  pagePromise ??= (async () => {
    const browser = await chromium.connectOverCDP(`http://127.0.0.1:${process.env.CDP_PORT || "9222"}`);
    after(() => browser.close());
    for (const p of browser.contexts().flatMap((c) => c.pages())) {
      if (await p.evaluate(() => !!globalThis.tabernetesTerminal).catch(() => false)) return p;
    }
    throw new Error("no page with a ready terminal");
  })();
  return pagePromise;
}

// evalOn runs fn(arg) on the Go thread of a node: the page for the tab
// node, the node worker (node.worker.js) for a node worker.
async function evalOn(node, fn, arg) {
  const p = await page();
  if (node === "tab") return p.evaluate(fn, arg);
  const w = await waitFor(async () => {
    for (const w of p.workers()) {
      if (await w.evaluate(() => globalThis.tabernetesNodeConfig?.name).catch(() => null) === node) return w;
    }
    return null;
  }, { timeout: 30, interval: 1, what: `the Web Worker of node ${node}` });
  return w.evaluate(fn, arg);
}

const stats = (node) => evalOn(node, () => globalThis.tabernetesWasi.stats());

// record wraps load of wasi.js on the Go thread of the node: it keeps the
// sources that the Go side asks for.
const record = (node) => evalOn(node, () => {
  const w = globalThis.tabernetesWasi;
  if (!w.e2eSources) {
    const load = w.load;
    w.e2eSources = [];
    w.load = (src) => {
      w.e2eSources.push(JSON.parse(JSON.stringify(src)));
      return load(src);
    };
  }
});
const sources = (node) => evalOn(node, () => globalThis.tabernetesWasi.e2eSources);

// loadBytes gives the bytes of a component to load of wasi.js on the Go
// thread of the node, from a Blob URL with a new digest. It resolves with
// the result and with the longest time between two tasks of the event loop
// of that thread during the load.
const loadBytes = (node, bytes) => evalOn(node, async ([b64, digest]) => {
  const bin = Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
  const url = URL.createObjectURL(new Blob([bin]));
  let last = performance.now(), maxGap = 0, stop = false;
  const tick = () => {
    const now = performance.now();
    maxGap = Math.max(maxGap, now - last);
    last = now;
    if (!stop) setTimeout(tick, 0);
  };
  tick();
  const t0 = performance.now();
  try {
    const r = await globalThis.tabernetesWasi.load({ layers: [{ url, digest, kind: "wasm" }], headers: {}, components: true });
    return {
      ms: performance.now() - t0, maxGap,
      unsupported: r.unsupported, exports: r.component?.exports,
      modules: r.component && Object.values(r.component.modules).every((m) => m instanceof WebAssembly.Module),
    };
  } finally {
    stop = true;
    URL.revokeObjectURL(url);
  }
}, [Buffer.from(bytes).toString("base64"), `e2e:${randomBytes(8).toString("hex")}`]);

// The waiting state of the container of a pod, when it has one with a
// message.
const waiting = async (ns, name) => {
  const s = (await pod(ns, name)).status?.containerStatuses?.[0]?.state?.waiting;
  return s?.message ? s : null;
};

const cmdPod = (name, node) => ({
  apiVersion: "v1",
  kind: "Pod",
  metadata: { name },
  spec: {
    runtimeClassName: "wasmtime",
    restartPolicy: "Never",
    nodeSelector: onNode(node),
    containers: [{ name: "c", image: IMAGE }],
  },
});

forEachNode(["tab", "worker"], (kind) => {
  test(`CO8: two pods of the same image cause one transpiler run on ${kind}`, async (t) => {
    const ns = await namespace(t);
    const node = await nodeName(kind);
    await record(node);
    const seen = (await sources(node)).some((s) => s.layers.length && JSON.stringify(s).includes("wasi-test-cmd"));
    const before = (await stats(node)).transpiles;
    await apply([cmdPod("a", node), cmdPod("b", node)], ns);
    for (const name of ["a", "b"]) {
      const w = await waitFor(() => waiting(ns, name), { what: `the waiting state of pod ${name}` });
      assert.equal(w.reason, "CreateContainerError", w.message);
      assert.match(w.message, /WASI preview 2 component: the tab node transpiles it, but cannot run components yet/);
    }
    const s = await stats(node);
    assert.equal(s.transpileWorker, true);
    // A page that ran this image before (a second run of the tests on the
    // same page) has the result in its cache already.
    assert.equal(s.transpiles - before, seen ? 0 : 1);

    // A second entrypoint (the function after "#") is a second key: one more
    // run, and then none for the same entrypoint again.
    const src = (await sources(node)).findLast((x) => x.path && JSON.stringify(x).includes("wasi-test-cmd"));
    assert.ok(src, "the Go side did not load the image");
    const fn = `e${randomBytes(6).toString("hex")}`;
    const load = (path) => evalOn(node, async (s) => {
      const r = await globalThis.tabernetesWasi.load(s);
      return { unsupported: r.unsupported, entrypoint: r.component?.entrypoint };
    }, { ...src, path, components: true });
    const n = (await stats(node)).transpiles;
    assert.deepEqual(await load(`${src.path}#${fn}`), { unsupported: undefined, entrypoint: fn });
    assert.equal((await stats(node)).transpiles, n + 1);
    await load(`${src.path}#${fn}`);
    await load(src.path);
    assert.equal((await stats(node)).transpiles, n + 1);
  });

  test(`CO9: the Go thread stays responsive during a large transpile on ${kind}`, async () => {
    const node = await nodeName(kind);
    // About 15 MB: on the Go thread, jco needs more than 500 ms for it.
    const bytes = await component(["go"], { functions: 20000, data: 8 << 20 });
    const r = await loadBytes(node, bytes);
    assert.equal(r.unsupported, undefined, r.unsupported);
    assert.deepEqual(r.exports, [["go", "function"]]);
    assert.equal(r.modules, true);
    // The terminal (tab) and the kubelet (node worker) get the event loop
    // between the steps of the load.
    assert.ok(r.maxGap < 300, `the event loop stopped for ${Math.round(r.maxGap)} ms (load: ${Math.round(r.ms)} ms)`);
    if (kind === "worker") {
      const ready = (await getJSON(`node ${node}`)).status.conditions.find((c) => c.type === "Ready");
      assert.equal(ready.status, "True");
    }
  });
});

forEachNode(["tab"], () => {
  test("CO10: without JSPI, a component pod gets CreateContainerError with the cause", async (t) => {
    const ns = await namespace(t);
    await evalOn("tab", () => {
      globalThis.e2eSuspending = WebAssembly.Suspending;
      delete WebAssembly.Suspending;
    });
    try {
      await apply(cmdPod("nojspi", "tab"), ns);
      const w = await waitFor(() => waiting(ns, "nojspi"), { what: "the waiting state of pod nojspi" });
      assert.equal(w.reason, "CreateContainerError", w.message);
      assert.match(w.message, /no JavaScript Promise Integration \(WebAssembly\.Suspending, WebAssembly\.promising\)/);
    } finally {
      await evalOn("tab", () => {
        WebAssembly.Suspending = globalThis.e2eSuspending;
        delete globalThis.e2eSuspending;
      });
    }
  });

  test("CO15, CO16: a component with a quotation mark in a name gives an error", async () => {
    const good = await component(["zz"]);
    for (const [from, to] of [["zz", 'z"'], ["environment", 'environmen"']]) {
      const r = await loadBytes("tab", withName(good, from, to));
      assert.match(r.unsupported ?? "", /the component cannot run: .*(not a valid extern name|does not obey the WIT syntax)/, to);
    }
  });
});
