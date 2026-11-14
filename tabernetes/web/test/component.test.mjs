// Unit tests of the component path of the pod worker (web/src/component.js):
// small components from the WebAssembly text format, transpiled with jco as
// the transpile worker does, run in JSPI mode in Node.js. CO1, CO3, CO4, the
// exit codes of CO18, a function that the shim does not give, stdin (CO5),
// the memory of CO6 and the pause of CO19. The real components are in the
// end-to-end tests (e2e/wasi-command.test.mjs, e2e/wasi-worlds.test.mjs).
import { test } from "node:test";
import assert from "node:assert/strict";
import { parse } from "@bytecodealliance/jco";
import { generate } from "@bytecodealliance/jco/component";
import { transpile } from "../src/transpile.js";
import { runComponent, runExport, importObject, missingText, environment, sink, createStdin, createMemory, pollWithTask, camel, kebab, ERROR_EXIT } from "../src/component.js";
import { createIdle } from "../src/sockets.js";
import { createPeer } from "../src/wasi_messages.js";
// The io.js of the browser shim, which the bundle of the pod worker uses.
import * as io from "../node_modules/@bytecodealliance/preview2-shim/dist/browser/io.js";

// command gives a command component. Its run does the step of mode:
//   ok: returns ok; err: returns err; exit-ok, exit-err: calls exit;
//   trap: unreachable.
async function command(mode, version = "0.2.3") {
  const body = {
    "ok": "i32.const 0",
    "err": "i32.const 1",
    "exit-ok": "i32.const 0 call $exit i32.const 0",
    "exit-err": "i32.const 1 call $exit i32.const 0",
    "trap": "unreachable",
  }[mode];
  const bytes = await parse(`(component
  (import "wasi:cli/exit@${version}" (instance $exit (export "exit" (func (param "status" (result))))))
  (core func $exit_lower (canon lower (func $exit "exit")))
  (core instance $imports (export "exit" (func $exit_lower)))
  (core module $m
    (import "cli" "exit" (func $exit (param i32)))
    (func (export "run") (result i32) ${body}))
  (core instance $i (instantiate $m (with "cli" (instance $imports))))
  (func $run (result (result)) (canon lift (core func $i "run")))
  (instance $run_instance (export "run" (func $run)))
  (export "wasi:cli/run@${version}" (instance $run_instance))
)`);
  return compiled(bytes, "");
}

async function compiled(bytes, entrypoint) {
  const r = await transpile(generate, bytes, entrypoint);
  assert.ok(!r.error, r.error);
  const modules = {};
  for (const [f, b] of Object.entries(r.modules)) modules[f] = await WebAssembly.compile(b);
  return { js: r.js, modules, imports: r.imports, exports: r.exports, names: r.names, world: r.world, entrypoint };
}

// core gives a component without wasi:cli/run that exports the function go
// (CO3, CO4). go calls exit with ok when exit is set, else it returns.
async function core({ exit = false, exitWithCode = false } = {}) {
  const withCode = exitWithCode ? '(export "exit-with-code" (func (param "status-code" u8)))' : "";
  const bytes = await parse(`(component
  (import "wasi:cli/exit@0.2.9" (instance $exit (export "exit" (func (param "status" (result)))) ${withCode}))
  (core func $exit_lower (canon lower (func $exit "exit")))
  ${exitWithCode ? '(core func $code_lower (canon lower (func $exit "exit-with-code")))' : ""}
  (core instance $imports (export "exit" (func $exit_lower)) ${exitWithCode ? '(export "code" (func $code_lower))' : ""})
  (core module $m
    (import "cli" "exit" (func $exit (param i32)))
    ${exitWithCode ? '(import "cli" "code" (func $code (param i32)))' : ""}
    (memory (export "memory") 2)
    (func (export "go") ${exit ? "i32.const 0 call $exit" : ""}))
  (core instance $i (instantiate $m (with "cli" (instance $imports))))
  (func $go (canon lift (core func $i "go")))
  (export "go" (func $go))
)`);
  return bytes;
}

