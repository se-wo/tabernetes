// browser.mjs starts headless Chromium with the Tabernetes page and keeps it
// running. tab.mjs connects to it on the DevTools port and runs commands.
//
//   node browser.mjs [url]
//
// Env: SITE_URL (default http://127.0.0.1:8080/#boot; #boot starts the
// download of the binary without a click), CDP_PORT (default 9222),
// CONSOLE_LOG (default browser-console.log): every console line of the page.
import { appendFileSync } from "node:fs";
import { createRequire } from "node:module";

// require() also finds a global install through NODE_PATH; import does not.
const { chromium } = createRequire(import.meta.url)("playwright");

const url = process.argv[2] || process.env.SITE_URL || "http://127.0.0.1:8080/#boot";
const port = process.env.CDP_PORT || "9222";
const logFile = process.env.CONSOLE_LOG || "browser-console.log";
const log = (s) => appendFileSync(logFile, `${new Date().toISOString()} ${s}\n`);

const browser = await chromium.launch({
  args: [
    `--remote-debugging-port=${port}`,
    // A headless tab counts as hidden: keep its timers and the Wasm runtime at
    // full speed.
    "--disable-background-timer-throttling",
    "--disable-renderer-backgrounding",
    "--disable-backgrounding-occluded-windows",
  ],
});
const page = await browser.newPage();
page.on("console", (m) => log(`[${m.type()}] ${m.text()}`));
page.on("pageerror", (e) => log(`[pageerror] ${e.stack || e}`));
page.on("crash", () => log("[crash] the page crashed"));
await page.goto(url);
console.log(`loading ${url}`);
try {
  // index.js sets tabernetesTerminal once the control plane is ready.
  await page.waitForFunction(() => globalThis.tabernetesTerminal?.exec, null, { timeout: 300000 });
} catch (e) {
  const dump = await page.evaluate(() => ({
    logs: (globalThis.tabernetesLogs || []).join("").slice(-4000),
    crash: (globalThis.tabernetesCrash || []).join("").slice(0, 4000),
  }));
  console.error(`the terminal did not start: ${e.message}\n${dump.crash || dump.logs}`);
  process.exit(1);
}
console.log(`ready, DevTools on 127.0.0.1:${port}; stop with Ctrl-C`);
process.on("SIGINT", () => browser.close().then(() => process.exit(0)));
process.on("SIGTERM", () => browser.close().then(() => process.exit(0)));
