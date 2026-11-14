// Unit tests of the transpile worker (docs/wasi-sockets.md, section 8.2):
// CO15, CO16 and the result of jco. Run: node --test test/ in web/.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { generate } from "@bytecodealliance/jco/component";
import { JCO_VERSION, ASYNC_EXPORTS, asyncExports } from "../src/async_lists.js";
import { witName, isLabel, bareImports, transpile, check, jcoAlias } from "../src/transpile.js";
import { component, withName } from "./component_fixtures.mjs";

const pkg = (p) => JSON.parse(readFileSync(new URL(p, import.meta.url), "utf8"));

test("JCO_VERSION agrees with web/package.json and the installed jco", () => {
  assert.equal(pkg("../package.json").devDependencies["@bytecodealliance/jco"], JCO_VERSION);
  assert.equal(pkg("../node_modules/@bytecodealliance/jco/package.json").version, JCO_VERSION);
});

test("asyncExports adds the function of the entrypoint", () => {
  assert.deepEqual(asyncExports(""), [...ASYNC_EXPORTS]);
  assert.deepEqual(asyncExports("go"), [...ASYNC_EXPORTS, "go"]);
});

test("witName accepts the names of WIT", () => {
  for (const n of ["go", "get-arguments", "HTTP-ok", "wasi:cli/run@0.2.3", "wasi:http/incoming-handler@0.2.0-rc-2023-11-10",
    "wasi:cli/environment", "a:b:c/d/e@1.2.3+build.5", "[method]tcp-socket.start-bind", "[constructor]fields",
    "[static]fields.from-list", "[async]run", "[async method]a.b"]) {
    assert.ok(witName(n), n);
  }
});

test("witName rejects other names (CO16)", () => {
  for (const n of ['go"', "go'", "a b", "Go", "a--b", "-a", "a-", "1a", "wasi:cli/run@0.2", "wasi:cli", "wasi:cli/run@01.0.0",
    "a;alert(1)", "a\nb", "${x}", "`x`", "a\\b", "", "x".repeat(2000), null, 7, "[method]a", "aé"]) {
    assert.ok(!witName(n), JSON.stringify(n));
  }
  assert.ok(isLabel("go") && !isLabel("_start") && !isLabel("wasi:cli/run"));
});

test("bareImports finds bare module names", () => {
  assert.deepEqual(bareImports('import x from "./a.js";\nimport "b";\nconst y = import("node:fs");\nimport("c");'), ["b", "c"]);
  assert.deepEqual(bareImports('export async function instantiate(getCoreModule, imports) {}'), []);
});

test("transpile gives JavaScript and core modules", async () => {
  const r = await transpile(generate, await component(["go"]), "go");
  assert.equal(r.error, undefined, r.error);
  assert.equal(typeof r.js, "string");
  assert.match(r.js, /export function instantiate\(getCoreModule, imports/);
  assert.deepEqual(bareImports(r.js), []);
  assert.deepEqual(r.exports, [["go", "function"]]);
  const files = Object.keys(r.modules);
  assert.ok(files.length >= 1 && files.every((f) => /^component\.core\d*\.wasm$/.test(f)), files.join());
  for (const b of Object.values(r.modules)) assert.ok(WebAssembly.validate(b));
});

test("an entrypoint that is not a WIT label does not go into the lists", async () => {
  let opts;
  await transpile(async (_b, o) => { opts = o; throw new Error("x"); }, new Uint8Array(), 'go"); evil(');
  assert.deepEqual(opts.asyncMode.val.exports, [...ASYNC_EXPORTS]);
  assert.deepEqual(opts.instantiation, { tag: "async" });
  assert.equal(opts.asyncMode.tag, "jspi");
});

test("a component with a quotation mark in a name gives an error (CO15, CO16)", async () => {
  const good = await component(["zz"]);
  for (const [from, to] of [["zz", 'z"'], ["get-arguments", 'get-argument"'], ["environment", 'environmen"']]) {
    const r = await transpile(generate, withName(good, from, to), "");
    assert.ok(r.error, `${to}: no error`);
    assert.equal(r.js, undefined);
  }
});

test("check examines the names in the result of generate (CO15)", () => {
  const js = new TextEncoder().encode("export async function instantiate() {}");
  const ok = { files: [["component.js", js], ["component.core.wasm", new Uint8Array([0, 97, 115, 109, 1, 0, 0, 0])]], imports: ["wasi:cli/environment@0.2.3"], exports: [["go", "function"]] };
  assert.equal(check(ok).error, undefined);
  assert.match(check({ ...ok, imports: ['wasi:cli/environment@0.2.3"'] }).error, /import with a name that does not obey the WIT syntax/);
  assert.match(check({ ...ok, exports: [['go"]; x', "function"]] }).error, /export with a name that does not obey the WIT syntax/);
  assert.match(check({ ...ok, exports: [["go", "x"]] }).error, /not correct/);
  assert.match(check({ ...ok, files: ok.files.slice(1) }).error, /component\.js is missing/);
  assert.match(check({ ...ok, files: [...ok.files, ["../x.js", js]] }).error, /unexpected file/);
  assert.match(check({ ...ok, files: [["component.js", new TextEncoder().encode('import "x";')]] }).error, /bare module names x/);
  assert.match(check(null).error, /not correct/);
});

test("check accepts the aliases of jco and rejects other names (CO16)", () => {
  const js = new TextEncoder().encode("export async function instantiate() {}");
  const ok = { files: [["component.js", js]], imports: [], exports: [] };
  const ex = (...names) => names.map((n) => [n, "instance"]);
  // jco adds run and incomingHandler for the interfaces that it exports.
  assert.equal(check({ ...ok, exports: ex("incomingHandler", "run", "wasi:cli/run@0.2.12", "wasi:http/incoming-handler@0.2.12") }).error, undefined);
  assert.equal(check({ ...ok, exports: ex("myIface", "a:b/my-iface") }).error, undefined);
  // An alias without its interface, or a name that jco does not make.
  assert.match(check({ ...ok, exports: ex("incomingHandler", "wasi:cli/run@0.2.12") }).error, /export with a name that does not obey the WIT syntax: "incomingHandler"/);
  assert.match(check({ ...ok, exports: ex("evilName", "wasi:cli/run@0.2.12") }).error, /"evilName"/);
  assert.match(check({ ...ok, imports: ["incomingHandler"], exports: ex("wasi:http/incoming-handler@0.2.12") }).error, /import with a name/);
  assert.equal(jcoAlias("wasi:http/incoming-handler@0.2.12"), "incomingHandler");
  assert.equal(jcoAlias("go"), "");
  assert.equal(jcoAlias('wasi:cli/run"'), "");
});

test("transpile gives the names of the component and the world (CO2)", async () => {
  const r = await transpile(generate, await component(["go"]), "go");
  assert.deepEqual(r.names, { imports: ["wasi:cli/environment@0.2.3"], exports: [["go", "function"]] });
  assert.equal(r.world, "");
});
