// Unit tests of web/src/cpu.js: the busy time of the instances of a pod
// worker (docs/wasi-sockets.md, AC2). A clock that the test controls gives the
// time.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createMeter, meterSuspending, tickImports } from "../src/cpu.js";
import { examine } from "../src/wasi_messages.js";

function setup(every = 1000) {
  let t = 0;
  const sent = [];
  const meter = createMeter({ send: (ms) => sent.push(ms), now: () => t, every });
  const total = () => sent.reduce((a, b) => a + b, 0);
  return { meter, sent, total, advance: (ms) => (t += ms) };
}

test("AC2: an export call is busy until it ends", () => {
  const { meter, sent, total, advance } = setup();
  advance(500); // before the call: not busy
  meter.begin();
  advance(250);
  meter.end();
  assert.deepEqual(sent, [250]);
  advance(5000); // after the call: not busy
  meter.flush();
  assert.equal(total(), 250);
});

test("AC2: a suspended call is not busy", async () => {
  const { meter, total, advance } = setup();
  let resolve;
  const wait = meter.suspending(() => new Promise((r) => (resolve = r)));
  meter.begin();
  advance(100);
  const p = wait();
  advance(10_000); // suspended, for example on stdin
  meter.tick();
  resolve(7);
  assert.equal(await p, 7);
  advance(50);
  meter.end();
  assert.equal(total(), 150);
});

test("AC2: an import that does not suspend stays busy", () => {
  const { meter, total, advance } = setup();
  const now = meter.suspending(() => 42);
  meter.begin();
  advance(30);
  assert.equal(now(), 42);
  advance(30);
  meter.end();
  assert.equal(total(), 60);
});

test("AC2: a rejected import continues the busy time", async () => {
  const { meter, total, advance } = setup();
  const fail = meter.suspending(() => Promise.reject(new Error("no")));
  meter.begin();
  advance(10);
  const p = fail();
  advance(1000);
  await assert.rejects(p, /no/);
  advance(20);
  meter.end();
  assert.equal(total(), 30);
});

test("AC2: a loop of CPU work that calls an import sends a cpu message each second", () => {
  const { meter, sent, advance } = setup();
  const imports = tickImports(meter, { "wasi:clocks/monotonic-clock@0.2.3": { now: () => 1n } });
  const now = imports["wasi:clocks/monotonic-clock@0.2.3"].now;
  meter.begin();
  for (let i = 0; i < 35; i++) {
    advance(100);
    now();
  }
  // 3.5 seconds: three messages, and the rest at the end.
  assert.deepEqual(sent, [1000, 1000, 1000]);
  meter.end();
  assert.deepEqual(sent, [1000, 1000, 1000, 500]);
  for (const ms of sent) assert.equal(examine({ type: "cpu", ms }, "w"), "");
});

test("tickImports keeps classes and other values", () => {
  const { meter } = setup();
  class Pollable {}
  const fn = () => 1;
  const out = tickImports(meter, { "wasi:io/poll": { Pollable, poll: fn, n: 3 } });
  assert.equal(out["wasi:io/poll"].Pollable, Pollable);
  assert.equal(out["wasi:io/poll"].n, 3);
  assert.notEqual(out["wasi:io/poll"].poll, fn);
  assert.equal(out["wasi:io/poll"].poll(), 1);
});

test("meterSuspending wraps the imports that jco makes with WebAssembly.Suspending", async () => {
  const { meter, total, advance } = setup();
  const made = [];
  class Suspending {
    constructor(fn) { this.fn = fn; made.push(this); }
  }
  const WA = { Suspending };
  const restore = meterSuspending(meter, WA);
  const s = new WA.Suspending(() => Promise.resolve(1));
  assert.ok(s instanceof Suspending);
  meter.begin();
  advance(10);
  const p = s.fn();
  advance(500);
  await p;
  meter.end();
  assert.equal(total(), 10);
  restore();
  assert.equal(WA.Suspending, Suspending);
});

test("a long busy time goes into more than one cpu message", () => {
  const { meter, sent, advance } = setup();
  meter.begin();
  advance(7200e3 + 5);
  meter.end();
  assert.deepEqual(sent, [3600e3, 3600e3, 5]);
});
