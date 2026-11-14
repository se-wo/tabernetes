// Unit tests of UDP in the pod worker (web/src/sockets.js) with a fake Go
// side: the messages of docs/wasi-sockets.md section 8.3.3 (rules 1 to 8),
// the pod worker part of UD2, UD3, UD5, UD6, UD9, UD11, UD14, UD15 and UD20,
// the UDP options of TC22 and the count of CO17 for the UDP calls. The tests
// that name a fact of package W4.5a (runwasi) have the expected value of
// section 8.3.3. The end-to-end tests are in package W4.5b.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  createIdle,
  createSockets,
  IDLE_LIMIT,
  CREDIT_STEP,
  UDP_DEFAULTS,
  UDP_MIN_RECEIVE_BUFFER,
  UDP_MIN_SEND_BUFFER,
  UDP_CAST_CODE,
  UDP_BROADCAST_CODE,
  UDP_MULTICAST_REMOTE,
  UDP_IPV6_REMOTE_CODE,
  UDP_SEND_PERMIT,
} from "../src/sockets.js";
import { LIMITS } from "../src/wasi_messages.js";
import { fakeGo, io, v4, v6, is } from "./socket_fixtures.mjs";

const POD = "10.244.0.5";
const bytes = (n, fill = 1) => new Uint8Array(n).fill(fill);
const buffer = (n, fill = 1) => bytes(n, fill).buffer;
// trap: a thrown Error is a trap in jco (wasmtime traps).
const trap = (e) => e instanceof Error;

// bound gives a UDP socket that bound to ip:port, with its ID. Go answers
// with the pod IP and the real port (rule 3).
function bound(g, ip = [0, 0, 0, 0], port = 0, real = port || 32768) {
  const sock = g.s.udpCreateSocket.createUdpSocket("ipv4");
  sock.startBind(g.net, v4(...ip, port));
  const id = g.take("bind")[0].socket;
  g.go({ type: "bound", socket: id, address: POD, port: real });
  sock.finishBind();
  return { sock, id };
}

// from gives a datagram message of the Go side.
const from = (socket, address, port, n = 10, fill = 1) => ({ type: "datagram", socket, address, port, bytes: buffer(n, fill) });

// sendTo sends datagrams after check-send and gives the result of send.
function sendTo(out, datagrams) {
  out.checkSend();
  return out.send(datagrams);
}

test("bind and bound: the messages, the states and the error codes", () => {
  const g = fakeGo();
  const sock = g.s.udpCreateSocket.createUdpSocket("ipv4");
  assert.ok(sock instanceof g.s.udp.UdpSocket);
  assert.equal(sock.addressFamily(), "ipv4");
  assert.throws(() => sock.finishBind(), is("not-in-progress"));
  assert.throws(() => sock.localAddress(), is("invalid-state"));
  assert.throws(() => sock.remoteAddress(), is("invalid-state"));
  // stream before bind.
  assert.throws(() => sock.stream(undefined), is("invalid-state"));
  assert.throws(() => sock.stream(v4(10, 96, 0, 10, 53)), is("invalid-state"));
  assert.equal(sock.subscribe().ready(), true);
  sock.startBind(g.net, v4(0, 0, 0, 0, 0));
  // Rule 1: a new ID of the pod worker and the protocol udp.
  assert.deepEqual(g.take(), [{ type: "bind", socket: 1, protocol: "udp", family: "ipv4", address: "0.0.0.0", port: 0 }]);
  assert.throws(() => sock.startBind(g.net, v4(0, 0, 0, 0, 0)), is("concurrency-conflict"));
  assert.throws(() => sock.stream(undefined), is("invalid-state"));
  assert.throws(() => sock.finishBind(), is("would-block"));
  const p = sock.subscribe();
  assert.equal(p.ready(), false);
  // UD3: Go gives port 0 the first free port from 32768.
  g.go({ type: "bound", socket: 1, address: POD, port: 32768 });
  assert.equal(p.ready(), true);
  sock.finishBind();
  assert.deepEqual(sock.localAddress(), v4(0, 0, 0, 0, 32768));
  assert.throws(() => sock.remoteAddress(), is("invalid-state"));
  assert.throws(() => sock.finishBind(), is("not-in-progress"));
  // A second startBind.
  assert.throws(() => sock.startBind(g.net, v4(0, 0, 0, 0, 0)), is("invalid-state"));
  // A bound message for a state that does not wait for it changes nothing.
  g.go({ type: "bound", socket: 1, address: POD, port: 40000 });
  assert.deepEqual(sock.localAddress(), v4(0, 0, 0, 0, 32768));
  assert.deepEqual(g.take(), []);
  assert.deepEqual(g.drops, []);
});

