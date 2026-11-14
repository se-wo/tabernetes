// Unit tests of TCP in the pod worker (web/src/sockets.js) with a fake Go
// side: the messages of docs/wasi-sockets.md section 8.3.2 (rules 1 to 9),
// TC5, TC6, TC13, TC17, TC20 to TC22, the credits of TC12 and the count of
// CO17 for the TCP streams. The end-to-end tests are in package W3.3b.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createIdle, DEFAULT_BACKLOG, WINDOW, CREDIT_STEP } from "../src/sockets.js";
import { LIMITS } from "../src/wasi_messages.js";
import { fakeGo, GO, v4, v6, is, lastFailed, closed } from "./socket_fixtures.mjs";

const bytes = (n, fill = 1) => new Uint8Array(n).fill(fill).buffer;

// listener gives a socket that listens on ip:port, with its ID.
function listener(g, ip = [127, 0, 0, 1], port = 8080) {
  const sock = g.s.tcpCreateSocket.createTcpSocket("ipv4");
  sock.startBind(g.net, v4(...ip, port));
  const id = g.take("bind")[0].socket;
  g.go({ type: "bound", socket: id, address: "10.244.0.5", port });
  sock.finishBind();
  sock.startListen();
  g.take("listen");
  g.go({ type: "listening", socket: id });
  sock.finishListen();
  return { sock, id };
}

// connection gives a socket that the component connected, with its ID and
// streams.
function connection(g, to = v4(10, 96, 0, 10, 80)) {
  const sock = g.s.tcpCreateSocket.createTcpSocket("ipv4");
  sock.startConnect(g.net, to);
  const id = g.take("connect")[0].socket;
  g.go({ type: "connected", socket: id, localAddress: "10.244.0.5", localPort: 40001, remoteAddress: "10.244.1.7", remotePort: 8080 });
  const [input, output] = sock.finishConnect();
  return { sock, id, input, output };
}

test("bind, listen and accept: messages, IDs, states, TC5 and TC17", () => {
  const g = fakeGo();
  const sock = g.s.tcpCreateSocket.createTcpSocket("ipv4");
  assert.throws(() => sock.finishBind(), is("not-in-progress"));
  assert.throws(() => sock.localAddress(), is("invalid-state"));
  assert.throws(() => sock.startListen(), is("invalid-state"));
  assert.throws(() => sock.accept(), is("invalid-state"));
  sock.startBind(g.net, v4(127, 0, 0, 1, 0));
  const [bind] = g.take();
  // Rule 1: the pod worker makes the ID, from 1 to 0x7fffffff.
  assert.deepEqual(bind, { type: "bind", socket: 1, protocol: "tcp", family: "ipv4", address: "127.0.0.1", port: 0 });
  assert.throws(() => sock.startBind(g.net, v4(127, 0, 0, 1, 0)), is("concurrency-conflict"));
  assert.throws(() => sock.finishBind(), is("would-block"));
  const p = sock.subscribe();
  assert.equal(p.ready(), false);
  // Rule 2: Go sends the pod IP and the real port.
  g.go({ type: "bound", socket: 1, address: "10.244.0.5", port: 40000 });
  assert.equal(p.ready(), true);
  sock.finishBind();
  // TC5: the IP of bind with the port of bound.
  assert.deepEqual(sock.localAddress(), v4(127, 0, 0, 1, 40000));
  assert.throws(() => sock.startBind(g.net, v4(127, 0, 0, 1, 0)), is("invalid-state"));
  assert.throws(() => sock.remoteAddress(), is("invalid-state"));
  sock.startListen();
  assert.deepEqual(g.take(), [{ type: "listen", socket: 1, backlog: DEFAULT_BACKLOG }]);
  assert.throws(() => sock.finishListen(), is("would-block"));
  assert.equal(sock.isListening(), false);
  g.go({ type: "listening", socket: 1 });
  sock.finishListen();
  assert.equal(sock.isListening(), true);
  assert.throws(() => sock.finishListen(), is("not-in-progress"));
  assert.throws(() => sock.accept(), is("would-block"));
  const ready = sock.subscribe();
  assert.equal(ready.ready(), false);
  // TC17: Go gives the pod IP; the component sees 127.0.0.1 at the two ends.
  g.go({ type: "accepted", socket: 1, accepted: GO + 1, localAddress: "10.244.0.5", localPort: 40000, remoteAddress: "10.244.0.5", remotePort: 41000 });
  assert.equal(ready.ready(), true);
  const [conn, input, output] = sock.accept();
  assert.ok(conn instanceof g.s.tcp.TcpSocket);
  assert.ok(input && output);
  // Rule 4: one credit of the kind connections for each accept.
  assert.deepEqual(g.take(), [{ type: "credit", socket: 1, kind: "connections", n: 1 }]);
  assert.deepEqual(conn.localAddress(), v4(127, 0, 0, 1, 40000));
  assert.deepEqual(conn.remoteAddress(), v4(127, 0, 0, 1, 41000));
  assert.equal(conn.isListening(), false);
  assert.equal(ready.ready(), false);
  assert.throws(() => sock.accept(), is("would-block"));
  assert.deepEqual(g.drops, []);
});

