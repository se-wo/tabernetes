// One WebAssembly pod container: runs a WASI preview 1 module in this
// worker. The file system, args and environ come from @bjorn3/browser_wasi_shim;
// stdin, poll_oneoff and the imports are done here, so a module can sleep
// and wait for stdin without spinning. wasi.js starts it.
import { WASI, WASIProcExit, Fd, Inode, File, Directory, PreopenDirectory, ConsoleStdout, wasi } from "@bjorn3/browser_wasi_shim";

// JavaScript Promise Integration lets an import wait for a promise: the
// worker keeps receiving messages (stdin) while the module waits.
const jspi = typeof WebAssembly.Suspending === "function" && typeof WebAssembly.promising === "function";
// Atomics.wait sleeps without burning a core. It needs a shared buffer,
// which shared wasm memory gives even without cross-origin isolation in
// Chromium. Elsewhere sleeps spin.
let sleeper = null;
try { sleeper = new Int32Array(new WebAssembly.Memory({ initial: 1, maximum: 1, shared: true }).buffer); } catch { /* not available */ }
const sleep = (ms) => {
  if (ms <= 0) return;
  if (sleeper) Atomics.wait(sleeper, 0, 0, ms);
  else for (const end = performance.now() + ms; performance.now() < end;);
};

const stdin = { chunks: [], closed: true, wake: null };
const wakeStdin = () => { const w = stdin.wake; stdin.wake = null; w?.(); };

onmessage = ({ data }) => {
  if (data.type === "start") run(data).then(
    (code) => postMessage({ type: "exit", code }),
    (e) => postMessage({ type: "exit", code: 128, error: String(e?.message ?? e) }),
  );
  else if (data.type === "stdin") {
    if (data.data) stdin.chunks.push(data.data);
    else stdin.closed = true;
    wakeStdin();
  }
};

class Stdin extends Fd {
  constructor() { super(); this.ino = Inode.issue_ino(); }
  fd_fdstat_get() {
    const fdstat = new wasi.Fdstat(wasi.FILETYPE_CHARACTER_DEVICE, 0);
    fdstat.fs_rights_base = BigInt(wasi.RIGHTS_FD_READ);
    return { ret: 0, fdstat };
  }
  fd_filestat_get() { return { ret: 0, filestat: new wasi.Filestat(this.ino, wasi.FILETYPE_CHARACTER_DEVICE, 0n) }; }
  fd_read(size) {
    const out = new Uint8Array(Math.min(size, stdin.chunks.reduce((n, c) => n + c.length, 0)));
    for (let n = 0; n < out.length;) {
      const c = stdin.chunks[0], take = Math.min(c.length, out.length - n);
      out.set(c.subarray(0, take), n);
      n += take;
      if (take === c.length) stdin.chunks.shift();
      else stdin.chunks[0] = c.subarray(take);
    }
    return { ret: 0, data: out };
  }
}

// tree turns the snapshot of a directory into the in-memory file system.
function tree({ files, dirs, readOnly }) {
  const root = {};
  const dirOf = (parts) => parts.reduce((d, p) => (d[p] ??= {}), root);
  for (const d of dirs) dirOf(d.split("/"));
  for (const [path, data] of files) {
    const parts = path.split("/");
    dirOf(parts.slice(0, -1))[parts.at(-1)] = new File(data, { readonly: readOnly });
  }
  const toMap = (node) => new Map(Object.entries(node).map(([k, v]) => [k, v instanceof File ? v : new Directory(toMap(v))]));
  return toMap(root);
}

