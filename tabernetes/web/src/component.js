// The component path of a pod worker (docs/wasi-sockets.md, sections 8.2 and
// 8.5): it runs a command component (wasi:cli/run) of the transpile cache
// in JSPI mode with WASIShim of @bytecodealliance/preview2-shim. All traffic
// with the Go side goes through the messages of wasi_messages.js.
// component.worker.js runs it in a worker from a data: URL (decision 13).
//
// This pod worker does not use Atomics.wait (section 8.5): a component
// waits only with JSPI, so the worker continues to receive messages.

import { browserFilesystem } from "./files.js";
import { tickImports } from "./cpu.js";

// The exit code of a trap, of an error result of run and of a component that
// cannot instantiate: runwasi gives 137 for each error that is not an exit of
// the component (section 9.2).
export const ERROR_EXIT = 137;

// runExport gives the export of the command world, as runwasi finds the world
// (fact 5): the first export with the prefix wasi:cli/run, if no export with
// the prefix wasi:http/incoming-handler comes before it. Else "".
export function runExport(exports) {
  for (const [name] of exports) {
    if (name.startsWith("wasi:http/incoming-handler")) return "";
    if (name.startsWith("wasi:cli/run")) return name;
  }
  return "";
}

// importObject gives the imports of the component from the shim, with the
// keys of its own import list. jco gives a name without a version when the
// component imports one version of the interface (wasi:cli/exit), else with
// the version (wasi:cli/exit@0.2.3). The shim has one object for each
// interface; getImportObject({asVersion}) gives it under a key with that
// version.
export function importObject(shim, names) {
  const byVersion = new Map();
  const out = {};
  for (const name of names) {
    const at = name.lastIndexOf("@");
    const version = at < 0 ? "" : name.slice(at + 1);
    if (!byVersion.has(version)) byVersion.set(version, shim.getImportObject(version ? { asVersion: version } : undefined));
    const v = byVersion.get(version)[name];
    if (v === undefined) continue;
    out[name] = v;
  }
  // CO18: runwasi does not link exit-with-code (section 9.2, item 13). The
  // shim has it, so the pod worker removes it.
  for (const [name, v] of Object.entries(out)) {
    if (name.startsWith("wasi:cli/exit") && v && "exitWithCode" in v) {
      const { exitWithCode: _, ...rest } = v;
      out[name] = rest;
    }
  }
  return out;
}

// environment gives the variables of "KEY=value" lines as an object without a
// prototype, so that a name such as __proto__ is a plain variable.
export function environment(lines) {
  const env = Object.create(null);
  for (const l of lines) {
    const i = l.indexOf("=");
    if (i < 0) env[l] = "";
    else env[l.slice(0, i)] = l.slice(i + 1);
  }
  return env;
}

// sink gives an output stream handler of the shim that sends each write as
// a message of the type stdout or stderr. The bytes are copied: the shim
// can give a view into the memory of the instance.
export function sink(peer, type, max = 1 << 20) {
  const send = (buf) => {
    for (let o = 0; o < buf.byteLength; o += max) {
      const part = buf.slice(o, o + max);
      peer.send(type, { bytes: part.buffer }, [part.buffer]);
    }
  };
  return {
    write: (buf) => send(new Uint8Array(buf.buffer, buf.byteOffset, buf.byteLength)),
    flush() {},
    blockingFlush() {},
  };
}

// loadBlob loads the generated JavaScript from a Blob URL that this worker
// makes (section 8.2).
export async function loadBlob(js) {
  const url = URL.createObjectURL(new Blob([js], { type: "text/javascript" }));
  try {
    return await import(url);
  } finally {
    URL.revokeObjectURL(url);
  }
}

// runCommand runs the start message {component, args, env, cwd, preopens}
// and gives the exit code. WASIShim is the class of the shim; load(js) gives
// the module of the generated JavaScript. Memory is the class
// InMemoryFilesystemAdapter of the shim: with it, the component gets the
// files of preopens (files.js, FS1 to FS3). meter is the CPU meter of cpu.js
// (AC2), or null.
export async function runCommand({ component, args, env, cwd, preopens }, { peer, WASIShim, load = loadBlob, Memory = null, meter = null }) {
  const error = (text) => {
    const b = new TextEncoder().encode(text + "\n");
    peer.send("stderr", { bytes: b.buffer }, [b.buffer]);
    return ERROR_EXIT;
  };
  const run = runExport(component.exports);
  if (!run) return error("the component has no export wasi:cli/run: the pod worker runs only command components");
  const shim = new WASIShim({
    arguments: args,
    environment: environment(env),
    initialCwd: cwd,
    stdout: sink(peer, "stdout"),
    stderr: sink(peer, "stderr"),
    ...(Memory ? { browserFilesystem: browserFilesystem(preopens, Memory) } : {}),
  });
  let instance;
  meter?.begin();
  try {
    const { instantiate } = await load(component.js);
    const imports = importObject(shim, component.imports);
    instance = await instantiate((file) => component.modules[file], meter ? tickImports(meter, imports) : imports);
  } catch (e) {
    if (e?.exitError) return e.code;
    return error(`the component cannot instantiate: ${e?.message ?? e}`);
  } finally {
    meter?.end();
  }
  const iface = instance[run] ?? instance.run;
  if (typeof iface?.run !== "function") return error(`the component has no function run in ${run}`);
  meter?.begin();
  try {
    // run is in asyncMode.val.exports: it gives a promise.
    await iface.run();
    return 0;
  } catch (e) {
    // exit of wasi:cli/exit: ok gives 0, err gives 1 (CO18).
    if (e?.exitError) return e.code === 0 ? 0 : 1;
    // jco throws a ComponentError with a payload for the error result of run.
    if (e !== null && typeof e === "object" && "payload" in e) return error("the component returned an error from wasi:cli/run");
    return error(`wasm trap: ${e?.message ?? e}`);
  } finally {
    meter?.end();
  }
}