test("rule 2: a listener on 0.0.0.0 shows the addresses of accepted", () => {
  const g = fakeGo();
  const { sock } = listener(g, [0, 0, 0, 0], 8080);
  assert.deepEqual(sock.localAddress(), v4(0, 0, 0, 0, 8080));
  g.go({ type: "accepted", socket: 1, accepted: GO + 7, localAddress: "10.244.0.5", localPort: 8080, remoteAddress: "10.244.1.9", remotePort: 33333 });
  const [conn] = sock.accept();
  assert.deepEqual(conn.localAddress(), v4(10, 244, 0, 5, 8080));
  assert.deepEqual(conn.remoteAddress(), v4(10, 244, 1, 9, 33333));
});

test("connect: the remote address is the ClusterIP of connect, the local port comes from connected", () => {
  const g = fakeGo();
  const sock = g.s.tcpCreateSocket.createTcpSocket("ipv4");
  assert.throws(() => sock.finishConnect(), is("not-in-progress"));
  sock.startConnect(g.net, v4(10, 96, 0, 10, 80));
  // Rule 1: a connect without bind has a new ID.
  assert.deepEqual(g.take(), [{ type: "connect", socket: 1, protocol: "tcp", address: "10.96.0.10", port: 80 }]);
  assert.throws(() => sock.startConnect(g.net, v4(10, 96, 0, 10, 80)), is("concurrency-conflict"));
  assert.throws(() => sock.finishConnect(), is("would-block"));
  assert.equal(sock.subscribe().ready(), false);
  g.go({ type: "connected", socket: 1, localAddress: "10.244.0.5", localPort: 40001, remoteAddress: "10.244.1.7", remotePort: 8080 });
  assert.equal(sock.subscribe().ready(), true);
  const streams = sock.finishConnect();
  assert.equal(streams.length, 2);
  assert.deepEqual(sock.remoteAddress(), v4(10, 96, 0, 10, 80));
  assert.deepEqual(sock.localAddress(), v4(10, 244, 0, 5, 40001));
  assert.throws(() => sock.startConnect(g.net, v4(10, 96, 0, 10, 80)), is("invalid-state"));
  assert.throws(() => sock.startListen(), is("invalid-state"));
  assert.throws(() => sock.startBind(g.net, v4(0, 0, 0, 0, 0)), is("invalid-state"));
  assert.throws(() => sock.setListenBacklogSize(5n), is("invalid-state"));
});

