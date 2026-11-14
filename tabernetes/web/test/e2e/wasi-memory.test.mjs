// The Wasm memories of one browser tab (docs/wasi-sockets.md, open question 2
// of section 14, PX9, PX34, and the rules of section 8.6).
//
// Chrome reserves address space for each Wasm memory, and one renderer
// process has a limit for this space: approximately 125 memories (section
// 9.2, item 15). All Go threads (the tab node and the node workers) and all
// their pod workers are workers of the page, so they share this limit.
//
// probe() counts the memories that the tab can still make: a worker of the
// page makes Wasm memories until the browser refuses one, and then stops.
// For this short time the tab is full, so the probe runs only while no pod
// starts. The tests write the numbers with t.diagnostic.
//
// The test file runs in the default run of run.mjs. It takes approximately
// 2 minutes. To run it alone:
//
//   node web/test/e2e/run.mjs web/test/e2e/wasi-memory.test.mjs
//
// Note: the background processes of a busybox pod on the tab node do not stop
// when the pod is deleted. So the clients of this file use wget -T to close
// their connections.
import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { evalOn, closeDevtools } from "./devtools.mjs";
import { apply, kubectl, logs, NODES, pod, podPhase, run, TAB_NODE, waitFor, waitForPod, worker } from "./harness.mjs";

const { chromium } = createRequire(import.meta.url)("playwright");

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const image = (name) => `ghcr.io/se-wo/tabernetes/${name}:latest`;
const ns = `e2e-mem-${Date.now().toString(36)}`;

// probe gives the number of Wasm memories that the tab can still make.
const PROBE = () => new Promise((resolve) => {
  const src = "const a = []; let n = 0; try { for (; n < 100000; n++) a.push(new WebAssembly.Memory({ initial: 1 })); } catch {} postMessage(n);";
  const w = new Worker(URL.createObjectURL(new Blob([src], { type: "text/javascript" })));
  w.onmessage = ({ data }) => {
    w.terminate();
    resolve(data);
  };
});
const probe = () => evalOn(TAB_NODE, PROBE);

// settled gives the probe when two probes, 1 second apart, agree.
async function settled() {
  let last = -1;
  for (let i = 0; i < 10; i++) {
    const n = await probe();
    if (n === last) return n;
    last = n;
    await sleep(1000);
  }
  return last;
}

// total gives the limit of an empty renderer process: the probe in a page of
// a new browser context.
async function total() {
  const browser = await chromium.connectOverCDP(`http://127.0.0.1:${process.env.CDP_PORT || "9222"}`);
  try {
    const ctx = await browser.newContext();
    try {
      const page = await ctx.newPage();
      await page.goto("about:blank");
      return await page.evaluate(PROBE);
    } finally {
      await ctx.close();
    }
  } finally {
    await browser.close();
  }
}

const component = (name, img, node, extra = {}, restartPolicy = "Never") => ({
  apiVersion: "v1",
  kind: "Pod",
  metadata: { name },
  spec: {
    runtimeClassName: "wasmtime",
    restartPolicy,
    nodeSelector: { "kubernetes.io/hostname": node },
    containers: [{ name: "c", image: image(img), ...extra }],
  },
});
const proxy = (name) => component(name, "wasi-test-proxy", TAB_NODE, {}, "Always");

// client is a busybox pod on the tab node that runs a script and then waits.
const client = (name, script) => ({
  apiVersion: "v1",
  kind: "Pod",
  metadata: { name },
  spec: {
    nodeSelector: { "kubernetes.io/hostname": TAB_NODE },
    containers: [{ name: "c", image: "busybox", command: ["sh", "-c", `${script}\nsleep 36000`] }],
  },
});
// park gives the lines of n requests to ip that wait for one hour, or until
// wget stops them after timeout seconds.
const park = (ip, n, timeout) => Array.from({ length: n }, () => `wget -T ${timeout} -q -O /dev/null http://${ip}:8080/wait/3600000 &`).join("\n");

