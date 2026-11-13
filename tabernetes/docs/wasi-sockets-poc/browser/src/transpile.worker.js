// The transpile worker of section 8.2 (phase 0, items 1, 2, 3 and 11):
// generate() of jco in a module worker. esbuild bundles it as ESM; the core
// modules of jco-transpile are in ../vendor/ of the bundle.
import { generate } from "@bytecodealliance/jco/component";
import { ASYNC_IMPORTS, ASYNC_EXPORTS } from "./async-lists.js";

const t0 = performance.now();
let ready = 0;

self.onmessage = async ({ data: { id, name, bytes } }) => {
  const start = performance.now();
  try {
    const out = await generate(bytes, {
      name,
      noTypescript: true,
      instantiation: { tag: "async" },
      asyncMode: { tag: "jspi", val: { imports: ASYNC_IMPORTS, exports: ASYNC_EXPORTS } },
    });
    if (!ready) ready = performance.now() - t0;
    const files = {};
    const transfer = [];
    for (const [file, data] of out.files) {
      files[file] = data;
      transfer.push(data.buffer);
    }
    const js = new TextDecoder().decode(files[`${name}.js`]);
    // Bare module names: an import of something that is not a relative path.
    // Bare module names: a static or a dynamic import of a name that is not
    // a relative path or a URL.
    const specs = [
      ...js.matchAll(/^\s*(?:import|export)\b[^;'"]*?\bfrom\s*["']([^"']+)["']/gm),
      ...js.matchAll(/^\s*import\s*["']([^"']+)["']/gm),
      ...js.matchAll(/\bimport\(\s*["']([^"']+)["']\s*\)/g),
    ].map((m) => m[1]);
    const bare = specs.filter((s) => !/^(\.{0,2}\/|[a-z]+:)/.test(s));
    postMessage({
      id, ok: true, ms: performance.now() - start, firstMs: ready,
      imports: out.imports, exports: out.exports.map(([n]) => n), bare,
      sizes: Object.fromEntries(Object.entries(files).map(([k, v]) => [k, v.byteLength])),
      files,
    }, transfer);
  } catch (e) {
    postMessage({ id, ok: false, ms: performance.now() - start, error: String(e?.payload ?? e?.message ?? e) });
  }
};