test("TCP rule 9: an error answer of bind keeps the ID open until the next bind or the drop", () => {
  const g = fakeGo();
  const sock = g.s.udpCreateSocket.createUdpSocket("ipv4");
  sock.startBind(g.net, v4(10, 1, 2, 3, 53));
  g.go({ type: "bound", socket: 1, error: "address-not-bindable" });
  assert.throws(() => sock.finishBind(), is("address-not-bindable"));
  assert.throws(() => sock.localAddress(), is("invalid-state"));
  assert.throws(() => sock.stream(undefined), is("invalid-state"));
  // The next bind closes the ID of the bind that failed and uses a new ID.
  sock.startBind(g.net, v4(0, 0, 0, 0, 53));
  assert.deepEqual(g.take().map((m) => [m.type, m.socket]), [["bind", 1], ["closed", 1], ["bind", 2]]);
  g.go({ type: "bound", socket: 2, error: "address-in-use" });
  assert.throws(() => sock.finishBind(), is("address-in-use"));
  // closed also after an error answer.
  sock[Symbol.dispose]();
  assert.deepEqual(g.take(), [{ type: "closed", socket: 2 }]);
  sock[Symbol.dispose]();
  // A socket without bind has no ID and sends nothing.
  g.s.udpCreateSocket.createUdpSocket("ipv6")[Symbol.dispose]();
  assert.deepEqual(g.take(), []);
  assert.throws(() => g.s.udpCreateSocket.createUdpSocket("unix"), is("not-supported"));
});

test("TCP rule 9: dispose sends closed; the drop of a stream does not close the socket", () => {
  const g = fakeGo();
  const { sock, id } = bound(g);
  const [input, output] = sock.stream(undefined);
  input[Symbol.dispose]();
  output[Symbol.dispose]();
  assert.deepEqual(g.take(), []);
  sock[Symbol.dispose]();
  assert.deepEqual(g.take(), [{ type: "closed", socket: id }]);
  // Messages for the ID after the drop are discarded.
  g.go(from(id, "10.244.1.7", 53));
  g.go({ type: "credit", socket: id, kind: "datagram-bytes", n: 10 });
  g.go({ type: "closed", socket: id });
  assert.deepEqual(g.take(), []);
  assert.deepEqual(g.drops, []);
});

test("TCP rule 9: closed from Go in each state; each later call fails and no closed goes back", () => {
  // While bind is in progress.
  let g = fakeGo();
  let sock = g.s.udpCreateSocket.createUdpSocket("ipv4");
  sock.startBind(g.net, v4(0, 0, 0, 0, 0));
  const p = sock.subscribe();
  g.go({ type: "closed", socket: 1 });
  assert.equal(p.ready(), true);
  assert.throws(() => sock.finishBind(), is("connection-aborted"));
  assert.throws(() => sock.startBind(g.net, v4(0, 0, 0, 0, 0)), is("invalid-state"));
  sock[Symbol.dispose]();
  assert.deepEqual(g.take("closed"), []);

  // Bound, with streams.
  g = fakeGo();
  ({ sock } = bound(g));
  const [input, output] = sock.stream(v4(10, 96, 0, 10, 53));
  g.take();
  const ready = [input.subscribe(), output.subscribe()];
  g.go({ type: "closed", socket: 1, error: "connection-reset" });
  assert.ok(ready.every((r) => r.ready()));
  assert.throws(() => input.receive(1n), is("connection-reset"));
  assert.throws(() => output.checkSend(), is("connection-reset"));
  assert.throws(() => output.send([{ data: bytes(1) }]), is("connection-reset"));
  assert.throws(() => sock.stream(undefined), trap);
  input[Symbol.dispose]();
  output[Symbol.dispose]();
  assert.throws(() => sock.stream(undefined), is("invalid-state"));
  assert.throws(() => sock.localAddress(), is("invalid-state"));
  assert.throws(() => sock.remoteAddress(), is("invalid-state"));
  assert.equal(sock.subscribe().ready(), true);
  sock[Symbol.dispose]();
  assert.deepEqual(g.take(), []);

  // Unknown IDs: the table discards the messages.
  g.go(from(77, "10.244.1.7", 53));
  g.go({ type: "bound", socket: 78, port: 1 });
  g.go({ type: "error", socket: 79, error: "connection-refused" });
  g.go({ type: "closed", socket: 80 });
  assert.deepEqual(g.take(), []);
  assert.deepEqual(g.drops, []);
});

