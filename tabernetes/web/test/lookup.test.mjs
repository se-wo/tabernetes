// Unit tests of name lookup in the pod worker (web/src/sockets.js) with a
// fake Go side: rule 11 of docs/wasi-sockets.md section 8.3.2 and the pod
// worker part of NL1 to NL3. The end-to-end tests are in package W3.3b.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createIdle, ipText, parseIP } from "../src/sockets.js";
import { fakeGo, is } from "./socket_fixtures.mjs";

const lookup = (g, name) => g.s.ipNameLookup.resolveAddresses(g.net, name);
const all = (stream) => {
  const out = [];
  for (let a; (a = stream.resolveNextAddress()) !== undefined; ) out.push(a);
  return out;
};

test("rule 11: an IP literal gives the address without a message", () => {
  const g = fakeGo();
  assert.deepEqual(all(lookup(g, "10.96.0.1")), [{ tag: "ipv4", val: [10, 96, 0, 1] }]);
  assert.deepEqual(all(lookup(g, "::1")), [{ tag: "ipv6", val: [0, 0, 0, 0, 0, 0, 0, 1] }]);
  assert.deepEqual(all(lookup(g, "[fd00::a:1]")), [{ tag: "ipv6", val: [0xfd00, 0, 0, 0, 0, 0, 0xa, 1] }]);
  assert.equal(lookup(g, "127.0.0.1").subscribe().ready(), true);
  assert.deepEqual(g.sent, []);
});

test("rule 11: a name that is not correct gives invalid-argument without a message", () => {
  const g = fakeGo();
  for (const name of ["", "a".repeat(254), "a b", "bad/name", "ex..ample", ".lead", "x".repeat(64) + ".svc", "bücher.de", "[::1"]) {
    assert.throws(() => lookup(g, name), is("invalid-argument"), JSON.stringify(name));
  }
  assert.deepEqual(g.sent, []);
});

test("NL1 to NL3: a name sends lookup; addresses gives one address for each call", () => {
  const g = fakeGo();
  const s = lookup(g, "kubernetes");
  assert.deepEqual(g.sent, [{ type: "lookup", lookup: 1, name: "kubernetes" }]);
  assert.throws(() => s.resolveNextAddress(), is("would-block"));
  const p = s.subscribe();
  assert.equal(p.ready(), false);
  g.go({ type: "addresses", lookup: 1, addresses: ["10.96.0.1", "fd00::1"] });
  assert.equal(p.ready(), true);
  assert.deepEqual(s.resolveNextAddress(), { tag: "ipv4", val: [10, 96, 0, 1] });
  assert.deepEqual(s.resolveNextAddress(), { tag: "ipv6", val: [0xfd00, 0, 0, 0, 0, 0, 0, 1] });
  assert.equal(s.resolveNextAddress(), undefined);
  assert.equal(s.resolveNextAddress(), undefined);
  // A name of 253 characters and a dot at the end are correct.
  lookup(g, `${"a".repeat(63)}.${"b".repeat(63)}.${"c".repeat(63)}.${"d".repeat(61)}`);
  lookup(g, "web.default.svc.cluster.local.");
  assert.deepEqual(g.sent.slice(1).map((m) => m.lookup), [2, 3]);
  assert.deepEqual(g.drops, []);
});

test("NL3: an error code of Go comes from resolveNextAddress", () => {
  const g = fakeGo();
  const s = lookup(g, "example.com");
  g.go({ type: "addresses", lookup: 1, error: "name-unresolvable" });
  assert.equal(s.subscribe().ready(), true);
  assert.throws(() => s.resolveNextAddress(), is("name-unresolvable"));
  assert.equal(s.resolveNextAddress(), undefined);
});

test("rule 11: addresses for a dropped stream is discarded", () => {
  const g = fakeGo();
  const s = lookup(g, "web");
  s[Symbol.dispose]();
  g.go({ type: "addresses", lookup: 1, addresses: ["10.0.0.1"] });
  assert.deepEqual(g.sent, [{ type: "lookup", lookup: 1, name: "web" }]);
  assert.deepEqual(g.drops, []);
  assert.equal(g.table.size, 0);
});

test("CO17: resolveNextAddress with an address resets the count", async () => {
  let pauses = 0;
  const idle = createIdle({ pause: () => { pauses++; return Promise.resolve(); } });
  const g = fakeGo({ idle });
  const s = lookup(g, "web");
  for (let i = 1; i < 1000; i++) assert.throws(() => s.resolveNextAddress(), is("would-block"));
  await assert.rejects(s.resolveNextAddress(), is("would-block"));
  assert.equal(pauses, 1);
  for (let i = 0; i < 10; i++) idle.call(false);
  g.go({ type: "addresses", lookup: 1, addresses: ["10.0.0.1"] });
  s.resolveNextAddress();
  assert.equal(idle.count, 0);
});

test("the text forms of addresses", () => {
  for (const [text, want] of [
    ["0.0.0.0", "0.0.0.0"],
    ["::", "::"],
    ["::1", "::1"],
    ["fd00:0:0:1::", "fd00:0:0:1::"],
    ["1:0:0:2:0:0:0:3", "1:0:0:2::3"],
    ["1:2:3:4:5:6:7:8", "1:2:3:4:5:6:7:8"],
    ["::ffff:10.0.0.1", "::ffff:a00:1"],
    ["1:0:2:0:3:0:4:0", "1:0:2:0:3:0:4:0"],
  ]) {
    const a = parseIP(text);
    assert.ok(a, text);
    assert.equal(ipText(a), want, text);
  }
  for (const bad of ["", "1.2.3", "256.0.0.1", "1:2:3:4:5:6:7:8:9", "1::2::3", "g::1", ":::", "1.2.3.4:5"]) assert.equal(parseIP(bad), null, bad);
});
