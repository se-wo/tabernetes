// Collects the licenses of the third-party code in the published page.
//
//   node site/third-party.mjs go <kubernetes checkout> <package> <out.json>
//     The Go modules compiled into the wasm (go list -deps, js/wasm), with the
//     license files from the vendor tree, and the Go standard library. Runs in
//     the wasm task, the only time the Kubernetes checkout is there.
//   node site/third-party.mjs js <web dir> <out.json> <entry>...
//     The npm packages that esbuild bundles from the entries, and the Helm
//     charts of charts/bitnami/ (adapted from Bitnami's, Apache-2.0).
//
// build.mjs merges both into THIRD_PARTY_LICENSES.txt. Tabernetes' own code
// and text are CC BY-NC-SA 4.0 (LICENSE), the code listed here keeps its own
// license.
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { createRequire } from "node:module";

const LICENSE_FILE = /^(LICEN[CS]E|COPYING|NOTICE|PATENTS|AUTHORS)([.-][\w.-]*)?$/i;
const NOT_A_LICENSE = /\.(go|js|mjs|ts|json|sh)$/i;

// licenseFiles returns the license files directly in dir.
const licenseFiles = (dir) => {
  try {
    return fs.readdirSync(dir).filter((f) => LICENSE_FILE.test(f) && !NOT_A_LICENSE.test(f)).sort()
      .map((f) => path.join(dir, f)).filter((f) => fs.statSync(f).isFile());
  } catch {
    return [];
  }
};

const read = (f) => fs.readFileSync(f, "utf8").replace(/\r\n/g, "\n").trimEnd();

function goModules(k8s, pkg, out) {
  k8s = path.resolve(k8s);
  const env = { ...process.env, GOOS: "js", GOARCH: "wasm", GOFLAGS: "-mod=vendor" };
  const fmt = "{{.ImportPath}}\t{{.Dir}}\t{{.Standard}}\t{{with .Module}}{{.Path}}\t{{.Version}}\t{{.Dir}}{{end}}";
  const lines = execFileSync("go", ["list", "-deps", "-f", fmt, pkg], { cwd: k8s, env, encoding: "utf8", maxBuffer: 64 << 20 })
    .trim().split("\n").map((l) => l.split("\t"));
  const goroot = execFileSync("go", ["env", "GOROOT"], { cwd: k8s, env, encoding: "utf8" }).trim();
  const goversion = execFileSync("go", ["env", "GOVERSION"], { cwd: k8s, env, encoding: "utf8" }).trim();
  let k8sVersion = "";
  try {
    k8sVersion = execFileSync("git", ["describe", "--tags", "--exact-match"], { cwd: k8s, encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] }).trim();
  } catch { /* not a tagged checkout */ }

  const modules = new Map(); // path -> { version, root, forks: Set(dir) }
  for (const [importPath, dir, std, modPath, version, modDir] of lines) {
    if (std === "true" || !modPath) continue;
    let m = modules.get(modPath);
    if (!m) {
      // In vendor mode a vendored module has no Dir; staging modules (replace
      // to a directory) and the main module have one.
      const root = modDir || path.join(k8s, "vendor", modPath);
      modules.set(modPath, (m = { version: version || "", root, forks: new Set() }));
    }
    // Code forked into a module (third_party/forked/...) brings its own license.
    if (importPath.includes("/third_party/")) {
      for (let d = dir; d.startsWith(m.root + path.sep); d = path.dirname(d)) {
        if (licenseFiles(d).length) { m.forks.add(d); break; }
      }
    }
  }

  const entries = [{
    name: "Go standard library and runtime (also wasm_exec.js)",
    version: goversion,
    url: "https://go.dev/",
    files: licenseFiles(goroot).filter((f) => /LICENSE|PATENTS/.test(path.basename(f))).map((f) => ({ name: path.basename(f), text: read(f) })),
  }];
  const missing = [];
  for (const [modPath, m] of [...modules].sort(([a], [b]) => a.localeCompare(b))) {
    // A module without a license file of its own (a nested module like
    // github.com/go-openapi/swag/conv) is covered by the one of its parent
    // directory in the repository.
    let files = [];
    for (let d = m.root; !files.length && d.startsWith(path.join(k8s, "vendor") + path.sep); d = path.dirname(d)) files = licenseFiles(d);
    if (!files.length) files = licenseFiles(m.root);
    if (!files.length) files = licenseFiles(path.join(k8s, "LICENSES", "vendor", modPath));
    if (!files.length) { missing.push(modPath); continue; }
    const rel = (f) => path.relative(m.root, f).split(path.sep).join("/");
    const entry = {
      name: modPath,
      version: m.version || (modPath.startsWith("k8s.io/") ? `Kubernetes ${k8sVersion || ""} source tree`.replace("  ", " ") : ""),
      url: `https://pkg.go.dev/${modPath}`,
      files: files.map((f) => ({ name: path.basename(f), text: read(f) })),
    };
    for (const d of [...m.forks].sort()) {
      for (const f of licenseFiles(d)) entry.files.push({ name: `${rel(f)} (forked code)`, text: read(f) });
    }
    entries.push(entry);
  }
  if (missing.length) throw new Error(`no license file found for: ${missing.join(", ")}`);
  fs.writeFileSync(out, JSON.stringify({ title: `Go modules compiled into tabernetes.wasm.zst (${pkg})`, entries }, null, 1));
}

