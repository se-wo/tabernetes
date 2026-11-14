// Smoke test of the harness: the WASI preview 1 image wasi-hello of
// wasm-images/images.txt runs as a pod on the tab node and on a node worker.
import { test } from "node:test";
import assert from "node:assert/strict";
import { apply, forEachNode, logs, namespace, nodeName, onNode, podPhase, waitForPod } from "./harness.mjs";

forEachNode(["tab", "worker"], (kind) => {
  test(`wasi-hello runs on ${kind}`, async (t) => {
    const ns = await namespace(t);
    const node = await nodeName(kind);
    await apply({
      apiVersion: "v1",
      kind: "Pod",
      metadata: { name: "hello" },
      spec: {
        runtimeClassName: "wasmtime",
        restartPolicy: "Never",
        nodeSelector: onNode(node),
        containers: [{
          name: "hello",
          image: "ghcr.io/se-wo/tabernetes/wasi-hello:latest",
          args: ["--ticks=1", "--interval=10ms"],
          env: [{ name: "NAME", value: "e2e" }],
        }],
      },
    }, ns);
    const pod = await waitForPod(ns, "hello", podPhase("Succeeded", "Failed"));
    const out = await logs(ns, "hello");
    assert.equal(pod.spec.nodeName, node);
    assert.equal(pod.status.phase, "Succeeded", out);
    assert.equal(pod.status.containerStatuses[0].state.terminated.exitCode, 0);
    assert.match(out, /^Hello, e2e! A Go program \(.*wasip1\/wasm\) in a WebAssembly sandbox\.$/m);
  });
});
