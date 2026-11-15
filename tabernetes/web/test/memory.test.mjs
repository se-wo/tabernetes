// Unit tests of the broker of the Wasm memories of the tab (web/src/memory.js,
// docs/wasi-sockets.md section 8.6 and open question 2 of section 14).
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  createBroker, createGrants, remoteBroker, serveRemote, isMemoryError, NO_MEMORY,
  MAX_INSTANCES, TAB_MEMORIES, RESERVE, GRANT_WAIT_MS,
} from "../src/memory.js";
import { createPeer } from "../src/wasi_messages.js";

// broker gives a broker with a clock of the test, and a pod worker that
// records the messages of the broker.
function setup(opts = {}) {
  let t = 0;
  const timers = [];
  const drops = [];
  const broker = createBroker({ now: () => t, setTimer: (fn, ms) => timers.push([t + ms, fn]), onDrop: (why) => drops.push(why), ...opts });
  const pod = (base = 1) => {
    const got = [];
    const c = broker.client({ base, send: (m) => got.push(m.type) });
    return { c, got, grants: () => got.filter((m) => m === "memory-grant").length, ask: () => c.message({ type: "memory-ask" }), free: () => c.message({ type: "memory-free" }) };
  };
  const advance = (ms) => {
    t += ms;
    for (const [at, fn] of timers.splice(0)) if (at <= t) fn(); else timers.push([at, fn]);
  };
  return { broker, pod, drops, advance };
}

test("the numbers of section 14 and of the decision of section 8.6", () => {
  assert.equal(TAB_MEMORIES, 125);
  assert.equal(MAX_INSTANCES, 16);
  assert.equal(RESERVE, 8);
  assert.equal(GRANT_WAIT_MS, 2000);
});

test("the broker counts the fixed memories and the grants", () => {
  const { broker, pod } = setup({ limit: 125 });
  broker.client({ base: 2 }); // the Go binary of the page
  broker.client({ base: 1 }); // a node worker
  const p = pod();
  assert.equal(broker.stats().used, 4);
  p.ask();
  p.ask();
  assert.equal(p.grants(), 2);
  assert.equal(broker.stats().used, 6);
  p.free();
  assert.deepEqual([broker.stats().used, broker.stats().free], [5, 120]);
});

test("the reserve: asks wait while the free memories are not more than the reserve", () => {
  const { broker, pod } = setup({ limit: 20, reserve: 8 });
  broker.client({ base: 10 });
  const p = pod(); // 11 used, 9 free
  p.ask();
  p.ask();
  // The first grant leaves 8 free: the second ask waits.
  assert.equal(p.grants(), 1);
  assert.equal(broker.stats().waiting, 1);
  // A start does not ask the broker: a new pod worker counts at once.
  const q = pod();
  assert.equal(broker.stats().free, 7);
  q.c.end();
  p.free();
  assert.equal(p.grants(), 2);
  assert.equal(broker.stats().waiting, 0);
});

test("the broker serves the waiting asks of all containers in sequence (FIFO)", () => {
  const { broker, pod } = setup({ limit: 10, reserve: 0 });
  const filler = broker.client({ base: 8 });
  const a = pod(0), b = pod(0), c = pod(0);
  for (let i = 0; i < 2; i++) a.ask(); // 2 grants: full
  a.ask();
  b.ask();
  a.ask();
  c.ask();
  assert.deepEqual([a.grants(), b.grants(), c.grants()], [2, 0, 0]);
  const order = [];
  for (const [n, p] of [["a", a], ["b", b], ["c", c]]) {
    const g = p.got.push.bind(p.got);
    p.got.push = (m) => (m === "memory-grant" && order.push(n), g(m));
  }
  // Each memory that a gives back goes to the next ask, in the sequence of
  // the asks: a second ask of a does not pass the asks of b and c.
  for (let i = 0; i < 4; i++) a.free();
  assert.deepEqual(order, ["a", "b", "a", "c"]);
  filler.end();
});

test("the end of a pod worker frees its memories, its grants and its asks", () => {
  const { broker, pod } = setup({ limit: 10, reserve: 0 });
  const a = pod(1);
  for (let i = 0; i < 12; i++) a.ask();
  assert.deepEqual([broker.stats().used, broker.stats().waiting], [10, 3]);
  const b = pod(0);
  b.ask();
  a.c.end();
  assert.deepEqual([broker.stats().used, broker.stats().waiting, b.grants()], [1, 0, 1]);
  // Messages after the end do nothing.
  a.ask();
  assert.equal(broker.stats().used, 1);
});

test("a pod worker has at most MAX_INSTANCES grants and asks", () => {
  const { broker, pod, drops } = setup({ limit: 1000 });
  const p = pod();
  for (let i = 0; i < MAX_INSTANCES + 3; i++) p.ask();
  assert.equal(p.grants(), MAX_INSTANCES);
  assert.equal(drops.length, 3);
  assert.match(drops[0], /at most 16 grants/);
  assert.equal(broker.stats().used, 1 + MAX_INSTANCES);
});

