// measure.mjs boots the Tabernetes page in headless Chromium again and again
// and measures the time to the shell prompt, phase by phase, and the first
// pod after it. docs/boot-time.md has the method and the results.
//
//   node docs/boot-time/measure.mjs [options]
//
//   --site DIR          the built page (default .build/site, "mise run site")
//   --rounds N          rounds; each round boots a first visit, a return visit
//                       and a reload in the same tab, alternating (default 8)
//   --cases LIST        first,return,reload (default all three)
//   --pod-delays LIST   ms after the prompt for "kubectl run web --image=nginx",
//                       rotated over the boots (default 0,1500,3000; "" for none)
//   --settle MS         how long a boot is watched after the prompt for the
//                       pods of the addons (default 25000)
//   --no-marks          boot without ?bootmarks: only the time to the prompt,
//                       to check what the marks cost
//   --profile           one first visit with a CPU profile of the page (CDP)
//                       instead of the rounds; needs a binary with function
//                       names (docs/boot-time.md)
//   --sampling US       sampling interval of the profile (default 200)
//   --throttle MBIT     limit the download of the page to MBIT Mbit/s
//   --out FILE          all raw data as JSON (default boot-time.json)
//   --log FILE          the console of the first boot (default none)
//   --summary FILE      print the tables of an earlier --out file, boot nothing
//
// It serves --site itself on 127.0.0.1 with Cache-Control: max-age=600 and
// ETags, like GitHub Pages, and uses the Chromium of Playwright
// (require("playwright"), no other install).
import { createHash } from "node:crypto";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import { createRequire } from "node:module";

const { chromium } = createRequire(import.meta.url)("playwright");

// ---- options ----
const argv = process.argv.slice(2);
const opt = (name, def) => {
  const i = argv.indexOf(`--${name}`);
  return i < 0 ? def : argv[i + 1];
};
const flag = (name) => argv.includes(`--${name}`);
const site = path.resolve(opt("site", ".build/site"));
const rounds = Number(opt("rounds", 8));
const cases = opt("cases", "first,return,reload").split(",").filter(Boolean);
const podDelays = opt("pod-delays", "0,1500,3000").split(",").filter((s) => s !== "").map(Number);
const settle = Number(opt("settle", 25000));
const marks = !flag("no-marks");
const profile = flag("profile");
const sampling = Number(opt("sampling", 200));
const throttle = Number(opt("throttle", 0));
const outFile = opt("out", "boot-time.json");
const logFile = opt("log", "");
if (!argv.includes("--summary") && !fs.existsSync(path.join(site, "index.html"))) {
  console.error(`${site}/index.html is missing; run "mise run build" and "mise run site" in tabernetes/`);
  process.exit(2);
}

// ---- static server ----
const types = {
  ".html": "text/html; charset=utf-8", ".js": "text/javascript", ".mjs": "text/javascript", ".css": "text/css",
  ".json": "application/json", ".wasm": "application/wasm", ".svg": "image/svg+xml", ".png": "image/png",
  ".txt": "text/plain; charset=utf-8", ".yaml": "text/yaml", ".tgz": "application/gzip", ".gz": "application/gzip",
};
let served = []; // {path, bytes, status} of the current boot
const server = http.createServer((req, res) => {
  const url = new URL(req.url, "http://x");
  let file = path.join(site, decodeURIComponent(url.pathname));
  if (!file.startsWith(site)) return res.writeHead(403).end();
  if (fs.existsSync(file) && fs.statSync(file).isDirectory()) file = path.join(file, "index.html");
  if (url.pathname === "/blank.html") {
    res.writeHead(200, { "Content-Type": "text/html" }).end("<!doctype html><title>blank</title>");
    return;
  }
  if (!fs.existsSync(file)) {
    served.push({ path: url.pathname, bytes: 0, status: 404 });
    return res.writeHead(404).end();
  }
  const st = fs.statSync(file);
  const etag = `"${st.size.toString(16)}-${Math.floor(st.mtimeMs).toString(16)}"`;
  const headers = { "Cache-Control": "max-age=600", ETag: etag, "Content-Type": types[path.extname(file)] || "application/octet-stream" };
  if (req.headers["if-none-match"] === etag) {
    served.push({ path: url.pathname, bytes: 0, status: 304 });
    return res.writeHead(304, headers).end();
  }
  headers["Content-Length"] = st.size;
  res.writeHead(200, headers);
  served.push({ path: url.pathname, bytes: st.size, status: 200 });
  fs.createReadStream(file).pipe(res);
});
await new Promise((r) => server.listen(0, "127.0.0.1", r));
const base = `http://127.0.0.1:${server.address().port}`;
const pageURL = `${base}/${marks ? "?bootmarks" : ""}#boot`;