test("rule 4: stream with an address sends connect and does not wait", () => {
  const g = fakeGo();
  const { sock, id } = bound(g);
  const [input, output] = sock.stream(v4(10, 96, 0, 10, 53));
  assert.ok(input instanceof g.s.udp.IncomingDatagramStream);
  assert.ok(output instanceof g.s.udp.OutgoingDatagramStream);
  assert.deepEqual(g.take(), [{ type: "connect", socket: id, protocol: "udp", address: "10.96.0.10", port: 53 }]);
  // No connected: the remote address is there at once.
  assert.deepEqual(sock.remoteAddress(), v4(10, 96, 0, 10, 53));
  assert.deepEqual(sock.localAddress(), v4(0, 0, 0, 0, 32768));
  // A send without an address goes to the remote address (rule 5).
  assert.equal(sendTo(output, [{ data: bytes(3, 7) }]), 1n);
  const [d] = g.take();
  assert.deepEqual({ ...d, bytes: [...new Uint8Array(d.bytes)] }, { type: "datagram", socket: id, address: "10.96.0.10", port: 53, bytes: [7, 7, 7] });
  // The same address is permitted; another address gives invalid-argument.
  assert.equal(sendTo(output, [{ data: bytes(1), remoteAddress: v4(10, 96, 0, 10, 53) }]), 1n);
  assert.throws(() => sendTo(output, [{ data: bytes(1), remoteAddress: v4(10, 96, 0, 11, 53) }]), is("invalid-argument"));
  assert.equal(g.take("datagram").length, 1);

  // The streams of an earlier stream call must be dropped first: wasmtime
  // traps (udp.wit).
  assert.throws(() => sock.stream(undefined), trap);
  input[Symbol.dispose]();
  assert.throws(() => sock.stream(undefined), trap);
  output[Symbol.dispose]();

  // stream without an address on a connected socket sends 0.0.0.0:0.
  const [in2, out2] = sock.stream(undefined);
  assert.deepEqual(g.take(), [{ type: "connect", socket: id, protocol: "udp", address: "0.0.0.0", port: 0 }]);
  assert.throws(() => sock.remoteAddress(), is("invalid-state"));
  // A socket that is not connected needs an address for each datagram.
  assert.throws(() => sendTo(out2, [{ data: bytes(1) }]), is("invalid-argument"));
  assert.equal(sendTo(out2, [{ data: bytes(1), remoteAddress: v4(10, 244, 1, 7, 9000) }]), 1n);
  assert.deepEqual(g.take().map((m) => [m.type, m.address, m.port]), [["datagram", "10.244.1.7", 9000]]);
  in2[Symbol.dispose]();
  out2[Symbol.dispose]();
  // stream without an address on a socket that is not connected sends
  // nothing.
  const [in3, out3] = sock.stream(undefined);
  assert.deepEqual(g.take(), []);
  in3[Symbol.dispose]();
  out3[Symbol.dispose]();

  // A connect to a new address on a connected socket: one connect.
  sock.stream(v4(10, 96, 0, 10, 53)).forEach((s) => s[Symbol.dispose]());
  sock.stream(v4(10, 96, 0, 12, 5353)).forEach((s) => s[Symbol.dispose]());
  assert.deepEqual(g.take().map((m) => [m.address, m.port]), [["10.96.0.10", 53], ["10.96.0.12", 5353]]);
  // As wasmtime: an address that is not correct removes the remote address
  // before the error.
  assert.throws(() => sock.stream(v4(0, 0, 0, 0, 53)), is("invalid-argument"));
  assert.deepEqual(g.take().map((m) => [m.address, m.port]), [["0.0.0.0", 0]]);
  assert.throws(() => sock.remoteAddress(), is("invalid-state"));
  assert.deepEqual(g.drops, []);
});

