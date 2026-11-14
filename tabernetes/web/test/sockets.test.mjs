// Unit tests of the stub of wasi:sockets (web/src/sockets.js) and of the
// count of CO17. The stub gives the results of the proxy world on runwasi
// (docs/wasi-sockets.md, section 9.2, item 7). The end-to-end test of CO17
// and CO19 with a socket is in phase 3 (W3.3).
import { test } from "node:test";
import assert from "node:assert/strict";
import { createIdle, createSockets, nextTask, IDLE_LIMIT } from "../src/sockets.js";
import * as io from "../node_modules/@bytecodealliance/preview2-shim/dist/browser/io.js";

const addr = { tag: "ipv4", val: { port: 80, address: [10, 0, 0, 1] } };

test("the stub gives the results of runwasi in the proxy world", () => {
  const s = createSockets({ pollableCreate: io.pollableCreate });
  const net = s.instanceNetwork.instanceNetwork();
  assert.ok(net instanceof s.network.Network);
  const tcp = s.tcpCreateSocket.createTcpSocket("ipv4");
  assert.ok(tcp instanceof s.tcp.TcpSocket);
  assert.equal(tcp.addressFamily(), "ipv4");
  assert.throws(() => tcp.startBind(net, addr), (e) => e === "access-denied");
  assert.throws(() => tcp.startConnect(net, addr), (e) => e === "access-denied");
  assert.throws(() => tcp.finishConnect(), (e) => e === "not-in-progress");
  assert.throws(() => tcp.localAddress(), (e) => e === "invalid-state");
  assert.equal(tcp.isListening(), false);
  assert.throws(() => tcp.setListenBacklogSize(0n), (e) => e === "invalid-argument");
  tcp.setKeepAliveEnabled(true);
  assert.equal(tcp.keepAliveEnabled(), true);
  tcp.setHopLimit(7);
  assert.equal(tcp.hopLimit(), 7);
  assert.equal(tcp.subscribe().ready(), true);
  const udp = s.udpCreateSocket.createUdpSocket("ipv6");
  assert.ok(udp instanceof s.udp.UdpSocket);
  assert.equal(udp.addressFamily(), "ipv6");
  assert.throws(() => udp.startBind(net, addr), (e) => e === "access-denied");
  assert.throws(() => udp.stream(undefined), (e) => e === "invalid-state");
  assert.throws(() => s.ipNameLookup.resolveAddresses(net, "kubernetes"), (e) => e === "permanent-resolver-failure");
  assert.throws(() => s.tcpCreateSocket.createTcpSocket("unix"), (e) => e === "not-supported");
});

test("CO17: the call after 1,000 calls without data waits for a pause", async () => {
  let pauses = 0;
  const idle = createIdle({ pause: () => { pauses++; return Promise.resolve(); } });
  const s = createSockets({ idle, pollableCreate: io.pollableCreate });
  const tcp = s.tcpCreateSocket.createTcpSocket("ipv4");
  for (let i = 1; i < IDLE_LIMIT; i++) assert.throws(() => tcp.localAddress(), (e) => e === "invalid-state");
  assert.equal(idle.count, IDLE_LIMIT - 1);
  // The 1,000th call returns a promise, with the same error after the pause.
  const p = tcp.localAddress();
  assert.ok(p instanceof Promise);
  await assert.rejects(p, (e) => e === "invalid-state");
  assert.equal(pauses, 1);
  assert.equal(idle.count, 0);
  // A call with data starts the count again.
  for (let i = 0; i < 10; i++) idle.call(false);
  idle.call(true);
  assert.equal(idle.count, 0);
});

test("idle.wrap keeps the result and this, and a promise counts as a wait", async () => {
  const idle = createIdle({ limit: 2, pause: () => Promise.resolve() });
  const obj = { v: 5, get: idle.wrap(function () { return this.v; }, (r) => r > 10) };
  assert.equal(obj.get(), 5);
  assert.equal(await obj.get(), 5);
  const waits = idle.wrap(() => Promise.resolve(1));
  idle.call(false);
  assert.equal(await waits(), 1);
  assert.equal(idle.count, 0);
});

test("CO19: nextTask continues through a MessageChannel", async () => {
  const realSetTimeout = globalThis.setTimeout;
  let timers = 0;
  globalThis.setTimeout = (...a) => { timers++; return realSetTimeout(...a); };
  try {
    const t0 = performance.now();
    for (let i = 0; i < 200; i++) await nextTask();
    assert.equal(timers, 0);
    // 200 timers of 4 ms would take 800 ms (fact 21).
    assert.ok(performance.now() - t0 < 400, `${performance.now() - t0} ms`);
  } finally {
    globalThis.setTimeout = realSetTimeout;
  }
});
