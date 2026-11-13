// The pod worker of a proxy component for phase 0, items 5, 12 and 15. Each
// `request` message gets a new instance (PX7). The candidate of item 12 is
// "own" (src/http-own.js) or "shim" (src/http-shim.js).
//
// Messages from the page (the Go side of the PoC):
//   {type: "load", candidate, name, files}
//   {type: "request", id, method, path, authority, headers, body}
//   {type: "cancel", id}
//   {type: "fetched", fid, status, headers, body} or {type: "fetched", fid, error}
//   {type: "stats"}
// Messages to the page: loaded, response, fetch, stats.
import { WASIShim } from "@bytecodealliance/preview2-shim/instantiation";
import { pollableCreate } from "@bytecodealliance/preview2-shim/io";
import { monotonicClock, wallClock } from "@bytecodealliance/preview2-shim/clocks";
import { createHttp } from "./http-own.js";
import { createShimHttp } from "./http-shim.js";

const modules = new Map();
let instantiate = null;
let candidate = null;
let own = null;
let shim = null;
const fetches = new Map(); // fid -> resolve
let nextFid = 0;
const live = new Map(); // request id -> {timers}
let done = 0;
// Instances that the garbage collector freed (CO7, items 6 and 15).
let made = 0;
let freed = 0;
const registry = new FinalizationRegistry(() => freed++);

const goFetch = (req) => new Promise((resolve) => {
  const fid = ++nextFid;
  fetches.set(fid, resolve);
  postMessage({ type: "fetch", fid, ...req });
});

// Clocks with timers that the pod worker can cancel for one instance (PX24).
function clocksFor(state) {
  return {
    monotonicClock: {
      ...monotonicClock,
      subscribeDuration(ns) {
        const ms = Number(ns / 1000000n);
        if (ms <= 0) return pollableCreate();
        // A source with wait(): a rejection reaches the component as an
        // exception (a promise as the source would turn it into "ready").
        let ready = false;
        const wait = new Promise((resolve, reject) => {
          const t = setTimeout(() => { state.timers.delete(t); state.rejects.delete(reject); ready = true; resolve(); }, ms);
          state.timers.add(t);
          state.rejects.add(reject);
        });
        wait.catch(() => {});
        return pollableCreate({ ready: () => ready, wait: () => wait });
      },
      subscribeInstant(instant) {
        return this.subscribeDuration(instant - monotonicClock.now());
      },
    },
    wallClock,
  };
}

async function load(data) {
  for (const [f, bytes] of Object.entries(data.files)) {
    if (f.endsWith(".wasm")) modules.set(f, await WebAssembly.compile(bytes));
  }
  const url = URL.createObjectURL(new Blob([data.files[`${data.name}.js`]], { type: "text/javascript" }));
  ({ instantiate } = await import(url));
  candidate = data.candidate;
  if (candidate === "own") own = createHttp({ fetch: goFetch });
  else shim = createShimHttp({ fetch: goFetch });
}

async function request(msg) {
  const state = { timers: new Set(), rejects: new Set() };
  live.set(msg.id, state);
  const env = { REQUEST_ID: String(msg.id) };
  const sink = { write() {}, flush() {}, blockingFlush() {} };
  const wasi = new WASIShim({ environment: env, arguments: [], stdout: sink, stderr: sink, clocks: clocksFor(state), ...(own ? { http: own.http } : {}) });
  const imports = { ...wasi.getImportObject(), ...wasi.getImportObject({ asVersion: "0.2.12" }) };
  let reply;
  try {
    const inst = await instantiate((f) => modules.get(f), imports);
    made++;
    registry.register(inst, null);
    const handler = inst.incomingHandler ?? inst["wasi:http/incoming-handler@0.2.12"];
    if (own) {
      const [out, response] = own.outparam();
      await handler.handle(own.incoming(msg), out);
      const r = await Promise.race([response, Promise.resolve(null)]);
      if (!r) reply = { none: true };
      else if (r.error) reply = { error: r.error };
      else reply = { status: r.status, headers: r.headers, body: await r.body };
    } else {
      reply = await shim.handle(msg, (r, out) => handler.handle(r, out));
    }
  } catch (e) {
    reply = { trap: String(e?.message ?? e) };
  }
  if (!live.has(msg.id)) return; // cancelled
  live.delete(msg.id);
  done++;
  postMessage({ type: "response", id: msg.id, ...reply });
}

self.onmessage = async ({ data }) => {
  switch (data.type) {
    case "load":
      await load(data);
      postMessage({ type: "loaded" });
      break;
    case "request":
      request(data);
      break;
    case "cancel": {
      // mode "clear": only clear the timers. Mode "reject": also continue
      // the suspended call with an exception, so the stack of the instance
      // unwinds.
      const s = live.get(data.id);
      if (s) for (const t of s.timers) clearTimeout(t);
      if (s && data.mode === "reject") for (const r of s.rejects) r(new Error("cancelled"));
      live.delete(data.id);
      break;
    }
    case "fetched":
      fetches.get(data.fid)?.(data);
      fetches.delete(data.fid);
      break;
    case "stats":
      if (data.gc && globalThis.gc) globalThis.gc();
      if (data.gc) await new Promise((r) => setTimeout(r, 100)); // finalizers run in a later task
      postMessage({ type: "stats", live: live.size, done, made, freed });
      break;
  }
};