test("fact 12 and rule 4: the addresses of bind, stream and send give their codes without a message", () => {
  const g = fakeGo();
  const six = g.s.udpCreateSocket.createUdpSocket("ipv6");
  // The wrong family and IPv4-mapped addresses.
  assert.throws(() => six.startBind(g.net, v4(0, 0, 0, 0, 0)), is("invalid-argument"));
  assert.throws(() => six.startBind(g.net, v6([0, 0, 0, 0, 0, 0xffff, 0, 0], 0)), is("invalid-argument"));
  const four = g.s.udpCreateSocket.createUdpSocket("ipv4");
  assert.throws(() => four.startBind(g.net, v6([0, 0, 0, 0, 0, 0, 0, 0], 0)), is("invalid-argument"));
  assert.throws(() => four.startBind(g.net, { tag: "unix", val: {} }), is("invalid-argument"));
  assert.deepEqual(g.take(), []);

  const { sock } = bound(g);
  const remote = [
    [v6([0, 0, 0, 0, 0, 0, 0, 1], 53), "invalid-argument"],
    [v4(0, 0, 0, 0, 53), "invalid-argument"],
    [v4(10, 96, 0, 10, 0), "invalid-argument"],
    [v4(255, 255, 255, 255, 53), UDP_BROADCAST_CODE],
  ];
  for (const [addr, code] of remote) assert.throws(() => sock.stream(addr), is(code), JSON.stringify(addr));
  const [, out] = sock.stream(undefined);
  for (const [addr, code] of remote) {
    assert.throws(() => sendTo(out, [{ data: bytes(1), remoteAddress: addr }]), is(code), JSON.stringify(addr));
  }
  assert.deepEqual(g.take(), []);
});

test("fact 5 of W4.5a: broadcast gives access-denied, multicast is sent, bind to them gives invalid-argument", () => {
  assert.equal(UDP_CAST_CODE, "invalid-argument");
  assert.equal(UDP_BROADCAST_CODE, "access-denied");
  assert.equal(UDP_MULTICAST_REMOTE, true);
  const g = fakeGo();
  const { sock: s4, id } = bound(g);
  const streams = s4.stream(undefined);
  const out = streams[1];
  assert.throws(() => sendTo(out, [{ data: bytes(1), remoteAddress: v4(255, 255, 255, 255, 53) }]), is("access-denied"));
  // The tab node drops a datagram to a multicast address: it has no
  // receivers.
  assert.equal(sendTo(out, [{ data: bytes(1), remoteAddress: v4(224, 0, 0, 1, 53) }]), 1n);
  assert.deepEqual(g.take().map((m) => [m.type, m.socket, m.address, m.port]), [["datagram", id, "224.0.0.1", 53]]);
  streams.forEach((x) => x[Symbol.dispose]());
  s4.stream(v4(224, 0, 0, 1, 53));
  assert.deepEqual(g.take().map((m) => [m.type, m.address]), [["connect", "224.0.0.1"]]);
  const sock = g.s.udpCreateSocket.createUdpSocket("ipv4");
  assert.throws(() => sock.startBind(g.net, v4(239, 1, 2, 3, 5000)), is(UDP_CAST_CODE));
  assert.throws(() => sock.startBind(g.net, v4(255, 255, 255, 255, 5000)), is(UDP_CAST_CODE));
  const six = g.s.udpCreateSocket.createUdpSocket("ipv6");
  assert.throws(() => six.startBind(g.net, v6([0xff02, 0, 0, 0, 0, 0, 0, 1], 5000)), is(UDP_CAST_CODE));
  assert.deepEqual(g.take(), []);
});

