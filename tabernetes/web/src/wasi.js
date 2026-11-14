// The WebAssembly runtime of the tab node: a pod with runtimeClassName
// wasmtime runs its WASI preview 1 module in a Web Worker (wasi.worker.js),
// a thread and a memory of its own. The Go side (tabnode_wasi.go) resolves
// the image and calls this as globalThis.tabernetesWasi.
//
// A WASI preview 2 component goes to the transpile worker of this Go thread
// (transpile.worker.js, docs/wasi-sockets.md section 8.2). This side keeps
// the generated JavaScript only as text: it runs only in a pod worker (CO14).
import { ASYNC_IMPORTS, ASYNC_EXPORTS, JCO_VERSION } from "./async_lists.js";
import { isLabel } from "./transpile.js";

const WASM_MAGIC = [0x00, 0x61, 0x73, 0x6d];

export function installWasi({ workerURL, mirror, transpileURL } = {}) {
  workerURL = new URL(workerURL || "wasi.worker.js", workerURL ? location.href : import.meta.url).href;
  mirror = mirror === "" ? "" : new URL(mirror || "oci/", mirror ? location.href : import.meta.url).href;
  // The transpile worker is in transpile/ next to wasi.worker.js, so that
  // the core modules of jco-transpile are in vendor/ next to them.
  transpileURL = new URL(transpileURL || "transpile/transpile.worker.js", transpileURL ? location.href : workerURL).href;
  globalThis.tabernetesWasi = createRuntime({ workerURL, mirror, transpileURL });
}

// createRuntime gives the object of globalThis.tabernetesWasi. The tests
// give it their own Worker and fetch.
export function createRuntime({ workerURL, mirror, transpileURL, Worker: W = globalThis.Worker, fetch: f = globalThis.fetch }) {
  // Compiled modules (and the bytes of components) by layer digests and
  // path: restarts and other pods of the same image start without a
  // download.
  const modules = new Map();
  // Transpile results by the key of decision 2.
  const components = new Map();
  const transpiler = newTranspiler(transpileURL, W);
  const fetchLayer = (l, headers) => fetchLayerWith(f, l, headers);
  return {
    mirror,
    // load({layers: [{url, digest, kind}], headers, path, components})
    // resolves to {module}, {component} or {unsupported: "why"} and rejects
    // when a download fails. path can end with "#<function>", the function
    // that the entrypoint names (runwasi). Only a caller that sets
    // components: true gets a {component}:
    //
    //   {js: string, modules: {file: WebAssembly.Module}, imports: [string],
    //    exports: [[name, "function" | "instance"]], entrypoint: string}
    //
    // The pod worker gives js and modules to jco: js exports instantiate(
    // getCoreModule, imports), getCoreModule(file) gives modules[file].
    // Other callers get {unsupported} for a component.
    load(src) {
      const hash = (src.path || "").indexOf("#");
      const path = hash < 0 ? src.path || "" : src.path.slice(0, hash);
      const entrypoint = hash < 0 ? "" : src.path.slice(hash + 1);
      const digests = src.layers.map((l) => l.digest).join(",");
      const key = digests + "#" + path;
      let p = modules.get(key);
      if (!p) {
        p = loadModule({ ...src, path }, fetchLayer);
        modules.set(key, p);
        p.then((r) => r.unsupported && modules.delete(key), () => modules.delete(key));
      }
      return p.then((r) => {
        if (!r.componentBytes) return r;
        // Only a WIT label can name an export. Another entrypoint does not
        // go into the lists of jco; the pod worker then finds no function.
        const fn = isLabel(entrypoint) ? entrypoint : "";
        return loadComponent({ digests, path, entrypoint: fn, bytes: r.componentBytes }).then((c) => {
          if (c.unsupported) return c;
          if (src.components) return { component: { ...c.component, entrypoint } };
          return { unsupported: "it is a WASI preview 2 component: the tab node transpiles it, but cannot run components yet, only WASI preview 1 modules (wasm32-wasip1, GOOS=wasip1)" };
        });
      });
    },
    start: (opts) => start(workerURL, opts),
    // stats gives the count of transpiler runs, for tests (CO8).
    stats: () => ({ transpiles: transpiler.runs, transpileWorker: transpiler.started }),
  };

  function loadComponent({ digests, path, entrypoint, bytes }) {
    // CO10: without JSPI, a component cannot run.
    if (typeof WebAssembly.Suspending !== "function" || typeof WebAssembly.promising !== "function") {
      return Promise.resolve({ unsupported: "it is a WASI preview 2 component, and this browser has no JavaScript Promise Integration (WebAssembly.Suspending, WebAssembly.promising), which components need: use Chrome or Edge 137, Firefox 153, Safari 27 or newer" });
    }
    // The key of decision 2.
    const key = JSON.stringify([digests, path, JCO_VERSION, entrypoint, ASYNC_IMPORTS, ASYNC_EXPORTS]);
    let p = components.get(key);
    if (!p) {
      p = transpiler.run(bytes, entrypoint).then(async (r) => {
        if (r.error) return { unsupported: `the component cannot run: ${r.error}` };
        const compiled = {};
        try {
          for (const [file, b] of Object.entries(r.modules)) compiled[file] = await WebAssembly.compile(b);
        } catch (e) {
          return { unsupported: `the transpiled component does not compile: ${e.message}` };
        }
        return { component: { js: r.js, modules: compiled, imports: r.imports, exports: r.exports } };
      });
      components.set(key, p);
      p.then((r) => r.unsupported && components.delete(key), () => components.delete(key));
    }
    return p;
  }
}

