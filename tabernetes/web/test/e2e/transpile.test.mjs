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
import http from "node:http";
import { apply, forEachNode, getJSON, namespace, nodeName, onNode, pod, waitFor } from "./harness.mjs";
import { component, withName } from "../component_fixtures.mjs";

// require() also finds a global install through NODE_PATH; import does not.
const { chromium } = createRequire(import.meta.url)("playwright");

const IMAGE = "ghcr.io/se-wo/tabernetes/wasi-test-cmd:latest";

// The page of the terminal, over the DevTools port of browser.mjs. The
// connection closes after all tests of this file; the browser continues to
// run.
let browserPromise = null, pagePromise = null;
after(async () => {
  if (sessionPromise) (await sessionPromise).close();
  if (browserPromise) await (await browserPromise).close();
});
function page() {
  pagePromise ??= (async () => {
    browserPromise = chromium.connectOverCDP(`http://127.0.0.1:${process.env.CDP_PORT || "9222"}`);
    const browser = await browserPromise;
    for (const p of browser.contexts().flatMap((c) => c.pages())) {
      if (await p.evaluate(() => !!globalThis.tabernetesTerminal).catch(() => false)) return p;
    }
    throw new Error("no page with a ready terminal");
  })();
  return pagePromise;
}

// evalOn runs fn(arg) on the Go thread of a node and resolves with its
// result: in the page for the tab node, in the node worker (node.worker.js)
// for a node worker. Playwright does not show the workers that started
// before it connected, so a node worker gets a session of its own over the
// DevTools protocol.
async function evalOn(node, fn, arg) {
  const p = await page();
  if (node === "tab") return p.evaluate(fn, arg);
  const expr = `(${fn})(${JSON.stringify(arg)})`;
  const cdp = await browserSession();
  const { targetInfos } = await cdp.send("Target.getTargets");
  for (const t of targetInfos.filter((t) => t.type === "worker" && t.url.endsWith("/node.worker.js"))) {
    const { sessionId } = await cdp.send("Target.attachToTarget", { targetId: t.targetId, flatten: true });
    try {
      const name = await cdp.evaluate(sessionId, "globalThis.tabernetesNodeConfig?.name");
      if (name === node) return await cdp.evaluate(sessionId, expr);
    } finally {
      await cdp.send("Target.detachFromTarget", { sessionId }).catch(() => {});
    }
  }
  throw new Error(`no Web Worker for node ${node}`);
}

