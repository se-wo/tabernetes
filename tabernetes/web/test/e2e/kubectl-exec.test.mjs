// kubectl exec in the shell of the tab, on the tab node and on a node
// worker. kubectl sends a WebSocket; the apiserver translates it into SPDY to
// the node with the transport of its kubelet client. That transport passes
// restBytes (k8s/tabernetes/controlplane.go), which must give the SPDY round
// tripper its TLS config ("transport main.restBytes does not expose TLS
// client config"). The stdin of kubectl attach is in wasi-command.test.mjs
// (CO5).
import { test } from "node:test";
import assert from "node:assert/strict";
import { apply, forEachNode, namespace, nodeName, onNode, podReady, run, waitForPod } from "./harness.mjs";

forEachNode(["tab", "worker"], (kind) => {
  test(`kubectl exec runs a command in a container on ${kind}`, async (t) => {
    const ns = await namespace(t);
    const node = await nodeName(kind);
    await apply({
      apiVersion: "v1",
      kind: "Pod",
      metadata: { name: "bb" },
      spec: {
        nodeSelector: onNode(node),
        terminationGracePeriodSeconds: 0,
        containers: [{ name: "c", image: "busybox", command: ["sleep", "3600"] }],
      },
    }, ns);
    await waitForPod(ns, "bb", podReady);
    const exec = (line) => run(`kubectl exec -n ${ns} bb -c c ${line}`, { timeout: 60 });

    // stdout.
    let r = await exec("-- echo plain");
    assert.equal(r.rc, 0, r.out);
    assert.equal(r.out.trim(), "plain");

    // stdin, with -i, until its end.
    r = await run(`printf 'one\\ntwo\\n' | kubectl exec -i -n ${ns} bb -c c -- sh -c 'while read l; do echo "got $l"; done; echo end'`, { timeout: 60 });
    assert.equal(r.rc, 0, r.out);
    assert.deepEqual(r.out.trim().split("\n"), ["got one", "got two", "end"]);

    // stderr and the exit code of the command.
    r = await exec(`-- sh -c 'echo oops >&2; exit 3'`);
    assert.equal(r.rc, 3, r.out);
    assert.match(r.out, /^oops$/m);
    assert.match(r.out, /command terminated with exit code 3/);
  });
});
