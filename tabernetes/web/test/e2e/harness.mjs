// harness.mjs: helpers for the end-to-end tests of the page. The tests run
// with node:test under run.mjs, which starts the page in headless Chromium
// (docs/docker-node-check/browser.mjs). Each helper runs command lines in the
// shell of the tab with docs/docker-node-check/tab.mjs. See README.md.
//
// Env: CDP_PORT (default 9222), E2E_NODES (default "tab,worker"): the nodes
// that forEachNode gives.
import { execFile } from "node:child_process";
import { randomBytes } from "node:crypto";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const tabScript = path.resolve(here, "../../../docs/docker-node-check/tab.mjs");

// The shell of the tab runs one command line at a time: all calls of this
// process go through one queue.
let queue = Promise.resolve();

// tab runs `node tab.mjs <args>` and resolves with {out, rc}. It rejects only
// if tab.mjs cannot connect to the page.
export function tab(args, { timeout = 120 } = {}) {
  const run = () => new Promise((resolve, reject) => {
    execFile(process.execPath, [tabScript, ...args], {
      env: { ...process.env, TIMEOUT: String(timeout) },
      maxBuffer: 64 << 20,
      timeout: (timeout + 60) * 1000,
    }, (err, stdout, stderr) => {
      const rc = err ? (typeof err.code === "number" ? err.code : 1) : 0;
      // 2: tab.mjs found no page with a ready terminal.
      if (err && (rc === 2 || err.killed)) reject(new Error(`tab.mjs ${args[0]}: ${stderr || err.message}`));
      else resolve({ out: stdout + stderr, rc });
    });
  });
  const p = queue.then(run, run);
  queue = p.catch(() => {});
  return p;
}

// run runs a command line in the shell of the tab: {out, rc}.
export const run = (line, opts) => tab(["run", line], opts);

// sh runs a command line and resolves with its output. It rejects if the
// command line does not exit with 0.
export async function sh(line, opts) {
  const { out, rc } = await run(line, opts);
  if (rc !== 0) throw new Error(`$ ${line}\nexit status ${rc}\n${out}`);
  return out;
}

// kubectl runs kubectl with a string of arguments and resolves with its
// output.
export const kubectl = (args, opts) => sh(`kubectl ${args}`, opts);

// getJSON resolves with the parsed output of `kubectl get <args> -o json`.
export async function getJSON(args, opts) {
  return JSON.parse(await kubectl(`get ${args} -o json`, opts));
}

// apply applies a manifest, in namespace ns if given. The manifest is YAML
// or JSON text, an object or a list of objects.
export function apply(manifest, ns) {
  if (typeof manifest !== "string") {
    manifest = JSON.stringify(Array.isArray(manifest) ? { apiVersion: "v1", kind: "List", items: manifest } : manifest, null, 1);
  }
  if (manifest.includes("\nEOF\n")) throw new Error("apply: the manifest contains a line EOF");
  return kubectl(`apply${ns ? ` -n ${ns}` : ""} -f - <<'EOF'\n${manifest.trimEnd()}\nEOF`);
}

// waitFor calls fn until it resolves with a truthy value and resolves with
// that value. Errors of fn count as "not yet". After timeout seconds it
// rejects with the last value or error and with what describe gives.
export async function waitFor(fn, { timeout = 180, interval = 2, what = "the condition" } = {}) {
  const end = Date.now() + timeout * 1000;
  let last;
  for (;;) {
    try {
      const v = await fn();
      if (v) return v;
      last = v;
    } catch (e) {
      last = e;
    }
    if (Date.now() >= end) {
      const why = last instanceof Error ? last.message : JSON.stringify(last);
      throw new Error(`waited ${timeout} s for ${what}; last: ${why}`);
    }
    await new Promise((r) => setTimeout(r, interval * 1000));
  }
}

// pod resolves with the Pod object.
export const pod = (ns, name) => getJSON(`pod -n ${ns} ${name}`);

// waitForPod waits until test(pod) is true and resolves with the pod. The
// default test is "the pod has ended or is Ready".
export async function waitForPod(ns, name, test = podDoneOrReady, { timeout = 180 } = {}) {
  let last = null;
  try {
    return await waitFor(async () => {
      last = await pod(ns, name);
      return test(last) && last;
    }, { timeout, what: `pod ${ns}/${name}` });
  } catch (e) {
    const status = last ? JSON.stringify(last.status) : "(no pod)";
    const events = await kubectl(`get events -n ${ns} --field-selector involvedObject.name=${name}`).catch((x) => x.message);
    throw new Error(`${e.message}\nstatus: ${status}\nevents:\n${events}`);
  }
}

// Tests for waitForPod.
export const podPhase = (...phases) => (p) => phases.includes(p.status?.phase);
export const podReady = (p) => !!p.status?.conditions?.some((c) => c.type === "Ready" && c.status === "True");
export const podDoneOrReady = (p) => podPhase("Succeeded", "Failed")(p) || podReady(p);

// logs resolves with the logs of a container of a pod.
export const logs = (ns, name, container) =>
  kubectl(`logs -n ${ns} ${name}${container ? ` -c ${container}` : ""}`);

// namespace creates a namespace with a random name for the test t and
// deletes it after the test. Pass E2E_KEEP=1 to keep it for a look.
export async function namespace(t, prefix = "e2e") {
  const ns = `${prefix}-${randomBytes(3).toString("hex")}`;
  await kubectl(`create namespace ${ns}`);
  if (!process.env.E2E_KEEP) t.after(() => kubectl(`delete namespace ${ns} --wait=false`).catch(() => {}));
  return ns;
}

// The name of the tab node of the page.
export const TAB_NODE = "tab";

// worker resolves with the name of a node worker (addnode) that is Ready.
// It starts one if the page has none. All tests share it.
let workerName = null;
export async function worker() {
  if (workerName) return workerName;
  const ready = (n) => n.status?.conditions?.some((c) => c.type === "Ready" && c.status === "True");
  const workers = async () => (await getJSON("nodes")).items.filter((n) => n.metadata.name.startsWith("tab-worker-"));
  if (!(await workers()).length) await sh("addnode");
  workerName = await waitFor(async () => (await workers()).find(ready)?.metadata.name,
    { timeout: 300, what: "a Ready node worker" });
  return workerName;
}

// nodeName resolves "tab" or "worker" to the name of the node.
export const nodeName = (kind) => (kind === "worker" ? worker() : Promise.resolve(TAB_NODE));

// NODES are the kinds of nodes that the tests run on: "tab" and "worker"
// (the column "Nodes" of docs/wasi-sockets.md, section 10.3).
export const NODES = (process.env.E2E_NODES || "tab,worker").split(",").map((s) => s.trim()).filter(Boolean);

// forEachNode(kinds, fn) gives fn each kind of kinds that is in NODES.
// Use it to make one test for each node:
//
//   forEachNode(["tab", "worker"], (kind) => test(`... on ${kind}`, ...));
export function forEachNode(kinds, fn) {
  for (const kind of kinds) if (NODES.includes(kind)) fn(kind);
}

// onNode returns the nodeSelector of a pod spec that puts the pod on a node.
export const onNode = (node) => ({ "kubernetes.io/hostname": node });
