// devtools.mjs: JavaScript on the Go thread of a node, for the end-to-end
// tests that must look into wasi.js (see README.md). It connects to the
// DevTools port of browser.mjs.
import { createRequire } from "node:module";

// require() also finds a global install through NODE_PATH; import does not.
const { chromium } = createRequire(import.meta.url)("playwright");

let browserPromise = null, pagePromise = null;

// closeDevtools closes the connections of this module. A test file calls it
// in an after hook; the browser continues to run.
export async function closeDevtools() {
  const s = sessionPromise, b = browserPromise;
  sessionPromise = browserPromise = pagePromise = null;
  if (s) (await s).close();
  if (b) await (await b).close();
}

// page gives the page of the terminal, over the DevTools port of browser.mjs.
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
export async function evalOn(node, fn, arg) {
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
