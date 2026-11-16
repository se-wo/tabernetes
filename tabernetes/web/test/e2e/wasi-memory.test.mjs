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
// The tests of package W2.4 check the broker of the page (web/src/memory.js,
// section 8.6): the state of a node worker without memory, a load with asks
// that wait at the broker, and collect.
//
// The test file runs in the default run of run.mjs. It takes approximately
// 6 minutes. To run it alone:
//
//   node web/test/e2e/run.mjs web/test/e2e/wasi-memory.test.mjs
//
// The clients of this file use wget -T, so each request has a time limit.
// When a pod is deleted, its background processes stop with it
// (pod-jobs.test.mjs).
import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import { createRequire } from "node:module";
import { evalOn, closeDevtools } from "./devtools.mjs";
import { apply, kubectl, logs, NODES, pod, podPhase, sh, TAB_NODE, waitFor, waitForPod, worker } from "./harness.mjs";

const { chromium } = createRequire(import.meta.url)("playwright");

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const image = (name) => `ghcr.io/se-wo/tabernetes/${name}:latest`;

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
// stats gives the count of the broker of the page (memory.js).
const stats = () => evalOn(TAB_NODE, () => globalThis.tabernetesWasmMemories.stats());
// statusLine gives the text of the status line under the terminal.
const statusLine = () => evalOn(TAB_NODE, () => document.getElementById("tabernetes-status")?.textContent ?? "");

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

// Each test has its own namespace. t.after deletes it and waits for the end
// of its pods, also when the test fails: a deleted pod stops its pod worker,
// and that frees all memories of the pod.
async function testNamespace(t) {
  const ns = `e2e-mem-${randomBytes(3).toString("hex")}`;
  await kubectl(`create namespace ${ns}`);
  t.after(() => process.env.E2E_KEEP ? undefined : kubectl(`delete namespace ${ns} --wait=true`, { timeout: 300 }).catch(() => {}));
  return ns;
}

const runningIP = (ns, name) => waitFor(async () => {
  const p = await pod(ns, name);
  return p.status?.containerStatuses?.[0]?.state?.running && p.status.podIP;
}, { what: `the start of ${name}` });
const terminated = async (ns, name) => (await waitForPod(ns, name, podPhase("Succeeded", "Failed"))).status.containerStatuses[0].state.terminated;
const del = (ns, ...names) => kubectl(`delete pod -n ${ns} ${names.join(" ")} --grace-period=0 --force --wait=true`, { timeout: 180 });

// waitFree waits until the probe is at least n.
const waitFree = (n, what) => waitFor(async () => {
  const f = await probe();
  return f >= n ? f : false;
}, { timeout: 120, interval: 2, what });

// hold starts a worker of the page that takes each Wasm memory that becomes
// free, every 100 ms, and keeps it. So the tab stays full while the checks of
// a full tab run. release stops the worker and frees its memories.
const hold = () => evalOn(TAB_NODE, () => {
  const src = "const a = []; setInterval(() => { try { for (;;) a.push(new WebAssembly.Memory({ initial: 1 })); } catch {} }, 100);";
  globalThis.tabernetesMemoryHold = new Worker(URL.createObjectURL(new Blob([src], { type: "text/javascript" })));
  return true;
});
const release = () => evalOn(TAB_NODE, () => {
  globalThis.tabernetesMemoryHold?.terminate();
  globalThis.tabernetesMemoryHold = null;
  return true;
});

// lateWaits checks that the request of late-client waits (PX34) while late
// has no Wasm memory. The hold takes a free memory only every 100 ms. On the
// full tab, the collect rounds of the broker (W2.4: after each memory-full of
// the parked containers, and while their asks wait) make the pod workers
// collect their garbage, so memories become free and are taken again all the
// time. The retry of late (every second) can take such a memory before the
// hold: section 8.6 allows this, other pods of the tab can free memory. One
// memory is enough: the first instance gets the memory of the link check at
// the garbage collection of the pod worker. So a served request must come
// after the line of late that its instance got its memory. It gives true if
// late got a memory during the hold.
async function lateWaits(ns, t) {
  if (!/served/.test(await logs(ns, "late-client"))) return false;
  // The two logs have no common order: wait a short time for the line of late.
  await waitFor(async () => /^tabernetes: a new instance got its Wasm memory after \d+\.\d s$/m.test(await logs(ns, "late")), {
    timeout: 30,
    interval: 1,
    what: "a served request of late only after its instance got its memory",
  });
  t.diagnostic("late got a Wasm memory that a pod worker freed before the hold took it; its request was served");
  return true;
}

