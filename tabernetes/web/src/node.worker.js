// A node of the cluster in a Web Worker (k8s/tabernetes/nodeworker.go). The
// page (nodes.js) sends the compiled WebAssembly module of the control plane
// and the credentials of the node; the same binary then runs a kubelet only.
// "api" channels go to the apiserver of the page, "kubelet:10250" channels
// come from it.
import "./wasm_exec.js";
import { installFS } from "./fs.js";
import { patchWalltime } from "./clock.js";
import { portChannel, openChannel } from "./channel.js";
import { installWasi } from "./wasi.js";
import { installGPU } from "./gpu.js";

const early = []; // channels that arrive before Go is up

const attach = (ch) => {
  if (globalThis.tabernetes?.attachChannel) globalThis.tabernetes.attachChannel(ch);
  else early.push(ch);
};

self.onmessage = (e) => {
  const m = e.data;
  if (m.type === "channel") attach(portChannel(m.port, m.label));
  else if (m.type === "start") start(m).catch((err) => self.postMessage({ type: "error", error: String(err?.message ?? err) }));
};

async function start({ module, config, wasi }) {
  let crashing = false;
  installFS((fd, s) => {
    // Logs of the kubelet: "Verbose" in the DevTools console of the worker.
    if (crashing || /^(panic:|fatal error:)/m.test(s)) {
      crashing = true;
      console.error(s);
      self.postMessage({ type: "crash", text: s });
    } else console.debug(s.replace(/\n$/, ""));
  });
  // WebAssembly pods of this node run in workers of this worker.
  if (wasi) installWasi(wasi);
  // addnode --gpu: this node probes WebGPU in the worker (config.gpu).
  installGPU();
  globalThis.tabernetesNodeConfig = config;
  globalThis.tabernetesOpen = (label) => Promise.resolve(openChannel(self, label));
  const go = new globalThis.Go();
  go.env = { HOME: "/root", USER: "root", PATH: "/usr/bin" };
  patchWalltime(go);
  const instance = await WebAssembly.instantiate(module, go.importObject);
  go.run(instance).then(
    () => self.postMessage({ type: "exit" }),
    (err) => self.postMessage({ type: "exit", error: String(err?.message ?? err) }),
  );
  // main() sets globalThis.tabernetes before it blocks for the first time.
  for (const ch of early.splice(0)) globalThis.tabernetes.attachChannel(ch);
  await globalThis.tabernetes.ready;
  self.postMessage({ type: "ready" });
}