test("TC17: a connection to 127.0.0.1 has 127.0.0.1 as its local address", () => {
  const g = fakeGo();
  const { sock } = connection(g, v4(127, 0, 0, 1, 8080));
  assert.deepEqual(sock.localAddress(), v4(127, 0, 0, 1, 40001));
  assert.deepEqual(sock.remoteAddress(), v4(127, 0, 0, 1, 8080));
});

test("connect after bind uses the ID of bind", () => {
  const g = fakeGo();
  const sock = g.s.tcpCreateSocket.createTcpSocket("ipv4");
  sock.startBind(g.net, v4(0, 0, 0, 0, 0));
  g.go({ type: "bound", socket: 1, address: "10.244.0.5", port: 45000 });
  sock.finishBind();
  sock.startConnect(g.net, v4(10, 244, 1, 7, 80));
  assert.deepEqual(g.take("connect"), [{ type: "connect", socket: 1, protocol: "tcp", address: "10.244.1.7", port: 80 }]);
  g.go({ type: "connected", socket: 1, localAddress: "10.244.0.5", localPort: 45000, remoteAddress: "10.244.1.7", remotePort: 80 });
  sock.finishConnect();
  assert.deepEqual(sock.localAddress(), v4(10, 244, 0, 5, 45000));
});

test("rule 5 and rule 9: an error answer gives the code, and the ID stays open until the drop", () => {
  const g = fakeGo();
  const a = g.s.tcpCreateSocket.createTcpSocket("ipv4");
  a.startBind(g.net, v4(10, 1, 2, 3, 80));
  g.go({ type: "bound", socket: 1, error: "address-not-bindable" });
  assert.throws(() => a.finishBind(), is("address-not-bindable"));
  assert.throws(() => a.localAddress(), is("invalid-state"));
  assert.deepEqual(g.take(), [{ type: "bind", socket: 1, protocol: "tcp", family: "ipv4", address: "10.1.2.3", port: 80 }]);
  // A second bind closes the ID of the bind that failed and uses a new ID.
  a.startBind(g.net, v4(0, 0, 0, 0, 80));
  assert.deepEqual(g.take().map((m) => [m.type, m.socket]), [["closed", 1], ["bind", 2]]);
  g.go({ type: "bound", socket: 2, port: 80 });
  a.finishBind();
  a.startListen();
  g.go({ type: "listening", socket: 2, error: "address-in-use" });
  assert.throws(() => a.finishListen(), is("address-in-use"));
  assert.equal(a.isListening(), false);
  a[Symbol.dispose]();
  assert.deepEqual(g.take().map((m) => [m.type, m.socket]), [["listen", 2], ["closed", 2]]);

  const b = g.s.tcpCreateSocket.createTcpSocket("ipv4");
  b.startConnect(g.net, v4(10, 96, 0, 11, 80));
  const id = g.take("connect")[0].socket;
  g.go({ type: "connected", socket: id, error: "connection-refused" });
  assert.throws(() => b.finishConnect(), is("connection-refused"));
  // As wasmtime: the socket is closed after a connect that failed.
  assert.throws(() => b.startConnect(g.net, v4(10, 96, 0, 11, 80)), is("invalid-state"));
  assert.throws(() => b.startBind(g.net, v4(0, 0, 0, 0, 0)), is("invalid-state"));
  assert.deepEqual(g.take("closed"), []);
  b[Symbol.dispose]();
  assert.deepEqual(g.take("closed"), [{ type: "closed", socket: id }]);
  b[Symbol.dispose]();
  // A socket without bind or connect has no ID and sends nothing.
  g.s.tcpCreateSocket.createTcpSocket("ipv6")[Symbol.dispose]();
  assert.deepEqual(g.take(), []);
});

