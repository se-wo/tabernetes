// Unit tests of the wasi:http of Tabernetes (web/src/http.js): the incoming
// request, the response-outparam and the response message (PX20, PX21,
// PX26, PX27), client requests (CR9, CR10) and the count of CO17. The proxy
// path is in proxy.test.mjs.
import { test } from "node:test";
import assert from "node:assert/strict";
import { parse } from "@bytecodealliance/jco";
import { generate } from "@bytecodealliance/jco/component";
import { createHttp, responseFields, errorCode, splitURL, methodVariant, ERROR_CODES, FORBIDDEN, BODY_LIMIT } from "../src/http.js";
import { createIdle } from "../src/sockets.js";
import { transpile } from "../src/transpile.js";
// The io.js of the browser shim, which the bundle of the pod worker uses.
import * as io from "../node_modules/@bytecodealliance/preview2-shim/dist/browser/io.js";

const enc = new TextEncoder();
const dec = new TextDecoder();
const ab = (s) => enc.encode(s).buffer;

// fakeFetch records the client requests. Each one gets {result, cancel};
// answer(i, fields) resolves the result of request i.
function fakeFetch() {
  const calls = [];
  const fetch = (req) => {
    let resolve;
    const result = new Promise((r) => (resolve = r));
    const call = { req, cancelled: 0, answer: (f) => resolve(f) };
    calls.push(call);
    return { result, cancel: () => call.cancelled++ };
  };
  return { calls, fetch };
}

function message(fields = {}) {
  return { type: "request", request: 0x80000001, method: "GET", url: "http://example:8080/a/b?c=d", headers: [], body: new ArrayBuffer(0), ...fields };
}

// readAll reads an input stream to its end.
function readAll(stream) {
  const parts = [];
  for (;;) {
    try {
      parts.push(...stream.blockingRead(1000n));
    } catch (e) {
      assert.equal(e.tag, "closed");
      return Uint8Array.from(parts);
    }
  }
}

test("splitURL gives the authority and the request target without a change", () => {
  assert.deepEqual(splitURL("http://example:8080/a/b?c=d"), { authority: "example:8080", pathWithQuery: "/a/b?c=d" });
  assert.deepEqual(splitURL("http://h/a/../b//c"), { authority: "h", pathWithQuery: "/a/../b//c" });
  assert.deepEqual(splitURL("http://h*"), { authority: "h", pathWithQuery: "*" });
  assert.deepEqual(splitURL("http:///x"), { authority: "", pathWithQuery: "/x" });
  assert.deepEqual(splitURL("http://h"), { authority: "h", pathWithQuery: "/" });
});

test("methodVariant keeps a method that is not standard (PX29)", () => {
  assert.deepEqual(methodVariant("GET"), { tag: "get" });
  assert.deepEqual(methodVariant("TRACE"), { tag: "trace" });
  assert.deepEqual(methodVariant("get"), { tag: "other", val: "get" });
  assert.deepEqual(methodVariant("PURGE"), { tag: "other", val: "PURGE" });
});

test("the incoming request gives the method, the URL parts, the headers and the body", () => {
  const h = createHttp({ io, fetch: fakeFetch().fetch });
  const req = h.incoming(message({
    method: "PURGE",
    headers: [["cookie", ab("a=1")], ["user-agent", ab("x")], ["host", ab("example:8080")], ["connection", ab("close")], ["cookie", ab("b=2")]],
    body: ab("hello"),
  }));
  assert.deepEqual(req.method(), { tag: "other", val: "PURGE" });
  assert.equal(req.pathWithQuery(), "/a/b?c=d");
  assert.equal(req.authority(), "example:8080");
  assert.deepEqual(req.scheme(), { tag: "HTTP" });
  const headers = req.headers();
  // Fact 20: wasmtime removes host and connection.
  assert.deepEqual(headers.entries().map(([k, v]) => [k, dec.decode(v)]), [["cookie", "a=1"], ["user-agent", "x"], ["cookie", "b=2"]]);
  assert.deepEqual(headers.get("Cookie").map((v) => dec.decode(v)), ["a=1", "b=2"]);
  assert.throws(() => headers.append("x", enc.encode("y")), (e) => e.tag === "immutable");
  const body = req.consume();
  assert.throws(() => req.consume());
  assert.equal(dec.decode(readAll(body.stream())), "hello");
  assert.throws(() => body.stream());
});