test("fact 5 of W4.5a: an IPv6 socket binds to :: and ::1; an IPv6 remote address gives remote-unreachable", () => {
  assert.equal(UDP_IPV6_REMOTE_CODE, "remote-unreachable");
  const g = fakeGo();
  const sock = g.s.udpCreateSocket.createUdpSocket("ipv6");
  sock.startBind(g.net, v6([0, 0, 0, 0, 0, 0, 0, 1], 0));
  assert.deepEqual(g.take(), [{ type: "bind", socket: 1, protocol: "udp", family: "ipv6", address: "::1", port: 0 }]);
  g.go({ type: "bound", socket: 1, address: "::1", port: 32768 });
  sock.finishBind();
  assert.deepEqual(sock.localAddress(), v6([0, 0, 0, 0, 0, 0, 0, 1], 32768));
  assert.throws(() => sock.stream(v6([0, 0, 0, 0, 0, 0, 0, 1], 53)), is(UDP_IPV6_REMOTE_CODE));
  const [, out] = sock.stream(undefined);
  assert.throws(() => sendTo(out, [{ data: bytes(1), remoteAddress: v6([0xfd00, 0, 0, 0, 0, 0, 0, 1], 53) }]), is(UDP_IPV6_REMOTE_CODE));
  // The unspecified address and the port 0 come first.
  assert.throws(() => sendTo(out, [{ data: bytes(1), remoteAddress: v6([0, 0, 0, 0, 0, 0, 0, 0], 53) }]), is("invalid-argument"));
  assert.deepEqual(g.take(), []);

  const any = g.s.udpCreateSocket.createUdpSocket("ipv6");
  any.startBind(g.net, v6([0, 0, 0, 0, 0, 0, 0, 0], 5353));
  assert.equal(g.take("bind")[0].address, "::");
});

test("rule 3 and UD20: a socket on 127.0.0.1 shows 127.0.0.1:<port> and the source pod IP as 127.0.0.1", () => {
  const g = fakeGo();
  const { sock, id } = bound(g, [127, 0, 0, 1], 0, 40000);
  assert.deepEqual(sock.localAddress(), v4(127, 0, 0, 1, 40000));
  const [input] = sock.stream(undefined);
  g.go(from(id, POD, 41000, 4, 9));
  const [d] = input.receive(5n);
  assert.deepEqual(d.remoteAddress, v4(127, 0, 0, 1, 41000));
  assert.deepEqual([...d.data], [9, 9, 9, 9]);
});

test("rule 3 and UD15: a socket on 0.0.0.0 shows the address of the message", () => {
  const g = fakeGo();
  const { sock, id } = bound(g, [0, 0, 0, 0], 5353);
  const [input] = sock.stream(undefined);
  g.go(from(id, POD, 41000, 3));
  g.go(from(id, "10.96.0.10", 53, 2));
  g.go(from(id, "10.244.1.7", 9000, 0));
  // UD15: the boundaries of the datagrams stay.
  const got = input.receive(10n);
  assert.deepEqual(got.map((d) => [d.remoteAddress, d.data.byteLength]), [
    [v4(10, 244, 0, 5, 41000), 3],
    [v4(10, 96, 0, 10, 53), 2],
    [v4(10, 244, 1, 7, 9000), 0],
  ]);
  assert.deepEqual(input.receive(10n), []);
});

test("rule 3: a socket on 0.0.0.0 that connects to 127.0.0.1 shows the pod IP as 127.0.0.1", () => {
  const g = fakeGo();
  const { sock, id } = bound(g);
  const [input] = sock.stream(v4(127, 0, 0, 1, 53));
  g.take();
  g.go(from(id, POD, 53, 5));
  assert.deepEqual(input.receive(1n).map((d) => d.remoteAddress), [v4(127, 0, 0, 1, 53)]);
  // Not dropped as a datagram of another source (rule 4).
  assert.deepEqual(g.take(), [{ type: "credit", socket: id, kind: "datagram-bytes", n: 5 }]);
});

