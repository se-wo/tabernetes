// CPU time and memory of command components on the tab node
// (docs/wasi-sockets.md, sections 7.1 and 7.11): AC2, CO6 and CO7. The image
// wasi-test-cmd (wasm-images/rust/cmd) does CPU work for $BUSY_MS ms and fills
// $ALLOC_MB MiB of memory.
//
// The part of CO7 for an instance of one request is for proxy components
// (phase 2). A command component has one instance: its memory counts until
// the container ends.
import { test } from "node:test";
import assert from "node:assert/strict";
import { apply, kubectl, namespace, nodeName, onNode, podPhase, waitFor, waitForPod } from "./harness.mjs";

const IMAGE = "ghcr.io/se-wo/tabernetes/wasi-test-cmd:latest";

const cmdPod = (name, node, env, resources) => ({
  apiVersion: "v1",
  kind: "Pod",
  metadata: { name },
  spec: {
    runtimeClassName: "wasmtime",
    restartPolicy: "Never",
    nodeSelector: onNode(node),
    containers: [{ name: "c", image: IMAGE, env, ...(resources ? { resources } : {}) }],
  },
});

// AC2: the pod worker sends cpu messages, and metrics-server shows them. Its
// window needs two scrapes of 15 seconds, so the loop runs longer than that.
test("AC2: a component in a loop of CPU work shows CPU in kubectl top pod", async (t) => {
  const ns = await namespace(t);
  const node = await nodeName("tab");
  await apply(cmdPod("busy", node, [{ name: "BUSY_MS", value: "240000" }]), ns);
  await waitForPod(ns, "busy", podPhase("Running"));
  const cpu = await waitFor(async () => {
    const out = await kubectl(`top pod -n ${ns} busy --no-headers`).catch(() => "");
    const m = /^busy\s+(\d+)m\s/m.exec(out);
    return m && Number(m[1]) >= 200 && Number(m[1]);
  }, { timeout: 150, interval: 5, what: "CPU of the pod busy in kubectl top pod" });
  // One loop of CPU work uses at most one core.
  assert.ok(cpu <= 1100, `${cpu}m`);
});