test("the broker examines each message and trusts no number of a pod worker", () => {
  const { broker, pod, drops } = setup();
  const p = pod();
  p.free(); // no grant
  p.c.message({ type: "memory-ask", n: 100 });
  p.c.message({ type: "memory-grant" });
  p.c.message({ type: "collect" });
  p.c.message({ type: "stdout", bytes: new ArrayBuffer(1) });
  p.c.message(null);
  p.c.message({ type: "toString" });
  assert.equal(drops.length, 7, drops.join("\n"));
  assert.match(drops[0], /memory-free without a grant/);
  assert.match(drops[1], /unknown field/);
  assert.match(drops[2], /does not go from the pod worker/);
  assert.match(drops[4], /stdout does not go to the broker/);
  // A user without messages (a Go binary) cannot ask.
  broker.client({ base: 1 }).message({ type: "memory-ask" });
  assert.match(drops[7], /without messages/);
  assert.equal(broker.stats().used, 2);
});

test("memory-full: the broker learns a lower limit for a time, and sends collect to the pod workers", () => {
  const { broker, pod, advance } = setup({ limit: 125, reserve: 8, learnMs: 10_000, collectMs: 1000 });
  broker.client({ base: 60 }); // a browser with a lower limit, or garbage
  const a = pod(), b = pod();
  a.c.message({ type: "memory-full" });
  assert.equal(broker.stats().limit, 62);
  assert.ok(a.got.includes("collect") && b.got.includes("collect"));
  b.ask();
  assert.equal(b.grants(), 0);
  // While asks wait, collect goes out at most once in collectMs.
  b.ask();
  assert.equal(b.got.filter((m) => m === "collect").length, 1);
  advance(1000);
  b.ask();
  assert.equal(b.got.filter((m) => m === "collect").length, 2);
  // After learnMs, the limit of section 14 applies again.
  advance(9100);
  assert.equal(broker.stats().limit, 125);
  assert.equal(b.grants(), 3);
  // A refusal at or above the limit teaches nothing.
  const { broker: full, pod: pod2 } = setup({ limit: 4 });
  full.client({ base: 4 });
  pod2(0).c.message({ type: "memory-full" });
  assert.equal(full.stats().limit, 4);
});

test("createGrants: a grant, a grant that does not come in time, a late grant, collect", async () => {
  const sent = [];
  const peer = createPeer({ post: (m) => sent.push(m.type) });
  const timers = [];
  const grants = createGrants(peer, { waitMs: 2000, setTimer: (fn) => (timers.push(fn), timers.length), clearTimer: (i) => (timers[i - 1] = null) });
  const a = grants.ask();
  assert.deepEqual(sent, ["memory-ask"]);
  peer.dispatch({ type: "memory-grant" });
  assert.equal(await a, true);
  assert.equal(grants.held, 1);
  // A wait that ends: the instance then tries without a grant.
  const b = grants.ask();
  timers[1]();
  assert.equal(await b, false);
  // The late grant goes back at once.
  peer.dispatch({ type: "memory-grant" });
  assert.deepEqual(sent, ["memory-ask", "memory-ask", "memory-free"]);
  grants.free();
  grants.free(); // no second grant: nothing
  assert.deepEqual(sent.slice(3), ["memory-free"]);
  grants.full();
  assert.equal(sent.at(-1), "memory-full");
  // collect makes one memory and drops it; it never throws.
  peer.dispatch({ type: "collect" });
});

test("a node worker: remoteBroker and serveRemote give the broker of the page", () => {
  const broker = createBroker({ limit: 30, reserve: 0 });
  let page = null;
  const remote = remoteBroker((m) => page.handle(structuredClone(m)));
  page = serveRemote(broker, (m) => remote.deliver(structuredClone(m)));
  const got = [];
  const c = remote.client({ base: 1, send: (m) => got.push(m.type) });
  remote.client({ base: 1 }); // a transpile worker
  c.message({ type: "memory-ask" });
  assert.deepEqual(got, ["memory-grant"]);
  assert.equal(broker.stats().used, 3);
  c.end();
  assert.equal(broker.stats().used, 1);
  // The page does not trust the numbers of the node worker.
  page.handle({ type: "memory", op: "open", id: 99, base: 100 });
  page.handle({ type: "memory", op: "open", id: 0, base: 1 });
  assert.equal(broker.stats().used, 1);
  // The end of the node worker frees all.
  page.end();
  assert.equal(broker.stats().used, 0);
});

test("isMemoryError and NO_MEMORY", () => {
  assert.ok(isMemoryError(new RangeError("WebAssembly.Instance(): Out of memory: Cannot allocate Wasm memory for new instance")));
  assert.ok(!isMemoryError(new Error("unreachable")));
  assert.equal(NO_MEMORY, "the browser tab has no Wasm memory left for a new instance");
});

test("createGrants: an ask that waited too long is used by the next ask; keep does not wait", async () => {
  const sent = [];
  const peer = createPeer({ post: (m) => sent.push(m.type) });
  const timers = [];
  const grants = createGrants(peer, { setTimer: (fn) => (timers.push(fn), timers.length), clearTimer: () => {} });
  const a = grants.ask();
  timers[0]();
  assert.equal(await a, false);
  // The ask of a still waits at the broker: b sends no second ask.
  const b = grants.ask();
  assert.deepEqual(sent, ["memory-ask"]);
  peer.dispatch({ type: "memory-grant" });
  assert.equal(await b, true);
  // keep: one ask, and the grant stays.
  grants.keep();
  assert.deepEqual(sent, ["memory-ask", "memory-ask"]);
  peer.dispatch({ type: "memory-grant" });
  assert.equal(grants.held, 2);
});