// newTranspiler gives the transpile worker of this Go thread. The worker
// starts with the first component; a Go thread without components does not
// load jco (section 8.2). If the worker fails, the next run starts a new
// one.
function newTranspiler(url, W) {
  let worker = null, nextID = 0;
  const pending = new Map();
  const t = {
    runs: 0,
    started: false,
    run(bytes, entrypoint) {
      if (!worker) {
        worker = new W(url, { type: "module", name: "transpile" });
        t.started = true;
        worker.onmessage = ({ data }) => {
          const p = pending.get(data.id);
          if (!p) return;
          pending.delete(data.id);
          p.resolve(data);
        };
        const fail = (msg) => {
          worker?.terminate?.();
          worker = null;
          for (const p of pending.values()) p.reject(new Error(msg));
          pending.clear();
        };
        worker.onerror = (e) => { e.preventDefault?.(); fail(`the transpile worker failed: ${e.message || "it did not start (is transpile/transpile.worker.js there?)"}`); };
        worker.onmessageerror = () => fail("the transpile worker sent a message that cannot be read");
      }
      t.runs++;
      const id = ++nextID;
      // A copy: the cache keeps the bytes for a second entrypoint.
      const copy = bytes.slice().buffer;
      return new Promise((resolve, reject) => {
        pending.set(id, { resolve, reject });
        worker.postMessage({ id, bytes: copy, entrypoint }, [copy]);
      });
    },
  };
  return t;
}

async function loadModule({ layers, headers, path }, fetchLayer) {
  let bytes = null;
  if (!path) bytes = await fetchLayer(layers[0], headers);
  else {
    // The topmost layer that has the file wins.
    for (const l of [...layers].reverse()) {
      const tar = await fetchLayer(l, headers);
      bytes = untar(tar, path);
      if (bytes === false) return { unsupported: `/${path} is deleted in the image` };
      if (bytes) break;
    }
    if (!bytes) return { unsupported: `/${path} (the entrypoint of the image) is not in its layers` };
  }
  if (bytes.length < 8 || WASM_MAGIC.some((b, i) => bytes[i] !== b)) {
    return { unsupported: `${path ? "/" + path : "the module layer"} is not a WebAssembly module` };
  }
  // A component has layer 1 in the version field (bytes 6 and 7).
  if (bytes[6] === 1 && bytes[7] === 0) return { componentBytes: bytes };
  try {
    return { module: await WebAssembly.compile(bytes) };
  } catch (e) {
    return { unsupported: `the module does not compile: ${e.message}` };
  }
}

