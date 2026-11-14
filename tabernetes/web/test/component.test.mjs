// Unit tests of the component path of the pod worker (web/src/component.js):
// small command components from the WebAssembly text format, transpiled
// with jco as the transpile worker does, run in JSPI mode in Node.js. CO1,
// the exit codes of CO18 and the import object. The arguments, the
// environment, stdout and stderr of a real component are in the end-to-end
// test (e2e/wasi-command.test.mjs).
import { test } from "node:test";
import assert from "node:assert/strict";
import { parse } from "@bytecodealliance/jco";
import { generate } from "@bytecodealliance/jco/component";
import { transpile } from "../src/transpile.js";
import { runCommand, runExport, importObject, environment, sink, ERROR_EXIT } from "../src/component.js";
import { createPeer } from "../src/wasi_messages.js";

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
  const r = await transpile(generate, bytes, "");
  assert.ok(!r.error, r.error);
  const modules = {};
  for (const [f, b] of Object.entries(r.modules)) modules[f] = await WebAssembly.compile(b);
  return { js: r.js, modules, imports: r.imports, exports: r.exports };
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

const text = (sent, type) => sent.filter((m) => m.type === type).map((m) => new TextDecoder().decode(m.bytes)).join("");

test("CO1, CO18: a command component exits with 0 or 1", async () => {
  for (const [mode, code] of [["ok", 0], ["exit-ok", 0], ["exit-err", 1]]) {
    const { sent, peer: p } = peer();
    const got = await runCommand({ component: await command(mode), args: ["c.wasm"], env: ["A=1"], cwd: "/" }, { peer: p, WASIShim: FakeShim, load });
    assert.equal(got, code, `${mode}: ${text(sent, "stderr")}`);
    assert.equal(text(sent, "stderr"), "");
  }
});

test("a trap and an error result of run give 137 and a text on stderr", async () => {
  for (const [mode, re] of [["trap", /wasm trap: .*unreachable/], ["err", /returned an error from wasi:cli\/run/]]) {
    const { sent, peer: p } = peer();
    const got = await runCommand({ component: await command(mode), args: [], env: [], cwd: "/" }, { peer: p, WASIShim: FakeShim, load });
    assert.equal(got, ERROR_EXIT, mode);
    assert.match(text(sent, "stderr"), re);
  }
});

test("the shim gets the arguments, the environment, the cwd and the outputs", async () => {
  const { sent, peer: p } = peer();
  await runCommand({ component: await command("ok", "0.2.9"), args: ["c.wasm", "x y"], env: ["A=1", "B=a=b", "__proto__=p"], cwd: "/work" }, { peer: p, WASIShim: FakeShim, load });
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

test("a component that cannot instantiate gives 137", async () => {
  const { sent, peer: p } = peer();
  const component = await command("ok");
  const got = await runCommand({ component, args: [], env: [], cwd: "/" }, {
    peer: p, WASIShim: class extends FakeShim { getImportObject() { return {}; } }, load,
  });
  assert.equal(got, ERROR_EXIT);
  assert.match(text(sent, "stderr"), /cannot instantiate/);
});

test("runExport finds the command world as runwasi does", () => {
  assert.equal(runExport([["wasi:cli/run@0.2.3", "instance"]]), "wasi:cli/run@0.2.3");
  assert.equal(runExport([["go", "function"], ["wasi:cli/run@0.2.0", "instance"], ["wasi:http/incoming-handler@0.2.0", "instance"]]), "wasi:cli/run@0.2.0");
  assert.equal(runExport([["wasi:http/incoming-handler@0.2.0", "instance"], ["wasi:cli/run@0.2.0", "instance"]]), "");
  assert.equal(runExport([["go", "function"]]), "");
});

test("importObject uses the versions of the import list and removes exit-with-code (CO18)", () => {
  const shim = new FakeShim({});
  const o = importObject(shim, ["wasi:cli/exit", "wasi:cli/environment@0.2.3", "wasi:cli/environment@0.2.12", "wasi:nothing/x@0.2.0"]);
  assert.deepEqual(Object.keys(o).sort(), ["wasi:cli/environment@0.2.12", "wasi:cli/environment@0.2.3", "wasi:cli/exit"]);
  assert.equal(typeof o["wasi:cli/exit"].exit, "function");
  assert.ok(!("exitWithCode" in o["wasi:cli/exit"]));
  assert.deepEqual(shim.versions.sort(), ["", "0.2.0", "0.2.12", "0.2.3"]);
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
