// run.mjs runs the end-to-end tests of the page:
//
//   node web/test/e2e/run.mjs [node --test options] [test files]
//
// It serves the built page (.build/site, from `mise run site`) on
// 127.0.0.1, starts it in headless Chromium with
// docs/docker-node-check/browser.mjs, runs the test files with node:test (all
// *.test.mjs of this directory if none are given), one after the other, and
// stops the browser. If a browser already answers on CDP_PORT, it uses that
// page and starts nothing. See README.md.
//
// Env: SITE_DIR (default .build/site), SITE_PORT (default 8080), CDP_PORT
// (default 9222), E2E_OUT (default .build/e2e): the console of the page,
// the log of the control plane and the terminal after the tests.
import { spawn } from "node:child_process";
import fs from "node:fs";
import http from "node:http";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(here, "../../.."); // tabernetes/
const siteDir = path.resolve(process.env.SITE_DIR || path.join(root, ".build/site"));
const sitePort = Number(process.env.SITE_PORT || 8080);
const cdpPort = process.env.CDP_PORT || "9222";
const out = path.resolve(process.env.E2E_OUT || path.join(root, ".build/e2e"));
fs.mkdirSync(out, { recursive: true });

const cdpUp = () => fetch(`http://127.0.0.1:${cdpPort}/json/version`).then((r) => r.ok, () => false);

let server = null, browser = null;
const stop = () => {
  browser?.kill("SIGTERM");
  server?.close();
  server?.closeAllConnections();
};

if (await cdpUp()) {
  console.log(`using the browser on DevTools port ${cdpPort}`);
} else {
  if (!fs.existsSync(path.join(siteDir, "index.html"))) {
    console.error(`${siteDir}/index.html is missing: run "mise run site" first`);
    process.exit(2);
  }
  server = await serve(siteDir, sitePort);
  const url = `http://127.0.0.1:${sitePort}/#boot`;
  browser = spawn(process.execPath, [path.join(root, "docs/docker-node-check/browser.mjs"), url], {
    env: { ...process.env, CDP_PORT: cdpPort, CONSOLE_LOG: path.join(out, "browser-console.log") },
    stdio: ["ignore", "pipe", "inherit"],
  });
  const ready = await new Promise((resolve) => {
    let buf = "";
    browser.stdout.on("data", (d) => {
      process.stdout.write(d);
      buf += d;
      if (/^ready/m.test(buf)) resolve(true);
    });
    browser.on("exit", () => resolve(false));
  });
  if (!ready) {
    console.error("browser.mjs stopped before the terminal was ready");
    stop();
    process.exit(1);
  }
}

// The tests. --test-concurrency=1: the shell of the tab runs one command line
// at a time.
const args = process.argv.slice(2);
if (!args.some((a) => a.endsWith(".mjs"))) {
  args.push(...fs.readdirSync(here).filter((f) => f.endsWith(".test.mjs")).sort().map((f) => path.join(here, f)));
}
const tests = spawn(process.execPath, ["--test", "--test-concurrency=1", ...args], {
  env: { ...process.env, CDP_PORT: cdpPort },
  stdio: "inherit",
});
const rc = await new Promise((resolve) => tests.on("exit", (code, sig) => resolve(code ?? (sig ? 1 : 0))));

// What the page shows after the tests, for a look at a failure.
const tabScript = path.join(root, "docs/docker-node-check/tab.mjs");
for (const [file, cmd] of [["control-plane.log", ["logs", "100000"]], ["terminal.txt", ["screen", "100000"]]]) {
  await new Promise((resolve) => {
    const p = spawn(process.execPath, [tabScript, ...cmd], {
      env: { ...process.env, CDP_PORT: cdpPort },
      stdio: ["ignore", fs.openSync(path.join(out, file), "w"), "inherit"],
    });
    p.on("exit", resolve);
  });
}
console.log(`logs of the page in ${out}`);
stop();
process.exit(rc);

// serve serves dir on 127.0.0.1:port, like python3 -m http.server.
function serve(dir, port) {
  const types = {
    ".html": "text/html; charset=utf-8", ".js": "text/javascript", ".mjs": "text/javascript",
    ".css": "text/css", ".json": "application/json", ".txt": "text/plain; charset=utf-8",
    ".svg": "image/svg+xml", ".png": "image/png", ".wasm": "application/wasm",
  };
  const srv = http.createServer((req, res) => {
    let p = decodeURIComponent(new URL(req.url, "http://x").pathname);
    if (p.endsWith("/")) p += "index.html";
    const file = path.join(dir, path.normalize(p));
    if (!file.startsWith(dir + path.sep) || !fs.statSync(file, { throwIfNoEntry: false })?.isFile()) {
      res.writeHead(404).end("not found");
      return;
    }
    res.writeHead(200, {
      "Content-Type": types[path.extname(file)] || "application/octet-stream",
      "Content-Length": fs.statSync(file).size,
    });
    if (req.method === "HEAD") res.end();
    else fs.createReadStream(file).pipe(res);
  });
  return new Promise((resolve, reject) => {
    srv.on("error", reject);
    srv.listen(port, "127.0.0.1", () => resolve(srv));
  });
}