// ---- browser ----
const chromeArgs = [
  // A headless tab counts as hidden: keep its timers at full speed.
  "--disable-background-timer-throttling",
  "--disable-renderer-backgrounding",
  "--disable-backgrounding-occluded-windows",
];
const launch = (dir) => chromium.launchPersistentContext(dir, { headless: true, args: chromeArgs, viewport: { width: 1280, height: 800 } });
const tmp = (p) => fs.mkdtempSync(path.join(os.tmpdir(), p));

// boot loads the page (navigate or reload) and waits for the prompt.
async function boot(ctx, page, how, podDelay, wantLog) {
  served = [];
  const external = [];
  const klog = [];
  const consoleLog = [];
  const onReq = (r) => {
    const u = new URL(r.url());
    if (!["127.0.0.1", "localhost"].includes(u.hostname) && !u.protocol.startsWith("data") && !u.protocol.startsWith("blob")) {
      external.push({ url: r.url(), start: Date.now() });
    }
  };
  const onFinished = (r) => {
    const e = external.find((x) => x.url === r.url() && !x.end);
    if (e) e.end = Date.now();
  };
  const onConsole = (m) => {
    const t = m.text();
    if (wantLog) consoleLog.push(`[${m.type()}] ${t}`);
    // klog lines of the components; the interesting ones only.
    if (/"Controller starting\.\.\."|controllermanager\.go:\d+\] "Starting"|"Starting Kubernetes Scheduler"|Serving securely|"Starting ipam controller"|"Warning: skipping controller"|"Warning: controller is disabled"/.test(t)) klog.push(t);
  };
  page.on("request", onReq);
  page.on("requestfinished", onFinished);
  page.on("console", onConsole);
  const t0 = Date.now();
  if (how === "reload") await page.reload({ waitUntil: "commit" });
  else await page.goto(pageURL, { waitUntil: "commit" });
  const res = { how, podDelay };
  try {
    if (marks) {
      await page.waitForFunction(() => performance.getEntriesByName("js:prompt").length > 0, null, { timeout: 300000, polling: 50 });
    } else {
      await page.waitForFunction(() => globalThis.tabernetesTerminal, null, { timeout: 300000, polling: 10 });
      res.promptNoMarks = await page.evaluate(() => performance.now());
    }
  } catch (e) {
    res.error = e.message;
    res.logs = await page.evaluate(() => (globalThis.tabernetesLogs || []).join("").slice(-4000)).catch(() => "");
    return res;
  }
  res.wallToPrompt = Date.now() - t0;
  // The size of the Wasm memory at the prompt: what a snapshot would hold.
  res.memBytes = await page.evaluate(() => globalThis.tabernetesMemory?.buffer.byteLength);
  if (marks && podDelay !== undefined) res.pod = await firstPod(page, podDelay);
  if (marks) {
    // The pods of the addons get ready after the prompt.
    await page.waitForFunction((settle) => performance.now() > performance.getEntriesByName("js:prompt")[0].startTime + settle,
      settle, { timeout: settle + 120000, polling: 500 });
    Object.assign(res, await page.evaluate(() => {
      const prompt = performance.getEntriesByName("js:prompt")[0].startTime;
      const idle = globalThis.tabernetesIdle || [];
      // The calls into Go up to the prompt (start, end, timer).
      const calls = [];
      for (let i = 0; i + 2 < idle.length && idle[i] <= prompt; i += 3) calls.push([idle[i], idle[i + 1], idle[i + 2]]);
      return {
        timeOrigin: performance.timeOrigin,
        tzOffsetMin: new Date().getTimezoneOffset(),
        marks: performance.getEntriesByType("mark").map((m) => ({ name: m.name, t: m.startTime, ...(m.detail || {}) })),
        calls,
        busyNow: globalThis.tabernetesBusy?.(),
        nav: (() => { const n = performance.getEntriesByType("navigation")[0]; return n && { responseEnd: n.responseEnd, domContentLoaded: n.domContentLoadedEventEnd }; })(),
        resources: performance.getEntriesByType("resource").map((r) => ({ name: r.name.replace(location.origin, ""), start: r.startTime, end: r.responseEnd, size: r.transferSize, decoded: r.decodedBodySize })),
      };
    }));
  }
  res.klog = klog;
  res.served = served;
  res.external = external;
  if (wantLog) res.consoleLog = consoleLog;
  page.off("request", onReq);
  page.off("requestfinished", onFinished);
  page.off("console", onConsole);
  return res;
}

