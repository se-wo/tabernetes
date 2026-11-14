// Unit tests of the names and the world of a component
// (web/src/component_world.js): CO2 and the import list with versions of
// CO11 and CO12. The end-to-end tests with the Rust components are in
// e2e/wasi-worlds.test.mjs.
import { test } from "node:test";
import assert from "node:assert/strict";
import { parse } from "@bytecodealliance/jco";
import { generate } from "@bytecodealliance/jco/component";
import { componentNames, selectWorld } from "../src/component_world.js";
import { transpile } from "../src/transpile.js";

// twoWorlds gives a component with the exports of exports, in this
// sequence. Each export is an instance with one function.
async function twoWorlds(exports, imports = ["wasi:cli/environment@0.2.3"]) {
  return parse(`(component
  ${imports.map((i, n) => `(import "${i}" (instance $i${n} (export "f" (func))))`).join("\n  ")}
  (core module $m (func (export "f")))
  (core instance $c (instantiate $m))
  (func $f (canon lift (core func $c "f")))
  (instance $inst (export "f" (func $f)))
  ${exports.map((e) => (e.includes(":") ? `(export "${e}" (instance $inst))` : `(export "${e}" (func $f))`)).join("\n  ")}
)`);
}

test("componentNames gives the imports with versions and the exports in sequence", async () => {
  const bytes = await twoWorlds(["wasi:http/incoming-handler@0.2.12", "wasi:cli/run@0.2.12", "go"],
    ["wasi:clocks/monotonic-clock@0.3.0", "wasi:clocks/monotonic-clock@0.2.9", "wasi:cli/exit@0.2.9"]);
  const n = componentNames(bytes);
  assert.deepEqual(n.imports, ["wasi:clocks/monotonic-clock@0.3.0", "wasi:clocks/monotonic-clock@0.2.9", "wasi:cli/exit@0.2.9"]);
  assert.deepEqual(n.exports, [["wasi:http/incoming-handler@0.2.12", "instance"], ["wasi:cli/run@0.2.12", "instance"], ["go", "function"]]);
});

test("CO2: the first export of the two worlds selects the world, as runwasi does", async () => {
  const cases = [
    [["wasi:cli/run@0.2.12", "wasi:http/incoming-handler@0.2.12"], "command"],
    [["wasi:http/incoming-handler@0.2.12", "wasi:cli/run@0.2.12"], "proxy"],
    [["go", "wasi:cli/run@0.2.0"], "command"],
    [["go"], ""],
  ];
  for (const [exports, world] of cases) {
    const n = componentNames(await twoWorlds(exports));
    assert.equal(selectWorld(n.exports), world, exports.join());
  }
  assert.equal(selectWorld([]), "");
});

test("CO2: jco sorts its exports, the transpile worker keeps the sequence of the component", async () => {
  const a = await transpile(generate, await twoWorlds(["wasi:cli/run@0.2.12", "wasi:http/incoming-handler@0.2.12"]), "");
  const b = await transpile(generate, await twoWorlds(["wasi:http/incoming-handler@0.2.12", "wasi:cli/run@0.2.12"]), "");
  assert.equal(a.error, undefined, a.error);
  assert.equal(b.error, undefined, b.error);
  // The lists of jco are the same; the world is not.
  assert.deepEqual(a.exports, b.exports);
  assert.equal(a.world, "command");
  assert.equal(b.world, "proxy");
  // The names of the component keep the version and also an import that
  // jco does not use.
  assert.deepEqual(a.names.imports, ["wasi:cli/environment@0.2.3"]);
});

test("componentNames rejects bytes that are not a component", async () => {
  assert.throws(() => componentNames(new Uint8Array([0, 97, 115, 109, 1, 0, 0, 0])), /not a component/);
  assert.throws(() => componentNames(new Uint8Array(3)), /not a component/);
  const good = await twoWorlds(["go"]);
  assert.throws(() => componentNames(good.subarray(0, good.length - 3)), /past the end|unexpected end/);
});