// browserSession opens a WebSocket to the DevTools endpoint of the browser:
// send(method, params, sessionId), evaluate(sessionId, expression) and
// close().
let sessionPromise = null;
function browserSession() {
  sessionPromise ??= (async () => {
    const { webSocketDebuggerUrl } = await (await fetch(`http://127.0.0.1:${process.env.CDP_PORT || "9222"}/json/version`)).json();
    const ws = new WebSocket(webSocketDebuggerUrl);
    await new Promise((resolve, reject) => {
      ws.onopen = resolve;
      ws.onerror = () => reject(new Error(`cannot connect to ${webSocketDebuggerUrl}`));
    });
    let id = 0;
    const pending = new Map();
    ws.onmessage = ({ data }) => {
      const m = JSON.parse(data);
      const p = pending.get(m.id);
      if (!p) return;
      pending.delete(m.id);
      if (m.error) p.reject(new Error(`${m.error.message}${m.error.data ? `: ${m.error.data}` : ""}`));
      else p.resolve(m.result);
    };
    const send = (method, params = {}, sessionId) => new Promise((resolve, reject) => {
      pending.set(++id, { resolve, reject });
      ws.send(JSON.stringify({ id, method, params, ...(sessionId ? { sessionId } : {}) }));
    });
    const evaluate = async (sessionId, expression) => {
      const r = await send("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true }, sessionId);
      if (r.exceptionDetails) throw new Error(r.exceptionDetails.exception?.description || r.exceptionDetails.text);
      return r.result.value;
    };
    return { send, evaluate, close: () => ws.close() };
  })();
  return sessionPromise;
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

// serve makes bytes available on a URL of a small server of this process,
// for fetch on a Go thread of the page (with CORS). A large text through
// the DevTools protocol would stop the event loop of that thread itself.
const served = new Map();
let serverPromise = null;
// A hook of node:test that a test adds belongs to that test: the hooks that
// close shared connections are added here, for the whole file.
after(async () => {
  if (serverPromise) (await serverPromise).srv.close();
});
async function serve(bytes) {
  serverPromise ??= new Promise((resolve) => {
    const srv = http.createServer((req, res) => {
      const b = served.get(req.url);
      res.writeHead(b ? 200 : 404, { "Access-Control-Allow-Origin": "*", "Content-Type": "application/wasm" });
      res.end(b);
    });
    srv.listen(0, "127.0.0.1", () => resolve({ srv, port: srv.address().port }));
  });
  const { port } = await serverPromise;
  const name = `/${randomBytes(8).toString("hex")}.wasm`;
  served.set(name, bytes);
  return `http://127.0.0.1:${port}${name}`;
}

// loadBytes gives the bytes of a component to load of wasi.js on the Go
// thread of the node, with a new digest. It resolves with the result and
// with the longest time between two tasks of the event loop of that thread
// during the load.
// idle is the longest stop in the second before the load, for comparison.
const loadBytes = async (node, bytes) => evalOn(node, async ([url, digest]) => {
  let last = performance.now(), maxGap = 0, stop = false;
  const tick = () => {
    const now = performance.now();
    maxGap = Math.max(maxGap, now - last);
    last = now;
    if (!stop) setTimeout(tick, 0);
  };
  tick();
  await new Promise((r) => setTimeout(r, 1000));
  const idle = maxGap;
  maxGap = 0;
  const t0 = performance.now();
  try {
    const r = await globalThis.tabernetesWasi.load({ layers: [{ url, digest, kind: "wasm" }], headers: {}, components: true });
    return {
      ms: performance.now() - t0, maxGap, idle,
      unsupported: r.unsupported, exports: r.component?.exports,
      modules: !!r.component && Object.values(r.component.modules).every((m) => m instanceof WebAssembly.Module),
    };
  } finally {
    stop = true;
  }
}, [await serve(bytes), `e2e:${randomBytes(8).toString("hex")}`]);

// The reason and the message of the container of a pod that did not start.
// Until W1.4, the Go side gives a component the state "terminated" with the
// reason StartError (restartPolicy Never). W1.4 changes it to the state
// "waiting" with CreateContainerError (CO10, CO16).
const NOT_STARTED = ["CreateContainerError", "StartError"];
const notStarted = async (ns, name) => {
  const st = (await pod(ns, name)).status?.containerStatuses?.[0]?.state;
  const s = st?.waiting ?? st?.terminated;
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
    const before = (await stats(node)).transpiles;
    await apply([cmdPod("a", node), cmdPod("b", node)], ns);
    for (const name of ["a", "b"]) {
      const w = await waitFor(() => notStarted(ns, name), { what: `the state of pod ${name}` });
      assert.ok(NOT_STARTED.includes(w.reason), `${w.reason}: ${w.message}`);
      assert.match(w.message, /WASI preview 2 component: the tab node transpiles it, but cannot run components yet/);
    }
    const s = await stats(node);
    assert.equal(s.transpileWorker, true);
    // One run for the two pods. A page that ran this image before (other
    // tests on the same page) has the result in its cache already: no run.
    if (before === 0) assert.equal(s.transpiles, 1);
    else assert.ok(s.transpiles - before <= 1, `${s.transpiles - before} runs for two pods`);

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
    const r = await load(`${src.path}#${fn}`);
    assert.equal(r.unsupported, undefined, r.unsupported);
    assert.equal(r.entrypoint, fn);
    assert.equal((await stats(node)).transpiles, n + 1);
    await load(`${src.path}#${fn}`);
    await load(src.path);
    assert.equal((await stats(node)).transpiles, n + 1);
  });

  test(`CO9: the Go thread stays responsive during a large transpile on ${kind}`, async (t) => {
    const node = await nodeName(kind);
    // About 23 MB with 40,000 functions: jco needs more than 1 second for
    // it, which would stop a Go thread that runs it for that long.
    const bytes = await component(["go"], { functions: 40000, data: 8 << 20 });
    const r = await loadBytes(node, bytes);
    t.diagnostic(`load ${Math.round(r.ms)} ms, longest stop of the event loop ${Math.round(r.maxGap)} ms (${Math.round(r.idle)} ms in the second before)`);
    assert.equal(r.unsupported, undefined, r.unsupported);
    assert.deepEqual(r.exports, [["go", "function"]]);
    assert.equal(r.modules, true);
    // The terminal (tab) and the kubelet (node worker) get the event loop
    // between the steps of the load.
    assert.ok(r.maxGap < 400, `the event loop stopped for ${Math.round(r.maxGap)} ms (load: ${Math.round(r.ms)} ms)`);
    if (kind === "worker") {
      const ready = (await getJSON(`node ${node}`)).status.conditions.find((c) => c.type === "Ready");
      assert.equal(ready.status, "True");
    }
  });
});

forEachNode(["tab"], () => {
  test("CO10: without JSPI, a component pod does not start, and the status gives the cause", async (t) => {
    const ns = await namespace(t);
    await evalOn("tab", () => {
      globalThis.e2eSuspending = WebAssembly.Suspending;
      delete WebAssembly.Suspending;
    });
    try {
      await apply(cmdPod("nojspi", "tab"), ns);
      const w = await waitFor(() => notStarted(ns, "nojspi"), { what: "the state of pod nojspi" });
      assert.ok(NOT_STARTED.includes(w.reason), `${w.reason}: ${w.message}`);
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
      assert.match(r.unsupported ?? "", /the component cannot run: [^]*(not a valid extern name|does not obey the WIT syntax)/, to);
    }
  });
});
