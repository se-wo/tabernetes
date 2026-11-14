// The pod worker of a WASI preview 2 component (docs/wasi-sockets.md,
// decision 13). wasi.js starts it from a data: URL, so it has an opaque
// origin: it cannot read IndexedDB, Cache Storage or files of the page. It
// gets the generated JavaScript and the compiled core modules in the message
// "start" and loads the JavaScript from a Blob URL that it makes.
//
// The task "web" of mise.toml bundles this file with WASIShim of
// @bytecodealliance/preview2-shim, sockets.js, http.js and wasi_messages.js into one
// script, as each *.worker.js: a data: worker cannot load other files of the
// page.
import { WASIShim } from "@bytecodealliance/preview2-shim/instantiation";
import { InMemoryFilesystemAdapter as Memory } from "@bytecodealliance/preview2-shim/filesystem";
import * as io from "@bytecodealliance/preview2-shim/io";
import { createPeer } from "./wasi_messages.js";
import { runComponent, ERROR_EXIT } from "./component.js";
import { createMeter, meterSuspending } from "./cpu.js";

const peer = createPeer({
  post: (msg, transfer) => postMessage(msg, transfer),
  onDrop: (why) => console.warn(`pod worker: dropped a message from the tab node: ${why}`),
});

let started = false;
peer.on("start", (msg) => {
  if (started) return;
  started = true;
  // AC2: the busy time of the instance goes to the Go side in cpu messages,
  // each second while the instance is suspended, and from its imports.
  const meter = createMeter({ send: (ms) => peer.send("cpu", { ms }) });
  meterSuspending(meter);
  const timer = setInterval(() => meter.tick(), 1000);
  runComponent(msg, { peer, WASIShim, io: { ...io }, Memory, meter }).finally(() => {
    clearInterval(timer);
    meter.flush();
  }).then(
    (code) => peer.send("exit", { code }),
    (e) => peer.send("exit", { code: ERROR_EXIT, error: `the pod worker failed: ${String(e?.message ?? e).slice(0, 4000)}` }),
  );
});

self.onmessage = ({ data }) => peer.dispatch(data);
// The first message: wasi.js then knows that a data: worker can run here.
peer.send("ready");
