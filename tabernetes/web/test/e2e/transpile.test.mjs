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
import { randomBytes } from "node:crypto";
import http from "node:http";
import { apply, forEachNode, getJSON, namespace, nodeName, onNode, pod, podPhase, waitFor, waitForPod } from "./harness.mjs";
import { component, withName } from "../component_fixtures.mjs";
import { evalOn, closeDevtools } from "./devtools.mjs";

const IMAGE = "ghcr.io/se-wo/tabernetes/wasi-test-cmd:latest";

// evalOn runs a function on the Go thread of a node (devtools.mjs). The
// connections close after all tests of this file.
after(closeDevtools);

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
    // The pod worker runs the command component (W1.4a).
    for (const name of ["a", "b"]) {
      const p = await waitForPod(ns, name, podPhase("Succeeded", "Failed"));
      assert.equal(p.status.containerStatuses[0].state.terminated?.exitCode, 0, JSON.stringify(p.status.containerStatuses[0].state));
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