// firstPod types "kubectl run web --image=nginx" podDelay ms after the
// prompt and times it: until the shell returns, until the tab node sees the
// pod Ready (mark go:pod-ready:default/web) and until it answers HTTP.
async function firstPod(page, podDelay) {
  return page.evaluate(async (delay) => {
    const prompt = performance.getEntriesByName("js:prompt")[0].startTime;
    await new Promise((r) => setTimeout(r, Math.max(0, prompt + delay - performance.now())));
    const sh = (src) => globalThis.tabernetes.shell(src, () => {}, globalThis.tabernetesTerminal?.builtins);
    const run = performance.now();
    const rc = await sh("kubectl run web --image=nginx");
    const returned = performance.now();
    const mark = (prefix) => performance.getEntriesByType("mark").find((m) => m.name.startsWith(prefix));
    const until = async (fn, ms) => {
      const end = performance.now() + ms;
      for (;;) {
        const v = await fn();
        if (v || performance.now() > end) return v;
        await new Promise((r) => setTimeout(r, 20));
      }
    };
    const ipMark = await until(() => mark("go:pod-ip:default/web="), 120000);
    const ip = ipMark?.name.split("=")[1];
    let http = null;
    if (ip) {
      await until(async () => {
        if ((await sh(`wget -q -O /dev/null -T 1 http://${ip}/`)) === 0) http = performance.now();
        return http;
      }, 120000);
    }
    const ready = await until(() => mark("go:pod-ready:default/web"), 120000);
    return { delay, run, rc, returned, ready: ready?.startTime, ip: ipMark?.startTime, http, prompt };
  }, podDelay);
}

// ---- rounds ----
const results = [];
const save = () => fs.writeFileSync(outFile, JSON.stringify({ site, pageURL, machine: machine(), throttle, results }, null, 1));
function machine() {
  const cpus = os.cpus();
  return { cpu: cpus[0]?.model, cores: cpus.length, mem: os.totalmem(), node: process.version, chromium: chromium.executablePath(), kernel: os.release() };
}
async function throttled(page) {
  if (!throttle) return;
  const cdp = await page.context().newCDPSession(page);
  await cdp.send("Network.enable");
  await cdp.send("Network.emulateNetworkConditions", { offline: false, latency: 20, downloadThroughput: (throttle * 1e6) / 8, uploadThroughput: (throttle * 1e6) / 8 });
}


function report(r, i) {
  if (r.error) return console.log(`#${i} ${r.how}: ERROR ${r.error}`);
  const m = (name) => r.marks?.find((x) => x.name === name)?.t;
  const prompt = marks ? m("js:prompt") : r.promptNoMarks;
  const pod = r.pod ? ` pod@${r.pod.delay}: ready ${(r.pod.ready - r.pod.run).toFixed(0)} http ${(r.pod.http - r.pod.run).toFixed(0)} ms` : "";
  console.log(`#${i} r${r.round} ${r.how}: prompt ${prompt?.toFixed(0)} ms${pod}`);
}

