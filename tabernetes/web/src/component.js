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
//   "proxy"    the pod worker serves the request messages of the Go side
//              with a new instance for each request (serveProxy, section 8.6)
//
// The Go side examines the imports before it starts the pod worker (CO11,
// CR6). A function that the shim does not give (exit-with-code of CO18, or a
// function of a newer wasi:*@0.2.x) fails at the instantiation of jco; the
// pod worker then names the interface and the function on stderr.
//
// This pod worker does not use Atomics.wait (section 8.5): a component
// waits only with JSPI, so the worker continues to receive messages.
import { createIdle, createSockets, nextTask } from "./sockets.js";

import { browserFilesystem } from "./files.js";
import { tickImports } from "./cpu.js";
import { createHttp, responseFields } from "./http.js";

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

// runComponent runs the start message {component, args, env, cwd, preopens}
// and gives the exit code. WASIShim is the class of the shim; io is the io.js
// of the shim ({poll, streams, error, pollableCreate}); load(js) gives the
// module of the generated JavaScript. The handler of the message stdin goes
// on peer. Memory is the class InMemoryFilesystemAdapter of the shim: with
// it, the component gets the files of preopens (files.js, FS1 to FS3). meter
// is the CPU meter of cpu.js (AC2), or null.
//
// For a proxy component, the other options go to serveProxy.
export async function runComponent({ component, args, env, cwd, preopens }, { peer, WASIShim, io, load = loadBlob, Memory = null, meter = null, ...proxy }) {
  const error = (text) => {
    const b = new TextEncoder().encode(text + "\n");
    peer.send("stderr", { bytes: b.buffer }, [b.buffer]);
    return ERROR_EXIT;
  };
  const world = component.world ?? "";
  // A proxy component gets no preopens (fact 4): Memory is not used.
  if (world === "proxy") return serveProxy({ component, env }, { peer, WASIShim, io, load, meter, ...proxy });
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
    // The files of the container (wasmPreopens, section 8.10).
    ...(Memory ? { browserFilesystem: browserFilesystem(preopens, Memory) } : {}),
  });
  const missing = [];
  const group = memory.group();
  let instance;
  meter?.begin();
  try {
    const { instantiate } = await load(component.js);
    const imports = importObject(shim, component.imports, missing);
    instance = await instantiate((file) => component.modules[file], meter ? tickImports(meter, imports) : imports, group.instantiateCore);
  } catch (e) {
    if (e?.exitError) return e.code === 0 ? 0 : 1;
    const why = missingText(missing, e?.message ?? e, component.names?.imports);
    return error(why || `the component cannot instantiate: ${e?.message ?? e}`);
  } finally {
    meter?.end();
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
  meter?.begin();
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
    meter?.end();
    memory.report();
  }
}

// The proxy path (docs/wasi-sockets.md, section 8.6, PX7 to PX15, PX21 to
// PX27, PX34). The Go side serves the address of the container and sends one
// request message for each request. The pod worker makes a new instance for
// each request, calls handle, sends one response message and discards the
// instance.

// MAX_INSTANCES is the maximum of instances of one container at the same
// time (PX9). More requests wait in a queue (PX10).
export const MAX_INSTANCES = 16;

// MEMORY_RETRY_MS is the time after which a request that could not get the
// memory of its instance (PX34) tries again, if no other instance of the
// container ends before.
export const MEMORY_RETRY_MS = 1000;

// isMemoryError tells if an instantiation failed because the browser cannot
// make one more Wasm memory (section 9.2, item 15). Chrome gives a RangeError
// "Out of memory: Cannot allocate Wasm memory for new instance".
export function isMemoryError(e) {
  return /out of memory|cannot allocate wasm memory|could not allocate memory/i.test(String(e?.message ?? e));
}