test("rule 4: a datagram of another source after stream with an address is dropped, and its credit goes back", () => {
  const g = fakeGo();
  const { sock, id } = bound(g);
  // In the queue before the connect.
  g.go(from(id, "10.244.1.9", 53, 100));
  g.go(from(id, "10.96.0.10", 53, 7));
  const [input] = sock.stream(v4(10, 96, 0, 10, 53));
  assert.deepEqual(g.take().map((m) => m.type), ["connect", "credit"]);
  // It crossed the connect.
  g.go(from(id, "10.244.1.9", 53, 200));
  g.go(from(id, "10.96.0.10", 54, 300));
  assert.deepEqual(g.take(), [
    { type: "credit", socket: id, kind: "datagram-bytes", n: 200 },
    { type: "credit", socket: id, kind: "datagram-bytes", n: 300 },
  ]);
  g.go(from(id, "10.96.0.10", 53, 8));
  assert.deepEqual(input.receive(10n).map((d) => d.data.byteLength), [7, 8]);
  assert.deepEqual(g.take(), [{ type: "credit", socket: id, kind: "datagram-bytes", n: 15 }]);
});

test("UD14: a datagram of 65,508 bytes gives datagram-too-large and no message", () => {
  const g = fakeGo();
  const { sock, id } = bound(g);
  const [, out] = sock.stream(v4(10, 96, 0, 10, 53));
  g.take();
  assert.equal(LIMITS.datagram, 65507);
  assert.throws(() => sendTo(out, [{ data: bytes(65508) }]), is("datagram-too-large"));
  assert.deepEqual(g.take(), []);
  assert.equal(sendTo(out, [{ data: bytes(65507) }]), 1n);
  const [d] = g.take();
  assert.equal(d.socket, id);
  assert.equal(d.bytes.byteLength, 65507);
  // An error on a later datagram ends the list (udp.wit).
  assert.equal(sendTo(out, [{ data: bytes(1) }, { data: bytes(65508) }, { data: bytes(1) }]), 1n);
  assert.equal(g.take("datagram").length, 1);
});

test("rule 6: the credits of datagram bytes in the two directions", () => {
  const g = fakeGo();
  const { sock, id } = bound(g);
  const [input, out] = sock.stream(v4(10, 96, 0, 10, 53));
  g.take();
  const ready = out.subscribe();
  assert.equal(ready.ready(), true);
  // check-send gives 16 (fact 6). 256 KiB of credit permit 4 datagrams of
  // 65,507 bytes; the fifth would block, and send gives 4.
  assert.equal(out.checkSend(), 16n);
  assert.equal(out.send([0, 1, 2, 3, 4].map(() => ({ data: bytes(65507) }))), 4n);
  // 256 KiB - 4 * 65,507 = 116 bytes: check-send gives 0 until the credit
  // permits a datagram of 65,507 bytes.
  assert.equal(out.checkSend(), 0n);
  assert.equal(ready.ready(), false);
  assert.equal(out.subscribe().ready(), false);
  // The pod worker never sends more than its credit (rule 6).
  const sent = g.take("datagram").reduce((n, m) => n + m.bytes.byteLength, 0);
  assert.ok(sent <= LIMITS.credits);
  // Other kinds of credit are not for UDP.
  g.go({ type: "credit", socket: id, kind: "bytes", n: 200000 });
  assert.equal(out.checkSend(), 0n);
  g.go({ type: "credit", socket: id, kind: "datagram-bytes", n: 65507 });
  assert.equal(ready.ready(), true);
  assert.equal(out.checkSend(), 16n);
  // A small datagram with a small credit: send gives 0, as a send that would
  // block on runwasi.
  assert.equal(out.send([{ data: bytes(65507) }]), 1n);
  assert.equal(out.checkSend(), 16n);
  assert.equal(out.send([{ data: bytes(200) }]), 0n);
  assert.equal(out.checkSend(), 0n);
  assert.equal(g.take("datagram").length, 1);

  // The pod worker sends credit after receive, at the latest when its
  // queue is empty or at 64 KiB.
  const n = 60 << 10;
  for (let i = 0; i < 4; i++) g.go(from(id, "10.96.0.10", 53, n));
  assert.deepEqual(g.take(), []);
  assert.equal(input.receive(1n).length, 1);
  assert.deepEqual(g.take(), []);
  assert.equal(input.receive(1n).length, 1);
  assert.deepEqual(g.take(), [{ type: "credit", socket: id, kind: "datagram-bytes", n: 2 * n }]);
  assert.ok(2 * n >= CREDIT_STEP);
  assert.equal(input.receive(5n).length, 2);
  assert.deepEqual(g.take(), [{ type: "credit", socket: id, kind: "datagram-bytes", n: 2 * n }]);
  // receive(0) takes nothing.
  g.go(from(id, "10.96.0.10", 53, 1));
  assert.deepEqual(input.receive(0n), []);
  assert.equal(input.receive(1n).length, 1);
  assert.deepEqual(g.take(), [{ type: "credit", socket: id, kind: "datagram-bytes", n: 1 }]);
});