// ---- summary ----
function stats(xs) {
  xs = xs.filter((x) => Number.isFinite(x)).sort((a, b) => a - b);
  if (!xs.length) return null;
  const q = (p) => {
    const i = (xs.length - 1) * p, lo = Math.floor(i), hi = Math.ceil(i);
    return xs[lo] + (xs[hi] - xs[lo]) * (i - lo);
  };
  return { n: xs.length, min: xs[0], p25: q(0.25), med: q(0.5), p75: q(0.75), max: xs[xs.length - 1] };
}
const fmt = (s) => (s ? `${s.med.toFixed(0)} (${s.p25.toFixed(0)}–${s.p75.toFixed(0)}, ${s.min.toFixed(0)}–${s.max.toFixed(0)})` : "–");

// Phases from mark to mark: name, from, to.
const phases = [
  ["HTML and tabernetes.js", null, "js:terminal"],
  ["binary: request to first bytes", "js:fetch", "js:first-chunk"],
  ["binary: transfer", "js:first-chunk", "js:downloaded"],
  ["binary: last zstd frames", "js:downloaded", "js:unpacked"],
  ["binary: compile after the last byte", "js:unpacked", "js:compiled"],
  ["Go runtime start and package init", "js:go-run", "go:main"],
  ["main until Go waits the first time", "go:main", "js:go-yield"],
  ["main to boot", "go:main", "go:boot"],
  ["PKI and kubeconfigs", "go:boot", "go:pki"],
  ["etcd shim start", "go:pki", "go:etcd"],
  ["kube-apiserver: start to first answer", "go:apiserver-launch", "go:apiserver-serving"],
  ["kube-apiserver: first answer to /readyz ok", "go:apiserver-serving", "go:apiserver-ready"],
  ["controller-manager and scheduler launch", "go:apiserver-ready", "go:scheduler-launched"],
  ["addons: kubectl apply", "go:scheduler-launched", "go:addons-applied"],
  ["tab node: register (Node, status)", "go:addons-applied", "go:node-registered"],
  ["tab node: kubelet server, static pods, network policy", "go:node-registered", "go:node-wait-pod-cidr"],
  ["tab node: wait for the pod CIDR", "go:node-wait-pod-cidr", "go:node-pod-cidr"],
  ["tab node: informers", "go:node-pod-cidr", "go:tab-node-started"],
  ["ready to prompt", "go:tab-node-started", "js:prompt"],
];

