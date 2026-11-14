// node site/charts.mjs <charts dir> <out dir>
//
// Packages the chart repositories of charts/ for the page: every
// <charts dir>/<repo>/<chart>/ becomes <out dir>/<repo>/<name>-<version>.tgz,
// next to the index.yaml of <repo>, as "helm package" and "helm repo index"
// would. helm in the terminal fetches well-known repositories without CORS
// headers from here (k8s/tabernetes/helm.go), e.g. charts.bitnami.com/bitnami
// from charts/bitnami/.
//
// A LICENSE next to the charts of a repository (charts/bitnami/LICENSE) goes
// into every chart archive of it that has none, so that the license travels
// with a chart that "helm pull" saves.
//
// The archives are reproducible: files sorted, owner root, the time of the
// last commit of the chart (of HEAD outside git) as mtime and created date.
import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import zlib from "node:zlib";

// Older releases of a chart: the same files with another version and
// appVersion, so that "helm search repo --versions" and "helm upgrade
// --version" have something to show. The appVersion is the image tag.
const OLDER = {
  "bitnami/nginx": [{ version: "22.0.0", appVersion: "1.29.1" }],
};

const [src, out] = process.argv.slice(2).map((p) => p && path.resolve(p));
if (!src || !out) {
  console.error("usage: charts.mjs <charts dir> <out dir>");
  process.exit(2);
}

for (const repo of dirs(src)) {
  const repoLicense = path.join(src, repo, "LICENSE");
  const entries = {};
  let newest = 0;
  for (const chart of dirs(path.join(src, repo))) {
    const dir = path.join(src, repo, chart);
    const mtime = commitTime(dir);
    newest = Math.max(newest, mtime);
    const files = walk(dir).sort();
    const releases = [{}, ...(OLDER[`${repo}/${chart}`] || [])];
    for (const rel of releases) {
      const content = new Map(files.map((f) => [f, fs.readFileSync(path.join(dir, f))]));
      if (!content.has("LICENSE") && fs.existsSync(repoLicense)) content.set("LICENSE", fs.readFileSync(repoLicense));
      const meta = chartYAML(content.get("Chart.yaml").toString());
      if (rel.version) {
        const set = (f, re, v) => {
          const old = content.get(f).toString();
          if (!re.test(old)) throw new Error(`${repo}/${chart}/${f}: no match for ${re}`);
          content.set(f, Buffer.from(old.replace(re, `$1${v}`)));
        };
        set("Chart.yaml", /^(version:\s*).*$/m, rel.version);
        set("Chart.yaml", /^(appVersion:\s*).*$/m, rel.appVersion);
        set("values.yaml", new RegExp(`^(  tag:\\s*)${meta.appVersion.replace(/\./g, "\\.")}$`, "m"), rel.appVersion);
        Object.assign(meta, rel);
      }
      const tgz = zlib.gzipSync(tar([...content].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0)).map(([f, b]) => [`${meta.name}/${f}`, b]), mtime), { level: 9 });
      const file = `${meta.name}-${meta.version}.tgz`;
      fs.mkdirSync(path.join(out, repo), { recursive: true });
      fs.writeFileSync(path.join(out, repo, file), tgz);
      (entries[meta.name] ||= []).push({
        ...meta,
        created: new Date(mtime * 1000).toISOString(),
        digest: createHash("sha256").update(tgz).digest("hex"),
        urls: [file],
      });
    }
  }
  fs.writeFileSync(path.join(out, repo, "index.yaml"), indexYAML(entries, newest));
  console.log(`charts: ${repo}: ${Object.values(entries).flat().map((e) => `${e.name}-${e.version}`).join(", ")}`);
}

function dirs(d) {
  return fs.readdirSync(d, { withFileTypes: true }).filter((e) => e.isDirectory()).map((e) => e.name).sort();
}

// walk lists the files below dir, relative, with "/".
function walk(dir, prefix = "") {
  return fs.readdirSync(path.join(dir, prefix), { withFileTypes: true }).flatMap((e) => {
    const rel = prefix ? `${prefix}/${e.name}` : e.name;
    return e.isDirectory() ? walk(dir, rel) : [rel];
  });
}

// commitTime is the time of the last commit of dir, else of HEAD, in seconds.
function commitTime(dir) {
  for (const args of [["log", "-1", "--format=%ct", "--", "."], ["log", "-1", "--format=%ct"]]) {
    try {
      const t = Number(execFileSync("git", args, { cwd: dir, encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] }).trim());
      if (t) return t;
    } catch { /* not a git checkout */ }
  }
  return 0;
}

// chartYAML reads the top-level scalars of a Chart.yaml.
function chartYAML(s) {
  const meta = {};
  for (const m of s.matchAll(/^(apiVersion|name|version|appVersion|description|type|home|icon):[ \t]*(.*?)[ \t]*$/gm)) {
    meta[m[1]] = m[2].replace(/^"(.*)"$/, "$1");
  }
  for (const k of ["apiVersion", "name", "version", "appVersion"]) {
    if (!meta[k]) throw new Error(`Chart.yaml: no ${k}`);
  }
  return meta;
}

// indexYAML writes the index.yaml of a repository. JSON strings are YAML.
function indexYAML(entries, time) {
  const q = JSON.stringify;
  let y = "apiVersion: v1\nentries:\n";
  for (const name of Object.keys(entries).sort()) {
    y += `  ${name}:\n`;
    for (const e of entries[name]) {
      y += `  - apiVersion: ${q(e.apiVersion)}\n`;
      for (const k of ["appVersion", "created", "description", "digest", "home", "icon", "name", "type"]) {
        if (e[k]) y += `    ${k}: ${q(e[k])}\n`;
      }
      y += `    urls:\n${e.urls.map((u) => `    - ${q(u)}\n`).join("")}    version: ${q(e.version)}\n`;
    }
  }
  return y + `generated: ${q(new Date(time * 1000).toISOString())}\n`;
}

// tar writes a ustar archive of regular files.
function tar(files, mtime) {
  const blocks = [];
  for (const [name, body] of files) {
    const h = Buffer.alloc(512);
    let prefix = "", base = name;
    if (Buffer.byteLength(name) > 100) {
      const i = name.lastIndexOf("/", 155);
      prefix = name.slice(0, i);
      base = name.slice(i + 1);
      if (i < 0 || Buffer.byteLength(base) > 100) throw new Error(`tar: name too long: ${name}`);
    }
    const put = (s, off, len) => h.write(s, off, len, "utf8");
    const oct = (n, len) => n.toString(8).padStart(len - 1, "0") + "\0";
    put(base, 0, 100);
    put(oct(0o644, 8), 100, 8);
    put(oct(0, 8), 108, 8); // uid
    put(oct(0, 8), 116, 8); // gid
    put(oct(body.length, 12), 124, 12);
    put(oct(mtime, 12), 136, 12);
    put("        ", 148, 8); // checksum, computed with spaces
    put("0", 156, 1);
    put("ustar\0" + "00", 257, 8);
    put("root", 265, 32);
    put("root", 297, 32);
    put(prefix, 345, 155);
    let sum = 0;
    for (const b of h) sum += b;
    put(sum.toString(8).padStart(6, "0") + "\0 ", 148, 8);
    blocks.push(h, body, Buffer.alloc((512 - (body.length % 512)) % 512));
  }
  blocks.push(Buffer.alloc(1024));
  return Buffer.concat(blocks);
}