// createScope gives the waits of one instance (PX24). wait(promise) gives a
// promise that also fails when cancel() runs. sleep(ms) is a timer of the
// instance. onCancel(fn) adds a step of cancel and gives a function that
// removes it. cancel() stops the timers, does the steps and continues each
// wait with an exception: Chrome frees a suspended instance only after its
// call continues (section 9.2, item 15).
export function createScope() {
  const timers = new Set();
  const rejects = new Set();
  const steps = new Set();
  let cancelled = false;
  const failure = () => new Error("the request is cancelled");
  const scope = {
    get cancelled() {
      return cancelled;
    },
    wait(promise) {
      if (cancelled) return Promise.reject(failure());
      return new Promise((resolve, reject) => {
        rejects.add(reject);
        promise.then(
          (v) => {
            rejects.delete(reject);
            resolve(v);
          },
          (e) => {
            rejects.delete(reject);
            reject(e);
          },
        );
      });
    },
    sleep(ms) {
      return scope.wait(new Promise((resolve) => {
        let left = ms;
        const next = () => {
          timers.delete(t);
          if (left <= 0) return resolve();
          const d = Math.min(left, 0x7fffffff);
          left -= d;
          t = setTimeout(next, d);
          timers.add(t);
        };
        let t = null;
        next();
      }));
    },
    onCancel(fn) {
      if (cancelled) {
        fn();
        return () => {};
      }
      steps.add(fn);
      return () => steps.delete(fn);
    },
    cancel() {
      if (cancelled) return;
      cancelled = true;
      for (const t of timers) clearTimeout(t);
      timers.clear();
      for (const fn of steps) {
        try {
          fn();
        } catch {
          // A step that fails must not stop the other steps.
        }
      }
      steps.clear();
      const e = failure();
      for (const r of rejects) r(e);
      rejects.clear();
    },
  };
  return scope;
}

// clocksFor gives the clocks of one instance (decision 3): the timers of
// monotonic-clock belong to the scope, so a cancel fails their waits.
export function clocksFor(scope, pollableCreate) {
  const now = () => BigInt(Math.floor(performance.now() * 1e6));
  const monotonicClock = {
    resolution: () => 1_000_000n,
    now,
    subscribeDuration(ns) {
      if (ns <= 0n) return pollableCreate();
      const ms = Number((ns + 999_999n) / 1_000_000n);
      let done = false;
      const p = scope.sleep(ms).then(() => {
        done = true;
      });
      p.catch(() => {});
      return pollableCreate({ ready: () => done, wait: () => p });
    },
    subscribeInstant(instant) {
      return monotonicClock.subscribeDuration(instant - now());
    },
  };
  const wallClock = {
    now() {
      const ms = Date.now();
      return { seconds: BigInt(Math.floor(ms / 1000)), nanoseconds: (ms % 1000) * 1e6 };
    },
    resolution: () => ({ seconds: 0n, nanoseconds: 1e6 }),
  };
  return { monotonicClock, wallClock };
}

// createFetches gives the client requests of one pod worker (CR9, CR10):
// start(req) sends fetch and gives {result, cancel}; fetched(msg) gives the
// fetched message to its request. cancel sends fetch-cancel only before
// fetched (section 8.3.1). The fetch IDs go from 1 to 0x7fffffff.
export function createFetches(peer) {
  const pending = new Map();
  let next = 0;
  const newID = () => {
    do next = next >= 0x7fffffff ? 1 : next + 1;
    while (pending.has(next));
    return next;
  };
  return {
    get size() {
      return pending.size;
    },
    start({ method, url, headers, body }) {
      const id = newID();
      let resolve;
      const result = new Promise((r) => (resolve = r));
      const hs = headers.map(([k, v]) => [k, v.slice().buffer]);
      const b = body.slice().buffer;
      try {
        peer.send("fetch", { fetch: id, method, url, headers: hs, body: b }, [b, ...hs.map(([, v]) => v)]);
      } catch {
        // The message fails the examination (too many headers, for example).
        return { result: Promise.resolve({ error: "HTTP-request-header-section-size" }), cancel() {} };
      }
      pending.set(id, resolve);
      return {
        result,
        cancel() {
          if (pending.delete(id)) peer.send("fetch-cancel", { fetch: id });
        },
      };
    },
    fetched(msg) {
      const r = pending.get(msg.fetch);
      if (!r) return;
      pending.delete(msg.fetch);
      r(msg);
    },
  };
}

