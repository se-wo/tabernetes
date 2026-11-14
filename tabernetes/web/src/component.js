// The component path of a pod worker (docs/wasi-sockets.md, sections 8.2,
// 8.3 and 8.5): it runs a component of the transpile cache in JSPI mode with
// WASIShim of @bytecodealliance/preview2-shim. All traffic with the Go side
// goes through the messages of wasi_messages.js. component.worker.js runs it
// in a worker from a data: URL (decision 13).
//
// The world of the component comes from the transpile worker (world of
// component_world.js), as runwasi finds it (CO2):
//
//   "command"  the pod worker calls run of wasi:cli/run
//   ""         the pod worker calls the function that the entrypoint names,
//              or _start (CO3, CO4)
//   "proxy"    phase 2; the Go side does not start a pod worker for it
//
// The Go side examines the imports before it starts the pod worker (CO11,
// CR6). A function that the shim does not give (exit-with-code of CO18, or a
// function of a newer wasi:*@0.2.x) fails at the instantiation of jco; the
// pod worker then names the interface and the function on stderr.
//
// This pod worker does not use Atomics.wait (section 8.5): a component
// waits only with JSPI, so the worker continues to receive messages.
import { createIdle, createSockets, nextTask } from "./sockets.js";

// The exit code of a trap, of an error result of run, of a function that is
// not there and of a component that cannot instantiate. runwasi gives 137 for
// each error of the component that is not an exit (executor.rs of
// containerd-shim-wasm v0.6.1: std::process::exit(137) for each error of
// run_wasi; section 9.2).
export const ERROR_EXIT = 137;

// The function that runwasi calls when the entrypoint names no function.
export const DEFAULT_FUNCTION = "_start";

// runExport gives the key of wasi:cli/run in the export list of jco, else
// "". The world comes from the transpile worker, not from this list: jco
// sorts its list.
export function runExport(exports) {
  for (const [name] of exports) {
    if (name.startsWith("wasi:cli/run")) return name;
  }
  return "";
}

// camel gives the name that jco uses in JavaScript for a WIT label:
// my-function gives myFunction.
export const camel = (name) => name.replace(/-([a-zA-Z0-9])/g, (_, c) => c.toUpperCase());

// kebab gives the WIT name of a name of jco: exitWithCode gives
// exit-with-code, TcpSocket gives tcp-socket.
export const kebab = (name) => name.replace(/[A-Z]/g, (c, i) => (i ? "-" : "") + c.toLowerCase());

// importObject gives the imports of the component from the shim, with the
// keys of its own import list. jco gives a name without a version when the
// component imports one version of the interface (wasi:cli/exit), else with
// the version (wasi:cli/exit@0.2.3). The shim has one object for each
// interface; getImportObject({asVersion}) gives it under a key with that
// version.
//
// Each interface goes through a Proxy that records the names that the
// generated JavaScript reads and does not find (missing): jco throws at the
// instantiation for such a function.
export function importObject(shim, names, missing = []) {
  const byVersion = new Map();
  const out = {};
  for (const name of names) {
    const at = name.lastIndexOf("@");
    const version = at < 0 ? "" : name.slice(at + 1);
    if (!byVersion.has(version)) byVersion.set(version, shim.getImportObject(version ? { asVersion: version } : undefined));
    let v = byVersion.get(version)[name];
    if (v === undefined) v = {};
    // CO18: runwasi does not link exit-with-code (section 9.2, item 13). The
    // shim has it, so the pod worker removes it.
    if (name.startsWith("wasi:cli/exit") && "exitWithCode" in v) {
      const { exitWithCode: _, ...rest } = v;
      v = rest;
    }
    out[name] = recording(v, name, missing);
  }
  return out;
}

function recording(target, iface, missing) {
  if (target === null || typeof target !== "object") return target;
  return new Proxy(target, {
    get(t, key, receiver) {
      const v = Reflect.get(t, key, receiver);
      if (v === undefined && typeof key === "string" && key !== "then") missing.push([iface, key]);
      return v;
    },
  });
}

