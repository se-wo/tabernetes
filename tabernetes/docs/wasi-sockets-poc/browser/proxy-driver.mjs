// proxy-driver.mjs <proxy.wasm>: phase 0, items 5, 12 and 15 in headless
// Chromium. The page is the Go side of the PoC: it sends `request` messages
// to a proxy pod worker and answers its `fetch` messages. Each candidate of
// item 12 ("own" and "shim") runs the same checks.
//
// Memory is the resident set size of the renderer processes, after a
// garbage collection in the pod worker and in the page.
import { createServer } from "node:http";
import { readFileSync, existsSync, readdirSync } from "node:fs";
import { createRequire } from "node:module";
import { extname, join } from "node:path";

const { chromium } = createRequire(import.meta.url)("playwright");
const here = new URL(".", import.meta.url).pathname;
const site = join(here, "site");
const types = { ".html": "text/html", ".js": "text/javascript", ".wasm": "application/wasm" };
const component = process.argv[2];

const server = createServer((req, res) => {
  const headers = { "Cross-Origin-Opener-Policy": "same-origin", "Cross-Origin-Embedder-Policy": "require-corp" };
  const url = new URL(req.url, "http://x").pathname;
  if (url === "/component.wasm") { res.writeHead(200, { ...headers, "Content-Type": "application/wasm" }); res.end(readFileSync(component)); return; }
  let path = join(site, decodeURIComponent(url));
  if (path.endsWith("/")) path = join(path, "index.html");
  if (!path.startsWith(site) || !existsSync(path)) { res.writeHead(404).end(); return; }
  res.writeHead(200, { ...headers, "Content-Type": types[extname(path)] ?? "application/octet-stream" });
  res.end(readFileSync(path));
}).listen(0, "127.0.0.1");
await new Promise((r) => server.once("listening", r));
const base = `http://127.0.0.1:${server.address().port}/`;

const browser = await chromium.launch({ args: ["--js-flags=--expose-gc"] });
const page = await browser.newPage();
page.on("console", (m) => console.log(`console: ${m.text()}`));
page.on("pageerror", (e) => console.log(`pageerror: ${e.message}`));
await page.goto(base);
console.log(`browser ${browser.version()}, crossOriginIsolated: ${await page.evaluate(() => crossOriginIsolated)}`);

// The Go side of the PoC, in the page.
await page.evaluate(async () => {
  const tw = new Worker("assets/transpile.worker.js", { type: "module" });
  const bytes = new Uint8Array(await (await fetch("component.wasm")).arrayBuffer());
  window.files = await new Promise((resolve, reject) => {
    tw.onmessage = ({ data }) => (data.ok ? resolve(data.files) : reject(new Error(data.error)));
    tw.postMessage({ id: 1, name: "proxy", bytes }, [bytes.buffer]);
  });
  tw.terminate();
  const enc = new TextEncoder();
  window.start = (candidate) => new Promise((resolve) => {
    const w = new Worker("assets/proxy.worker.js", { type: "module" });
    const waits = new Map();
    let id = 0;
    w.onmessage = ({ data }) => {
      if (data.type === "loaded") resolve();
      else if (data.type === "response" || data.type === "stats") { waits.get(data.id ?? "stats")?.(data); waits.delete(data.id ?? "stats"); }
      else if (data.type === "fetch") {
        // The Go side: an error code for example.invalid, else a response.
        const host = new URL(data.url).host;
        if (host === "example.invalid") w.postMessage({ type: "fetched", fid: data.fid, error: { tag: "DNS-error", val: { rcode: "NXDOMAIN", infoCode: undefined } } });
        else w.postMessage({ type: "fetched", fid: data.fid, status: 203, headers: [["set-cookie", enc.encode("s=1")], ["set-cookie", enc.encode("t=2")]], body: enc.encode("fetched") });
      }
    };
    window.worker = w;
    window.req = (path, { method = "GET", headers = [], body } = {}) => new Promise((res) => {
      const rid = ++id;
      waits.set(rid, res);
      w.postMessage({ type: "request", id: rid, method, path, authority: "pod:8080", headers: headers.map(([k, v]) => [k, enc.encode(v)]), body: body ? enc.encode(body) : undefined });
    });
    window.reqId = () => id;
    window.cancel = (rid, mode = "clear") => { waits.delete(rid); w.postMessage({ type: "cancel", id: rid, mode }); };
    window.stats = (gc) => new Promise((res) => { waits.set("stats", res); w.postMessage({ type: "stats", gc }); });
    w.postMessage({ type: "load", candidate, name: "proxy", files });
  });
  window.gcAll = async () => {
    if (window.gc) gc();
    await stats(true);
    await new Promise((r) => setTimeout(r, 200));
  };
});