const runningIP = (name) => waitFor(async () => {
  const p = await pod(ns, name);
  return p.status?.containerStatuses?.[0]?.state?.running && p.status.podIP;
}, { what: `the start of ${name}` });
const terminated = async (name) => (await waitForPod(ns, name, podPhase("Succeeded", "Failed"))).status.containerStatuses[0].state.terminated;
const del = (...names) => kubectl(`delete pod -n ${ns} ${names.join(" ")} --grace-period=0 --force --wait=true`, { timeout: 180 });

// waitFree waits until the probe is at least n.
const waitFree = (n, what) => waitFor(async () => {
  const f = await probe();
  return f >= n ? f : false;
}, { timeout: 120, interval: 2, what });

let start = 0; // the probe before the tests

before(async () => {
  await kubectl(`create namespace ${ns}`);
  // The first component of the tab node starts the transpile worker of the
  // Go thread: a component that ends at once.
  await apply(component("first", "wasi-test-core", TAB_NODE, { command: ["/wasi-test-core.wasm#go"] }), ns);
  await terminated("first");
  await del("first");
  start = await settled();
});

after(async () => {
  if (!process.env.E2E_KEEP) await kubectl(`delete namespace ${ns} --wait=true`, { timeout: 300 }).catch(() => {});
  await closeDevtools();
});

test("the Wasm memories of one tab, and the memories of each kind of user", async (t) => {
  const limit = await total();
  t.diagnostic(`an empty renderer process: ${limit} Wasm memories`);
  t.diagnostic(`the tab with its nodes, before the tests: ${start} free, ${limit - start} used`);
  assert.ok(limit >= 32, `limit ${limit}`);
  // The Go binary of the tab node uses at least one.
  assert.ok(start < limit, `free ${start} of ${limit}`);

  // A command component that waits for stdin: one memory.
  await apply(component("cmd", "wasi-test-cmd", TAB_NODE, { stdin: true, env: [{ name: "STDIN", value: "1" }] }), ns);
  await waitFor(async () => (await pod(ns, "cmd")).status?.containerStatuses?.[0]?.state?.running, { what: "cmd" });
  await sleep(2000);
  const cmd = start - (await settled());
  t.diagnostic(`a command component that runs: ${cmd}`);
  assert.equal(cmd, 1);
  await del("cmd");
  await waitFree(start, "the memory of cmd after its deletion");

  // A WASI preview 1 module: the module and the shared memory of its sleeps.
  await apply(component("module", "wasi-hello", TAB_NODE, { args: ["--ticks=-1"] }), ns);
  await waitFor(async () => (await pod(ns, "module")).status?.containerStatuses?.[0]?.state?.running, { what: "module" });
  await sleep(2000);
  const mod = start - (await settled());
  t.diagnostic(`a module that runs: ${mod}`);
  assert.equal(mod, 2);
  await del("module");
  await waitFree(start, "the memories of the module after its deletion");

  // A proxy component without requests: the instance of the link check, until
  // the garbage collection of its pod worker.
  await apply(proxy("idle"), ns);
  const ip = await runningIP("idle");
  await sleep(2000);
  const idle = start - (await settled());
  t.diagnostic(`a proxy container without requests: ${idle}`);
  assert.ok(idle <= 1, `idle ${idle}`);

  // Each active instance: one memory.
  await apply(client("parker", park(ip, 4, 600)), ns);
  const four = await waitFor(async () => {
    const used = start - (await probe());
    return used - idle >= 4 ? used : false;
  }, { interval: 1, what: "4 active instances" });
  await sleep(2000);
  const active = start - (await settled()) - idle;
  t.diagnostic(`4 active instances of a proxy component: ${active} (first ${four - idle})`);
  assert.ok(active >= 4 && active <= 5, `active ${active}`);
  await kubectl(`delete namespace ${ns} --wait=true`, { timeout: 300 });
  await kubectl(`create namespace ${ns}`);
  await waitFree(start, "the memories of the proxy container after its deletion");

  // A pod on a node worker uses the limit of the same renderer process.
  if (NODES.includes("worker")) {
    const node = await worker();
    await apply(component("remote", "wasi-hello", node, { args: ["--ticks=-1"] }), ns);
    await waitFor(async () => (await pod(ns, "remote")).status?.containerStatuses?.[0]?.state?.running, { what: "remote" });
    await sleep(2000);
    // The first component of the node worker can also start its transpile
    // worker: 2 or 3.
    const remote = start - (await settled());
    t.diagnostic(`a module on the node worker ${node}, seen from the page: ${remote}`);
    assert.ok(remote >= 2, `remote ${remote}`);
    await del("remote");
  }
});