function summary() {
  const ok = results.filter((r) => !r.error);
  const byCase = (c) => ok.filter((r) => r.how === c);
  console.log(`\nmachine: ${JSON.stringify(machine())}`);
  console.log(`errors: ${results.length - ok.length}`);
  if (!marks) {
    for (const c of cases) console.log(`${c}: prompt ${fmt(stats(byCase(c).map((r) => r.promptNoMarks)))} ms`);
    return;
  }
  const at = (r, name) => r.marks.find((x) => x.name === name);
  const t = (r, name) => (name ? at(r, name)?.t : 0);
  console.log("\nTime to the prompt (ms after navigation), median (p25–p75, min–max):");
  for (const c of cases) console.log(`  ${c}: ${fmt(stats(byCase(c).map((r) => t(r, "js:prompt"))))}`);

  console.log("\nPhases (ms), median (p25–p75, min–max), per case:");
  console.log(`| phase | ${cases.join(" | ")} |`);
  console.log(`|---|${cases.map(() => "---").join("|")}|`);
  for (const [name, from, to] of phases) {
    console.log(`| ${name} | ${cases.map((c) => fmt(stats(byCase(c).map((r) => t(r, to) - t(r, from))))).join(" | ")} |`);
  }

  // Absolute times of the milestones.
  const names = [...new Set(ok.flatMap((r) => r.marks.map((m) => m.name)))].filter((n) => !n.startsWith("go:pod-ip:"));
  console.log("\nMarks (ms after navigation), median (p25–p75, min–max):");
  console.log(`| mark | ${cases.join(" | ")} |`);
  console.log(`|---|${cases.map(() => "---").join("|")}|`);
  for (const n of names.sort((a, b) => (t(ok[0], a) ?? 1e9) - (t(ok[0], b) ?? 1e9))) {
    console.log(`| ${n} | ${cases.map((c) => fmt(stats(byCase(c).map((r) => t(r, n))))).join(" | ")} |`);
  }

  // Busy, GC and waits from go-run to the prompt.
  console.log("\nGo thread from go.run to the prompt (ms): wall, busy, GC, idle:");
  for (const c of cases) {
    const rs = byCase(c);
    const wall = rs.map((r) => t(r, "js:prompt") - t(r, "js:go-run"));
    const busy = rs.map((r) => at(r, "js:prompt").busy - at(r, "js:go-run").busy);
    const gc = rs.map((r) => at(r, "go:tab-node-started").gc);
    console.log(`  ${c}: wall ${fmt(stats(wall))} busy ${fmt(stats(busy))} GC ${fmt(stats(gc))} idle ${fmt(stats(wall.map((w, i) => w - busy[i])))}`);
  }
  console.log("\nBusy, GC and idle per phase (ms, medians over all cases):");
  console.log("| phase | wall | busy | GC | idle |");
  console.log("|---|---|---|---|---|");
  for (const [name, from, to] of phases.slice(5)) {
    const rs = ok.filter((r) => at(r, to) && (from === null || at(r, from)));
    const d = (k) => rs.map((r) => (at(r, to)[k] ?? NaN) - (from ? at(r, from)[k] ?? NaN : 0));
    const wall = rs.map((r) => t(r, to) - t(r, from));
    const busy = d("busy");
    console.log(`| ${name} | ${fmt(stats(wall))} | ${fmt(stats(busy))} | ${fmt(stats(d("gc")))} | ${fmt(stats(wall.map((w, i) => w - busy[i])))} |`);
  }

  // Idle time of the Go thread by the timer Go waited for.
  const buckets = [[-1, -1, "no timer (a JS event)"], [0, 0, "0 ms"], [1, 9, "1–9 ms"], [10, 49, "10–49 ms"], [50, 99, "50–99 ms"], [100, 249, "100–249 ms"], [250, 999, "250–999 ms"], [1000, Infinity, "≥ 1 s"]];
  console.log("\nIdle gaps of the Go thread from go.run to the prompt, by the timer it asked for (ms per boot, medians):");
  console.log("| timer | gaps | idle ms | gaps woken early |");
  console.log("|---|---|---|---|");
  for (const [lo, hi, label] of buckets) {
    const per = ok.map((r) => {
      const goRun = t(r, "js:go-run");
      let count = 0, sum = 0, early = 0;
      for (let i = 0; i + 1 < r.calls.length; i++) {
        if (r.calls[i][0] < goRun) continue;
        const timer = r.calls[i][2], gap = r.calls[i + 1][0] - r.calls[i][1];
        if (timer < lo || timer > hi) continue;
        count++;
        sum += gap;
        if (timer >= 0 && gap < timer - 1) early++;
      }
      return { count, sum, early };
    });
    console.log(`| ${label} | ${stats(per.map((p) => p.count))?.med ?? 0} | ${stats(per.map((p) => p.sum))?.med.toFixed(0)} | ${stats(per.map((p) => p.early))?.med ?? 0} |`);
  }
  // Long calls into Go (no return to the event loop).
  console.log("\nLongest single calls into Go before the prompt (ms per boot, medians): 1st, 2nd, 3rd, 10th, number > 50 ms, > 200 ms");
  const longest = ok.map((r) => r.calls.map((c) => c[1] - c[0]).sort((a, b) => b - a));
  console.log(`  ${[0, 1, 2, 9].map((i) => stats(longest.map((l) => l[i] ?? 0)).med.toFixed(0)).join(", ")}, ${stats(longest.map((l) => l.filter((x) => x > 50).length)).med}, ${stats(longest.map((l) => l.filter((x) => x > 200).length)).med}`);

  // The controller-manager, from its log (klog time stamps, 0.1 ms).
  console.log("\nkube-controller-manager and kube-scheduler from their logs (ms after navigation):");
  const klogT = (r, line) => {
    const m = /^[IWE](\d\d)(\d\d) (\d\d):(\d\d):(\d\d)\.(\d+)/.exec(line);
    if (!m) return NaN;
    const y = new Date(r.timeOrigin).getUTCFullYear();
    const ms = Date.UTC(y, m[1] - 1, m[2], m[3], m[4], m[5]) + Number(`0.${m[6]}`) * 1000 + r.tzOffsetMin * 60000;
    return ms - r.timeOrigin;
  };
  const lines = (r, re) => r.klog.filter((l) => re.test(l)).map((l) => klogT(r, l));
  const kcm = (r) => {
    const start = lines(r, /controllermanager\.go:\d+\] "Starting"/)[0];
    const ctl = lines(r, /"Controller starting\.\.\."/);
    return { start, n: ctl.length, first: Math.min(...ctl), last: Math.max(...ctl), ipam: lines(r, /"Starting ipam controller"/)[0],
      sched: lines(r, /"Starting Kubernetes Scheduler"/)[0], prompt: t(r, "js:prompt") };
  };
  console.log("| | " + cases.join(" | ") + " |");
  console.log(`|---|${cases.map(() => "---").join("|")}|`);
  const row = (label, f) => console.log(`| ${label} | ${cases.map((c) => fmt(stats(byCase(c).map((r) => f(kcm(r)))))).join(" | ")} |`);
  row("controller-manager: command starts", (k) => k.start);
  row("controller-manager: controllers built (first \"Controller starting...\")", (k) => k.first);
  row("controller-manager: build time (start to first controller start)", (k) => k.first - k.start);
  row("controller-manager: last \"Controller starting...\"", (k) => k.last);
  row("controller-manager: controllers started (count)", (k) => k.n);
  row("node IPAM controller starts", (k) => k.ipam);
  row("scheduler: command starts", (k) => k.sched);
  row("last controller start minus prompt", (k) => k.last - k.prompt);

  // Pods of the addons and the first pod.
  console.log("\nPods Ready (ms after the prompt), median (p25–p75, min–max):");
  const podNames = [...new Set(ok.flatMap((r) => r.marks.filter((m) => m.name.startsWith("go:pod-ready:")).map((m) => m.name.replace(/-[a-z0-9]{8,10}(-[a-z0-9]{5})?$|-[a-z0-9]{5}$/, "-*"))))].sort();
  for (const n of podNames) {
    const v = ok.map((r) => {
      const m = r.marks.find((x) => x.name.startsWith("go:pod-ready:") && x.name.replace(/-[a-z0-9]{8,10}(-[a-z0-9]{5})?$|-[a-z0-9]{5}$/, "-*") === n);
      return m ? m.t - t(r, "js:prompt") : NaN;
    });
    console.log(`  ${n}: ${fmt(stats(v))}`);
  }
  if (podDelays.length) {
    console.log("\nFirst pod: kubectl run web --image=nginx, ms after the command (median (p25–p75, min–max)):");
    console.log("| typed | n | shell returns | Ready | HTTP |");
    console.log("|---|---|---|---|---|");
    for (const d of podDelays) {
      const ps = ok.map((r) => r.pod).filter((p) => p && p.delay === d);
      console.log(`| ${d} ms after the prompt | ${ps.length} | ${fmt(stats(ps.map((p) => p.returned - p.run)))} | ${fmt(stats(ps.map((p) => p.ready - p.run)))} | ${fmt(stats(ps.map((p) => p.http - p.run)))} |`);
    }
  }

  // The download.
  const wasm = ok.flatMap((r) => r.served.filter((s) => s.path.endsWith(".wasm.zst")));
  console.log(`\nBinary transfers: ${wasm.map((s) => `${s.status}:${s.bytes}`).filter((v, i, a) => a.indexOf(v) === i).join(", ")}`);
  for (const c of cases) {
    const rs = byCase(c);
    console.log(`  ${c}: requests ${fmt(stats(rs.map((r) => r.served.length)))}, bytes ${fmt(stats(rs.map((r) => r.served.reduce((a, s) => a + s.bytes, 0))))}`);
  }
  const mem = stats(ok.map((r) => r.memBytes / 1048576));
  if (mem) console.log(`Wasm memory at the prompt: ${fmt(mem)} MiB`);
  const ext = ok.flatMap((r) => r.external);
  console.log(`External requests: ${ext.length ? ext.map((e) => `${e.url} ${e.end ? e.end - e.start : "?"} ms`).join(", ") : "none"}`);
}