// rss: the resident set size of all renderer processes, in bytes.
function rss() {
  let sum = 0;
  for (const pid of readdirSync("/proc").filter((d) => /^\d+$/.test(d))) {
    try {
      if (!readFileSync(`/proc/${pid}/cmdline`, "latin1").includes("--type=renderer")) continue;
      const m = /VmRSS:\s+(\d+) kB/.exec(readFileSync(`/proc/${pid}/status`, "latin1"));
      if (m) sum += Number(m[1]) * 1024;
    } catch { /* the process ended */ }
  }
  return sum;
}
const mem = async () => { await page.evaluate(() => gcAll()); return rss(); };

const show = (r) => {
  const d = new TextDecoder();
  if (!r.status) return JSON.stringify({ error: r.error, trap: r.trap, none: r.none });
  const h = r.headers.map(([k, v]) => `${k}: ${d.decode(new Uint8Array(v))}`).join("; ");
  const body = d.decode(new Uint8Array(r.body)).trim().replace(/\n/g, " | ");
  return `${r.status} [${h}] ${body.length > 160 ? body.slice(0, 160) + "..." : body}`;
};

for (const candidate of ["own", "shim"]) {
  console.log(`\n=== candidate ${candidate}`);
  await page.evaluate((c) => start(c), candidate);
  const checks = [
    ["PX28 headers in", "/headers", { headers: [["Cookie", "c=1"], ["User-Agent", "ua/1"], ["Origin", "http://o"], ["Host", "pod:8080"]] }],
    ["PX16 Set-Cookie out", "/cookie", {}],
    ["PX29 PURGE", "/headers", { method: "PURGE" }],
    ["PX29 TRACE", "/headers", { method: "TRACE" }],
    ["PX29 GET with body", "/headers", { method: "GET", body: "b" }],
    ["PX30 204", "/status/204", {}],
    ["PX30 304", "/status/304", {}],
    ["PX30 599", "/status/599", {}],
    ["PX27 error code", "/error", {}],
    ["PX21 trap", "/trap", {}],
    ["PX26 no response", "/noresponse", {}],
    ["CR9 DNS-error", "/fetch/example.invalid/x", {}],
    ["CR fetched response", "/fetch/svc.ns/x", {}],
    ["PX20 body of 17 MiB", `/big/${17 << 20}`, {}],
  ];
  for (const [what, path, opts] of checks) {
    const t = Date.now();
    const r = await page.evaluate(([p, o]) => req(p, o), [path, opts]);
    console.log(`${what}: ${show(r).slice(0, 300)} (${Date.now() - t} ms)`);
  }
  // Time and memory of 1,000 requests in a sequence.
  const before = await mem();
  const ms = await page.evaluate(async () => {
    const t = performance.now();
    for (let i = 0; i < 1000; i++) await req("/context");
    return performance.now() - t;
  });
  const after = await mem();
  const st = await page.evaluate(() => stats(true));
  console.log(`1,000 requests: ${ms.toFixed(0)} ms (${(ms / 1000).toFixed(2)} ms each); memory ${(before / 1048576).toFixed(1)} MiB -> ${(after / 1048576).toFixed(1)} MiB; instances made ${st.made}, freed ${st.freed}`);
  await page.evaluate(() => worker.terminate());
}