// FakeShim gives wasi:cli/exit for each version, as WASIShim does.
class FakeShim {
  static last = null;
  constructor(config) {
    this.config = config;
    this.versions = [];
    FakeShim.last = this;
  }
  getImportObject(opts) {
    this.versions.push(opts?.asVersion ?? "");
    const v = opts?.asVersion ? `@${opts.asVersion}` : "";
    const exit = (code) => { throw Object.assign(new Error(`exit ${code}`), { exitError: true, code }); };
    return {
      [`wasi:cli/exit${v}`]: { exit: (s) => exit(s.tag === "err" ? 1 : 0), exitWithCode: (c) => exit(c) },
      [`wasi:cli/environment${v}`]: { getArguments: () => [] },
    };
  }
}

const load = (js) => import("data:text/javascript;base64," + Buffer.from(js).toString("base64"));

function peer() {
  const sent = [];
  return { sent, peer: createPeer({ post: (m) => sent.push(m) }) };
}

// A fake io.js for FakeShim: runComponent takes pollableCreate and poll.
const fakeIO = { pollableCreate: (s) => s ?? {}, poll: {} };
const deps = (p, extra = {}) => ({ peer: p, WASIShim: FakeShim, io: fakeIO, load, ...extra });

const text = (sent, type) => sent.filter((m) => m.type === type).map((m) => new TextDecoder().decode(m.bytes)).join("");

test("CO1, CO18: a command component exits with 0 or 1", async () => {
  for (const [mode, code] of [["ok", 0], ["exit-ok", 0], ["exit-err", 1]]) {
    const { sent, peer: p } = peer();
    const got = await runComponent({ component: await command(mode), args: ["c.wasm"], env: ["A=1"], cwd: "/" }, deps(p));
    assert.equal(got, code, `${mode}: ${text(sent, "stderr")}`);
    assert.equal(text(sent, "stderr"), "");
  }
});

test("a trap and an error result of run give 137 and a text on stderr", async () => {
  for (const [mode, re] of [["trap", /wasm trap: .*unreachable/], ["err", /returned an error from wasi:cli\/run/]]) {
    const { sent, peer: p } = peer();
    const got = await runComponent({ component: await command(mode), args: [], env: [], cwd: "/" }, deps(p));
    assert.equal(got, ERROR_EXIT, mode);
    assert.match(text(sent, "stderr"), re);
  }
});

test("the shim gets the arguments, the environment, the cwd and the outputs", async () => {
  const { sent, peer: p } = peer();
  await runComponent({ component: await command("ok", "0.2.9"), args: ["c.wasm", "x y"], env: ["A=1", "B=a=b", "__proto__=p"], cwd: "/work" }, deps(p));
  const c = FakeShim.last.config;
  assert.deepEqual(c.arguments, ["c.wasm", "x y"]);
  assert.deepEqual(Object.entries(c.environment), [["A", "1"], ["B", "a=b"], ["__proto__", "p"]]);
  assert.equal(c.initialCwd, "/work");
  // jco gives the import without its version: the key has no version.
  assert.deepEqual(FakeShim.last.versions, [""]);
  c.stdout.write(new TextEncoder().encode("out"));
  c.stderr.write(new TextEncoder().encode("err"));
  assert.equal(text(sent, "stdout"), "out");
  assert.equal(text(sent, "stderr"), "err");
});

test("a component that cannot instantiate gives 137 and the name of the interface", async () => {
  const { sent, peer: p } = peer();
  const component = await command("ok");
  const got = await runComponent({ component, args: [], env: [], cwd: "/" }, deps(p, {
    WASIShim: class extends FakeShim { getImportObject() { return {}; } },
  }));
  assert.equal(got, ERROR_EXIT);
  assert.match(text(sent, "stderr"), /^component imports instance `wasi:cli\/exit@0\.2\.3`, but a matching implementation was not found in the linker: instance export `exit` is missing\n$/);
});

test("CO18: an import of exit-with-code fails at the instantiation with the name of the function", async () => {
  const { sent, peer: p } = peer();
  const component = await compiled(await core({ exitWithCode: true }), "go");
  // One version of wasi:cli/exit: jco gives the key without the version.
  assert.deepEqual(component.imports, ["wasi:cli/exit"]);
  assert.deepEqual(component.names.imports, ["wasi:cli/exit@0.2.9"]);
  const got = await runComponent({ component, args: [], env: [], cwd: "/" }, deps(p));
  assert.equal(got, ERROR_EXIT);
  assert.equal(text(sent, "stderr"), "component imports instance `wasi:cli/exit@0.2.9`, but a matching implementation was not found in the linker: instance export `exit-with-code` is missing\n");
});