// missingText gives the text of runwasi for an import that the linker does
// not have (wasmtime: "component imports instance ..., but a matching
// implementation was not found in the linker"), with the function. versions
// is the import list of the component with versions (names.imports).
export function missingText(missing, message, versions = []) {
  const m = /instance import '([^']+)'/.exec(String(message));
  const hit = (m && missing.find(([, k]) => k === m[1])) || missing[0];
  if (!hit) return "";
  const [iface, key] = hit;
  const full = iface.includes("@") ? iface : versions.find((n) => n.startsWith(iface + "@")) ?? iface;
  return `component imports instance \`${full}\`, but a matching implementation was not found in the linker: instance export \`${kebab(key)}\` is missing`;
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
// can give a view into the memory of the instance. onWrite runs after each
// write (the count of CO17 and the memory of CO6).
export function sink(peer, type, max = 1 << 20, onWrite = () => {}) {
  const send = (buf) => {
    for (let o = 0; o < buf.byteLength; o += max) {
      const part = buf.slice(o, o + max);
      peer.send(type, { bytes: part.buffer }, [part.buffer]);
    }
  };
  return {
    write: (buf) => {
      send(new Uint8Array(buf.buffer, buf.byteOffset, buf.byteLength));
      onWrite();
    },
    flush() {},
    blockingFlush() {},
  };
}

// createStdin gives the stdin of the component (CO5): an input stream handler
// for WASIShim with the bytes of the messages stdin. push(bytes, end) adds
// the bytes of one message. A read without data waits with JSPI until the
// next message, so the pod worker receives messages during the wait.
// pollableCreate is the function of the io.js of the shim; idle is the count
// of CO17.
export function createStdin({ pollableCreate, idle = createIdle() }) {
  const chunks = [];
  let ended = false;
  let waiters = [];
  const wake = () => {
    const w = waiters;
    waiters = [];
    for (const fn of w) fn();
  };
  const available = () => chunks.length > 0 || ended;
  const wait = () => (available() ? Promise.resolve() : new Promise((resolve) => waiters.push(resolve)));
  const closed = () => {
    throw { tag: "closed" };
  };
  // take gives up to len bytes of the chunks.
  const take = (len) => {
    let n = Number(len > 0x7fffffffn ? 0x7fffffffn : len);
    const parts = [];
    let size = 0;
    while (n > 0 && chunks.length) {
      const c = chunks[0];
      if (c.byteLength <= n) {
        parts.push(chunks.shift());
        n -= c.byteLength;
        size += c.byteLength;
      } else {
        parts.push(c.subarray(0, n));
        chunks[0] = c.subarray(n);
        size += n;
        n = 0;
      }
    }
    if (parts.length === 1) return parts[0];
    const out = new Uint8Array(size);
    let o = 0;
    for (const p of parts) {
      out.set(p, o);
      o += p.byteLength;
    }
    return out;
  };
  const read = (len) => {
    if (chunks.length) {
      idle.call(true);
      return take(len);
    }
    if (ended) closed();
    // No data: the count of CO17.
    const p = idle.call(false);
    return p ? p.then(() => new Uint8Array(0)) : new Uint8Array(0);
  };
  const blockingRead = (len) => {
    if (chunks.length) return take(len);
    if (ended) closed();
    return wait().then(() => blockingRead(len));
  };
  const size = (b) => (b instanceof Promise ? b.then((x) => BigInt(x.byteLength)) : BigInt(b.byteLength));
  return {
    handler: {
      read,
      blockingRead,
      skip: (len) => size(read(len)),
      blockingSkip: (len) => size(blockingRead(len)),
      subscribe: () => pollableCreate({ ready: available, wait }),
    },
    push(bytes, end) {
      if (ended) return;
      if (bytes?.byteLength) chunks.push(new Uint8Array(bytes));
      if (end) ended = true;
      wake();
    },
  };
}

// createMemory counts the memories of the instances (CO6, CO7). jco calls
// instantiateCore for each core module; the memories that an instance
// exports or imports belong to its group. report() sends the message mem
// when the size changes. release(group) removes the memories of an instance
// that the pod worker discards (CO7; the proxy path of phase 2 uses it).
export function createMemory(peer) {
  const groups = new Set();
  let last = -1;
  const memory = {
    group() {
      const mems = new Set();
      groups.add(mems);
      return {
        instantiateCore: async (module, imports = {}) => {
          const instance = await WebAssembly.instantiate(module, imports);
          for (const ns of Object.values(imports)) {
            for (const v of Object.values(ns ?? {})) if (v instanceof WebAssembly.Memory) mems.add(v);
          }
          for (const v of Object.values(instance.exports)) if (v instanceof WebAssembly.Memory) mems.add(v);
          return instance;
        },
        release() {
          groups.delete(mems);
          memory.report();
        },
      };
    },
    size() {
      const all = new Set();
      for (const g of groups) for (const m of g) all.add(m);
      let n = 0;
      for (const m of all) n += m.buffer.byteLength;
      return n;
    },
    report() {
      const n = memory.size();
      if (n === last) return;
      last = n;
      peer.send("mem", { size: n });
    },
  };
  return memory;
}