test("fields check names and values as wasmtime and forbid the headers of fact 20", () => {
  const { Fields } = createHttp({ io, fetch: fakeFetch().fetch }).http.types;
  const f = new Fields();
  f.append("X-A", enc.encode("1"));
  f.append("x-a", enc.encode("2"));
  assert.deepEqual(f.entries().map(([k, v]) => [k, dec.decode(v)]), [["x-a", "1"], ["x-a", "2"]]);
  f.set("x-a", [enc.encode("3")]);
  assert.deepEqual(f.get("X-A").map((v) => dec.decode(v)), ["3"]);
  assert.equal(f.has("x-a"), true);
  const c = f.clone();
  f.delete("x-a");
  assert.equal(f.has("x-a"), false);
  assert.equal(c.has("x-a"), true);
  for (const name of FORBIDDEN) assert.throws(() => f.append(name, enc.encode("v")), (e) => e.tag === "forbidden", name);
  assert.throws(() => f.append("bad name", enc.encode("v")), (e) => e.tag === "invalid-syntax");
  assert.throws(() => f.append("x", enc.encode("a\nb")), (e) => e.tag === "invalid-syntax");
  assert.throws(() => Fields.fromList([["host", enc.encode("h")]]), (e) => e.tag === "forbidden");
});

// respond makes a response as a component does and gives the outparam state.
function respond(h, { status = 200, headers = [], chunks = null, finish = true } = {}) {
  const { Fields, OutgoingResponse, OutgoingBody, ResponseOutparam } = h.http.types;
  const [out, state] = h.outparam();
  const resp = new OutgoingResponse(Fields.fromList(headers));
  resp.setStatusCode(status);
  let error = null;
  if (chunks) {
    const body = resp.body();
    ResponseOutparam.set(out, { tag: "ok", val: resp });
    const stream = body.write();
    try {
      for (const c of chunks) {
        stream.checkWrite();
        stream.write(c);
      }
    } catch (e) {
      error = e;
    }
    if (finish && !error) OutgoingBody.finish(body, undefined);
  } else {
    ResponseOutparam.set(out, { tag: "ok", val: resp });
  }
  return { state, error };
}

test("PX16: the response message has the status, all headers in sequence and the body", () => {
  const h = createHttp({ io, fetch: fakeFetch().fetch });
  const { state } = respond(h, { status: 599, headers: [["set-cookie", enc.encode("a=1")], ["Set-Cookie", enc.encode("b=2")]], chunks: [enc.encode("ab"), enc.encode("c")] });
  const r = responseFields(state, false);
  assert.equal(r.status, 599);
  assert.deepEqual(r.headers.map(([k, v]) => [k, dec.decode(v)]), [["set-cookie", "a=1"], ["set-cookie", "b=2"]]);
  assert.ok(r.headers.every(([, v]) => v instanceof ArrayBuffer));
  assert.equal(dec.decode(r.body), "abc");
  // A response without a body has an empty body.
  const empty = responseFields(respond(h, { status: 204 }).state, false);
  assert.deepEqual([empty.status, empty.headers, empty.body.byteLength], [204, [], 0]);
});