// WebGPU for the module, as the import module "tabernetes_webgpu". It works
// only when the kubelet prepared a DRA device of webgpu.tabernetes.io for the
// container (gpu is then {device, pool, ...}, tabnode_gpu.go); otherwise every
// call fails with an error that says how to get one. The calls wait for the GPU
// with JSPI, so the module sees plain synchronous functions:
//
//   adapter_info(buf, len) -> n   the adapter as JSON; n > len: call again with n bytes
//   compute(wgsl, wgsl_len, data, data_len, x, y, z) -> 0
//                                 runs the entry point "main" of the WGSL
//                                 compute shader over data, bound read-write as
//                                 @group(0) @binding(0), with x*y*z workgroups,
//                                 and copies the buffer back into data
//   last_error(buf, len) -> n     the message of the last call that returned -1
//
// This is the small first step of the design in tabernetes/README.md ("WebGPU
// through DRA"); wasi:webgpu of wasi-gfx needs WASI preview 2 components.
function webgpuImports(gpu, mem) {
  let lastError = "";
  let device = null;
  const fail = (msg) => { lastError = String(msg); return -1; };
  const bytes = (ptr, len) => new Uint8Array(mem(), ptr, len);
  const put = (ptr, len, s) => {
    const b = new TextEncoder().encode(s);
    if (b.length <= len) bytes(ptr, b.length).set(b);
    return b.length;
  };
  const noDevice = "no WebGPU device is allocated to this container: request one with a ResourceClaim of the " +
    "DeviceClass webgpu.tabernetes.io and list it in resources.claims of the container";
  const getDevice = async () => {
    if (device) return device;
    const adapter = await navigator.gpu?.requestAdapter({ powerPreference: "high-performance" });
    if (!adapter) throw new Error("the browser has no WebGPU adapter (any more)");
    device = await adapter.requestDevice({
      requiredLimits: {
        maxBufferSize: adapter.limits.maxBufferSize,
        maxStorageBufferBindingSize: adapter.limits.maxStorageBufferBindingSize,
      },
    });
    device.lost.then((info) => { device = null; lastError = `the GPU device was lost: ${info.message}`; });
    return device;
  };
  return {
    adapter_info(ptr, len) {
      if (!gpu) return fail(noDevice);
      return put(ptr, len, JSON.stringify(gpu));
    },
    last_error(ptr, len) { return put(ptr, len, lastError); },
    // Without a device or JSPI the answer is synchronous: a promise would
    // reach the module as 0.
    compute(codePtr, codeLen, dataPtr, dataLen, x, y, z) {
      if (!gpu) return fail(noDevice);
      if (!jspi) return fail("WebGPU calls need JavaScript Promise Integration (WebAssembly.Suspending), which this browser lacks");
      return computeAsync(codePtr, codeLen, dataPtr, dataLen, x, y, z);
    },
  };
  async function computeAsync(codePtr, codeLen, dataPtr, dataLen, x, y, z) {
    try {
      const code = new TextDecoder().decode(bytes(codePtr, codeLen));
      const input = bytes(dataPtr, dataLen).slice(); // memory may grow while the GPU works
      const dev = await getDevice();
      const module = dev.createShaderModule({ code });
      let pipeline;
      try {
        pipeline = await dev.createComputePipelineAsync({ layout: "auto", compute: { module, entryPoint: "main" } });
      } catch (e) {
        const info = await module.getCompilationInfo();
        const msgs = info.messages.map((m) => `${m.lineNum}:${m.linePos}: ${m.type}: ${m.message}`);
        throw new Error(msgs.length ? `WGSL: ${msgs.join("; ")}` : e.message);
      }
      const size = Math.max(4, Math.ceil(dataLen / 4) * 4);
      const storage = dev.createBuffer({ size, usage: GPUBufferUsage.STORAGE | GPUBufferUsage.COPY_SRC | GPUBufferUsage.COPY_DST });
      const readback = dev.createBuffer({ size, usage: GPUBufferUsage.MAP_READ | GPUBufferUsage.COPY_DST });
      try {
        const padded = new Uint8Array(size);
        padded.set(input);
        dev.queue.writeBuffer(storage, 0, padded);
        const bind = dev.createBindGroup({ layout: pipeline.getBindGroupLayout(0), entries: [{ binding: 0, resource: { buffer: storage } }] });
        const enc = dev.createCommandEncoder();
        const pass = enc.beginComputePass();
        pass.setPipeline(pipeline);
        pass.setBindGroup(0, bind);
        pass.dispatchWorkgroups(x >>> 0, y >>> 0, z >>> 0);
        pass.end();
        enc.copyBufferToBuffer(storage, 0, readback, 0, size);
        dev.pushErrorScope("validation");
        dev.queue.submit([enc.finish()]);
        const err = await dev.popErrorScope();
        if (err) throw new Error(err.message);
        await readback.mapAsync(GPUMapMode.READ);
        bytes(dataPtr, dataLen).set(new Uint8Array(readback.getMappedRange(), 0, dataLen));
        readback.unmap();
      } finally {
        storage.destroy();
        readback.destroy();
      }
      return 0;
    } catch (e) {
      return fail(e?.message ?? e);
    }
  }
}

