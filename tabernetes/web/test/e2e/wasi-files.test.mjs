// Files of command components (docs/wasi-sockets.md, sections 7.10 and
// 8.10): FS1 on the tab node and on a node worker, FS2 to FS4 on the tab
// node. The image wasi-test-cmd is a test image of section 10.1
// (wasm-images/rust/cmd). The step wasi-command of the Docker check has the
// same tests on runwasi.
import { test } from "node:test";
import assert from "node:assert/strict";
import { apply, forEachNode, kubectl, logs, namespace, nodeName, onNode, pod, podPhase, waitFor, waitForPod } from "./harness.mjs";

const IMAGE = "ghcr.io/se-wo/tabernetes/wasi-test-cmd:latest";

const cmdPod = (name, node, { args, env = [], mounts = [], volumes = [], restartPolicy = "Never", image = IMAGE } = {}) => ({
  apiVersion: "v1",
  kind: "Pod",
  metadata: { name },
  spec: {
    runtimeClassName: "wasmtime",
    restartPolicy,
    nodeSelector: onNode(node),
    containers: [{ name: "c", image, ...(args ? { args } : {}), env, volumeMounts: mounts }],
    volumes,
  },
});

forEachNode(["tab", "worker"], (kind) => {
  // FS1 with CO1 and CO5: the arguments, the environment and a file of a
  // volume, as in the step wasi-command of the Docker check.
  test(`CO1, CO5, FS1: a command component reads a file of a volume on ${kind}`, async (t) => {
    const ns = await namespace(t);
    const node = await nodeName(kind);
    await apply([
      { apiVersion: "v1", kind: "ConfigMap", metadata: { name: "files" }, data: { file: "from a volume" } },
      cmdPod("cmd", node, {
        args: ["first arg", "second"],
        env: [{ name: "GREETING", value: "hello component" }, { name: "FILE", value: "/config/file" }],
        mounts: [{ name: "config", mountPath: "/config" }],
        volumes: [{ name: "config", configMap: { name: "files" } }],
      }),
    ], ns);
    const p = await waitForPod(ns, "cmd", podPhase("Succeeded", "Failed"));
    const out = await logs(ns, "cmd");
    assert.equal(p.status.phase, "Succeeded", out);
    assert.match(out, /^arg first arg$/m);
    assert.match(out, /^arg second$/m);
    assert.match(out, /^env GREETING=hello component$/m);
    assert.match(out, /^file \/config\/file: from a volume$/m);
  });
});

// FS2: runwasi gives "I/O error (os error 29)" for a write to a read-only
// volume (the step wasi-command of the Docker check).
test("FS2: a write to a read-only volume fails as on runwasi", async (t) => {
  const ns = await namespace(t);
  const node = await nodeName("tab");
  await apply(cmdPod("ro", node, {
    env: [{ name: "WRITE", value: "/ro/x" }],
    mounts: [{ name: "ro", mountPath: "/ro", readOnly: true }],
    volumes: [{ name: "ro", emptyDir: {} }],
  }), ns);
  const p = await waitForPod(ns, "ro", podPhase("Succeeded", "Failed"));
  const out = await logs(ns, "ro");
  assert.equal(p.status.phase, "Succeeded", out);
  assert.match(out, /^write \/ro\/x: Err\("I\/O error \(os error 29\)"\)$/m);
});

// FS3: the writes stay in the pod worker and end with it, as for a module.
// The component writes the file and exits with 1; after the restart, the file
// is not there. On runwasi the volume keeps the write (section 11).
test("FS3: a restart does not show the writes of the run before it", async (t) => {
  const ns = await namespace(t);
  const node = await nodeName("tab");
  await apply(cmdPod("rw", node, {
    restartPolicy: "OnFailure",
    env: [{ name: "WRITE", value: "/rw/x" }, { name: "EXIT_CODE", value: "1" }],
    mounts: [{ name: "rw", mountPath: "/rw" }],
    volumes: [{ name: "rw", emptyDir: {} }],
  }), ns);
  await waitForPod(ns, "rw", (p) => (p.status?.containerStatuses?.[0]?.restartCount ?? 0) >= 1, { timeout: 240 });
  // The log of the second run: wait until it has the write.
  const out = await waitFor(async () => {
    const o = await logs(ns, "rw").catch(() => "");
    return /^write \/rw\/x: /m.test(o) && o;
  }, { what: "the log of the second run" });
  const previous = await kubectl(`logs -n ${ns} rw --previous`);
  for (const o of [previous, out]) {
    assert.match(o, /^old \/rw\/x: error .*No such file or directory/m, o);
    assert.match(o, /^write \/rw\/x: Ok\(\(\)\)$/m, o);
  }
});

// FS4: the copy of more than 64 MiB fails with the same text for a module
// and for a component. A shell pod writes 65 MiB into a hostPath directory,
// and the two WebAssembly pods mount it.
test("FS4: a volume of 65 MiB gives the same text for a component and for a module", async (t) => {
  const ns = await namespace(t);
  const node = await nodeName("tab");
  const dir = `/tmp/e2e-fs4-${ns}`;
  const volumes = [{ name: "big", hostPath: { path: dir, type: "DirectoryOrCreate" } }];
  const mounts = [{ name: "big", mountPath: "/big" }];
  await apply({
    apiVersion: "v1",
    kind: "Pod",
    metadata: { name: "writer" },
    spec: {
      nodeSelector: onNode(node),
      restartPolicy: "Never",
      containers: [{ name: "c", image: "busybox", command: ["sh", "-c", "head -c 68157440 /dev/zero > /big/file && echo done && sleep 3600"], volumeMounts: mounts }],
      volumes,
    },
  }, ns);
  try {
    await waitFor(async () => /done/.test(await logs(ns, "writer").catch(() => "")), { what: "the file of 65 MiB" });
    await apply([
      cmdPod("component", node, { mounts, volumes }),
      cmdPod("module", node, { mounts, volumes, image: "ghcr.io/se-wo/tabernetes/wasi-hello:latest" }),
    ], ns);
    const ends = {};
    for (const name of ["component", "module"]) {
      ends[name] = await waitFor(async () => (await pod(ns, name)).status?.containerStatuses?.[0]?.state?.terminated, { what: `the end of ${name}` });
      assert.match(ends[name].message ?? "", /the volumes of the container are larger than 64 MiB, too large to copy into the worker/, name);
    }
    assert.equal(ends.component.message, ends.module.message);
    assert.equal(ends.component.reason, ends.module.reason);
  } finally {
    await kubectl(`exec -n ${ns} writer -- rm -f /big/file`).catch(() => {});
  }
});