test("CO3: without wasi:cli/run, the pod worker calls the function of the entrypoint", async () => {
  for (const [exit, code] of [[false, 0], [true, 0]]) {
    const { sent, peer: p } = peer();
    const component = await compiled(await core({ exit }), "go");
    assert.equal(component.world, "");
    const got = await runComponent({ component, args: [], env: [], cwd: "/" }, deps(p));
    assert.equal(got, code, text(sent, "stderr"));
  }
});

test("CO4: a function that is not in the exports gives 137 and the text of runwasi", async () => {
  for (const [entrypoint, fn] of [["", "_start"], ["stop", "stop"]]) {
    const { sent, peer: p } = peer();
    const component = await compiled(await core(), entrypoint);
    const got = await runComponent({ component, args: [], env: [], cwd: "/" }, deps(p));
    assert.equal(got, ERROR_EXIT);
    assert.equal(text(sent, "stderr"), `component does not have exported function "${fn}"\n`);
  }
});

test("CO6: the pod worker sends the size of the memories of the instance", async () => {
  const { sent, peer: p } = peer();
  const component = await compiled(await core(), "go");
  await runComponent({ component, args: [], env: [], cwd: "/" }, deps(p));
  const sizes = sent.filter((m) => m.type === "mem").map((m) => m.size);
  assert.ok(sizes.length >= 1);
  assert.ok(sizes.includes(2 * 65536), String(sizes));
});

test("createMemory counts each memory one time and release removes a group (CO7)", async () => {
  const { sent, peer: p } = peer();
  const memory = createMemory(p);
  const bytes = await parse("(module (memory (export \"m\") 1))");
  const mod = await WebAssembly.compile(bytes);
  const g1 = memory.group(), g2 = memory.group();
  const i1 = await g1.instantiateCore(mod);
  await g2.instantiateCore(mod);
  // A module that imports the memory of the first instance: no second count.
  const imp = await WebAssembly.compile(await parse("(module (import \"a\" \"m\" (memory 1)))"));
  await g2.instantiateCore(imp, { a: { m: i1.exports.m } });
  memory.report();
  assert.equal(memory.size(), 2 * 65536);
  g1.release();
  assert.equal(memory.size(), 2 * 65536, "the memory of g1 is also in g2");
  g2.release();
  assert.equal(memory.size(), 0);
  assert.deepEqual(sent.filter((m) => m.type === "mem").map((m) => m.size), [2 * 65536, 0]);
});

test("runExport finds wasi:cli/run in the list of jco", () => {
  assert.equal(runExport([["run", "instance"], ["wasi:cli/run@0.2.3", "instance"]]), "wasi:cli/run@0.2.3");
  assert.equal(runExport([["incomingHandler", "instance"], ["run", "instance"], ["wasi:cli/run@0.2.0", "instance"], ["wasi:http/incoming-handler@0.2.0", "instance"]]), "wasi:cli/run@0.2.0");
  assert.equal(runExport([["go", "function"]]), "");
});

test("camel and kebab give the names of jco and of WIT", () => {
  assert.equal(camel("go"), "go");
  assert.equal(camel("my-func-2"), "myFunc2");
  assert.equal(kebab("exitWithCode"), "exit-with-code");
  assert.equal(kebab("TcpSocket"), "tcp-socket");
});

test("importObject uses the versions of the import list and removes exit-with-code (CO18)", () => {
  const shim = new FakeShim({});
  const missing = [];
  const o = importObject(shim, ["wasi:cli/exit", "wasi:cli/environment@0.2.3", "wasi:cli/environment@0.2.12", "wasi:nothing/x@0.2.0"], missing);
  assert.deepEqual(Object.keys(o).sort(), ["wasi:cli/environment@0.2.12", "wasi:cli/environment@0.2.3", "wasi:cli/exit", "wasi:nothing/x@0.2.0"]);
  assert.equal(typeof o["wasi:cli/exit"].exit, "function");
  assert.ok(!("exitWithCode" in o["wasi:cli/exit"]));
  assert.deepEqual(shim.versions.sort(), ["", "0.2.0", "0.2.12", "0.2.3"]);
  // The reads of names that are not there go into missing.
  assert.equal(o["wasi:cli/exit"].exitWithCode, undefined);
  assert.equal(o["wasi:nothing/x@0.2.0"].get, undefined);
  assert.deepEqual(missing, [["wasi:cli/exit", "exitWithCode"], ["wasi:nothing/x@0.2.0", "get"]]);
});