test("TC6: an IPv6 socket binds to ::", () => {
  const g = fakeGo();
  const sock = g.s.tcpCreateSocket.createTcpSocket("ipv6");
  assert.equal(sock.addressFamily(), "ipv6");
  sock.startBind(g.net, v6([0, 0, 0, 0, 0, 0, 0, 0], 8080));
  assert.deepEqual(g.take(), [{ type: "bind", socket: 1, protocol: "tcp", family: "ipv6", address: "::", port: 8080 }]);
  g.go({ type: "bound", socket: 1, address: "::", port: 8080 });
  sock.finishBind();
  assert.deepEqual(sock.localAddress(), v6([0, 0, 0, 0, 0, 0, 0, 0], 8080));
  const one = g.s.tcpCreateSocket.createTcpSocket("ipv6");
  one.startBind(g.net, v6([0, 0, 0, 0, 0, 0, 0, 1], 0));
  assert.equal(g.take("bind")[0].address, "::1");
});

test("TC20: the wrong family and IPv4-mapped addresses give invalid-argument without a message", () => {
  const g = fakeGo();
  const six = g.s.tcpCreateSocket.createTcpSocket("ipv6");
  assert.throws(() => six.startConnect(g.net, v4(10, 96, 0, 10, 80)), is("invalid-argument"));
  assert.throws(() => six.startConnect(g.net, v6([0, 0, 0, 0, 0, 0xffff, 0x0a60, 0x000a], 80)), is("invalid-argument"));
  assert.throws(() => six.startBind(g.net, v4(0, 0, 0, 0, 0)), is("invalid-argument"));
  assert.throws(() => six.startBind(g.net, v6([0, 0, 0, 0, 0, 0xffff, 0, 0], 0)), is("invalid-argument"));
  const four = g.s.tcpCreateSocket.createTcpSocket("ipv4");
  assert.throws(() => four.startBind(g.net, v6([0, 0, 0, 0, 0, 0, 0, 0], 0)), is("invalid-argument"));
  assert.throws(() => four.startConnect(g.net, v6([0, 0, 0, 0, 0, 0, 0, 1], 80)), is("invalid-argument"));
  // As wasmtime: the unspecified address, port 0, multicast and broadcast.
  assert.throws(() => four.startConnect(g.net, v4(0, 0, 0, 0, 80)), is("invalid-argument"));
  assert.throws(() => four.startConnect(g.net, v4(10, 0, 0, 1, 0)), is("invalid-argument"));
  assert.throws(() => four.startConnect(g.net, v4(224, 0, 0, 1, 80)), is("invalid-argument"));
  assert.throws(() => four.startBind(g.net, v4(255, 255, 255, 255, 80)), is("invalid-argument"));
  assert.deepEqual(g.sent, []);
  // The socket stays usable.
  four.startConnect(g.net, v4(10, 0, 0, 1, 80));
  assert.equal(g.take("connect").length, 1);
});

test("TC21: a backlog of 0 gives invalid-argument; listen sends the backlog within 1 to 4096", () => {
  const g = fakeGo();
  for (const [set, want] of [[null, DEFAULT_BACKLOG], [7n, 7], [100000n, LIMITS.backlog]]) {
    const sock = g.s.tcpCreateSocket.createTcpSocket("ipv4");
    assert.throws(() => sock.setListenBacklogSize(0n), is("invalid-argument"));
    if (set !== null) sock.setListenBacklogSize(set);
    sock.startBind(g.net, v4(0, 0, 0, 0, 0));
    const id = g.take("bind")[0].socket;
    g.go({ type: "bound", socket: id, port: 9000 });
    sock.finishBind();
    sock.startListen();
    assert.deepEqual(g.take("listen"), [{ type: "listen", socket: id, backlog: want }]);
  }
});