function jsPackages(web, out, entryPoints) {
  web = path.resolve(web);
  const esbuild = createRequire(path.join(web, "package.json"))("esbuild");
  const pkgs = new Map(); // dir -> package.json
  for (const entry of entryPoints) {
    const r = esbuild.buildSync({
      entryPoints: [path.resolve(web, entry)], bundle: true, write: false, metafile: true, outdir: "/nonexistent",
      format: "esm", target: "es2022", logLevel: "silent", absWorkingDir: web,
      // jco-transpile imports node:* only in Node.js (the transpile worker).
      external: ["node:*"],
    });
    for (const input of Object.keys(r.metafile.inputs)) {
      const m = /^(.*node_modules\/)((?:@[^/]+\/)?[^/]+)\//.exec(input);
      if (m) pkgs.set(fs.realpathSync(path.join(web, m[1], m[2])), null);
    }
  }
  // xterm.css is copied from the xterm package.
  pkgs.set(fs.realpathSync(path.join(web, "node_modules/@xterm/xterm")), null);
  const pjs = new Map([...pkgs.keys()].map((dir) => [dir, JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8"))]));
  const repoOf = (pj) => (typeof pj.repository === "string" ? pj.repository : pj.repository?.url);
  const entries = [...pkgs.keys()].sort().map((dir) => {
    const pj = pjs.get(dir);
    const repo = repoOf(pj);
    let files = licenseFiles(dir);
    // A package without a license file (@bytecodealliance/jco) gets the
    // files of another bundled package from the same repository with the
    // same license (@bytecodealliance/jco-transpile).
    if (!files.length && repo) {
      const sibling = [...pjs].find(([d, o]) => d !== dir && repoOf(o) === repo && o.license === pj.license && licenseFiles(d).length);
      if (sibling) files = licenseFiles(sibling[0]);
    }
    if (!files.length) throw new Error(`no license file in ${dir} (package.json says ${pj.license})`);
    return {
      name: pj.name, version: pj.version, license: pj.license,
      url: pj.homepage || repo?.replace(/^git\+/, "") || `https://www.npmjs.com/package/${pj.name}`,
      files: files.map((f) => ({ name: path.basename(f), text: read(f) })),
    };
  });
  // The Helm charts that helm in the terminal installs (charts/bitnami/) are
  // reduced and modified copies of charts of github.com/bitnami/charts. Their
  // license is in charts/bitnami/LICENSE, outside the chart directories.
  const chartLicense = licenseFiles(path.join(web, "..", "charts", "bitnami"));
  if (!chartLicense.length) throw new Error("no LICENSE in charts/bitnami/");
  entries.push({
    name: "Bitnami charts nginx, apache and redis (reduced and modified)",
    version: "",
    license: "Apache-2.0",
    url: "https://github.com/bitnami/charts",
    files: chartLicense.map((f) => ({ name: `bitnami charts ${path.basename(f)}`, text: read(f) })),
  });
  fs.writeFileSync(out, JSON.stringify({ title: "npm packages bundled into the page (tabernetes.js, workers, xterm.css) and the Helm charts in charts/", entries }, null, 1));
}

const [mode, ...args] = process.argv.slice(2);
if (mode === "go" && args.length === 3) goModules(...args);
else if (mode === "js" && args.length >= 3) jsPackages(args[0], args[1], args.slice(2));
else {
  console.error("usage: third-party.mjs go <kubernetes checkout> <package> <out.json> | js <web dir> <out.json> <entry>...");
  process.exit(2);
}