// ---- CPU profile ----
async function profileRun() {
  const dir = tmp("tabernetes-prof-");
  const ctx = await launch(dir);
  const page = ctx.pages()[0] || (await ctx.newPage());
  // A page of the same origin first, so that the page and its profiler stay
  // in one renderer process when the page loads.
  await page.goto(`${base}/blank.html`);
  const cdp = await ctx.newCDPSession(page);
  await cdp.send("Profiler.enable");
  await cdp.send("Profiler.setSamplingInterval", { interval: sampling });
  await cdp.send("Profiler.start");
  const r = await boot(ctx, page, "first", undefined, false);
  if (r.error) throw new Error(r.error);
  // Align the clock of the profile with performance.now(): a function of a
  // known name runs 30 ms at a known time.
  const align = await page.evaluate(() => {
    const t = performance.now();
    (function tabernetesProfileAlign() { const e = performance.now() + 30; while (performance.now() < e); })();
    return t;
  });
  const { profile: prof } = await cdp.send("Profiler.stop");
  await ctx.close();
  fs.rmSync(dir, { recursive: true, force: true });
  fs.writeFileSync(outFile.replace(/\.json$/, "") + ".cpuprofile", JSON.stringify(prof));
  const out = analyzeProfile(prof, r, align);
  fs.writeFileSync(outFile, JSON.stringify({ site, machine: machine(), sampling, result: r, profile: out }, null, 1));
}