// emptyStdin is the stdin of an instance: no bytes (PX13).
function emptyStdin(pollableCreate) {
  const closed = () => {
    throw { tag: "closed" };
  };
  return { read: closed, blockingRead: closed, skip: closed, blockingSkip: closed, subscribe: () => pollableCreate() };
}

// handlerExport gives the key of wasi:http/incoming-handler in the export list
// of jco, else "".
export function handlerExport(exports) {
  for (const [name] of exports ?? []) {
    if (name.startsWith("wasi:http/incoming-handler")) return name;
  }
  return "";
}

// serveProxy serves the request messages for a proxy component. It gives a
// promise of an exit code only for a component that cannot start; else the
// container runs until the Go side stops the pod worker.
//
// The handlers of request, cancel and fetched go on peer, one for each type;
// they find the request or the client request by its ID. stdin of the Go
// side has a handler that discards the bytes (PX13).
export function serveProxy({ component, env }, { peer, WASIShim, io, load = loadBlob, meter = null, limit = MAX_INSTANCES, retryMs = MEMORY_RETRY_MS, onInstance = () => {} }) {
  const memory = createMemory(peer);
  const idle = createIdle({ onCall: () => memory.report() });
  const fetches = createFetches(peer);
  const base = environment(env);
  const requests = new Map(); // request ID of Go -> entry
  const queue = [];
  let nextRequestID = 0;
  let running = 0;
  let instantiate = null;
  let memoryWait = null; // the timer of PX34, or true while no timer runs
  const key = handlerExport(component.exports);

  const textExit = (text) => {
    const b = new TextEncoder().encode(text + "\n");
    peer.send("stderr", { bytes: b.buffer }, [b.buffer]);
    return ERROR_EXIT;
  };

  // reply sends the one response message of a request.
  const reply = (entry, fields) => {
    if (entry.done) return;
    entry.done = true;
    requests.delete(entry.msg.request);
    const transfer = [];
    if (fields.body) transfer.push(fields.body);
    for (const [, v] of fields.headers ?? []) transfer.push(v);
    try {
      peer.send("response", { request: entry.msg.request, ...fields }, transfer);
    } catch {
      // The response fails the examination (too many headers, for example):
      // the Go side closes the connection, as for an error code.
      peer.send("response", { request: entry.msg.request, error: "HTTP-response-header-section-size" });
    }
  };

  // newInstance makes the instance of one request: a new WASIShim with the
  // context of runwasi (PX11 to PX15) and its own clocks and wasi:http.
  const newInstance = async (requestID, scope) => {
    const http = createHttp({
      io,
      idle,
      wait: (p) => scope.wait(p),
      fetch: (req) => {
        const f = fetches.start(req);
        const off = scope.onCancel(f.cancel);
        f.result.then(off);
        return f;
      },
    });
    const environment = Object.assign(Object.create(null), base);
    if (requestID !== null) environment.REQUEST_ID = String(requestID);
    const discard = { write: () => idle.call(true), flush() {}, blockingFlush() {} };
    const poll = pollWithTask(() => idle.call(true));
    const scoped = (list) => scope.wait(Promise.resolve(poll(list)));
    const shim = new WASIShim({
      arguments: [],
      environment,
      stdin: emptyStdin(io.pollableCreate),
      stdout: discard,
      stderr: discard,
      io: { ...io, poll: { ...io.poll, poll: scoped, pollList: scoped } },
      sockets: createSockets({ idle, pollableCreate: io.pollableCreate }),
      http: http.http,
      clocks: clocksFor(scope, io.pollableCreate),
    });
    const missing = [];
    const imports = importObject(shim, component.imports, missing);
    const group = memory.group();
    meter?.begin();
    try {
      const instance = await instantiate((file) => component.modules[file], meter ? tickImports(meter, imports) : imports, group.instantiateCore);
      return { instance, group, http, missing };
    } catch (e) {
      group.release();
      e.missing = missing;
      throw e;
    } finally {
      meter?.end();
    }
  };

  const handlerOf = (instance) => {
    const iface = instance[key] ?? instance.incomingHandler;
    return typeof iface?.handle === "function" ? iface : null;
  };

  // run serves one request with one instance.
  const run = async (entry) => {
    const scope = (entry.scope = createScope());
    let made;
    try {
      made = await newInstance(entry.id, scope);
    } catch (e) {
      running--;
      if (isMemoryError(e) && !entry.done) {
        // PX34: the request waits at the front of the queue.
        queue.unshift(entry);
        entry.scope = null;
        waitForMemory();
        return;
      }
      // runwasi gives an error to hyper for an instance that fails.
      reply(entry, { error: "trap" });
      pump();
      return;
    }
    memory.report();
    onInstance(made.instance);
    const { instance, group, http } = made;
    made = null;
    let trapped = false;
    const [out, state] = http.outparam();
    if (!entry.done) {
      meter?.begin();
      try {
        // handle is in asyncMode.val.exports: it gives a promise.
        await handlerOf(instance).handle(http.incoming(entry.msg), out);
      } catch {
        trapped = true;
      } finally {
        meter?.end();
      }
      reply(entry, responseFields(state, trapped));
    }
    // PX25: discard the instance. Its timers and client requests end.
    scope.cancel();
    entry.scope = null;
    group.release();
    running--;
    memoryFree();
    pump();
  };

  const waitForMemory = () => {
    if (memoryWait) return;
    if (running > 0) {
      // The end of another instance of this container starts the queue again.
      memoryWait = true;
      return;
    }
    memoryWait = setTimeout(() => {
      memoryWait = null;
      pump();
    }, retryMs);
  };

  const memoryFree = () => {
    if (memoryWait && memoryWait !== true) clearTimeout(memoryWait);
    memoryWait = null;
  };

  const pump = () => {
    while (instantiate && !memoryWait && running < limit && queue.length) {
      const entry = queue.shift();
      if (entry.done) continue;
      running++;
      run(entry);
    }
  };

  peer.on("stdin", () => {});
  peer.on("request", (msg) => {
    if (requests.has(msg.request)) return;
    // PX11, PX12: REQUEST_ID in the sequence of arrival, from 0.
    const entry = { msg, id: nextRequestID++, done: false, scope: null };
    requests.set(msg.request, entry);
    queue.push(entry);
    pump();
  });
  peer.on("cancel", (msg) => {
    const entry = requests.get(msg.request);
    if (!entry) return;
    reply(entry, { error: "cancelled" });
    // A request in the queue stays there until pump removes it.
    entry.scope?.cancel();
  });
  peer.on("fetched", (msg) => fetches.fetched(msg));

  return (async () => {
    try {
      ({ instantiate } = await load(component.js));
    } catch (e) {
      return textExit(`the component cannot instantiate: ${e?.message ?? e}`);
    }
    // runwasi links the component before it serves (instance_pre). The pod
    // worker makes one instance without a request for the same examination
    // (CO11, CO12), and discards it.
    try {
      const { instance, group } = await newInstance(null, createScope());
      memory.report();
      group.release();
      memory.report();
      if (!handlerOf(instance)) return textExit("the component has no function handle in wasi:http/incoming-handler");
    } catch (e) {
      if (!isMemoryError(e)) {
        const why = missingText(e.missing ?? [], e?.message ?? e, component.names?.imports);
        return textExit(why || `the component cannot instantiate: ${e?.message ?? e}`);
      }
    }
    pump();
    // The container runs until the Go side stops the pod worker.
    return new Promise(() => {});
  })();
}