async function run({ module, args, env, preopens, stdin: withStdin, gpu }) {
  stdin.closed = !withStdin;
  // Not transferred: the shim counts the bytes written after the call.
  const out = (type) => new ConsoleStdout((data) => postMessage({ type, data }));
  const fds = [new Stdin(), out("stdout"), out("stderr"), ...preopens.map((p) => new PreopenDirectory(p.guest, tree(p)))];
  const w = new WASI(args, env, fds);
  const mem = () => w.inst.exports.memory.buffer;
  const funcs = { ...w.wasiImport, sched_yield: () => 0 };

  // fd_read on stdin waits for data with JSPI; without it, an empty stdin
  // reads as the end.
  const read = funcs.fd_read;
  funcs.fd_read = (fd, iovs, n, nread) => {
    if (fd === 0 && w.fds[0] instanceof Stdin && !stdin.chunks.length && !stdin.closed && jspi) {
      return new Promise((resolve) => { stdin.wake = resolve; }).then(() => funcs.fd_read(fd, iovs, n, nread));
    }
    return read(fd, iovs, n, nread);
  };
  funcs.poll_oneoff = (inPtr, outPtr, nsubs, neventsPtr) => poll(mem, w.fds, inPtr, outPtr, nsubs, neventsPtr);

  // The linear memory counts for limits.memory. The module runs without
  // timers, so its size is reported from the imports it calls.
  let memory = null, reported = -1;
  const report = () => {
    const n = memory?.buffer.byteLength;
    if (n !== reported && n !== undefined) { reported = n; postMessage({ type: "mem", data: n }); }
  };
  const watched = (fn) => (...a) => { report(); return fn(...a); };

  const imports = {};
  let gpuFuncs = null;
  for (const imp of WebAssembly.Module.imports(module)) {
    const ns = (imports[imp.module] ??= {});
    if (imp.kind !== "function") throw new Error(`the module imports ${imp.kind} ${imp.module}.${imp.name}: threads and other imports are not supported in the browser`);
    if (imp.module === "wasi_snapshot_preview1" || imp.module === "wasi_unstable") {
      let fn = watched(funcs[imp.name] ?? (() => wasi.ERRNO_NOSYS));
      if (jspi && (imp.name === "fd_read" || imp.name === "poll_oneoff")) fn = new WebAssembly.Suspending(fn);
      ns[imp.name] = fn;
    } else if (imp.module === "tabernetes_webgpu") {
      gpuFuncs ??= webgpuImports(gpu, mem);
      const fn = gpuFuncs[imp.name];
      if (!fn) ns[imp.name] = () => { throw new Error(`tabernetes_webgpu.${imp.name} does not exist`); };
      else ns[imp.name] = jspi && imp.name === "compute" ? new WebAssembly.Suspending(fn) : fn;
    } else {
      ns[imp.name] = () => { throw new Error(`${imp.module}.${imp.name} is not available in the browser`); };
    }
  }
  const instance = await WebAssembly.instantiate(module, imports);
  if (!instance.exports.memory) throw new Error("the module exports no memory");
  if (typeof instance.exports._start !== "function") throw new Error("the module has no _start: it is a library (reactor), not a command");
  w.inst = instance;
  memory = instance.exports.memory;
  report();
  try {
    await (jspi ? WebAssembly.promising(instance.exports._start) : instance.exports._start)();
    report();
    return 0;
  } catch (e) {
    report();
    if (e instanceof WASIProcExit) return e.code;
    // A trap: like wasmtime, report it and exit like an abort.
    postMessage({ type: "stderr", data: new TextEncoder().encode(`wasm trap: ${e?.message ?? e}\n`) });
    return 134;
  }
}