// analyzeProfile sums the samples by function and by Go package: self time
// (the function was on top) and total time (on the stack). Go on js/wasm
// unwinds the Wasm stack when a goroutine yields, so the stacks are cut
// there and the total times of callers are too low; the self times hold.
function analyzeProfile(prof, r, align) {
  const nodes = new Map(prof.nodes.map((n) => [n.id, n]));
  const parent = new Map();
  for (const n of prof.nodes) for (const c of n.children || []) parent.set(c, n.id);
  const nameOf = (n) => {
    const f = n.callFrame;
    if (f.functionName) return f.url.startsWith("wasm://") ? f.functionName.replace(/^\$/, "") : f.functionName;
    if (f.url) return `(anonymous ${f.url.split("/").pop()}:${f.lineNumber})`;
    return "(anonymous)";
  };
  const kind = (n) => {
    const f = n.callFrame;
    if (f.url.startsWith("wasm://")) return "wasm";
    if (["(program)", "(idle)", "(garbage collector)", "(root)"].includes(f.functionName)) return f.functionName;
    return "js";
  };
  const pkgOf = (name) => {
    if (name.startsWith("(")) return name;
    const slash = name.lastIndexOf("/", name.indexOf("(") < 0 ? name.length : name.indexOf("("));
    const dot = name.indexOf(".", slash + 1);
    return dot < 0 ? name : name.slice(0, dot);
  };
  // Time of each sample: timeDeltas, in µs from startTime.
  let ts = prof.startTime;
  const times = prof.timeDeltas.map((d) => (ts += d));
  // performance.now() of the page = µs/1000 - offset.
  const alignIdx = prof.samples.findIndex((id) => {
    for (let x = id; x !== undefined; x = parent.get(x)) if (nodes.get(x).callFrame.functionName === "tabernetesProfileAlign") return true;
    return false;
  });
  const offset = alignIdx >= 0 ? times[alignIdx] / 1000 - align : null;
  const pageT = (i) => (offset === null ? NaN : times[i] / 1000 - offset);
  const mark = (n) => r.marks?.find((m) => m.name === n)?.t;
  const windows = phases.filter(([, f, to]) => (f === null || mark(f) !== undefined) && mark(to) !== undefined)
    .map(([name, f, to]) => ({ name, from: f ? mark(f) : 0, to: mark(to) }));
  const self = new Map(), total = new Map(), pkgSelf = new Map(), kinds = new Map(), byPhase = new Map();
  const prompt = mark("js:prompt"), goRun = mark("js:go-run");
  const add = (m, k, v) => m.set(k, (m.get(k) || 0) + v);
  for (let i = 0; i < prof.samples.length; i++) {
    const tp = pageT(i);
    if (!(tp >= 0 && tp <= prompt)) continue; // the boot only
    const dt = (i + 1 < times.length ? times[i + 1] - times[i] : 0) / 1000;
    const leaf = nodes.get(prof.samples[i]);
    const name = nameOf(leaf);
    add(self, name, dt);
    add(kinds, kind(leaf), dt);
    if (kind(leaf) === "wasm") add(pkgSelf, pkgOf(name), dt);
    const seen = new Set();
    for (let x = leaf.id; x !== undefined; x = parent.get(x)) {
      const nm = nameOf(nodes.get(x));
      if (!seen.has(nm)) { seen.add(nm); add(total, nm, dt); }
    }
    const w = windows.find((w) => tp >= w.from && tp < w.to);
    if (w && kind(leaf) === "wasm") {
      if (!byPhase.has(w.name)) byPhase.set(w.name, new Map());
      add(byPhase.get(w.name), pkgOf(name), dt);
    }
  }
  const top = (m, n) => [...m].sort((a, b) => b[1] - a[1]).slice(0, n).map(([k, v]) => [k, Math.round(v)]);
  const out = {
    aligned: offset !== null, prompt, goRun,
    kinds: top(kinds, 10),
    topSelf: top(self, 40), topTotal: top(total, 60), topPackagesSelf: top(pkgSelf, 40),
    phases: Object.fromEntries([...byPhase].map(([k, m]) => [k, top(m, 8)])),
  };
  console.log(JSON.stringify(out, null, 1));
  return out;
}

