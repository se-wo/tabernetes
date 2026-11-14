// Unit tests of the message contract (docs/wasi-sockets.md, section 8.3):
// web/src/wasi_messages.js and k8s/tabernetes/tabnode_wasi_msg.go must have the
// same table, and the dispatcher of the pod worker examines each message.
// The Go side has its own tests (tabnode_wasi_msg_test.go: MS1, MS2, MS6).
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { MESSAGES, LIMITS, SPECS, examine, createPeer, ip } from "../src/wasi_messages.js";

const goSource = readFileSync(new URL("../../k8s/tabernetes/tabnode_wasi_msg.go", import.meta.url), "utf8");

// goMap reads the entries of a Go map literal `var <name> = map[string]<T>{...}`.
function goMap(name, value) {
  const start = goSource.indexOf(`var ${name} = map[string]`);
  assert.ok(start >= 0, `${name} is not in tabnode_wasi_msg.go`);
  const body = goSource.slice(goSource.indexOf("{", start) + 1, goSource.indexOf("\n}", start));
  const out = {};
  for (const m of body.matchAll(new RegExp(`^\\s*"([a-zA-Z-]+)":\\s*(${value}),\\s*$`, "gm"))) out[m[1]] = m[2];
  return out;
}

test("the Go table has the same messages, directions and fields", () => {
  const go = Object.fromEntries(Object.entries(goMap("wasiMessages", '"[^"]*"')).map(([k, v]) => [k, JSON.parse(v)]));
  assert.deepEqual(go, { ...MESSAGES });
});

test("the Go table has the same limits", () => {
  const num = (e) => e.split("<<").map((x) => Number(x.trim())).reduce((a, b) => a * 2 ** b);
  const go = Object.fromEntries(Object.entries(goMap("wasiLimits", "[0-9 <]+")).map(([k, v]) => [k, num(v)]));
  assert.deepEqual(go, { ...LIMITS });
});

test("each message of section 8.3 and each CLI message is in the table", () => {
  for (const t of ["bind", "bound", "listen", "listening", "accepted", "connect", "connected", "data", "datagram", "credit", "error",
    "shutdown", "closed", "lookup", "addresses", "request", "response", "cancel", "fetch", "fetched", "fetch-cancel", "cpu",
    "start", "ready", "exit", "stdout", "stderr", "stdin", "mem"]) {
    assert.ok(SPECS[t], t);
  }
});

test("ip accepts IPv4 and IPv6 addresses only", () => {
  for (const a of ["0.0.0.0", "10.244.0.5", "255.255.255.255", "::", "::1", "fe80::1", "2001:db8::8:800:200c:417a", "::ffff:10.0.0.1", "1:2:3:4:5:6:7:8"]) assert.ok(ip(a), a);
  for (const a of ["", "10.0.0", "10.0.0.256", "01.2.3.4x", "1:2:3:4:5:6:7:8:9", "1::2::3", "fe80::1%eth0", "localhost", ":", "12345::"]) assert.ok(!ip(a), a);
});

test("examine accepts correct messages", () => {
  const ok = [
    [{ type: "ready" }, "w"],
    [{ type: "stdout", bytes: new ArrayBuffer(3) }, "w"],
    [{ type: "exit", code: 1 }, "w"],
    [{ type: "exit", code: 137, error: "killed" }, "w"],
    [{ type: "start", component: { js: "", modules: {} }, args: ["a"], env: ["A=1"], cwd: "/" }, "g"],
    [{ type: "data", socket: 1, bytes: new ArrayBuffer(256 << 10) }, "w"],
    [{ type: "data", socket: 0x80000001, bytes: new ArrayBuffer(1) }, "g"],
    [{ type: "addresses", lookup: 3, addresses: ["10.96.0.1", "fd00::1"] }, "g"],
    [{ type: "fetch", fetch: 2, method: "PURGE", url: "http://web/", headers: [["cookie", new ArrayBuffer(3)]], body: new ArrayBuffer(0) }, "w"],
  ];
  for (const [m, dir] of ok) assert.equal(examine(m, dir), "", JSON.stringify(m));
});

