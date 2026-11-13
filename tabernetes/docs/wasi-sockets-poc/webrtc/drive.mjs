// drive.mjs <rtc binary>: starts the pion side of main.go and opens its page
// in headless Chromium. Chromium shows host IPs, not mDNS names, so pion can
// reach it without STUN.
import { spawn } from "node:child_process";
import { createRequire } from "node:module";
const { chromium } = createRequire(new URL("../browser/", import.meta.url).pathname)("playwright");
const p = spawn(process.argv[2], [], { stdio: ["ignore", "pipe", "inherit"] });
let url;
p.stdout.on("data", (d) => { process.stdout.write(d); const m = /URL (\S+)/.exec(d); if (m && !url) { url = m[1]; go(); } });
async function go() {
  const b = await chromium.launch({ args: ["--disable-features=WebRtcHideLocalIpsWithMdns"] });
  const pg = await b.newPage();
  pg.on("console", (m) => console.log("console:", m.text())); pg.on("pageerror", (e) => console.log("pageerror:", e.message));
  await pg.goto(url);
  p.on("exit", async (c) => { await b.close(); process.exit(c ?? 0); });
}