test("fact 6 of W4.5a: check-send gives the fixed permit of wasmtime (16), not a count of bytes", () => {
  assert.equal(UDP_SEND_PERMIT, 16);
  const g = fakeGo();
  const { sock, id } = bound(g);
  const [, out] = sock.stream(v4(10, 96, 0, 10, 53));
  assert.equal(out.checkSend(), 16n);
  assert.equal(out.checkSend(), 16n);
  for (let i = 0; i < 20; i++) g.go({ type: "credit", socket: id, kind: "datagram-bytes", n: LIMITS.credits });
  assert.equal(out.checkSend(), 16n);
  assert.equal(out.send(Array.from({ length: 16 }, () => ({ data: bytes(10) }))), 16n);
  assert.equal(g.take("datagram").length, 16);
});

test("udp.wit: send without check-send or with more datagrams than check-send permitted traps", () => {
  const g = fakeGo();
  const { sock } = bound(g);
  const [, out] = sock.stream(v4(10, 96, 0, 10, 53));
  g.take();
  // An empty list sends nothing and needs no check-send.
  assert.equal(out.send([]), 0n);
  assert.throws(() => out.send([{ data: bytes(1) }]), trap);
  assert.equal(out.checkSend(), 16n);
  assert.throws(() => out.send(Array.from({ length: 17 }, () => ({ data: bytes(1) }))), trap);
  // Each send needs a new check-send.
  out.checkSend();
  assert.equal(out.send([{ data: bytes(1) }]), 1n);
  assert.throws(() => out.send([{ data: bytes(1) }]), trap);
  assert.equal(g.take("datagram").length, 1);
});

test("rule 7 and fact 3 of W4.5a: error gives one throw of connection-refused to the next send only, and the socket continues", () => {
  const g = fakeGo();
  const { sock, id } = bound(g);
  const [input, out] = sock.stream(v4(10, 96, 0, 10, 53));
  g.take();
  g.go({ type: "error", socket: id, error: "connection-refused" });
  // A second error before the send does not add a second throw.
  g.go({ type: "error", socket: id, error: "connection-refused" });
  // receive never takes the error; check-send gives 16 (runwasi).
  assert.equal(input.subscribe().ready(), false);
  assert.deepEqual(input.receive(1n), []);
  assert.equal(out.checkSend(), 16n);
  // The send that takes the error sends nothing.
  assert.throws(() => out.send([{ data: bytes(1) }]), is("connection-refused"));
  assert.deepEqual(g.take(), []);
  assert.equal(sendTo(out, [{ data: bytes(1) }]), 1n);
  assert.equal(g.take("datagram").length, 1);
  // A datagram in the queue does not take it either.
  g.go(from(id, "10.96.0.10", 53, 3));
  g.go({ type: "error", socket: id, error: "connection-refused" });
  assert.equal(input.receive(1n).length, 1);
  assert.throws(() => sendTo(out, [{ data: bytes(1) }]), is("connection-refused"));
  assert.equal(sendTo(out, [{ data: bytes(1) }]), 1n);
});