test("a full tab: requests wait (PX34), a new proxy container waits with a line in its log, other pods fail with a clear status", async (t) => {
  const free = await settled();
  // 8 proxy containers with a maximum of 16 instances each (PX9) are more
  // than the tab can make.
  const names = Array.from({ length: 8 }, (_, i) => `p${i}`);
  await apply(names.map(proxy), ns);
  const ips = [];
  for (const n of names) ips.push(await runningIP(n));
  const began = Date.now();
  const TIMEOUT = 60; // wget closes the parked requests after this time
  await apply(client("parker", ips.map((ip) => park(ip, 16, TIMEOUT)).join("\n")), ns);
  const full = await waitFor(async () => (await probe()) === 0 && Date.now(), { interval: 1, what: "a full tab" });
  t.diagnostic(`8 proxy containers with 16 requests each: the tab is full after ${((full - began) / 1000).toFixed(1)} s (${free} were free)`);

  // A new proxy container: it starts, writes a line into its log, and its
  // requests wait.
  await apply(proxy("late"), ns);
  const lateIP = await runningIP("late");
  await apply(client("late-client", `wget -q -O /dev/null http://${lateIP}:8080/context && echo served $(date +%s)`), ns);
  await waitFor(async () => /no Wasm memory left for a new instance; .*\(PX34\)/.test(await logs(ns, "late")), { what: "the line of PX34 in the log of late" });
  assert.doesNotMatch(await logs(ns, "late-client"), /served/);

  // A command component fails with exit code 137 and the text of the full tab.
  await apply(component("cmd", "wasi-test-cmd", TAB_NODE), ns);
  const cmd = await terminated("cmd");
  const cmdLog = await logs(ns, "cmd");
  assert.equal(cmd.exitCode, 137, cmdLog);
  assert.match(cmdLog, /^the component cannot instantiate: the browser tab has no Wasm memory left for a new instance/m);

  // A WASI preview 1 module fails with exit code 128 and the text of Chrome.
  await apply(component("module", "wasi-hello", TAB_NODE), ns);
  const mod = await terminated("module");
  const modLog = await logs(ns, "module");
  assert.equal(mod.exitCode, 128, modLog);
  assert.match(modLog, /Out of memory/);
  assert.ok(Date.now() - began < TIMEOUT * 1000, "the checks of the full tab ended before the parked requests");

  // The parked requests end: the waiting request of late gets an instance.
  const served = await waitFor(async () => /served/.test(await logs(ns, "late-client")) && Date.now(), { timeout: 180, what: "the request of late" });
  t.diagnostic(`the request of the new container: served ${((served - began) / 1000 - TIMEOUT).toFixed(1)} s after the end of the parked requests`);
  assert.match(await logs(ns, "late"), /^tabernetes: a new instance got its Wasm memory after \d+\.\d s$/m);

  // The memories of the ended instances stay until the garbage collection of
  // each pod worker. Record how many the tab can make after the load.
  for (const s of [5, 30]) {
    await sleep(Math.max(0, began + (TIMEOUT + s) * 1000 - Date.now()));
    t.diagnostic(`${s} s after the end of the parked requests: ${await probe()} free`);
  }
  // The deletion of the containers stops their pod workers and frees all.
  await kubectl(`delete namespace ${ns} --wait=true`, { timeout: 300 });
  const back = await waitFree(free, "the memories after the deletion of the containers");
  t.diagnostic(`after the deletion of the containers: ${back} free`);
});