// pollWithTask gives the function poll of wasi:io/poll. It is the function of
// the shim, but a list with a ready pollable continues after one message on
// a MessageChannel (CO19), not after setTimeout (fact 9).
export function pollWithTask(onPoll = () => {}) {
  const sweep = (list) => {
    const out = [];
    for (let i = 0; i < list.length; i++) if (list[i].ready()) out.push(i);
    return new Uint32Array(out);
  };
  return (list) => {
    if (list.length === 0) throw new Error("poll list must not be empty");
    onPoll();
    for (const p of list) {
      if (p.ready()) return nextTask().then(() => sweep(list));
    }
    return Promise.race(list.map((p) => p.block())).then(() => sweep(list));
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

// runComponent runs the start message {component, args, env, cwd} and gives
// the exit code. WASIShim is the class of the shim; io is the io.js of the
// shim ({poll, streams, error, pollableCreate}); load(js) gives the module of
// the generated JavaScript. The handler of the message stdin goes on peer.
export async function runComponent({ component, args, env, cwd }, { peer, WASIShim, io, load = loadBlob }) {
  const error = (text) => {
    const b = new TextEncoder().encode(text + "\n");
    peer.send("stderr", { bytes: b.buffer }, [b.buffer]);
    return ERROR_EXIT;
  };
  const world = component.world ?? "";
  if (world === "proxy") return error("the component is a proxy component (wasi:http/incoming-handler): the tab node cannot serve proxy components yet");
  const fn = world === "command" ? "" : component.entrypoint || DEFAULT_FUNCTION;

  const memory = createMemory(peer);
  const idle = createIdle({ onCall: () => memory.report() });
  const stdin = createStdin({ pollableCreate: io.pollableCreate, idle });
  peer.on("stdin", (m) => stdin.push(m.bytes, m.end));
  const shim = new WASIShim({
    arguments: args,
    environment: environment(env),
    initialCwd: cwd,
    stdin: stdin.handler,
    stdout: sink(peer, "stdout", undefined, () => idle.call(true)),
    stderr: sink(peer, "stderr", undefined, () => idle.call(true)),
    io: { ...io, poll: { ...io.poll, poll: pollWithTask(() => idle.call(true)), pollList: pollWithTask(() => idle.call(true)) } },
    sockets: createSockets({ idle, pollableCreate: io.pollableCreate }),
    // W1.5: the browserFilesystem adapter over the files of the container
    // (wasmPreopens, section 8.10) goes here. Until then, a component gets
    // no preopened directories.
  });
  const missing = [];
  const group = memory.group();
  let instance;
  try {
    const { instantiate } = await load(component.js);
    instance = await instantiate((file) => component.modules[file], importObject(shim, component.imports, missing), group.instantiateCore);
  } catch (e) {
    if (e?.exitError) return e.code === 0 ? 0 : 1;
    const why = missingText(missing, e?.message ?? e, component.names?.imports);
    return error(why || `the component cannot instantiate: ${e?.message ?? e}`);
  }
  memory.report();

  let call;
  if (world === "command") {
    const run = runExport(component.exports);
    const iface = instance[run] ?? instance.run;
    if (typeof iface?.run !== "function") return error(`the component has no function run in ${run || "wasi:cli/run"}`);
    call = () => iface.run();
  } else {
    // CO3, CO4: the text of runwasi for a function that is not there.
    const f = Object.hasOwn(instance, fn) ? instance[fn] : Object.hasOwn(instance, camel(fn)) ? instance[camel(fn)] : undefined;
    if (typeof f !== "function") return error(`component does not have exported function ${JSON.stringify(fn)}`);
    call = () => f();
  }
  try {
    // run and the function of the entrypoint are in asyncMode.val.exports:
    // they give a promise.
    await call();
    return 0;
  } catch (e) {
    // exit of wasi:cli/exit: ok gives 0, err gives 1 (CO18).
    if (e?.exitError) return e.code === 0 ? 0 : 1;
    // jco throws a ComponentError with a payload for the error result of run.
    if (e !== null && typeof e === "object" && "payload" in e) return error("the component returned an error from wasi:cli/run");
    return error(`wasm trap: ${e?.message ?? e}`);
  } finally {
    memory.report();
  }
}
