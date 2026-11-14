// Command components (docs/wasi-sockets.md, section 10.3) on the tab node
// and on a node worker: CO1 and CO5 (arguments, environment, stdin, stdout,
// stderr), and the exit codes of CO18. The pod worker of a component starts
// from a data: URL (decision 13); on a node worker it is a nested worker.
// The image wasi-test-cmd is a test image of section 10.1
// (wasm-images/rust/cmd). CO13 (modules with no change) is in
// wasi-hello.test.mjs. The worlds and the statuses of runwasi (CO2 to CO4,
// CO11, CO12, CR6, exit-with-code) are in wasi-worlds.test.mjs.
import { test } from "node:test";
import assert from "node:assert/strict";
import { apply, forEachNode, logs, namespace, nodeName, onNode, pod, podPhase, waitFor, waitForPod } from "./harness.mjs";
import { evalOn, closeDevtools } from "./devtools.mjs";

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

// CO5: stdin of a command component. The component reads one line of
// stdin when STDIN is set (wasm-images/rust/cmd). The step wasi-command of
// the Docker check gives the line with crictl attach. The shell of the tab
// cannot attach to a container without a TTY yet ("SpdyRoundTripper: unable
// to retrieve TLS config"; also kubectl exec). So the test gives the line
// as the Go side does: a message stdin on the transport of wasi.js, on the
// Go thread of the node. With stdin in the container spec, the Go side keeps
// stdin open and the component waits. Without it, the Go side ends stdin at
// once, as /dev/null does.
forEachNode(["tab", "worker"], (kind) => {
  test(`CO5: a command component reads stdin on ${kind}`, async (t) => {
    t.after(closeDevtools);
    const ns = await namespace(t);
    const node = await nodeName(kind);
    // Keep the transport of the pod worker of ns/stdin/c.
    await evalOn(node, (name) => {
      const w = globalThis.tabernetesWasi;
      const start = (w.e2eStart ??= w.start);
      w.start = (opts) => {
        const h = start(opts);
        if (opts.component && opts.name === name) {
          globalThis.e2eStdin = (text, end) => {
            const b = new TextEncoder().encode(text).buffer;
            h.post({ type: "stdin", bytes: b, end }, [b]);
          };
          w.start = start;
        }
        return h;
      };
    }, `${ns}/stdin/c`);
    const withStdin = cmdPod("stdin", node, { env: [{ name: "STDIN", value: "1" }] });
    Object.assign(withStdin.spec.containers[0], { stdin: true, stdinOnce: true });
    await apply([withStdin, cmdPod("nostdin", node, { env: [{ name: "STDIN", value: "1" }] })], ns);

    const none = await waitForPod(ns, "nostdin", podPhase("Succeeded", "Failed"));
    const noneLog = await logs(ns, "nostdin");
    assert.equal(none.status.containerStatuses[0].state.terminated.exitCode, 0, noneLog);
    assert.match(noneLog, /^stdin $/m);

    await waitForPod(ns, "stdin", (p) => !!p.status?.containerStatuses?.[0]?.state?.running);
    await waitFor(() => evalOn(node, () => typeof globalThis.e2eStdin === "function"), { what: "the transport of the pod worker" });
    // The component waits for its line: it does not end without stdin.
    await new Promise((r) => setTimeout(r, 2000));
    assert.equal((await pod(ns, "stdin")).status.phase, "Running");
    await evalOn(node, () => {
      globalThis.e2eStdin("line from ", false);
      globalThis.e2eStdin("stdin\nmore", true);
      delete globalThis.e2eStdin;
    });
    const p = await waitForPod(ns, "stdin", podPhase("Succeeded", "Failed"));
    const out = await logs(ns, "stdin");
    assert.equal(p.status.containerStatuses[0].state.terminated.exitCode, 0, out);
    assert.match(out, /^stdin line from stdin$/m);
  });
});