test("TC22: the socket options keep their values", () => {
  const g = fakeGo();
  const sock = g.s.tcpCreateSocket.createTcpSocket("ipv4");
  assert.equal(sock.keepAliveEnabled(), false);
  assert.equal(sock.hopLimit(), 64);
  // The defaults of runwasi (W3.3a).
  assert.equal(sock.keepAliveIdleTime(), 7_200_000_000_000n);
  assert.equal(sock.keepAliveInterval(), 75_000_000_000n);
  assert.equal(sock.keepAliveCount(), 9);
  assert.equal(sock.receiveBufferSize(), 65_536n);
  assert.equal(sock.sendBufferSize(), 8_192n);
  sock.setKeepAliveIdleTime(1_500_000_000n);
  assert.equal(sock.keepAliveIdleTime(), 2_000_000_000n);
  sock.setKeepAliveEnabled(true);
  sock.setKeepAliveIdleTime(5_000_000_000n);
  sock.setKeepAliveInterval(1_000_000_000n);
  sock.setKeepAliveCount(3);
  sock.setHopLimit(9);
  sock.setReceiveBufferSize(4096n);
  sock.setSendBufferSize(8192n);
  assert.equal(sock.keepAliveEnabled(), true);
  assert.equal(sock.keepAliveIdleTime(), 5_000_000_000n);
  assert.equal(sock.keepAliveInterval(), 1_000_000_000n);
  assert.equal(sock.keepAliveCount(), 3);
  assert.equal(sock.hopLimit(), 9);
  assert.equal(sock.receiveBufferSize(), 4096n);
  assert.equal(sock.sendBufferSize(), 8192n);
  assert.throws(() => sock.setHopLimit(0), is("invalid-argument"));
  assert.throws(() => sock.setKeepAliveIdleTime(0n), is("invalid-argument"));
  assert.deepEqual(g.sent, []);
});

test("rule 6: never more than 256 KiB of data without credit", () => {
  const g = fakeGo();
  const { id, output } = connection(g);
  assert.equal(output.checkWrite(), BigInt(WINDOW));
  const big = new Uint8Array(WINDOW).fill(7);
  output.write(big);
  const data = g.take("data");
  assert.equal(data.reduce((n, m) => n + m.bytes.byteLength, 0), WINDOW);
  for (const m of data) {
    assert.equal(m.socket, id);
    assert.ok(m.bytes.byteLength > 0 && m.bytes.byteLength <= LIMITS.data);
  }
  assert.equal(output.checkWrite(), 0n);
  assert.throws(() => output.write(new Uint8Array(1)), /permit/);
  const p = output.subscribe();
  assert.equal(p.ready(), false);
  g.go({ type: "credit", socket: id, kind: "bytes", n: 1000 });
  assert.equal(p.ready(), true);
  assert.equal(output.checkWrite(), 1000n);
  output.write(new Uint8Array(1000));
  assert.equal(g.take("data")[0].bytes.byteLength, 1000);
  // Rule 7: a write of 0 bytes sends no data message.
  g.go({ type: "credit", socket: id, kind: "bytes", n: 10 });
  output.checkWrite();
  output.write(new Uint8Array(0));
  assert.deepEqual(g.take("data"), []);
});

test("rule 6: credit after the reads, in steps of 64 KiB or when the buffer is empty", () => {
  const g = fakeGo();
  const { id, input } = connection(g);
  assert.equal(input.read(100n).byteLength, 0);
  g.go({ type: "data", socket: id, bytes: bytes(100 << 10) });
  g.go({ type: "data", socket: id, bytes: bytes(20 << 10, 2) });
  let got = input.read(BigInt(CREDIT_STEP - 1));
  assert.equal(got.byteLength, CREDIT_STEP - 1);
  assert.deepEqual(g.take("credit"), []);
  got = input.read(1n);
  assert.deepEqual(g.take("credit"), [{ type: "credit", socket: id, kind: "bytes", n: CREDIT_STEP }]);
  // The rest of the two messages in one read.
  got = input.read(1n << 20n);
  assert.equal(got.byteLength, (120 << 10) - CREDIT_STEP);
  assert.equal(got[got.byteLength - 1], 2);
  assert.deepEqual(g.take("credit"), [{ type: "credit", socket: id, kind: "bytes", n: (120 << 10) - CREDIT_STEP }]);
  // skip also takes bytes and gives credit.
  g.go({ type: "data", socket: id, bytes: bytes(10) });
  assert.equal(input.skip(4n), 4n);
  assert.equal(input.skip(100n), 6n);
  assert.deepEqual(g.take("credit").map((m) => m.n), [10]);
});