const CLOCK = 0, FD_READ = 1, FD_WRITE = 2, ABSTIME = 1, HANGUP = 1;
const now = (clock) => (clock === 0 ? BigInt(Date.now()) * 1000000n : BigInt(Math.round(performance.now() * 1e6)));

// poll waits for the first of the subscriptions: clocks (sleeps), stdin
// and other fds (always ready).
function poll(mem, fds, inPtr, outPtr, nsubs, neventsPtr) {
  let v = new DataView(mem());
  const subs = [];
  for (let i = 0; i < nsubs; i++) {
    const b = inPtr + i * 48, tag = v.getUint8(b + 8);
    const s = { userdata: v.getBigUint64(b, true), tag, fd: v.getUint32(b + 16, true) };
    if (tag === CLOCK) {
      const timeout = v.getBigUint64(b + 24, true), clock = v.getUint32(b + 16, true);
      s.clock = clock;
      s.deadline = v.getUint16(b + 40, true) & ABSTIME ? timeout : now(clock) + timeout;
    }
    subs.push(s);
  }
  const pending = () => stdin.chunks.reduce((n, c) => n + c.length, 0);
  const isStdin = (fd) => fd === 0 && fds[0] instanceof Stdin;
  const ready = () => subs.flatMap((s) => {
    if (s.tag === CLOCK) return now(s.clock) >= s.deadline ? [{ ...s, error: 0 }] : [];
    if (!fds[s.fd]) return [{ ...s, error: wasi.ERRNO_BADF }];
    if (s.tag === FD_READ && isStdin(s.fd)) {
      const n = pending();
      return n || stdin.closed ? [{ ...s, error: 0, nbytes: BigInt(n), flags: n ? 0 : HANGUP }] : [];
    }
    return [{ ...s, error: 0, nbytes: s.tag === FD_WRITE ? 65536n : 1n }];
  });
  const write = (evs) => {
    v = new DataView(mem());
    evs.forEach((e, i) => {
      const b = outPtr + i * 32;
      v.setBigUint64(b, e.userdata, true);
      v.setUint16(b + 8, e.error, true);
      v.setUint8(b + 10, e.tag);
      v.setBigUint64(b + 16, e.nbytes ?? 0n, true);
      v.setUint16(b + 24, e.flags ?? 0, true);
    });
    v.setUint32(neventsPtr, evs.length, true);
    return 0;
  };
  if (nsubs === 0) return wasi.ERRNO_INVAL;
  let evs = ready();
  if (evs.length) return write(evs);
  const ms = Math.min(...subs.filter((s) => s.tag === CLOCK).map((s) => Number(s.deadline - now(s.clock)) / 1e6));
  const onStdin = subs.some((s) => s.tag === FD_READ && isStdin(s.fd));
  if (onStdin && jspi) {
    return new Promise((resolve) => {
      const t = Number.isFinite(ms) ? setTimeout(resolve, Math.max(0, ms)) : null;
      stdin.wake = () => { clearTimeout(t); resolve(); };
    }).then(() => { stdin.wake = null; evs = ready(); return evs.length ? write(evs) : poll(mem, fds, inPtr, outPtr, nsubs, neventsPtr); });
  }
  if (!Number.isFinite(ms)) {
    // Only stdin, and no way to wait for it: it reads as closed.
    return write(subs.filter((s) => s.tag === FD_READ).map((s) => ({ ...s, error: 0, nbytes: 0n, flags: HANGUP })));
  }
  if (!sleeper && jspi) return new Promise((resolve) => setTimeout(resolve, ms)).then(() => write(ready()));
  sleep(ms);
  // Sleeps end a little early with a coarse clock: wait for the deadline.
  for (evs = ready(); !evs.length; evs = ready()) sleep(1);
  return write(evs);
}