// quiet waits until no namespace of the cluster is in deletion. The other
// test files delete their namespaces without a wait: until their pods end,
// their pod workers free memories during the measurement.
const quiet = () => waitFor(async () => {
  const items = JSON.parse(await kubectl("get namespaces -o json")).items;
  return !items.some((n) => n.status?.phase === "Terminating");
}, { timeout: 300, interval: 2, what: "the end of the namespaces in deletion" });

// usedBy gives the memories of the pods in names: the probe while they run,
// and the probe after their deletion. The deletion stops their pod workers,
// so their garbage also goes. Two probes a short time apart do not depend on
// the garbage of other pod workers, which can go at any time.
async function usedBy(ns, names) {
  const running = await settled();
  await del(ns, ...names);
  await sleep(1000);
  const after = await settled();
  return after - running;
}

before(async () => {
  await quiet();
  const ns = `e2e-mem-${randomBytes(3).toString("hex")}`;
  await kubectl(`create namespace ${ns}`);
  try {
    // The first component of the tab node starts the transpile worker of the
    // Go thread: a component that ends at once.
    await apply(component("first", "wasi-test-core", TAB_NODE, { command: ["/wasi-test-core.wasm#go"] }), ns);
    await terminated(ns, "first");
  } finally {
    await kubectl(`delete namespace ${ns} --wait=true`, { timeout: 300 }).catch(() => {});
  }
});

after(() => closeDevtools());

test("the Wasm memories of one tab, and the memories of each kind of user", async (t) => {
  const ns = await testNamespace(t);
  const limit = await total();
  const start = await settled();
  t.diagnostic(`an empty renderer process: ${limit} Wasm memories`);
  t.diagnostic(`the tab with its nodes, before the tests: ${start} free, ${limit - start} used`);
  assert.ok(limit >= 32, `limit ${limit}`);
  // The Go binary of the tab node uses at least one.
  assert.ok(start < limit, `free ${start} of ${limit}`);

  // A command component that waits for stdin: one memory.
  await apply(component("cmd", "wasi-test-cmd", TAB_NODE, { stdin: true, env: [{ name: "STDIN", value: "1" }] }), ns);
  await runningIP(ns, "cmd");
  await sleep(2000);
  const cmd = await usedBy(ns, ["cmd"]);
  t.diagnostic(`a command component that runs: ${cmd}`);
  assert.equal(cmd, 1);

  // A WASI preview 1 module: the module and the shared memory of its sleeps.
  await apply(component("module", "wasi-hello", TAB_NODE, { args: ["--ticks=-1"] }), ns);
  await runningIP(ns, "module");
  await sleep(2000);
  const mod = await usedBy(ns, ["module"]);
  t.diagnostic(`a module that runs: ${mod}`);
  assert.equal(mod, 2);

  // A proxy component without requests: the instance of the link check, until
  // the garbage collection of its pod worker (0 or 1).
  await apply(proxy("idle"), ns);
  await runningIP(ns, "idle");
  await sleep(2000);
  const idle = await usedBy(ns, ["idle"]);
  t.diagnostic(`a proxy container without requests: ${idle}`);
  assert.ok(idle >= 0 && idle <= 1, `idle ${idle}`);

  // Each active instance: one memory. The container then uses 4, and 1 more
  // while the instance of its link check is not collected.
  const before4 = await settled();
  await apply(proxy("active"), ns);
  const ip = await runningIP(ns, "active");
  await apply(client("parker", park(ip, 4, 120)), ns);
  await runningIP(ns, "parker");
  // The 4 requests start in a few seconds. This wait only gives them time:
  // the assertion is on the result of usedBy.
  await waitFor(async () => before4 - (await probe()) >= 4, { interval: 1, timeout: 60, what: "4 active instances" }).catch(() => {});
  await sleep(2000);
  const active = await usedBy(ns, ["active"]);
  t.diagnostic(`a proxy container with 4 active instances: ${active}`);
  assert.ok(active >= 4 && active <= 5, `active ${active}`);
  await del(ns, "parker");

  // A pod on a node worker uses the limit of the same renderer process.
  if (NODES.includes("worker")) {
    const node = await worker();
    await apply(component("remote", "wasi-hello", node, { args: ["--ticks=-1"] }), ns);
    await runningIP(ns, "remote");
    await sleep(2000);
    const remote = await usedBy(ns, ["remote"]);
    t.diagnostic(`a module on the node worker ${node}, seen from the page: ${remote}`);
    assert.equal(remote, 2);
  }
});

