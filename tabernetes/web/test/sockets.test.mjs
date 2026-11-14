// Unit tests of the stub of wasi:sockets (web/src/sockets.js), of the count
// of CO17 and of the choice between the socket table and the stub in
// runComponent. The stub gives the results of the proxy world on runwasi
// (docs/wasi-sockets.md, section 9.2, item 7). TCP is in tcp.test.mjs, name
// lookup in lookup.test.mjs. The end-to-end test of CO17 and CO19 with a
// socket is in package W3.3b.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createIdle, createSockets, nextTask, IDLE_LIMIT } from "../src/sockets.js";
import { runComponent } from "../src/component.js";
import { createPeer } from "../src/wasi_messages.js";
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

// runComponent makes the socket table for the command path (and the
// function of an entrypoint). The proxy path keeps the stub (rule 12, PX15).
test("runComponent gives the command path the socket table; the proxy path keeps the stub", async () => {
  const run = async (world, load) => {
    const sent = [];
    const drops = [];
    const peer = createPeer({ post: (m) => sent.push(m), onDrop: (why) => drops.push(why) });
    let config = null;
    const configs = new Promise((resolve) => {
      class Shim {
        constructor(c) {
          config = c;
          resolve(c);
        }
        getImportObject() {
          return {};
        }
      }
      const exports = world === "proxy" ? [["wasi:http/incoming-handler@0.2.3", "instance"]] : [["wasi:cli/run@0.2.3", "instance"]];
      const component = { js: "", modules: {}, imports: [], exports, names: { imports: [], exports: [] }, world, entrypoint: "" };
      runComponent({ component, args: [], env: [], cwd: "/" }, { peer, WASIShim: Shim, io, load }).catch(() => {});
    });
    await configs;
    return { sent, drops, peer, sockets: config.sockets };
  };

  const cmd = await run("command", async () => {
    throw new Error("no module in this test");
  });
  const tcp = cmd.sockets.tcpCreateSocket.createTcpSocket("ipv4");
  tcp.startBind(cmd.sockets.instanceNetwork.instanceNetwork(), addr);
  assert.deepEqual(cmd.sent.filter((m) => m.type === "bind"), [{ type: "bind", socket: 1, protocol: "tcp", family: "ipv4", address: "10.0.0.1", port: 80 }]);
  cmd.peer.dispatch({ type: "bound", socket: 1, port: 80 });
  tcp.finishBind();
  assert.deepEqual(cmd.drops, []);

  const memory = new WebAssembly.Module(new Uint8Array([0, 97, 115, 109, 1, 0, 0, 0, 5, 3, 1, 0, 1, 7, 10, 1, 6, 109, 101, 109, 111, 114, 121, 2, 0]));
  const proxy = await run("proxy", async () => ({
    async instantiate(_get, _imports, instantiateCore) {
      await instantiateCore(memory);
      return { "wasi:http/incoming-handler@0.2.3": { handle() {} } };
    },
  }));
  const ptcp = proxy.sockets.tcpCreateSocket.createTcpSocket("ipv4");
  assert.throws(() => ptcp.startBind(proxy.sockets.instanceNetwork.instanceNetwork(), addr), (e) => e === "access-denied");
  assert.throws(() => proxy.sockets.ipNameLookup.resolveAddresses(undefined, "kubernetes"), (e) => e === "permanent-resolver-failure");
  assert.deepEqual(proxy.sent.filter((m) => !["mem", "stderr"].includes(m.type)), []);
  // The proxy path has no handlers of the socket messages.
  proxy.peer.dispatch({ type: "bound", socket: 1, port: 80 });
  assert.deepEqual(proxy.drops, ["no handler for bound"]);
});
