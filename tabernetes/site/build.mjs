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
// The transpile worker of components (docs/wasi-sockets.md, section 8.2) and
// the four core modules of jco-transpile, which it loads from ../vendor/
// relative to itself with WebAssembly.compileStreaming. The server must send
// them as application/wasm: GitHub Pages and other static servers do this
// for the extension .wasm.
for (const dir of ["transpile", "vendor"]) {
  const files = fs.readdirSync(need(path.join(assets, dir))).sort();
  if (dir === "vendor" && files.filter((f) => /\.core\d*\.wasm$/.test(f)).length !== 4) {
    throw new Error(`${path.join(assets, dir)} must have the four core modules of jco-transpile`);
  }
  fs.mkdirSync(path.join(out, dir));
  for (const f of files) copy(path.join(dir, f));
}

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

// The chart repositories that helm in the terminal fetches from charts/ next
// to tabernetes.js instead of servers without CORS headers (charts.bitnami.com).
execFileSync(process.execPath, [path.join(here, "charts.mjs"), path.join(tab, "charts"), path.join(out, "charts")], { stdio: "inherit" });

// ---- licenses ----
fs.copyFileSync(need(path.join(root, "LICENSE")), path.join(out, "LICENSE.txt"));
const goLicenses = JSON.parse(fs.readFileSync(need(path.join(assets, "THIRD_PARTY_LICENSES.go.json")), "utf8"));
const jsJSON = path.join(assets, "THIRD_PARTY_LICENSES.js.json");
execFileSync(process.execPath, [path.join(here, "third-party.mjs"), "js", path.join(tab, "web"), jsJSON,
  "src/index.js", "src/unzstd.js", ...workers.map((w) => `src/${w}`), "src/transpile.worker.js"], { stdio: "inherit" });
const jsLicenses = JSON.parse(fs.readFileSync(jsJSON, "utf8"));
const groups = [goLicenses, jsLicenses, manifests(goLicenses), reproduced(goLicenses)];
// The Rust crates of the components in oci/, if build.sh built any.
if (hasImages && imageLines().some(([kind]) => kind === "component")) {
  const rustJSON = path.join(assets, "THIRD_PARTY_LICENSES.rust.json");
  execFileSync(process.execPath, [path.join(here, "third-party.mjs"), "rust", path.join(images, "rust", "Cargo.toml"), "wasm32-wasip2", rustJSON], { stdio: "inherit" });
  groups.push(JSON.parse(fs.readFileSync(rustJSON, "utf8")));
}
fs.writeFileSync(path.join(out, "THIRD_PARTY_LICENSES.txt"), thirdParty(groups, hasImages && imageList()));

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

// imageLines are the images of wasm-images/images.txt that made it into the
// mirror (<mirror>/<repository>/manifests/<tag>), as [kind, ref, src].
function imageLines() {
  return fs.readFileSync(path.join(images, "images.txt"), "utf8").split("\n")
    .map((l) => l.trim().split(/\s+/)).filter(([kind]) => kind && !kind.startsWith("#"))
    .filter(([, ref]) => {
      const m = /^(.*?)(?::([\w.-]+))?$/.exec(ref);
      return fs.existsSync(path.join(out, "oci", m[1], "manifests", m[2] || "latest"));
    });
}

// imageList describes the images of the mirror for THIRD_PARTY_LICENSES.txt.
function imageList() {
  const describe = {
    mirror: () => "copied unchanged from its registry; it keeps the license of its source.",
    component: (src) => `built from tabernetes/wasm-images/rust/${src} with Rust (wasm32-wasip2): Tabernetes code\n  (CC BY-NC-SA 4.0), the Rust standard library and the crates listed under\n  "Rust crates compiled into the components of oci/" above.`,
    // image and artifact
    go: (src) => `built from tabernetes/wasm-images/${src} with Go (GOOS=wasip1): Tabernetes code\n  (CC BY-NC-SA 4.0) and the Go standard library (listed first above).`,
  };
  return imageLines().map(([kind, ref, src]) => `${ref}\n  ${(describe[kind] || describe.go)(src)}\n`).join("");
}

