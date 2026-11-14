// The world of a component and the statuses of runwasi
// (docs/wasi-sockets.md, section 10.3) on the tab node and on a node worker:
// CO2, CO3, CO4, CO11, CO12, CR6 and the import of exit-with-code of CO18.
// The images are the test images of section 10.1 (wasm-images/rust/).
//
// Each status is the status of the step wasi-command of the Docker check
// (docs/docker-node-check/wasi-command.sh) on runwasi v0.6.1:
//
//   wasi-core-go     Succeeded, restarts 0, exit code 0, Completed; "go was called"
//   wasi-core-start  Failed, restarts 0, exit code 137, Error
//   wasi-wasi03      Failed, restarts 0, exit code 137, Error
//   wasi-exitcode    Failed, restarts 0, exit code 137, Error
//   wasi-both        Failed, restarts 0, exit code 137, Error (CR6)
//   wasi-bothrev     the proxy world: ready
//
// runwasi writes the cause only into the log of its shim. The tab node writes
// the same text into the log of the container (CO12).
import { test } from "node:test";
import assert from "node:assert/strict";
import { apply, forEachNode, logs, namespace, nodeName, onNode, pod, podPhase, waitFor, waitForPod } from "./harness.mjs";

const image = (name) => `ghcr.io/se-wo/tabernetes/wasi-test-${name}:latest`;

const componentPod = (name, node, img, extra = {}) => ({
  apiVersion: "v1",
  kind: "Pod",
  metadata: { name },
  spec: {
    runtimeClassName: "wasmtime",
    restartPolicy: "Never",
    nodeSelector: onNode(node),
    containers: [{ name: "c", image: image(img), ...extra }],
  },
});

// state gives "<phase> <restarts> <exit code> <reason>", as pod_state of
// wasi-command.sh.
const state = (p) => {
  const s = p.status.containerStatuses?.[0];
  const t = s?.state?.terminated;
  return `${p.status.phase} ${s?.restartCount} ${t?.exitCode} ${t?.reason}`;
};

// The runwasi text of an import that the linker does not have.
const missingImport = (name) => new RegExp(`^component imports instance \`${name.replace(/[.*+?^${}()|[\]\\/]/g, "\\$&")}\`, but a matching implementation was not found in the linker`, "m");

forEachNode(["tab", "worker"], (kind) => {
  test(`CO3, CO4: the function of the entrypoint, and the status of runwasi without it, on ${kind}`, async (t) => {
    const ns = await namespace(t);
    const node = await nodeName(kind);
    await apply([
      componentPod("core-go", node, "core", { command: ["/wasi-test-core.wasm#go"] }),
      componentPod("core-start", node, "core"),
    ], ns);
    const go = await waitForPod(ns, "core-go", podPhase("Succeeded", "Failed"));
    const goLog = await logs(ns, "core-go");
    assert.equal(state(go), "Succeeded 0 0 Completed", goLog);
    assert.match(goLog, /^go was called$/m);
    const start = await waitForPod(ns, "core-start", podPhase("Succeeded", "Failed"));
    const startLog = await logs(ns, "core-start");
    assert.equal(state(start), "Failed 0 137 Error", startLog);
    assert.match(startLog, /^component does not have exported function "_start"$/m);
  });

  test(`CO11, CO12: a WASI 0.3 import gives the status of runwasi with the name of the import on ${kind}`, async (t) => {
    const ns = await namespace(t);
    const node = await nodeName(kind);
    await apply(componentPod("wasi03", node, "wasi03"), ns);
    const p = await waitForPod(ns, "wasi03", podPhase("Succeeded", "Failed"));
    const out = await logs(ns, "wasi03");
    assert.equal(state(p), "Failed 0 137 Error", out);
    assert.match(out, missingImport("wasi:clocks/monotonic-clock@0.3.0"));
    // The component did not run: its output "now ..." is not in the log.
    assert.doesNotMatch(out, /^now /m);
  });

  test(`CO18: an import of exit-with-code gives the status of CO11 on ${kind}`, async (t) => {
    const ns = await namespace(t);
    const node = await nodeName(kind);
    await apply(componentPod("exitcode", node, "exitcode", { env: [{ name: "EXIT_CODE", value: "3" }] }), ns);
    const p = await waitForPod(ns, "exitcode", podPhase("Succeeded", "Failed"));
    const out = await logs(ns, "exitcode");
    assert.equal(state(p), "Failed 0 137 Error", out);
    assert.match(out, missingImport("wasi:cli/exit@0.2.9"));
    assert.match(out, /instance export `exit-with-code` is missing/);
    assert.doesNotMatch(out, /^exit-with-code 3$/m);
  });

  test(`CR6: a command component with an import of wasi:http gets the status of runwasi on ${kind}`, async (t) => {
    const ns = await namespace(t);
    const node = await nodeName(kind);
    await apply(componentPod("cmdhttp", node, "cmdhttp"), ns);
    const p = await waitForPod(ns, "cmdhttp", podPhase("Succeeded", "Failed"));
    const out = await logs(ns, "cmdhttp");
    assert.equal(state(p), "Failed 0 137 Error", out);
    assert.match(out, /^component imports instance `wasi:http\/(types|outgoing-handler)@0\.2\.12`/m);
  });

  test(`CO2: the first export selects the world on ${kind}`, async (t) => {
    const ns = await namespace(t);
    const node = await nodeName(kind);
    await apply([componentPod("both", node, "both"), componentPod("bothrev", node, "bothrev")], ns);
    // wasi:cli/run first: the command world, which has no wasi:http (CR6).
    const both = await waitForPod(ns, "both", podPhase("Succeeded", "Failed"));
    const out = await logs(ns, "both");
    assert.equal(state(both), "Failed 0 137 Error", out);
    assert.match(out, missingImport("wasi:http/types@0.2.12"));
    // wasi:http/incoming-handler first: the proxy world. runwasi serves it.
    // The Go side starts the proxy server since W2.1a. Until W2.2, the pod
    // worker refuses a proxy component with the exit code 137; after W2.2,
    // the container runs. W2.3 sends requests to it.
    const st = await waitFor(async () => {
      const s = (await pod(ns, "bothrev")).status?.containerStatuses?.[0]?.state;
      return s?.running || s?.terminated ? s : undefined;
    }, { what: "the start of bothrev" });
    if (st.terminated) {
      const out = await logs(ns, "bothrev");
      assert.equal(st.terminated.exitCode, 137, out);
      assert.match(out, /is a proxy component \(wasi:http\/incoming-handler\)/);
    }
  });
});