test("missingText names the interface with its version and the function", () => {
  const missing = [["wasi:io/poll", "poll"], ["wasi:cli/exit", "exitWithCode"]];
  assert.match(missingText(missing, "unexpectedly undefined instance import 'exitWithCode'", ["wasi:cli/exit@0.2.9"]), /`wasi:cli\/exit@0\.2\.9`.*`exit-with-code` is missing$/);
  assert.match(missingText(missing, "other", []), /`wasi:io\/poll`.*`poll`/);
  assert.equal(missingText([], "x"), "");
});

test("CO5: stdin gives the bytes of the messages and waits for the next one", async () => {
  const idle = createIdle({ limit: 3, pause: () => Promise.resolve("pause") });
  const s = createStdin({ pollableCreate: io.pollableCreate, idle });
  const dec = (b) => new TextDecoder().decode(b);
  // No data: read gives no bytes, and the third call pauses (CO17).
  assert.equal(s.handler.read(10n).byteLength, 0);
  assert.equal(s.handler.read(10n).byteLength, 0);
  assert.ok(s.handler.read(10n) instanceof Promise);
  const pollable = s.handler.subscribe();
  assert.equal(pollable.ready(), false);
  const pending = s.handler.blockingRead(4n);
  assert.ok(pending instanceof Promise);
  s.push(new TextEncoder().encode("line\nmore").buffer, false);
  assert.equal(pollable.ready(), true);
  assert.equal(dec(await pending), "line");
  assert.equal(dec(s.handler.read(100n)), "\nmore");
  s.push(new TextEncoder().encode("ab").buffer, false);
  s.push(new TextEncoder().encode("cd").buffer, true);
  assert.equal(await s.handler.blockingSkip(3n), 3n);
  assert.equal(dec(s.handler.blockingRead(10n)), "d");
  assert.throws(() => s.handler.read(1n), (e) => e.tag === "closed");
  assert.throws(() => s.handler.blockingRead(1n), (e) => e.tag === "closed");
});

test("CO5: the end of stdin wakes a read that waits", async () => {
  const s = createStdin({ pollableCreate: io.pollableCreate });
  const pending = s.handler.blockingRead(1n);
  s.push(new ArrayBuffer(0), true);
  await assert.rejects(pending, (e) => e.tag === "closed");
});

test("CO19: poll with a ready pollable continues after a message, not after a timer", async () => {
  const realSetTimeout = globalThis.setTimeout;
  let timers = 0;
  globalThis.setTimeout = (...a) => { timers++; return realSetTimeout(...a); };
  try {
    let polls = 0;
    const poll = pollWithTask(() => polls++);
    const ready = io.pollableCreate();
    let late = false;
    const never = io.pollableCreate({ ready: () => late, wait: () => new Promise(() => {}) });
    const r = await poll([never, ready]);
    assert.deepEqual([...r], [1]);
    assert.equal(timers, 0);
    assert.equal(polls, 1);
    late = true;
    assert.deepEqual([...(await poll([never, ready]))], [0, 1]);
  } finally {
    globalThis.setTimeout = realSetTimeout;
  }
});

test("environment keeps a value with = and a name __proto__", () => {
  const e = environment(["A=1", "B=x=y", "C", "__proto__=v"]);
  assert.equal(Object.getPrototypeOf(e), null);
  assert.deepEqual(Object.entries(e), [["A", "1"], ["B", "x=y"], ["C", ""], ["__proto__", "v"]]);
});

test("sink copies the bytes and cuts them into messages of 1 MiB", () => {
  const { sent, peer: p } = peer();
  const mem = new Uint8Array(10);
  mem.set([1, 2, 3, 4], 2);
  sink(p, "stdout").write(mem.subarray(2, 6));
  mem.fill(0);
  assert.deepEqual([...new Uint8Array(sent[0].bytes)], [1, 2, 3, 4]);
  sink(p, "stderr", 3).write(new Uint8Array(7));
  assert.deepEqual(sent.slice(1).map((m) => m.bytes.byteLength), [3, 3, 1]);
});
