// Command components (docs/wasi-sockets.md, section 10.3) on the tab node
// and on a node worker: CO1 and CO5 (arguments, environment, stdout,
// stderr), and the exit codes of CO18. The pod worker of a component starts
// from a data: URL (decision 13); on a node worker it is a nested worker.
// The image wasi-test-cmd is a test image of section 10.1
// (wasm-images/rust/cmd). CO13 (modules with no change) is in
// wasi-hello.test.mjs.
import { test } from "node:test";
import assert from "node:assert/strict";
import { apply, forEachNode, logs, namespace, nodeName, onNode, pod, podPhase, waitForPod, waitFor } from "./harness.mjs";

const IMAGE = "ghcr.io/se-wo/tabernetes/wasi-test-cmd:latest";

const cmdPod = (name, node, { args, env = [] } = {}) => ({
  apiVersion: "v1",
  kind: "Pod",
  metadata: { name },
  spec: {
    runtimeClassName: "wasmtime",
    restartPolicy: "Never",
    nodeSelector: onNode(node),
    containers: [{ name: "c", image: IMAGE, ...(args ? { args } : {}), env }],
  },
});

forEachNode(["tab", "worker"], (kind) => {
  test(`CO1, CO5: a command component writes its arguments and its environment on ${kind}`, async (t) => {
    const ns = await namespace(t);
    const node = await nodeName(kind);
    await apply(cmdPod("cmd", node, { args: ["first arg", "second"], env: [{ name: "GREETING", value: "hello component" }] }), ns);
    const p = await waitForPod(ns, "cmd", podPhase("Succeeded", "Failed"));
    const out = await logs(ns, "cmd");
    assert.equal(p.spec.nodeName, node);
    assert.equal(p.status.phase, "Succeeded", out);
    assert.equal(p.status.containerStatuses[0].state.terminated.exitCode, 0, out);
    const args = out.split("\n").filter((l) => l.startsWith("arg ")).map((l) => l.slice(4));
    // argv[0] names the .wasm file, as on runwasi; then the args of the pod.
    assert.match(args[0] ?? "", /\.wasm$/, out);
    assert.deepEqual(args.slice(1), ["first arg", "second"], out);
    assert.match(out, /^env GREETING=hello component$/m);
    // Kubernetes variables of the container and PWD of the tab node.
    assert.match(out, /^env KUBERNETES_SERVICE_HOST=\S+$/m);
    assert.match(out, /^env PWD=\/\S*$/m);
    // stderr of the component.
    assert.match(out, /^exit 0$/m);
  });

  test(`CO18: a command component exits with 0 or 1 on ${kind}`, async (t) => {
    const ns = await namespace(t);
    const node = await nodeName(kind);
    const codes = { "exit0": ["0", 0], "exit1": ["1", 1], "exit3": ["3", 1] };
    await apply(Object.entries(codes).map(([name, [v]]) => cmdPod(name, node, { env: [{ name: "EXIT_CODE", value: v }] })), ns);
    for (const [name, [v, want]] of Object.entries(codes)) {
      const p = await waitForPod(ns, name, podPhase("Succeeded", "Failed"));
      const out = await logs(ns, name);
      assert.equal(p.status.containerStatuses[0].state.terminated.exitCode, want, `${name}: ${out}`);
      assert.equal(p.status.phase, want === 0 ? "Succeeded" : "Failed");
      assert.match(out, new RegExp(`^exit ${v}$`, "m"));
    }
  });
});

// Until W1.4b, a component that is not a command component gets a clear
// text. W1.4b changes this to the statuses of runwasi (CO2 to CO4, CO11).
test("a proxy component gets a text that the tab node cannot serve it yet", async (t) => {
  const ns = await namespace(t);
  const node = await nodeName("tab");
  await apply({
    apiVersion: "v1",
    kind: "Pod",
    metadata: { name: "proxy" },
    spec: {
      runtimeClassName: "wasmtime",
      nodeSelector: onNode(node),
      containers: [{ name: "c", image: "ghcr.io/se-wo/tabernetes/wasi-test-proxy:latest" }],
    },
  }, ns);
  const msg = await waitFor(async () => {
    const w = (await pod(ns, "proxy")).status?.containerStatuses?.[0]?.state?.waiting;
    return w?.reason === "CreateContainerError" && w.message;
  }, { what: "CreateContainerError of the proxy component" });
  assert.match(msg, /proxy component.*cannot serve proxy components yet/);
});
