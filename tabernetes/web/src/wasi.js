// The WebAssembly runtime of the tab node: a pod with runtimeClassName
// wasmtime runs its WASI preview 1 module in a Web Worker (wasi.worker.js),
// a thread and a memory of its own. The Go side (tabnode_wasi.go) resolves
// the image and calls this as globalThis.tabernetesWasi.

const WASM_MAGIC = [0x00, 0x61, 0x73, 0x6d];

export function installWasi({ workerURL, mirror } = {}) {
  workerURL = new URL(workerURL || "wasi.worker.js", workerURL ? location.href : import.meta.url).href;
  mirror = mirror === "" ? "" : new URL(mirror || "oci/", mirror ? location.href : import.meta.url).href;
  // Compiled modules by layer digests and path: restarts and other pods of
  // the same image start without a download.
  const modules = new Map();
  globalThis.tabernetesWasi = {
    mirror,
    // load({layers: [{url, digest, kind}], headers, path}) resolves to
    // {module} or {unsupported: "why"} and rejects when a download fails.
    load(src) {
      const key = src.layers.map((l) => l.digest).join(",") + "#" + (src.path || "");
      let p = modules.get(key);
      if (!p) {
        p = loadModule(src);
        modules.set(key, p);
        p.then((r) => r.unsupported && modules.delete(key), () => modules.delete(key));
      }
      return p;
    },
    start: (opts) => start(workerURL, opts),
  };
}

async function loadModule({ layers, headers, path }) {
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
  if (bytes[6] === 1 && bytes[7] === 0) {
    return { unsupported: "it is a WASI preview 2 component: components are not supported in the browser yet, only WASI preview 1 modules (wasm32-wasip1, GOOS=wasip1)" };
  }
  try {
    return { module: await WebAssembly.compile(bytes) };
  } catch (e) {
    return { unsupported: `the module does not compile: ${e.message}` };
  }
}

async function fetchLayer({ url, digest, kind }, headers) {
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
function start(workerURL, { module, args, env, preopens, stdin, name }) {
  const worker = new Worker(workerURL, { name: name ? `wasm ${name}` : "wasm" });
  const handlers = {}, queued = { stdout: [], stderr: [], exit: [] };
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
  worker.postMessage({ type: "start", module, args, env, preopens, stdin });
  return {
    onStdout: on("stdout"),
    onStderr: on("stderr"),
    onExit: on("exit"),
    // write(bytes) feeds stdin, write(null) closes it.
    write: (data) => !exited && worker.postMessage({ type: "stdin", data }),
    kill: () => exit(137),
  };
}
