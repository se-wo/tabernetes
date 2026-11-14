// The WebAssembly runtime of the tab node: a pod with runtimeClassName
// wasmtime runs its WASI preview 1 module in a Web Worker (wasi.worker.js),
// a thread and a memory of its own. The Go side (tabnode_wasi.go) resolves
// the image and calls this as globalThis.tabernetesWasi.
//
// A WASI preview 2 component goes to the transpile worker of this Go thread
// (transpile.worker.js, docs/wasi-sockets.md section 8.2). This side keeps
// the generated JavaScript only as text: it runs only in a pod worker (CO14).
// The pod worker of a component (component.worker.js) starts from a data:
// URL, so it has an opaque origin (decision 13). This side moves its messages
// (wasi_messages.js) to the Go side and back, and does not interpret them:
// the Go side examines them (tabnode_wasi_msg.go).
import { ASYNC_IMPORTS, ASYNC_EXPORTS, JCO_VERSION } from "./async_lists.js";
import { isLabel } from "./transpile.js";

const WASM_MAGIC = [0x00, 0x61, 0x73, 0x6d];

export function installWasi({ workerURL, mirror, transpileURL, componentWorkerURL } = {}) {
  workerURL = new URL(workerURL || "wasi.worker.js", workerURL ? location.href : import.meta.url).href;
  mirror = mirror === "" ? "" : new URL(mirror || "oci/", mirror ? location.href : import.meta.url).href;
  // The transpile worker is in transpile/ next to wasi.worker.js, so that
  // the core modules of jco-transpile are in vendor/ next to them.
  transpileURL = new URL(transpileURL || "transpile/transpile.worker.js", transpileURL ? location.href : workerURL).href;
  // The pod worker of components is next to wasi.worker.js.
  componentWorkerURL = new URL(componentWorkerURL || "component.worker.js", componentWorkerURL ? location.href : workerURL).href;
  globalThis.tabernetesWasi = createRuntime({ workerURL, mirror, transpileURL, componentWorkerURL });
}

// createRuntime gives the object of globalThis.tabernetesWasi. The tests
// give it their own Worker and fetch.
export function createRuntime({ workerURL, mirror, transpileURL, componentWorkerURL, Worker: W = globalThis.Worker, fetch: f = globalThis.fetch }) {
  // Compiled modules (and the bytes of components) by layer digests and
  // path: restarts and other pods of the same image start without a
  // download.
  const modules = new Map();
  // Transpile results by the key of decision 2.
  const components = new Map();
  const transpiler = newTranspiler(transpileURL, W);
  const fetchLayer = (l, headers) => fetchLayerWith(f, l, headers);
  const podWorkers = newPodWorkers(componentWorkerURL, W, f);
  return {
    mirror,
    // load({layers: [{url, digest, kind}], headers, path, components})
    // resolves to {module}, {component} or {unsupported: "why"} and rejects
    // when a download fails. path can end with "#<function>", the function
    // that the entrypoint names (runwasi). Only a caller that sets
    // components: true gets a {component}:
    //
    //   {js: string, modules: {file: WebAssembly.Module}, imports: [string],
    //    exports: [[name, "function" | "instance"]], entrypoint: string,
    //    names: {imports: [string], exports: [[name, kind]]},
    //    world: "command" | "proxy" | ""}
    //
    // imports and exports are the lists of jco (names without versions, and
    // aliases); names has the names of the component with their versions, in
    // the sequence of the component (component_world.js). world is the world
    // that runwasi finds (CO2).
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
          return { unsupported: "it is a WASI preview 2 component, and the caller of load did not ask for components (components: true)" };
        });
      });
    },
    // start({module, ...}) runs a module (start below). start({component:
    // true, name}) starts the pod worker of a component and gives the
    // transport of its messages:
    //
    //   onMessage(fn)          fn(msg) gets each message of the pod worker
    //   post(msg, transfer)    sends a message to the pod worker
    //   kill()                 stops the pod worker; fn then gets {type: "exit", code: 137}
    //   close()                stops the pod worker without a message
    //
    // If the pod worker fails, fn gets {type: "exit", code: 128, error}.
    start: (opts) => (opts.component ? podWorkers.start(opts) : start(workerURL, opts)),
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
        return { component: { js: r.js, modules: compiled, imports: r.imports, exports: r.exports, names: r.names, world: r.world } };
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

// newPodWorkers starts the pod workers of components. The script is
// component.worker.js, which has all its code in one bundle. This side gets
// the text one time and starts each pod worker from a data: URL with it: the
// worker then has an opaque origin (decision 13). If a data: worker does not
// start in this browser, the next pod workers start from a Blob URL on the
// origin of the page. The JavaScript of a component never goes into these
// URLs: it goes to the pod worker in a message (CO14).
function newPodWorkers(url, W, f) {
  let text = null;
  let blobURL = null;
  const p = {
    mode: "data",
    start({ name }) {
      text ??= f(url).then((r) => {
        if (!r.ok) throw new Error(`GET ${url}: ${r.status} ${r.statusText}`);
        return r.text();
      });
      text.catch(() => { text = null; });
      let fn = null, worker = null, ready = false, ended = false;
      const early = [], queue = [];
      const deliver = (msg) => (fn ? fn(msg) : early.push(msg));
      const end = (msg) => {
        if (ended) return;
        ended = true;
        worker?.terminate();
        queue.length = 0;
        if (msg) deliver(msg);
      };
      const launch = (mode) => {
        let w;
        try {
          if (mode === "data") w = new W("data:text/javascript," + encodeURIComponent(textValue), { name: name ? `wasm ${name}` : "wasm" });
          else {
            blobURL ??= URL.createObjectURL(new Blob([textValue], { type: "text/javascript" }));
            w = new W(blobURL, { name: name ? `wasm ${name}` : "wasm" });
          }
        } catch (e) {
          return fail(mode, e?.message ?? String(e));
        }
        worker = w;
        w.onmessage = ({ data }) => {
          if (ended || worker !== w) return;
          // The transport consumes the first message, ready.
          if (!ready && data?.type === "ready") {
            ready = true;
            for (const [m, t] of queue.splice(0)) w.postMessage(m, t);
            return;
          }
          deliver(data);
        };
        w.onerror = (e) => {
          e.preventDefault?.();
          if (worker === w) fail(mode, e.message || "it did not start");
        };
        w.onmessageerror = () => {};
      };
      const fail = (mode, why) => {
        if (ended) return;
        // A data: worker that does not start: the Blob URL for this pod
        // worker and the next ones.
        if (!ready && mode === "data") {
          p.mode = "blob";
          worker?.terminate();
          worker = null;
          return launch("blob");
        }
        end({ type: "exit", code: 128, error: `the pod worker failed: ${String(why).slice(0, 2000)}` });
      };
      let textValue = "";
      text.then((t) => {
        textValue = t;
        if (!ended) launch(p.mode);
      }, (e) => end({ type: "exit", code: 128, error: `the pod worker cannot start: ${String(e?.message ?? e).slice(0, 2000)}` }));
      return {
        onMessage(handler) {
          fn = handler;
          for (const m of early.splice(0)) handler(m);
        },
        post(msg, transfer = []) {
          if (ended) return;
          if (ready) worker.postMessage(msg, transfer);
          else queue.push([msg, transfer]);
        },
        kill: () => end({ type: "exit", code: 137 }),
        close: () => end(null),
      };
    },
  };
  return p;
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