test("CO17: 1,000 calls without data give a pause; a receive with a datagram resets the count", async () => {
  let pauses = 0;
  const idle = createIdle({ pause: () => { pauses++; return Promise.resolve(); } });
  const g = fakeGo({ idle });
  const { sock, id } = bound(g);
  // finish-bind without an error gives data.
  assert.equal(idle.count, 0);
  const [input, out] = sock.stream(v4(10, 96, 0, 10, 53));
  assert.equal(idle.count, 1);
  idle.count = 0;
  for (let i = 1; i < IDLE_LIMIT; i++) assert.deepEqual(input.receive(1n), []);
  assert.equal(idle.count, IDLE_LIMIT - 1);
  const p = input.receive(1n);
  assert.ok(p instanceof Promise);
  assert.deepEqual(await p, []);
  assert.equal(pauses, 1);
  for (let i = 0; i < 10; i++) input.receive(1n);
  assert.equal(idle.count, 10);
  g.go(from(id, "10.96.0.10", 53, 1));
  assert.equal(input.receive(1n).length, 1);
  assert.equal(idle.count, 0);
  // check-send with more than 0 gives data; check-send of 0 does not.
  out.checkSend();
  assert.equal(idle.count, 0);
  out.send([0, 1, 2, 3, 4].map(() => ({ data: bytes(65507) })));
  assert.equal(idle.count, 1);
  assert.equal(out.checkSend(), 0n);
  assert.equal(idle.count, 2);
  // An error code counts as a call without data.
  assert.throws(() => sock.startBind(g.net, v4(0, 0, 0, 0, 0)), is("invalid-state"));
  assert.equal(idle.count, 3);
});

test("TC22 and fact 7 of W4.5a: the UDP options keep their values; a set to 0 gives invalid-argument", () => {
  assert.deepEqual(UDP_DEFAULTS, { unicastHopLimit: 64, receiveBufferSize: 106_496n, sendBufferSize: 106_496n });
  assert.equal(UDP_MIN_RECEIVE_BUFFER, 1_152n);
  assert.equal(UDP_MIN_SEND_BUFFER, 2_304n);
  const check = (sock) => {
    assert.equal(sock.unicastHopLimit(), UDP_DEFAULTS.unicastHopLimit);
    assert.equal(sock.receiveBufferSize(), UDP_DEFAULTS.receiveBufferSize);
    assert.equal(sock.sendBufferSize(), UDP_DEFAULTS.sendBufferSize);
    sock.setUnicastHopLimit(42);
    assert.equal(sock.unicastHopLimit(), 42);
    sock.setUnicastHopLimit(255);
    assert.equal(sock.unicastHopLimit(), 255);
    assert.throws(() => sock.setUnicastHopLimit(0), is("invalid-argument"));
    assert.throws(() => sock.setReceiveBufferSize(0n), is("invalid-argument"));
    assert.throws(() => sock.setSendBufferSize(0n), is("invalid-argument"));
    sock.setReceiveBufferSize(65_536n);
    sock.setSendBufferSize(65_536n);
    assert.equal(sock.receiveBufferSize(), 65_536n);
    assert.equal(sock.sendBufferSize(), 65_536n);
    // A small size reads as the minimum of the kernel.
    sock.setReceiveBufferSize(1n);
    sock.setSendBufferSize(1n);
    assert.equal(sock.receiveBufferSize(), UDP_MIN_RECEIVE_BUFFER);
    assert.equal(sock.sendBufferSize(), UDP_MIN_SEND_BUFFER);
  };
  const g = fakeGo();
  check(g.s.udpCreateSocket.createUdpSocket("ipv4"));
  check(bound(g).sock);
  // The stub of the proxy world has the same options.
  check(createSockets({ pollableCreate: io.pollableCreate }).udpCreateSocket.createUdpSocket("ipv6"));
});

test("the proxy world keeps the stub of UDP", () => {
  const s = createSockets({ pollableCreate: io.pollableCreate });
  const net = s.instanceNetwork.instanceNetwork();
  const sock = s.udpCreateSocket.createUdpSocket("ipv4");
  assert.throws(() => sock.startBind(net, v4(0, 0, 0, 0, 0)), is("access-denied"));
  assert.throws(() => sock.finishBind(), is("not-in-progress"));
  assert.throws(() => sock.stream(undefined), is("invalid-state"));
  assert.throws(() => sock.localAddress(), is("invalid-state"));
  assert.equal(sock.subscribe().ready(), true);
});