// ---- main ----
const old = opt("summary", "");
if (old) {
  results.push(...JSON.parse(fs.readFileSync(old, "utf8")).results);
  summary();
} else if (profile) {
  await profileRun();
} else {
  // A warm profile for the return visits: one boot that fills the HTTP cache
  // and the Cache Storage of the page.
  const warmDir = tmp("tabernetes-warm-");
  if (cases.includes("return") || cases.includes("reload")) {
    const ctx = await launch(warmDir);
    const page = ctx.pages()[0] || (await ctx.newPage());
    await throttled(page);
    const r = await boot(ctx, page, "warmup", undefined, false);
    if (r.error) throw new Error(`warm-up boot failed: ${r.error}\n${r.logs}`);
    await page.waitForFunction(async () => (await (await caches.open("tabernetes")).keys()).length > 0, null, { timeout: 60000, polling: 500 });
    await ctx.close();
  }
  let n = 0;
  for (let round = 0; round < rounds; round++) {
    // Alternate: first visit before the return visit in even rounds, after it in odd ones.
    const order = round % 2 ? ["return", "first"] : ["first", "return"];
    for (const c of order) {
      const delay = (i) => (podDelays.length ? podDelays[(round + i) % podDelays.length] : undefined);
      if (c === "first" && cases.includes("first")) {
        const dir = tmp("tabernetes-first-");
        const ctx = await launch(dir);
        const page = ctx.pages()[0] || (await ctx.newPage());
        await throttled(page);
        const r = await boot(ctx, page, "first", delay(0), logFile && n === 0);
        r.round = round;
        results.push(r);
        report(r, n++);
        await ctx.close();
        fs.rmSync(dir, { recursive: true, force: true });
      }
      if (c === "return" && (cases.includes("return") || cases.includes("reload"))) {
        const ctx = await launch(warmDir);
        const page = ctx.pages()[0] || (await ctx.newPage());
        await throttled(page);
        if (cases.includes("return")) {
          const r = await boot(ctx, page, "return", delay(1), logFile && n === 0);
          r.round = round;
          results.push(r);
          report(r, n++);
        } else {
          await boot(ctx, page, "warmup", undefined, false);
        }
        if (cases.includes("reload")) {
          const r = await boot(ctx, page, "reload", delay(2), false);
          r.round = round;
          results.push(r);
          report(r, n++);
        }
        await ctx.close();
      }
      save();
    }
  }
  fs.rmSync(warmDir, { recursive: true, force: true });
  if (logFile && results[0]?.consoleLog) fs.writeFileSync(logFile, results[0].consoleLog.join("\n"));
  save();
  summary();
}
server.close();
