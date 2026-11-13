// A pod worker for phase 0 (items 3, 5 and 15). It gets the files of the
// transpile worker, loads the JavaScript from a Blob URL and runs it with
// WASIShim in JSPI mode.
import { WASIShim } from "@bytecodealliance/preview2-shim/instantiation";

const modules = new Map(); // file name -> WebAssembly.Module
let instantiate = null;

function lines(type) {
  const dec = new TextDecoder();
  let pending = "";
  const emit = () => {
    const parts = pending.split("\n");
    pending = parts.pop();
    for (const l of parts) postMessage({ type, line: l });
  };
  const flush = () => { pending += dec.decode(); if (pending) postMessage({ type, line: pending }); pending = ""; };
  return { write(b) { pending += dec.decode(b, { stream: true }); emit(); }, flush, blockingFlush: flush, drop: flush };
}

async function load({ name, files }) {
  const t = performance.now();
  for (const [f, data] of Object.entries(files)) {
    if (f.endsWith(".wasm")) modules.set(f, await WebAssembly.compile(data));
  }
  const url = URL.createObjectURL(new Blob([files[`${name}.js`]], { type: "text/javascript" }));
  ({ instantiate } = await import(url));
  return performance.now() - t;
}

async function run({ args, env }) {
  const shim = new WASIShim({ arguments: args, environment: env, stdout: lines("stdout"), stderr: lines("stderr") });
  const imports = { ...shim.getImportObject(), ...shim.getImportObject({ asVersion: "0.2.12" }), ...shim.getImportObject({ asVersion: "0.2.9" }) };
  let code = 0;
  try {
    const inst = await instantiate((f) => modules.get(f), imports);
    const run = inst.run ?? inst["wasi:cli/run@0.2.0"]?.run ?? inst["wasi:cli/run@0.2.12"]?.run;
    await run.run();
  } catch (e) {
    if (e?.exitError) code = e.code;
    else { postMessage({ type: "stderr", line: `trap: ${e?.message ?? e}` }); code = 128; }
  }
  return code;
}

self.onmessage = async ({ data }) => {
  try {
    if (data.type === "load") postMessage({ type: "loaded", ms: await load(data) });
    else if (data.type === "run") postMessage({ type: "exit", code: await run(data) });
  } catch (e) {
    postMessage({ type: "error", error: String(e?.stack ?? e) });
  }
};