test("TC13: shutdown sends the message; data with 0 bytes gives closed after the bytes", () => {
  const g = fakeGo();
  const { sock, id, input, output } = connection(g);
  sock.shutdown("send");
  assert.deepEqual(g.take(), [{ type: "shutdown", socket: id, how: "send" }]);
  assert.throws(() => output.checkWrite(), closed);
  assert.throws(() => output.write(new Uint8Array(0)), closed);
  g.go({ type: "data", socket: id, bytes: bytes(3) });
  g.go({ type: "data", socket: id, bytes: new ArrayBuffer(0) });
  assert.equal(input.read(10n).byteLength, 3);
  assert.throws(() => input.read(10n), closed);
  assert.equal(input.subscribe().ready(), true);
  g.take();
  // shutdown receive: reads give closed, and data gets credit at once.
  const c = connection(g);
  g.go({ type: "data", socket: c.id, bytes: bytes(5) });
  c.sock.shutdown("receive");
  assert.deepEqual(g.take().map((m) => [m.type, m.how ?? m.n]), [["credit", 5], ["shutdown", "receive"]]);
  assert.throws(() => c.input.read(1n), closed);
  g.go({ type: "data", socket: c.id, bytes: bytes(9) });
  assert.deepEqual(g.take("credit").map((m) => m.n), [9]);
  assert.equal(c.output.checkWrite(), BigInt(WINDOW));
  c.sock.shutdown("both");
  assert.throws(() => c.output.checkWrite(), closed);
});

test("rule 8: error gives last-operation-failed to the next read or write", () => {
  const g = fakeGo();
  const a = connection(g);
  g.go({ type: "data", socket: a.id, bytes: bytes(2) });
  g.go({ type: "error", socket: a.id, error: "connection-reset" });
  assert.equal(a.input.read(10n).byteLength, 2);
  assert.throws(() => a.input.read(10n), (e) => lastFailed(e) && e.val.toDebugString() === "connection-reset");
  assert.throws(() => a.input.read(10n), closed);
  assert.throws(() => a.output.checkWrite(), closed);
  const b = connection(g);
  g.go({ type: "error", socket: b.id, error: "connection-reset" });
  assert.throws(() => b.output.checkWrite(), lastFailed);
  assert.throws(() => b.input.read(1n), closed);
  // error does not end the ID.
  assert.deepEqual(g.take("closed"), []);
  b.sock[Symbol.dispose]();
  assert.deepEqual(g.take("closed"), [{ type: "closed", socket: b.id }]);
});

test("blockingRead waits for data; blockingWriteAndFlush waits for credit", async () => {
  const g = fakeGo();
  const { id, input, output } = connection(g);
  const r = input.blockingRead(10n);
  assert.ok(r instanceof Promise);
  g.go({ type: "data", socket: id, bytes: bytes(4, 9) });
  assert.deepEqual([...(await r)], [9, 9, 9, 9]);
  output.checkWrite();
  output.write(new Uint8Array(WINDOW - 2));
  g.take("data");
  const w = output.blockingWriteAndFlush(new Uint8Array(4));
  assert.ok(w instanceof Promise);
  assert.deepEqual(g.take("data"), []);
  g.go({ type: "credit", socket: id, kind: "bytes", n: 2 });
  await w;
  assert.equal(g.take("data")[0].bytes.byteLength, 4);
  // The end of the stream ends a blockingRead.
  const r2 = input.blockingRead(10n);
  g.go({ type: "data", socket: id, bytes: new ArrayBuffer(0) });
  await assert.rejects(r2, closed);
});