test("a full tab: requests wait (PX34), a new proxy container waits with a line in its log, other pods fail with a clear status", async (t) => {
  const ns = await testNamespace(t);
  await quiet();
  const free = await settled();
  // 8 proxy containers with a maximum of 16 instances each (PX9) are more
  // than the tab can make.
  const names = Array.from({ length: 8 }, (_, i) => `p${i}`);
  await apply(names.map(proxy), ns);
  const ips = [];
  for (const n of names) ips.push(await runningIP(ns, n));
  const began = Date.now();
  // wget closes the parked requests after this time. The checks of the full
  // tab must end before it, also on a slow runner.
  const TIMEOUT = 150;
  await apply(client("parker", ips.map((ip) => park(ip, 16, TIMEOUT)).join("\n")), ns);
  const full = await waitFor(async () => (await probe()) === 0 && Date.now(), { interval: 1, what: "a full tab" });
  t.diagnostic(`8 proxy containers with 16 requests each: the tab is full after ${((full - began) / 1000).toFixed(1)} s (${free} were free)`);
  // Pod workers free some memories at their garbage collection. The hold
  // takes them, so that each check below sees a full tab.
  t.after(() => release().catch(() => {}));
  await hold();

  // A new proxy container: it starts, writes a line into its log, and its
  // requests wait.
  await apply(proxy("late"), ns);
  const lateIP = await runningIP(ns, "late");
  await apply(client("late-client", `wget -q -O /dev/null http://${lateIP}:8080/context && echo served $(date +%s)`), ns);
  await waitFor(async () => /no Wasm memory left for a new instance; .*\(PX34\)/.test(await logs(ns, "late")), { what: "the line of PX34 in the log of late" });
  let lateServed = await lateWaits(ns, t);

  // A command component fails with exit code 137 and the text of the full tab.
  await apply(component("cmd", "wasi-test-cmd", TAB_NODE), ns);
  const cmd = await terminated(ns, "cmd");
  const cmdLog = await logs(ns, "cmd");
  assert.equal(cmd.exitCode, 137, cmdLog);
  assert.match(cmdLog, /^the component cannot instantiate: the browser tab has no Wasm memory left for a new instance/m);

  // A WASI preview 1 module fails with exit code 128 and the text of Chrome.
  await apply(component("module", "wasi-hello", TAB_NODE), ns);
  const mod = await terminated(ns, "module");
  const modLog = await logs(ns, "module");
  assert.equal(mod.exitCode, 128, modLog);
  assert.match(modLog, /^the module cannot instantiate: the browser tab has no Wasm memory left for a new instance \(.*Out of memory/m);

  // A new node worker: its Go binary gets no memory. The status line shows
  // the state (W2.4, item 4), and rmnode removes it.
  await sh("addnode");
  const noMemory = await waitFor(async () => /(tab-worker-\d+) no Wasm memory/.exec(await statusLine())?.[1], { timeout: 60, interval: 1, what: "the state no Wasm memory of the new node worker" });
  t.diagnostic(`a new node worker on the full tab: ${noMemory} no Wasm memory`);
  await sh(`rmnode --force ${noMemory}`);
  const checks = (Date.now() - began) / 1000;
  t.diagnostic(`the checks of the full tab ended ${checks.toFixed(1)} s after the start of the parked requests`);
  assert.ok(checks < TIMEOUT, "the checks of the full tab ended before the parked requests");
  lateServed = lateServed || (await lateWaits(ns, t));

  // The hold ends, and then the parked requests: the waiting request of late
  // gets an instance.
  await release();
  const released = Date.now();
  const served = await waitFor(async () => /served/.test(await logs(ns, "late-client")) && Date.now(), { timeout: TIMEOUT + 180, what: "the request of late" });
  // The time of the wait (section 14) only applies to a request that waited
  // until the end of the hold.
  if (!lateServed) t.diagnostic(`the request of the new container: served ${((served - released) / 1000).toFixed(1)} s after the end of the hold, ${((served - began) / 1000).toFixed(1)} s after the start of the parked requests`);
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

test("W2.4: under a load, asks wait at the broker, and each request still gets its response", async (t) => {
  const ns = await testNamespace(t);
  await quiet();
  const names = Array.from({ length: 8 }, (_, i) => `l${i}`);
  await apply(names.map(proxy), ns);
  const ips = [];
  for (const n of names) ips.push(await runningIP(ns, n));
  const dropsBefore = (await stats()).drops;
  // 16 clients for each container, each with 10 requests of 1 second in a
  // sequence: more instances than the tab can make.
  const loops = ips.flatMap((ip) => Array.from({ length: 16 }, () =>
    `(n=0; for j in $(seq 10); do wget -T 60 -q -O /dev/null http://${ip}:8080/wait/1000 || n=$((n+1)); done; echo "failed $n") &`));
  await apply(client("loader", `${loops.join("\n")}\nwait\necho all-done`), ns);
  const began = Date.now();
  // The load reaches the reserve: asks wait at the broker.
  const busy = await waitFor(async () => {
    const s = await stats();
    return s.waiting > 0 && s;
  }, { interval: 0.5, timeout: 60, what: "asks that wait at the broker" });
  t.diagnostic(`the load: after ${((Date.now() - began) / 1000).toFixed(1)} s, the broker counts ${busy.used} of ${busy.limit}, ${busy.waiting} asks wait`);
  // A command component during the load. The reserve keeps memories for it
  // only while the asks wait less than 2 seconds, and the memories of ended
  // instances are not in the count (section 8.6): record the result.
  await apply(component("cmd", "wasi-test-core", TAB_NODE, { command: ["/wasi-test-core.wasm#go"] }), ns);
  const cmd = await terminated(ns, "cmd");
  t.diagnostic(`a command component during the load: exit code ${cmd.exitCode} after ${((Date.now() - began) / 1000).toFixed(1)} s; the broker: ${JSON.stringify(await stats())}`);
  // No request fails: an instance without a grant tries anyway (PX34).
  const out = await waitFor(async () => {
    const l = await logs(ns, "loader");
    return /all-done/.test(l) && l;
  }, { timeout: 300, interval: 2, what: "the end of the load" });
  t.diagnostic(`the load of 1,280 requests ended after ${((Date.now() - began) / 1000).toFixed(1)} s`);
  assert.equal((out.match(/^failed 0$/gm) ?? []).length, 128, out);
  const end = await stats();
  // The pod workers keep their asks and grants in the limits of the broker.
  assert.equal(end.drops - dropsBefore, 0, "the broker dropped messages of the pod workers");
});

test("W2.4: collect frees the memories of ended instances on a full tab, not on a tab with free memories", async (t) => {
  const ns = await testNamespace(t);
  await quiet();
  await apply(proxy("seq"), ns);
  const ip = await runningIP(ns, "seq");
  const empty = await settled();
  // 60 requests in a sequence leave the memories of ended instances.
  await apply(client("seq-client", `for i in $(seq 60); do wget -T 10 -q -O /dev/null http://${ip}:8080/context; done; echo done`), ns);
  await waitFor(async () => /done/.test(await logs(ns, "seq-client")), { what: "60 requests" });
  await sleep(1000);
  const garbage = await settled();
  // collect on a tab with free memories: the one new memory of each pod
  // worker does not fail, so its garbage stays.
  await evalOn(TAB_NODE, () => globalThis.tabernetesWasmMemories.collect());
  await sleep(500);
  const notFull = await settled();
  // collect on a full tab: the new memory fails, and the pod worker collects
  // its garbage first.
  await hold();
  await sleep(300);
  const t0 = Date.now();
  await evalOn(TAB_NODE, () => globalThis.tabernetesWasmMemories.collect());
  await sleep(500);
  await release();
  const full = await settled();
  t.diagnostic(`free memories: ${empty} before the requests, ${garbage} after 60 requests, ${notFull} after collect on a tab with free memories, ${full} after collect on a full tab (${Date.now() - t0} ms)`);
  assert.ok(full > notFull || garbage >= empty - 1, `collect on a full tab freed nothing: ${garbage} -> ${notFull} -> ${full}`);
});
