// node site/build.mjs <assets dir> <out dir>
//
// Builds the standalone Tabernetes page (GitHub Pages) from the output of
// "mise run wasm web" in <assets dir>. All URLs are relative, so the site works
// under any base path (https://<owner>.github.io/<repo>/ or a domain root).
// The page keeps the binary in the Cache Storage keyed by its URL; ?v=<hash of
// the content> makes a new build a new URL.
import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const [assets, out] = process.argv.slice(2).map((p) => p && path.resolve(p));
if (!assets || !out) {
  console.error("usage: build.mjs <assets dir> <out dir>");
  process.exit(2);
}
const here = path.dirname(fileURLToPath(import.meta.url));
const tab = path.dirname(here); // tabernetes/
const root = path.dirname(tab); // the repository
// The source of this build, for the footer: owner/repo on GitHub.
const repo = process.env.GITHUB_REPOSITORY || "se-wo/tabernetes";

const hash = (b) => createHash("sha256").update(b).digest("hex").slice(0, 16);
const sri = (b) => `sha384-${createHash("sha384").update(b).digest("base64")}`;
const esc = (s) => String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
const need = (f) => {
  if (!fs.existsSync(f)) throw new Error(`${f} is missing, run "mise run wasm web" in tabernetes/ first`);
  return f;
};

fs.rmSync(out, { recursive: true, force: true });
fs.mkdirSync(out, { recursive: true });
const copy = (name) => {
  const b = fs.readFileSync(need(path.join(assets, name)));
  fs.writeFileSync(path.join(out, name), b);
  return { url: `${name}?v=${hash(b)}`, sri: sri(b), size: b.length };
};
const wasm = copy("tabernetes.wasm.zst");
const js = copy("tabernetes.js");
const unzstd = copy("unzstd.js");
const css = copy("xterm.css");
// Workers that tabernetes.js starts itself; they sit next to it.
const workers = fs.readdirSync(assets).filter((f) => f.endsWith(".worker.js")).sort();
workers.forEach(copy);

// The service worker of the "proxy" command. Its scope is the directory it is
// in, below the page (web/src/proxy.js registers it relative to the page).
fs.mkdirSync(path.join(out, "tabernetes-proxy"));
fs.copyFileSync(path.join(tab, "web/proxy-sw.js"), path.join(out, "tabernetes-proxy/sw.js"));

// The OCI mirror with the WebAssembly images for the tab node, if this
// checkout has them. web/src/wasi.js looks for it in oci/ next to
// tabernetes.js.
const images = path.join(tab, "wasm-images");
const hasImages = fs.existsSync(path.join(images, "build.sh"));
if (hasImages) execFileSync("bash", [path.join(images, "build.sh"), path.join(out, "oci")], { stdio: "inherit", cwd: tab });

// ---- licenses ----
fs.copyFileSync(need(path.join(root, "LICENSE")), path.join(out, "LICENSE.txt"));
const goLicenses = JSON.parse(fs.readFileSync(need(path.join(assets, "THIRD_PARTY_LICENSES.go.json")), "utf8"));
const jsJSON = path.join(assets, "THIRD_PARTY_LICENSES.js.json");
execFileSync(process.execPath, [path.join(here, "third-party.mjs"), "js", path.join(tab, "web"), jsJSON,
  "src/index.js", "src/unzstd.js", ...workers.map((w) => `src/${w}`)], { stdio: "inherit" });
const jsLicenses = JSON.parse(fs.readFileSync(jsJSON, "utf8"));
fs.writeFileSync(path.join(out, "THIRD_PARTY_LICENSES.txt"), thirdParty([goLicenses, jsLicenses], hasImages && imageList()));

// ---- the page ----
const md = fs.readFileSync(path.join(here, "index.md"), "utf8");
const front = /^\+\+\+\n([^]*?)\n\+\+\+\n/.exec(md);
const field = (k) => (front && new RegExp(`^${k}\\s*=\\s*"(.*)"$`, "m").exec(front[1])?.[1]) || "";
const terminal = fill(fs.readFileSync(path.join(here, "terminal.html"), "utf8"), {
  CSS: css.url, CSS_SRI: css.sri, JS: js.url, JS_SRI: js.sri, WORKER: unzstd.url,
  WASM: wasm.url, SIZE: wasm.size, MB: Math.round(wasm.size / 1048576),
});
const content = markdown(front ? md.slice(front[0].length) : md, { tabernetes: terminal });
fs.writeFileSync(path.join(out, "index.html"), fill(fs.readFileSync(path.join(here, "page.html"), "utf8"), {
  TITLE: esc(field("title")), DESCRIPTION: esc(field("description")), CONTENT: content,
  REPO: esc(repo), REPO_URL: esc(`https://github.com/${repo}`),
}));
console.log(`site: ${out} (tabernetes.wasm.zst ${(wasm.size / 1048576).toFixed(1)} MB)`);