test("PX20: a response body of more than 16 MiB fails the write and gives status 500", () => {
  const h = createHttp({ io, fetch: fakeFetch().fetch });
  const mib = new Uint8Array(1 << 20);
  const chunks = Array.from({ length: 17 }, () => mib);
  const { state, error } = respond(h, { headers: [["content-type", enc.encode("text/plain")]], chunks });
  assert.equal(error.tag, "last-operation-failed");
  // The component traps after the failed write, as the Rust component of
  // phase 0 does: the result stays 500.
  for (const trapped of [false, true]) {
    const r = responseFields(state, trapped);
    assert.deepEqual([r.status, r.headers, r.body.byteLength], [500, [], 0]);
  }
  // Exactly 16 MiB is not more than the limit.
  const ok = responseFields(respond(h, { chunks: chunks.slice(1) }).state, false);
  assert.equal(ok.status, 200);
  assert.equal(ok.body.byteLength, BODY_LIMIT);
});

test("PX21, PX26, PX27: a trap, no response and an error code", () => {
  const h = createHttp({ io, fetch: fakeFetch().fetch });
  const { ResponseOutparam } = h.http.types;
  // No response.
  const [, none] = h.outparam();
  assert.deepEqual(responseFields(none, false), {});
  assert.deepEqual(responseFields(none, true), { error: "trap" });
  // An error code.
  const [out, err] = h.outparam();
  ResponseOutparam.set(out, { tag: "err", val: { tag: "HTTP-request-denied" } });
  assert.deepEqual(responseFields(err, false), { error: "HTTP-request-denied" });
  assert.throws(() => ResponseOutparam.set(out, { tag: "err", val: { tag: "internal-error" } }));
  // A response with a body that is not finished.
  const open = respond(h, { chunks: [enc.encode("x")], finish: false }).state;
  assert.deepEqual(responseFields(open, false), { error: "HTTP-response-incomplete" });
  assert.deepEqual(responseFields(open, true), { error: "trap" });
  // A complete response, then a trap: runwasi sends the response.
  const done = respond(h, { chunks: [enc.encode("x")] }).state;
  assert.equal(responseFields(done, true).status, 200);
});

// request makes a client request as a component does.
function request(h, { scheme = { tag: "HTTP" }, authority = "svc.ns:8080", path = "/p?q", body = null, method = { tag: "post" } } = {}) {
  const { Fields, OutgoingRequest, OutgoingBody } = h.http.types;
  const req = new OutgoingRequest(Fields.fromList([["x-a", enc.encode("1")]]));
  req.setMethod(method);
  if (scheme) req.setScheme(scheme);
  if (authority !== null) req.setAuthority(authority);
  if (path !== null) req.setPathWithQuery(path);
  let ob = null;
  if (body !== null) {
    ob = req.body();
    const s = ob.write();
    s.checkWrite();
    s.write(enc.encode(body));
  }
  const future = h.http.outgoingHandler.handle(req, undefined);
  return { future, finish: () => OutgoingBody.finish(ob, undefined), ob };
}

const tick = () => new Promise((r) => setTimeout(r, 0));

test("a client request sends fetch with an absolute URL after its body is complete", async () => {
  const f = fakeFetch();
  const h = createHttp({ io, fetch: f.fetch });
  const { future, finish } = request(h, { body: "data" });
  await tick();
  assert.equal(f.calls.length, 0, "the body is not complete");
  finish();
  await tick();
  assert.equal(f.calls.length, 1);
  const { req } = f.calls[0];
  assert.equal(req.method, "POST");
  assert.equal(req.url, "http://svc.ns:8080/p?q");
  assert.deepEqual(req.headers.map(([k, v]) => [k, dec.decode(v)]), [["x-a", "1"]]);
  assert.equal(dec.decode(req.body), "data");
  assert.equal(future.get(), undefined);
  const pollable = future.subscribe();
  assert.equal(pollable.ready(), false);
  f.calls[0].answer({ status: 201, headers: [["set-cookie", ab("a")]], body: ab("resp") });
  await pollable.block();
  const got = future.get();
  assert.equal(got.tag, "ok");
  assert.equal(got.val.tag, "ok");
  const resp = got.val.val;
  assert.equal(resp.status(), 201);
  assert.deepEqual(resp.headers().entries().map(([k, v]) => [k, dec.decode(v)]), [["set-cookie", "a"]]);
  assert.equal(dec.decode(readAll(resp.consume().stream())), "resp");
  assert.deepEqual(future.get(), { tag: "err", val: undefined });
  // A drop after fetched sends no fetch-cancel.
  future[Symbol.dispose]();
  assert.equal(f.calls[0].cancelled, 0);
});