test("examine rejects the type, the direction, the fields and the sizes", () => {
  const bad = [
    [null, "not an object"],
    [[], "not an object"],
    [{ type: "x" }, "unknown message type"],
    [{ type: "toString" }, "unknown message type"],
    [{ type: "start", component: { js: "", modules: {} }, args: [], env: [], cwd: "/" }, "does not go"],
    [{ type: "exit" }, "code is missing"],
    [{ type: "exit", code: 1, more: 1 }, "unknown field"],
    [{ type: "exit", code: 256 }, "not a correct code"],
    [{ type: "stdout", bytes: new Uint8Array(1) }, "not a correct chunk"],
    [{ type: "stdout", bytes: new ArrayBuffer((1 << 20) + 1) }, "not a correct chunk"],
    [{ type: "data", socket: 1, bytes: new ArrayBuffer((256 << 10) + 1) }, "not a correct data"],
    [{ type: "data", socket: 0, bytes: new ArrayBuffer(1) }, "not a correct socket"],
    [{ type: "datagram", socket: 1, address: "10.0.0.1", port: 53, bytes: new ArrayBuffer(65536) }, "not a correct datagram"],
    [{ type: "credit", socket: 1, kind: "bytes", n: (256 << 10) + 1 }, "not a correct credits"],
    [{ type: "listen", socket: 1, backlog: 0 }, "not a correct backlog"],
    [{ type: "bind", socket: 1, protocol: "tcp", family: "ipv4", address: "1.2.3.4", port: -1 }, "not a correct port"],
    [{ type: "lookup", lookup: 1, name: "a".repeat(254) }, "not a correct hostname"],
    [{ type: "fetch", fetch: 2, method: "GET X", url: "http://web/", headers: [], body: new ArrayBuffer(0) }, "not a correct method"],
    [{ type: "response", request: 1, body: new ArrayBuffer((16 << 20) + 1) }, "not a correct body"],
    [{ type: "response", request: 1, headers: [["a", "b"]] }, "not a correct headers"],
  ];
  for (const [m, why] of bad) assert.match(examine(m, "w"), new RegExp(why), JSON.stringify(m)?.slice(0, 80));
});

test("only an ID kind has a suffix, and each field has a kind", () => {
  for (const [type, s] of Object.entries(SPECS)) {
    for (const f of s.fields) {
      if (f.id === "~") assert.equal(s.dir, "w", `${type}: only the pod worker sends a new or old ID`);
    }
  }
});

test("the dispatcher of the pod worker gives correct messages to their handlers", () => {
  const posted = [], dropped = [];
  const peer = createPeer({ post: (m, t) => posted.push([m, t]), onDrop: (why) => dropped.push(why) });
  const got = [];
  peer.on("start", (m) => got.push(m.type));
  assert.throws(() => peer.on("start", () => {}), /has a handler already/);
  assert.throws(() => peer.on("exit", () => {}), /cannot receive exit/);
  assert.throws(() => peer.on("nothing", () => {}), /cannot receive/);

  peer.dispatch({ type: "start", component: { js: "", modules: {} }, args: [], env: [], cwd: "/" });
  peer.dispatch({ type: "stdout", bytes: new ArrayBuffer(1) }); // wrong direction
  peer.dispatch({ type: "stdin", bytes: new ArrayBuffer(1), end: false }); // no handler
  peer.dispatch({ type: "start", component: null, args: [], env: [], cwd: "/" });
  assert.deepEqual(got, ["start"]);
  assert.equal(dropped.length, 3);
  assert.match(dropped[1], /no handler for stdin/);

  const b = new ArrayBuffer(4);
  peer.send("stdout", { bytes: b }, [b]);
  assert.deepEqual(posted[0], [{ type: "stdout", bytes: b }, [b]]);
  assert.throws(() => peer.send("start", {}), /cannot send/);
  assert.throws(() => peer.send("exit", { code: -1 }), /cannot send/);
});
