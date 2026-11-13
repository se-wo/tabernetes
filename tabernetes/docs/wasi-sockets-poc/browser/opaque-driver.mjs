// opaque-driver.mjs: phase 0, item 16 in headless Chromium. A worker from a
// data: URL has an opaque origin. Can the page send it a compiled
// WebAssembly.Module, and can it import a Blob URL and use JSPI?
import { createServer } from "node:http";
import { createRequire } from "node:module";
const { chromium } = createRequire(import.meta.url)("playwright");
const browser = await chromium.launch();
const page = await browser.newPage();
const server = createServer((req, res) => {
  res.writeHead(200, { "Content-Type": "text/html" });
  res.end(req.url === "/secret" ? "secret of the page origin" : "<p>item 16</p>");
}).listen(0, "127.0.0.1");
await new Promise((r) => server.once("listening", r));
await page.goto(`http://127.0.0.1:${server.address().port}/`);
const r = await page.evaluate(async () => {
  // A module with one function: (func (export "f") (result i32) i32.const 42)
  const bytes = new Uint8Array([0, 97, 115, 109, 1, 0, 0, 0, 1, 5, 1, 96, 0, 1, 127, 3, 2, 1, 0, 7, 5, 1, 1, 102, 0, 0, 10, 6, 1, 4, 0, 65, 42, 11]);
  const mod = await WebAssembly.compile(bytes);
  const src = `onmessage = async ({ data }) => {
    const out = { origin: self.origin, jspi: typeof WebAssembly.Suspending === "function" };
    try { out.module = data instanceof WebAssembly.Module; out.f = (await WebAssembly.instantiate(data)).exports.f(); } catch (e) { out.moduleError = e.name + ": " + e.message; }
    try { indexedDB.open("x"); out.indexedDB = "open"; } catch (e) { out.indexedDB = e.name; }
    try { await caches.keys(); out.caches = "open"; } catch (e) { out.caches = e.name; }
    try { const u = URL.createObjectURL(new Blob(["export const v = 7;"], { type: "text/javascript" })); out.blobImport = (await import(u)).v; } catch (e) { out.blobImport = e.name + ": " + e.message; }
    try { const r = await fetch(PAGE + "secret"); out.pageFetch = r.type + " " + r.status; out.pageText = await r.text(); } catch (e) { out.pageFetch = e.name; }
    postMessage(out);
  };`;
  const w = new Worker("data:text/javascript," + encodeURIComponent(`const PAGE = ${JSON.stringify(location.href)};\n` + src), { type: "module" });
  return new Promise((resolve) => {
    w.onmessage = ({ data }) => resolve(data);
    w.onerror = (e) => resolve({ error: e.message });
    w.onmessageerror = () => resolve({ error: "messageerror: the module did not arrive" });
    w.postMessage(mod);
  });
});
console.log(`browser ${browser.version()}:`, JSON.stringify(r));
await browser.close();
server.close();
