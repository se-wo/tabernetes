// tab.mjs talks to the page that browser.mjs keeps open.
//
//   node tab.mjs run '<command line>'  run it in the tab's shell, print the
//                                      output, exit with its status
//   node tab.mjs join [--kubectl]      type "join" in the terminal, print
//                                      {kind, token, port, name} as JSON
//   node tab.mjs accept '<line>'       paste the "accept ..." line of the node
//   node tab.mjs status                the status line below the terminal
//   node tab.mjs screen [n]            the last n lines of the terminal
//   node tab.mjs logs [n]              the last n lines of the control plane log
//
// Env: CDP_PORT (default 9222), TIMEOUT for run in seconds (default 120).
import { createRequire } from "node:module";

// require() also finds a global install through NODE_PATH; import does not.
const { chromium } = createRequire(import.meta.url)("playwright");

const [cmd, arg] = process.argv.slice(2);
const port = process.env.CDP_PORT || "9222";
const timeout = Number(process.env.TIMEOUT || 120) * 1000;

const browser = await chromium.connectOverCDP(`http://127.0.0.1:${port}`);
let page = null;
for (const p of browser.contexts().flatMap((c) => c.pages())) {
  if (await p.evaluate(() => !!globalThis.tabernetesTerminal).catch(() => false)) page = p;
}
if (!page) {
  console.error("no page with a ready terminal; is browser.mjs running?");
  process.exit(2);
}
// Exit without browser.close(): the browser belongs to browser.mjs.
const done = (out, rc = 0) => {
  if (out) process.stdout.write(out.endsWith("\n") ? out : out + "\n");
  process.exit(rc);
};
const clean = (s) => s.replace(/\r/g, "").replace(/\x1b\[[0-9;?]*[A-Za-z]/g, "");

switch (cmd) {
  case "run": {
    // The same call as exec in terminal.js, but the output is collected here.
    // The shell runs one command line at a time.
    const r = await page.evaluate(async ([src, ms]) => {
      let out = "";
      const t = globalThis.tabernetesTerminal;
      const run = globalThis.tabernetes.shell(src, (s) => (out += s), t.builtins);
      const timer = new Promise((r) => setTimeout(() => r("timeout"), ms));
      const rc = await Promise.race([run, timer]);
      if (rc === "timeout") {
        globalThis.tabernetes.cancel();
        return { out: out + "\n(tab.mjs: timed out, cancelled)\n", rc: 124 };
      }
      return { out, rc };
    }, [arg, timeout]);
    done(clean(r.out), Number(r.rc) || 0);
    break;
  }
  case "join": {
    // join only runs from the real terminal input (it needs a busy terminal
    // to ask for the accept line), so type it there.
    const line = arg === "--kubectl" ? "join --kubectl" : "join";
    await page.evaluate((line) => {
      globalThis.tabernetesLastJoin = null;
      globalThis.tabernetesTerminal.term.input(line + "\r");
    }, line);
    await page.waitForFunction(() => globalThis.tabernetesLastJoin, null, { timeout: 60000 });
    done(JSON.stringify(await page.evaluate(() => globalThis.tabernetesLastJoin)));
    break;
  }
  case "accept": {
    // join waits in "accept line: " and reads the keys.
    await page.evaluate((l) => globalThis.tabernetesTerminal.term.input(l.trim() + "\r"), arg);
    await page.waitForTimeout(1000);
    done(await page.evaluate(() => document.getElementById("tabernetes-status")?.textContent || ""));
    break;
  }
  case "status":
    done(await page.evaluate(() => document.getElementById("tabernetes-status")?.textContent || ""));
    break;
  case "screen": {
    // The text in the terminal, for what join printed.
    const n = Number(arg) || 40;
    done(await page.evaluate((n) => {
      const b = globalThis.tabernetesTerminal.term.buffer.active, rows = [];
      for (let i = 0; i < b.length; i++) rows.push(b.getLine(i)?.translateToString(true) ?? "");
      return rows.join("\n").trimEnd().split("\n").slice(-n).join("\n");
    }, n));
    break;
  }
  case "logs": {
    const n = Number(arg) || 40;
    done(await page.evaluate((n) => (globalThis.tabernetesLogs || []).join("").split("\n").slice(-n).join("\n"), n));
    break;
  }
  default:
    console.error("usage: tab.mjs run '<cmd>' | join [--kubectl] | accept '<line>' | status | screen [n] | logs [n]");
    process.exit(2);
}