test("rule 9: the drop of a listener closes the connections in its queue", () => {
  const g = fakeGo();
  const { sock, id } = listener(g);
  g.go({ type: "accepted", socket: id, accepted: GO + 1, localAddress: "10.244.0.5", localPort: 8080, remoteAddress: "10.244.0.5", remotePort: 41000 });
  g.go({ type: "accepted", socket: id, accepted: GO + 2, localAddress: "10.244.0.5", localPort: 8080, remoteAddress: "10.244.0.5", remotePort: 41001 });
  g.go({ type: "accepted", socket: id, accepted: GO + 3, localAddress: "10.244.0.5", localPort: 8080, remoteAddress: "10.244.0.5", remotePort: 41002 });
  // Go closes one connection in the queue: no closed for it later.
  g.go({ type: "closed", socket: GO + 3, error: "invalid-argument" });
  const [conn, input] = sock.accept();
  g.take();
  sock[Symbol.dispose]();
  assert.deepEqual(g.take().map((m) => [m.type, m.socket]), [["closed", GO + 2], ["closed", id]]);
  // A connection that the component took stays open.
  g.go({ type: "data", socket: GO + 1, bytes: bytes(1) });
  assert.equal(input.read(5n).byteLength, 1);
  g.take();
  // The drop of a stream does not close the socket.
  input[Symbol.dispose]();
  assert.deepEqual(g.take("closed"), []);
  conn[Symbol.dispose]();
  assert.deepEqual(g.take("closed"), [{ type: "closed", socket: GO + 1 }]);
  // accepted after the close gets closed for the new ID.
  g.go({ type: "accepted", socket: id, accepted: GO + 4, localAddress: "10.244.0.5", localPort: 8080, remoteAddress: "10.244.0.5", remotePort: 41003 });
  assert.deepEqual(g.take(), [{ type: "closed", socket: GO + 4 }]);
  // Messages for IDs that the table does not know: no answer, no drop.
  g.go({ type: "data", socket: GO + 1, bytes: bytes(1) });
  g.go({ type: "credit", socket: 77, kind: "bytes", n: 1 });
  g.go({ type: "bound", socket: 78, port: 1 });
  g.go({ type: "closed", socket: 79 });
  g.go({ type: "error", socket: 80, error: "connection-reset" });
  assert.deepEqual(g.take(), []);
  assert.deepEqual(g.drops, []);
  assert.equal(g.table.size, 0);
});

test("rule 9: closed from Go in each state; the pod worker then sends no closed", () => {
  const states = {
    "bind-started": (g, s) => s.startBind(g.net, v4(0, 0, 0, 0, 0)),
    "bound": (g, s) => {
      s.startBind(g.net, v4(0, 0, 0, 0, 0));
      g.go({ type: "bound", socket: 1, port: 1 });
      s.finishBind();
    },
    "listen-started": (g, s) => {
      states.bound(g, s);
      s.startListen();
    },
    "listening": (g, s) => {
      states["listen-started"](g, s);
      g.go({ type: "listening", socket: 1 });
      s.finishListen();
    },
    "connect-started": (g, s) => s.startConnect(g.net, v4(10, 0, 0, 1, 80)),
  };
  for (const [name, to] of Object.entries(states)) {
    const g = fakeGo();
    const s = g.s.tcpCreateSocket.createTcpSocket("ipv4");
    to(g, s);
    const p = s.subscribe();
    g.go({ type: "closed", socket: 1, error: "invalid-argument" });
    assert.equal(p.ready(), true, name);
    if (name.endsWith("-started")) {
      const finish = { bind: "finishBind", listen: "finishListen", connect: "finishConnect" }[name.split("-")[0]];
      assert.throws(() => s[finish](), is("invalid-argument"), name);
    }
    assert.throws(() => s.accept(), is("invalid-state"), name);
    assert.throws(() => s.startListen(), (e) => e === "invalid-state" || e === "concurrency-conflict", name);
    assert.throws(() => s.startConnect(g.net, v4(10, 0, 0, 1, 80)), (e) => e === "invalid-state" || e === "concurrency-conflict", name);
    g.take();
    s[Symbol.dispose]();
    assert.deepEqual(g.take(), [], name);
  }
  // A connection: its streams fail.
  const g = fakeGo();
  const { sock, id, input, output } = connection(g);
  g.go({ type: "closed", socket: id, error: "invalid-argument" });
  assert.throws(() => input.read(1n), lastFailed);
  assert.throws(() => output.checkWrite(), lastFailed);
  assert.throws(() => sock.shutdown("both"), is("invalid-state"));
  sock[Symbol.dispose]();
  assert.deepEqual(g.take(), []);
});