test("a client request without a scheme uses https, without an authority it fails, as wasmtime", async () => {
  const f = fakeFetch();
  const h = createHttp({ io, fetch: f.fetch });
  request(h, { scheme: null, path: null, method: { tag: "get" } });
  await tick();
  assert.equal(f.calls[0].req.url, "https://svc.ns:8080/");
  assert.equal(f.calls[0].req.method, "GET");
  assert.equal(f.calls[0].req.body.byteLength, 0);
  assert.throws(() => request(h, { authority: null }), (e) => e.tag === "HTTP-request-URI-invalid");
});

test("CR9: each error code of fetched goes to the component without a change", async () => {
  for (const code of [...ERROR_CODES, "no-such-code"]) {
    const f = fakeFetch();
    const h = createHttp({ io, fetch: f.fetch });
    const { future } = request(h);
    await tick();
    f.calls[0].answer({ error: code });
    await future.subscribe().block();
    const got = future.get();
    assert.equal(got.tag, "ok");
    assert.equal(got.val.tag, "err");
    assert.deepEqual(got.val.val, errorCode(code));
    assert.equal(got.val.val.tag, ERROR_CODES.has(code) ? code : "internal-error");
  }
  assert.deepEqual(errorCode("DNS-error"), { tag: "DNS-error", val: { rcode: undefined, infoCode: undefined } });
  assert.deepEqual(errorCode("no-such-code"), { tag: "internal-error", val: "no-such-code" });
});

// errorCodeComponent gives a component that imports code(n) -> error-code of
// the complete variant of wasi:http 0.2 and exports check(n): the case number
// that the component gets.
async function errorCodeComponent() {
  const payload = {
    "DNS-error": "$dns", "TLS-alert-received": "$tls", "HTTP-request-body-size": "(option u64)",
    "HTTP-request-header-section-size": "(option u32)", "HTTP-request-header-size": "(option $fs)",
    "HTTP-request-trailer-section-size": "(option u32)", "HTTP-request-trailer-size": "$fs",
    "HTTP-response-header-section-size": "(option u32)", "HTTP-response-header-size": "$fs",
    "HTTP-response-body-size": "(option u64)", "HTTP-response-trailer-section-size": "(option u32)",
    "HTTP-response-trailer-size": "$fs", "HTTP-response-transfer-coding": "(option string)",
    "HTTP-response-content-coding": "(option string)", "internal-error": "(option string)",
  };
  const cases = [...ERROR_CODES].map((c) => `(case "${c}"${payload[c] ? " " + payload[c] : ""})`).join(" ");
  return parse(`(component
  (import "test:codes/codes" (instance $c
    (type $dnsr (record (field "rcode" (option string)) (field "info-code" (option u16))))
    (export "DNS-error-payload" (type $dns (eq $dnsr)))
    (type $tlsr (record (field "alert-id" (option u8)) (field "alert-message" (option string))))
    (export "TLS-alert-received-payload" (type $tls (eq $tlsr)))
    (type $fsr (record (field "field-name" (option string)) (field "field-size" (option u32))))
    (export "field-size-payload" (type $fs (eq $fsr)))
    (type $ecv (variant ${cases}))
    (export "error-code" (type $ec (eq $ecv)))
    (export "code" (func (param "n" u32) (result $ec)))))
  (core module $mem
    (memory (export "memory") 1)
    (global $next (mut i32) (i32.const 1024))
    (func (export "realloc") (param i32 i32 i32 i32) (result i32)
      (local $p i32)
      (local.set $p (global.get $next))
      (global.set $next (i32.add (global.get $next) (i32.add (local.get 3) (i32.const 8))))
      (local.get $p)))
  (core instance $mi (instantiate $mem))
  (alias core export $mi "memory" (core memory $m))
  (alias core export $mi "realloc" (core func $r))
  (core func $code (canon lower (func $c "code") (memory $m) (realloc $r)))
  (core instance $x (export "code" (func $code)))
  (core module $main
    (import "x" "code" (func $code (param i32 i32)))
    (import "m" "memory" (memory 1))
    (func (export "check") (param $n i32) (result i32)
      (call $code (local.get $n) (i32.const 0))
      (i32.load8_u (i32.const 0))))
  (core instance $i (instantiate $main (with "x" (instance $x)) (with "m" (instance $mi))))
  (func $check (param "n" u32) (result u32) (canon lift (core func $i "check")))
  (export "check" (func $check))
)`);
}

