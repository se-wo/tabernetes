import "./wasm_exec.js";
import { startTerminal } from "./terminal.js";
import { joinBuiltins } from "./join.js";
import { proxyBuiltins } from "./proxy.js";
import { installWasi } from "./wasi.js";
import { nodeBuiltins } from "./nodes.js";
import { installGPU } from "./gpu.js";
import { vncBuiltins } from "./vnc.js";
import { createBroker } from "./memory.js";

const el = document.getElementById("tabernetes");
// The panel of "proxy" goes next to the terminal: both share a row.
if (!el.parentElement.classList.contains("tabernetes-row")) {
  const row = document.createElement("div");
  row.className = "tabernetes-row";
  el.before(row);
  row.append(el);
}
// A data attribute of the page, else a file next to tabernetes.js. Absolute,
// because the workers of nodes.js resolve them, too.
const url = (attr, file) => new URL(attr || file, attr ? location.href : import.meta.url).href;
// WebAssembly pods run in workers.
const wasi = {
  workerURL: url(el.dataset.wasiWorker, "wasi.worker.js"),
  mirror: el.dataset.oci === "" ? "" : url(el.dataset.oci, "oci/"),
};
// The broker of the Wasm memories of the tab (memory.js, section 8.6). The Go
// binary of the page counts 2 (section 14).
const memory = createBroker();
memory.client({ base: 2, name: "the Go binary of the page" });
// For the DevTools console and the end-to-end tests.
globalThis.tabernetesWasmMemories = memory;
installWasi({ ...wasi, memory });
// helm downloads chart repositories without CORS headers from copies in
// charts/ (k8s/tabernetes/helm.go).
globalThis.tabernetesCharts = url(el.dataset.charts, "charts/");
// The tab node offers the WebGPU adapter of the browser as a DRA device.
installGPU();
// The status line under the terminal: the nodes (Docker, then Web Workers),
// then the forwarded ports.
const status = document.getElementById("tabernetes-status");
// The connections of join (Docker nodes and kubectl), for rmnode.
const connections = new Map();
const nodes = document.createElement("span");
const workerNodes = document.createElement("span");
const ports = document.createElement("span");
status?.append(nodes, workerNodes, ports);
startTerminal(el, {
  wasmURL: el.dataset.wasm,
  workerURL: el.dataset.worker,
  wasmSize: Number(el.dataset.size) || 0,
  builtins: {
    ...joinBuiltins({
      image: el.dataset.image || TABERNETES_IMAGE,
      stun: el.dataset.stun ?? undefined,
      status: status && nodes,
      connections,
    }),
    ...nodeBuiltins({ workerURL: url(el.dataset.nodeWorker, "node.worker.js"), wasi, memory, status: status && workerNodes, connections }),
    ...proxyBuiltins({ status: status && ports, terminal: el }),
    // The VNC screen covers the terminal, the panel of "proxy" and the status line.
    ...vncBuiltins({ box: el.closest(".tabernetes-wrap") || el.parentElement }),
  },
}).then((t) => (globalThis.tabernetesTerminal = t)).catch((e) => {
  el.textContent = "Tabernetes failed to start: " + e;
  console.error(e);
});
