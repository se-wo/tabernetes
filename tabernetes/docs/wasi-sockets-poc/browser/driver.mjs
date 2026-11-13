// driver.mjs <component.wasm>...: phase 0, items 1, 2, 3, 11 and 13 (shim) in
// headless Chromium. It serves site/ itself, with Content-Type
// application/wasm, transpiles each component in the transpile worker and
// runs the command components in a pod worker.
//
// Env: EXIT_CODES (default "0 1 3"), the codes for the command components.
import { createServer } from "node:http";
import { readFileSync, existsSync } from "node:fs";
import { createRequire } from "node:module";
import { basename, extname, join } from "node:path";

const { chromium } = createRequire(import.meta.url)("playwright");
const here = new URL(".", import.meta.url).pathname;
const site = join(here, "site");
const types = { ".html": "text/html", ".js": "text/javascript", ".wasm": "application/wasm" };

const components = new Map(); // URL path -> file
const server = createServer((req, res) => {
  const comp = components.get(new URL(req.url, "http://x").pathname);
  if (comp) { res.writeHead(200, { "Content-Type": "application/wasm" }); res.end(readFileSync(comp)); return; }
  let path = join(site, decodeURIComponent(new URL(req.url, "http://x").pathname));
  if (path.endsWith("/")) path = join(path, "index.html");
  if (!path.startsWith(site) || !existsSync(path)) { res.writeHead(404).end(); return; }
  res.writeHead(200, { "Content-Type": types[extname(path)] ?? "application/octet-stream" });
  res.end(readFileSync(path));
}).listen(0, "127.0.0.1");
await new Promise((r) => server.once("listening", r));
const base = `http://127.0.0.1:${server.address().port}/`;

const browser = await chromium.launch();
const page = await browser.newPage();
const requests = [];
page.on("response", (r) => requests.push({ url: r.url().slice(base.length), type: r.headers()["content-type"] }));
page.on("console", (m) => console.log(`console: ${m.text()}`));
await page.goto(base);
console.log(`browser ${browser.version()}, JSPI: ${await page.evaluate(() => typeof WebAssembly.Suspending === "function")}`);

await page.evaluate(() => {
  window.tw = new Worker("assets/transpile.worker.js", { type: "module" });
  let n = 0;
  window.transpile = (name, bytes) => new Promise((resolve) => {
    const id = ++n;
    const on = ({ data }) => { if (data.id === id) { tw.removeEventListener("message", on); resolve(data); } };
    tw.addEventListener("message", on);
    tw.postMessage({ id, name, bytes }, [bytes.buffer]);
  });
  window.runPod = (name, files, args, env) => new Promise((resolve) => {
    const w = new Worker("assets/pod.worker.js", { type: "module" });
    const out = [];
    let loadMs = 0;
    w.onmessage = ({ data }) => {
      if (data.type === "loaded") { loadMs = data.ms; w.postMessage({ type: "run", args, env }); }
      else if (data.type === "stdout" || data.type === "stderr") out.push(`${data.type}: ${data.line}`);
      else if (data.type === "exit" || data.type === "error") { w.terminate(); resolve({ ...data, loadMs, out }); }
    };
    w.postMessage({ type: "load", name, files });
  });
});

const codes = (process.env.EXIT_CODES ?? "0 1 3").split(" ");
for (const file of process.argv.slice(2)) {
  const name = basename(file, ".wasm").replace(/[^a-z0-9]/gi, "");
  const path = `/c/${components.size}/${name}.wasm`;
  components.set(path, file);
  const r = await page.evaluate(async ([n, p]) => {
    const b = new Uint8Array(await (await fetch(p)).arrayBuffer());
    const size = b.length; // transpile() transfers the buffer
    const r = await transpile(n, b);
    window.last = r;
    return { ...r, files: undefined, size };
  }, [name, path]);
  console.log(`\n== ${file} (${r.size} bytes)`);
  if (!r.ok) { console.log(`transpile failed after ${r.ms.toFixed(0)} ms: ${r.error}`); continue; }
  const total = Object.values(r.sizes).reduce((a, b) => a + b, 0);
  console.log(`transpile ${r.ms.toFixed(0)} ms (first ready ${r.firstMs.toFixed(0)} ms after the worker started)`);
  console.log(`output ${total} bytes: ${JSON.stringify(r.sizes)}; bare module names: ${JSON.stringify(r.bare)}`);
  console.log(`exports ${JSON.stringify(r.exports)}`);
  if (!r.exports.some((e) => e.startsWith("wasi:cli/run"))) continue;
  for (const code of codes) {
    const p = await page.evaluate(([n, c]) => runPod(n, last.files, ["prog", "a1"], { EXIT_CODE: c, A: "b" }), [name, code]);
    if (p.type === "error") { console.log(`pod worker error: ${p.error}`); break; }
    console.log(`EXIT_CODE=${code}: exit ${p.code}, load ${p.loadMs.toFixed(0)} ms; ${p.out.filter((l) => !l.includes("env PATH")).join(" | ")}`);
  }
}
console.log("\nrequests:");
for (const r of requests) console.log(`  ${r.url} ${r.type}`);
await browser.close();
server.close();