// Item 5: 16 instances at the same time, with waits in the opposite sequence.
console.log("\n=== item 5 (candidate own)");
await page.evaluate(() => start("own"));
const order = await page.evaluate(async () => {
  const done = [];
  await Promise.all(Array.from({ length: 16 }, (_, i) => req(`/wait/${(16 - i) * 50}`).then(() => done.push(i + 1))));
  return done;
});
console.log(`sent 1..16, completed: ${order.join(" ")}`);
const t20 = await page.evaluate(async () => {
  const t = performance.now();
  await Promise.all(Array.from({ length: 20 }, () => req("/wait/1000")));
  return performance.now() - t;
});
console.log(`20 requests that wait 1 s, no limit of 16 in the PoC: ${t20.toFixed(0)} ms`);

// Item 15: cancel 1,000 instances that JSPI suspended.
console.log("\n=== item 15 (candidate own)");
const m0 = await mem();
const early = await page.evaluate(async () => {
  window.ids = [];
  const ends = {};
  for (let i = 0; i < 1000; i++) {
    req("/wait/600000").then((r) => { const k = r.trap ?? r.error ?? r.status; ends[k] = (ends[k] ?? 0) + 1; });
    ids.push(reqId());
  }
  await new Promise((r) => setTimeout(r, 5000));
  const s = await stats(false);
  return { live: s.live, ends };
});
console.log(`after 5 s: ${early.live} instances wait; ended early: ${JSON.stringify(early.ends)}`);
const m1 = await mem();
await page.evaluate(async () => {
  for (const id of ids) cancel(id);
  await new Promise((r) => setTimeout(r, 500));
});
const m2 = await mem();
await page.evaluate(() => new Promise((r) => setTimeout(r, 2000)));
const m3 = await mem();
console.log(`memory: before ${(m0 / 1048576).toFixed(1)} MiB, 1,000 suspended ${(m1 / 1048576).toFixed(1)} MiB, after cancel and gc ${(m2 / 1048576).toFixed(1)} MiB, 2 s later ${(m3 / 1048576).toFixed(1)} MiB`);
const st = await page.evaluate(() => stats(true));
console.log(`instances made ${st.made}, freed ${st.freed}, waiting ${st.live}`);
const next = await page.evaluate(async () => { const r = await req("/context"); return r.status ?? r.trap; });
console.log(`a request after the cancel: ${next}`);

// Item 15, continued: cancel with an exception in the suspended call.
console.log("\n=== item 15, cancel with an exception (candidate own)");
await page.evaluate(() => { worker.terminate(); return start("own"); });
for (const round of [1, 2, 3]) {
  const r = await page.evaluate(async () => {
    window.ids = [];
    const ends = {};
    for (let i = 0; i < 100; i++) {
      req("/wait/600000").then((x) => { const k = x.trap ?? x.status; ends[k] = (ends[k] ?? 0) + 1; });
      ids.push(reqId());
    }
    await new Promise((r) => setTimeout(r, 2000));
    const before = await stats(false);
    for (const id of ids) cancel(id, "reject");
    await new Promise((r) => setTimeout(r, 500));
    const after = await stats(true);
    return { waiting: before.live, ends, made: after.made, freed: after.freed };
  });
  console.log(`round ${round}: ${r.waiting} waited; ended early ${JSON.stringify(r.ends)}; after the cancel: made ${r.made}, freed ${r.freed}`);
}
const m4 = await mem();
console.log(`memory after 3 rounds: ${(m4 / 1048576).toFixed(1)} MiB`);

// The limit of Wasm memories: for each worker or for the process?
console.log("\n=== the limit of instances, two pod workers");
await page.evaluate(() => worker.terminate());
const two = await page.evaluate(async () => {
  const out = [];
  for (const name of ["A", "B"]) {
    await start("own");
    const ends = {};
    for (let i = 0; i < 100; i++) req("/wait/600000").then((x) => { const k = x.trap ?? x.status; ends[k] = (ends[k] ?? 0) + 1; });
    await new Promise((r) => setTimeout(r, 3000));
    const s = await stats(false);
    out.push(`worker ${name}: ${s.live} wait, ended early ${JSON.stringify(ends)}`);
  }
  return out;
});
for (const l of two) console.log(l);

await browser.close();
server.close();