test("CR9: jco lowers each error code, also the codes with a record payload", async () => {
  const r = await transpile(generate, await errorCodeComponent(), "");
  assert.ok(!r.error, r.error);
  const modules = {};
  for (const [f, b] of Object.entries(r.modules)) modules[f] = await WebAssembly.compile(b);
  const { instantiate } = await import("data:text/javascript;base64," + Buffer.from(r.js).toString("base64"));
  const codes = [...ERROR_CODES];
  const instance = await instantiate((f) => modules[f], { "test:codes/codes": { code: (n) => errorCode(codes[n]) } });
  for (let n = 0; n < codes.length; n++) assert.equal(instance.check(n), n, codes[n]);
});

test("CR10: a drop of a client request before fetched cancels it", async () => {
  const f = fakeFetch();
  const h = createHttp({ io, fetch: f.fetch });
  const a = request(h);
  await tick();
  a.future[Symbol.dispose]();
  assert.equal(f.calls[0].cancelled, 1);
  // A drop before the body is complete sends no fetch.
  const b = request(h, { body: "x" });
  b.future[Symbol.dispose]();
  b.finish();
  await tick();
  assert.equal(f.calls.length, 1);
});

test("a request body over the limit fails the write; trailers fail the client request", async () => {
  const f = fakeFetch();
  const h = createHttp({ io, fetch: f.fetch, limit: 3 });
  const { OutgoingBody } = h.http.types;
  // The limit is 3 bytes: a write of 4 bytes fails.
  assert.throws(() => request(h, { body: "abcd" }), (e) => e.tag === "last-operation-failed");
  const { future, ob } = request(h, { body: "abc" });
  assert.throws(() => OutgoingBody.finish(ob, { trailers: true }), (e) => e.tag === "internal-error");
  await future.subscribe().block();
  assert.deepEqual(future.get().val, { tag: "err", val: { tag: "internal-error", val: undefined } });
  assert.equal(f.calls.length, 0);
});

test("CO17: reads of a body without data go through the count", async () => {
  let pauses = 0;
  const idle = createIdle({ limit: 3, pause: () => (pauses++, Promise.resolve()) });
  const h = createHttp({ io, idle, fetch: fakeFetch().fetch });
  const stream = h.incoming(message({ body: ab("ab") })).consume().stream();
  assert.equal(dec.decode(stream.read(10n)), "ab");
  assert.throws(() => stream.read(1n), (e) => e.tag === "closed");
  assert.throws(() => stream.read(1n), (e) => e.tag === "closed");
  // The third read without data pauses, then gives its result.
  const p = stream.read(1n);
  assert.ok(p instanceof Promise);
  await assert.rejects(p, (e) => e.tag === "closed");
  assert.equal(pauses, 1);
});