test("the drop of a socket during bind or connect sends closed; the late answer is discarded", () => {
  const g = fakeGo();
  const s = g.s.tcpCreateSocket.createTcpSocket("ipv4");
  s.startConnect(g.net, v4(10, 0, 0, 1, 80));
  s[Symbol.dispose]();
  assert.deepEqual(g.take().map((m) => m.type), ["connect", "closed"]);
  g.go({ type: "connected", socket: 1, localPort: 4, localAddress: "10.244.0.5", remoteAddress: "10.0.0.1", remotePort: 80 });
  g.go({ type: "data", socket: 1, bytes: bytes(3) });
  assert.deepEqual(g.take(), []);
  // A connection: closed after its writes.
  const c = connection(g);
  c.output.checkWrite();
  c.output.write(new Uint8Array(3));
  c.sock[Symbol.dispose]();
  assert.deepEqual(g.take().map((m) => m.type), ["data", "closed"]);
  assert.throws(() => c.output.checkWrite(), closed);
});

test("IDs go from 1 to 0x7fffffff and skip IDs in use", () => {
  const g = fakeGo();
  const ids = new Set();
  for (let i = 0; i < 5; i++) ids.add(g.table.newID());
  assert.deepEqual([...ids], [1, 2, 3, 4, 5]);
  g.table.add(7, {});
  assert.equal(g.table.newID(), 6);
  assert.equal(g.table.newID(), 8);
});

test("CO17: TCP reads without data give a pause after 1,000 calls; a read with data resets the count", async () => {
  let pauses = 0;
  const idle = createIdle({ pause: () => { pauses++; return Promise.resolve(); } });
  const g = fakeGo({ idle });
  const { id, input, output } = connection(g);
  for (let i = 1; i < 1000; i++) assert.equal(input.read(10n).byteLength, 0);
  // A Rust read loop with set_nonblocking(true): the 1,000th read suspends.
  const p = input.read(10n);
  assert.ok(p instanceof Promise);
  assert.equal((await p).byteLength, 0);
  assert.equal(pauses, 1);
  for (let i = 0; i < 500; i++) input.read(10n);
  g.go({ type: "data", socket: id, bytes: bytes(1) });
  assert.equal(input.read(10n).byteLength, 1);
  assert.equal(idle.count, 0);
  // check-write that gives 0 counts too.
  output.checkWrite();
  output.write(new Uint8Array(WINDOW));
  for (let i = 1; i < 1000; i++) assert.equal(output.checkWrite(), 0n);
  const q = output.checkWrite();
  assert.ok(q instanceof Promise);
  assert.equal(await q, 0n);
  assert.equal(pauses, 2);
  // finish-connect, accept and a check-write with credit give data.
  g.go({ type: "credit", socket: id, kind: "bytes", n: 5 });
  for (let i = 0; i < 10; i++) idle.call(false);
  assert.equal(output.checkWrite(), 5n);
  assert.equal(idle.count, 0);
  for (let i = 0; i < 10; i++) idle.call(false);
  connection(g);
  assert.equal(idle.count, 0);
});
