// The transpile worker of a Go thread (docs/wasi-sockets.md, section 8.2).
// wasi.js starts it as a module worker for the first component. It loads
// jco-transpile, whose core modules are in ../vendor/ relative to this
// bundle (the task "web" of mise.toml copies them there).
//
// Request:  {id, bytes: ArrayBuffer, entrypoint}
// Response: {id, js, modules: {file: ArrayBuffer}, imports, exports, names, world, ms}
//           or {id, error}
import { generate } from "@bytecodealliance/jco/component";
import { transpile } from "./transpile.js";

self.onmessage = async ({ data: { id, bytes, entrypoint } }) => {
  const t0 = performance.now();
  let r;
  try {
    r = await transpile(generate, new Uint8Array(bytes), entrypoint || "");
  } catch (e) {
    r = { error: `the transpile worker failed: ${e?.message ?? e}` };
  }
  if (r.error) {
    self.postMessage({ id, error: r.error });
    return;
  }
  const modules = {}, transfer = [];
  for (const [file, data] of Object.entries(r.modules)) {
    const b = data.byteOffset === 0 && data.byteLength === data.buffer.byteLength ? data.buffer : data.slice().buffer;
    modules[file] = b;
    transfer.push(b);
  }
  self.postMessage({ id, js: r.js, modules, imports: r.imports, exports: r.exports, names: r.names, world: r.world, ms: performance.now() - t0 }, transfer);
};