async function fetchLayerWith(fetch, { url, digest, kind }, headers) {
  let resp;
  try {
    resp = await fetch(url, { headers });
  } catch (e) {
    throw new Error(`${new URL(url).host} does not allow the download (${e.message}; no CORS headers on the blob or its redirect)`);
  }
  if (!resp.ok) throw new Error(`GET ${url}: ${resp.status} ${resp.statusText}`);
  const buf = new Uint8Array(await resp.arrayBuffer());
  if (crypto.subtle && digest.startsWith("sha256:")) {
    const sum = [...new Uint8Array(await crypto.subtle.digest("SHA-256", buf))].map((b) => b.toString(16).padStart(2, "0")).join("");
    if ("sha256:" + sum !== digest) throw new Error(`layer ${digest}: the download does not match its digest`);
  }
  if (kind !== "tar+gzip") return buf;
  const out = new Blob([buf]).stream().pipeThrough(new DecompressionStream("gzip"));
  return new Uint8Array(await new Response(out).arrayBuffer());
}

// untar returns the file at path in a tar archive, false when a whiteout
// deletes it, null when the archive does not have it.
function untar(tar, path) {
  const dec = new TextDecoder();
  const str = (o, n) => { const b = tar.subarray(o, o + n); const z = b.indexOf(0); return dec.decode(z < 0 ? b : b.subarray(0, z)); };
  const clean = (p) => p.replace(/^(\.?\/)+/, "").replace(/\/+$/, "");
  const dir = path.includes("/") ? path.slice(0, path.lastIndexOf("/") + 1) : "";
  const base = path.slice(dir.length);
  let longName = null, found = null;
  for (let o = 0; o + 512 <= tar.length;) {
    if (tar[o] === 0) break; // the end: two zero blocks
    const size = parseInt(str(o + 124, 12).trim() || "0", 8);
    const type = String.fromCharCode(tar[o + 156] || 0x30);
    let name = longName ?? (str(o + 345, 155) ? str(o + 345, 155) + "/" : "") + str(o, 100);
    longName = null;
    const data = o + 512;
    o = data + Math.ceil(size / 512) * 512;
    if (type === "L") { longName = str(data, size); continue; } // GNU long name
    if (type === "x") { // PAX header: path=
      const m = /\d+ path=([^\n]*)\n/.exec(dec.decode(tar.subarray(data, data + size)));
      if (m) longName = m[1];
      continue;
    }
    name = clean(name);
    if (name === dir + ".wh." + base) found = false;
    else if (name === path && (type === "0" || type === "\0" || type === "7")) found = tar.slice(data, data + size);
  }
  return found;
}

// start runs a module in a new worker. The callbacks may come later than
// the first events: those wait for them.
// gpu is the WebGPU device the kubelet prepared for the container
// (tabnode_gpu.go), or null.
function start(workerURL, { module, args, env, preopens, stdin, name, gpu }) {
  const worker = new Worker(workerURL, { name: name ? `wasm ${name}` : "wasm" });
  const handlers = {}, queued = { stdout: [], stderr: [], exit: [], mem: [] };
  let exited = false;
  const emit = (type, ...a) => (handlers[type] ? handlers[type](...a) : queued[type].push(a));
  const exit = (code, msg) => {
    if (exited) return;
    exited = true;
    worker.terminate();
    emit("exit", code, msg);
  };
  const on = (type) => (fn) => { handlers[type] = fn; for (const a of queued[type].splice(0)) fn(...a); };
  worker.onmessage = ({ data }) => {
    if (data.type === "exit") exit(data.code, data.error);
    else if (!exited) emit(data.type, data.data);
  };
  worker.onerror = (e) => { e.preventDefault(); exit(128, `wasm worker: ${e.message || "failed to start (is wasi.worker.js there?)"}`); };
  worker.postMessage({ type: "start", module, args, env, preopens, stdin, gpu: gpu ?? null });
  return {
    onStdout: on("stdout"),
    onStderr: on("stderr"),
    onExit: on("exit"),
    onMem: on("mem"),
    // write(bytes) feeds stdin, write(null) closes it.
    write: (data) => !exited && worker.postMessage({ type: "stdin", data }),
    kill: () => exit(137),
  };
}