// apacheLicense is the text of the Apache License 2.0, from the Kubernetes
// entry of the Go modules.
function apacheLicense(go) {
  const apache = go.entries.find((e) => e.name === "k8s.io/kubernetes")?.files.find((f) => f.name === "LICENSE");
  if (!apache || !/Apache License/.test(apache.text)) throw new Error("no Apache License in the k8s.io/kubernetes entry of the Go modules");
  return { name: "LICENSE", text: apache.text };
}

// manifests lists the upstream Kubernetes manifests that tabernetes.wasm
// embeds (k8s/tabernetes/addons/): adapted copies, all under the Apache
// License 2.0.
function manifests(go) {
  const file = apacheLicense(go);
  const m = (name, url, what) => ({ name: `${name}: ${what}`, version: "", license: "Apache-2.0", url, files: [file] });
  return {
    title: "Kubernetes manifests embedded in tabernetes.wasm, adapted from their projects",
    entries: [
      m("coredns.yaml, kube-proxy.yaml", "https://github.com/kubernetes/kubernetes", "CoreDNS and kube-proxy as kubeadm deploys them"),
      m("flannel.yaml", "https://github.com/flannel-io/flannel", "kube-flannel.yml of flannel"),
      m("ingress-nginx.yaml", "https://github.com/kubernetes/ingress-nginx", "the bare-metal deploy.yaml of ingress-nginx"),
      m("kube-network-policies.yaml", "https://github.com/kubernetes-sigs/kube-network-policies", "install-cnp.yaml of kube-network-policies"),
      m("clusternetworkpolicy-crd.yaml", "https://github.com/kubernetes-sigs/network-policy-api", "the ClusterNetworkPolicy CRD of network-policy-api"),
      m("local-path-storage.yaml", "https://github.com/rancher/local-path-provisioner", "local-path-storage.yaml of local-path-provisioner"),
      m("metrics-server.yaml", "https://github.com/kubernetes-sigs/metrics-server", "components.yaml of metrics-server"),
      // Not a copy: written for Tabernetes. Only the image is KubeDoom's.
      {
        name: "doom/kubedoom.yaml: written for Tabernetes; it runs the KubeDoom image (GPL-3.0), which the cluster\n  pulls from ghcr.io/storax/kubedoom when doom runs and which is not part of this page",
        version: "", license: "", url: "https://github.com/storax/kubedoom", files: [],
      },
    ],
  };
}

// reproduced lists the upstream files that the built-in images of the tab
// node contain (k8s/tabernetes/tabnode_images*.go), with the license texts of
// site/licenses/.
function reproduced(go) {
  const apache = apacheLicense(go);
  const lic = (f, name) => ({ name, text: fs.readFileSync(path.join(here, "licenses", f), "utf8").replace(/\r\n/g, "\n").trimEnd() });
  const e = (name, license, url, files) => ({ name, version: "", license, url, files });
  return {
    title: "Files of upstream images in the built-in images of the tab node",
    entries: [
      e("nginx 1.29.3: the default pages, nginx.conf, default.conf and mime.types", "BSD-2-Clause", "https://github.com/nginx/nginx", [lic("nginx-LICENSE", "nginx LICENSE")]),
      e("the official nginx image: its configuration", "BSD-2-Clause", "https://github.com/nginx/docker-nginx", [lic("docker-nginx-LICENSE", "docker-nginx LICENSE")]),
      e("nginxdemos/hello: its server configurations and page", "Apache-2.0", "https://github.com/nginxinc/NGINX-Demos", [apache]),
      e("Apache HTTP Server: httpd.conf, shortened", "Apache-2.0", "https://github.com/apache/httpd", [apache, lic("httpd-NOTICE", "Apache HTTP Server NOTICE")]),
      e("the official httpd image: httpd-foreground", "Apache-2.0", "https://github.com/docker-library/httpd", [apache]),
      e("ingress-nginx: the beginning of its nginx.conf template", "Apache-2.0", "https://github.com/kubernetes/ingress-nginx", [apache]),
    ],
  };
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