// fill replaces %NAME% in a template.
function fill(tpl, vars) {
  return tpl.replace(/%([A-Z_]+)%/g, (m, k) => (k in vars ? String(vars[k]) : m));
}

// markdown renders the little Markdown the page text uses: paragraphs,
// headings, lists, `code`, **bold**, *em* and [links](url). A paragraph
// {{< name >}} is replaced by shortcodes[name].
function markdown(src, shortcodes) {
  const inline = (s) => esc(s)
    .replace(/`([^`]+)`/g, "<code>$1</code>")
    .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
    .replace(/\*([^*]+)\*/g, "<em>$1</em>")
    .replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, '<a href="$2">$1</a>');
  return src.trim().split(/\n\s*\n/).map((block) => {
    block = block.trim();
    const sc = /^\{\{<\s*(\w+)\s*>\}\}$/.exec(block);
    if (sc) {
      if (!(sc[1] in shortcodes)) throw new Error(`unknown shortcode ${sc[1]}`);
      return shortcodes[sc[1]];
    }
    const h = /^(#{1,6})\s+(.*)$/.exec(block);
    if (h) return `<h${h[1].length}>${inline(h[2])}</h${h[1].length}>`;
    if (/^[-*]\s/.test(block)) {
      const items = block.split(/\n(?=[-*]\s)/).map((i) => `<li>${inline(i.replace(/^[-*]\s+/, "").replace(/\n\s*/g, " "))}</li>`);
      return `<ul>\n${items.join("\n")}\n</ul>`;
    }
    return `<p>${inline(block.replace(/\n\s*/g, " "))}</p>`;
  }).join("\n\n");
}

// imageList describes the images of wasm-images/images.txt for
// THIRD_PARTY_LICENSES.txt.
function imageList() {
  const lines = fs.readFileSync(path.join(images, "images.txt"), "utf8").split("\n")
    .map((l) => l.trim().split(/\s+/)).filter(([kind]) => kind && !kind.startsWith("#"))
    // Only what made it into the mirror: <mirror>/<repository>/manifests/<tag>.
    .filter(([, ref]) => {
      const m = /^(.*?)(?::([\w.-]+))?$/.exec(ref);
      return fs.existsSync(path.join(out, "oci", m[1], "manifests", m[2] || "latest"));
    });
  return lines.map(([kind, ref, src]) => kind === "mirror"
    ? `${ref}\n  copied unchanged from its registry; it keeps the license of its source.\n`
    : `${ref}\n  built from tabernetes/wasm-images/${src} with Go (GOOS=wasip1): Tabernetes code\n  (CC BY-NC-SA 4.0) and the Go standard library (license [1] and [2] below).\n`).join("");
}

// thirdParty writes THIRD_PARTY_LICENSES.txt: the list of components, then
// each distinct license text once.
function thirdParty(groups, images) {
  const texts = new Map(); // normalized text -> { n, name, text }
  const ref = (f) => {
    const key = f.text.replace(/\s+/g, " ").trim();
    if (!texts.has(key)) texts.set(key, { n: texts.size + 1, name: f.name, text: f.text });
    return `[${texts.get(key).n}] ${f.name}`;
  };
  const rule = "=".repeat(78);
  let s = `Third-party software in the Tabernetes page
${rule}

Tabernetes itself (its own code and text) is licensed under CC BY-NC-SA 4.0,
see LICENSE.txt and https://creativecommons.org/licenses/by-nc-sa/4.0/.
Created by Jan-Otto Kröpke (https://github.com/jkroepke), modified in
https://github.com/${repo}.

The page also contains the third-party software listed below. It is NOT
covered by the Creative Commons license: each component keeps its own license,
whose text follows the list. Tabernetes changes some files of Kubernetes when it
builds (tabernetes/k8s/patch.go); the changed files remain under the Apache
License 2.0.
`;
  for (const g of groups) {
    s += `\n\n${g.title}\n${"-".repeat(g.title.length)}\n\n`;
    for (const e of g.entries) {
      s += `${e.name}${e.version ? ` ${e.version}` : ""}${e.license ? ` (${e.license})` : ""}\n  ${e.url}\n`;
      s += e.files.map((f) => `  ${ref(f)}\n`).join("");
    }
  }
  if (images) {
    const t = "WebAssembly images in oci/ (the OCI mirror of the page)";
    s += `\n\n${t}\n${"-".repeat(t.length)}\n\n${images}`;
  }
  s += `\n\nLicense texts\n${rule}\n`;
  for (const t of texts.values()) s += `\n[${t.n}] ${t.name}\n${"-".repeat(78)}\n${t.text}\n`;
  return s;
}
