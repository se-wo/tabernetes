// The steps of the transpile worker (docs/wasi-sockets.md, section 8.2),
// without the worker, so that the unit tests can run them in Node.js:
//
//  1. Transpile with instantiation "async" and the lists of section 8.5.
//  2. Read the names of all imports and exports in the result.
//  3. Make sure that each name obeys the WIT syntax (CO15, CO16).
//  4. If a name does not obey it, discard the result and give an error.
//
// The generated JavaScript is only text here: nothing on this side runs it
// (CO14). The pod worker runs it.
import { ASYNC_IMPORTS, asyncExports } from "./async_lists.js";

// A label of WIT: words of lower-case or of upper-case letters and digits,
// joined with "-". Each word starts with a letter.
const word = "(?:[a-z][a-z0-9]*|[A-Z][A-Z0-9]*)";
const label = `${word}(?:-${word})*`;
const ident = "[0-9A-Za-z-]+";
const semver = `(?:0|[1-9][0-9]*)\\.(?:0|[1-9][0-9]*)\\.(?:0|[1-9][0-9]*)(?:-${ident}(?:\\.${ident})*)?(?:\\+${ident}(?:\\.${ident})*)?`;
// A plain name (a function or an instance), a function name with an
// annotation, or an interface name such as wasi:cli/run@0.2.3.
const NAME = new RegExp(
  `^(?:${label}` +
  `|\\[(?:async )?(?:method|static)\\]${label}\\.${label}` +
  `|\\[constructor\\]${label}` +
  `|\\[async\\]${label}` +
  `|${label}(?::${label})+(?:/${label})+(?:@${semver})?)$`,
);

// witName reports whether name obeys the WIT syntax for the names of
// imports and exports.
export function witName(name) {
  return typeof name === "string" && name.length <= 1024 && NAME.test(name);
}

// isLabel reports whether name is a plain WIT label, for example the
// function that an entrypoint names.
export function isLabel(name) {
  return typeof name === "string" && new RegExp(`^${label}$`).test(name);
}

// bareImports gives the module names of static and dynamic imports in js
// that are not a relative path or a URL. A Blob URL cannot find them.
export function bareImports(js) {
  const specs = [
    ...js.matchAll(/^\s*(?:import|export)\b[^;'"]*?\bfrom\s*["']([^"']+)["']/gm),
    ...js.matchAll(/^\s*import\s*["']([^"']+)["']/gm),
    ...js.matchAll(/\bimport\(\s*["']([^"']+)["']\s*\)/g),
  ].map((m) => m[1]);
  return specs.filter((s) => !/^(\.{0,2}\/|[a-z]+:)/.test(s));
}

const NAME_OF_RESULT = "component";

// transpile runs generate of jco for the bytes of a component. It resolves
// to {js, modules: {file: Uint8Array}, imports, exports}, or to {error}.
// The entrypoint is the function that the entrypoint of the image names, or
// "".
export async function transpile(generate, bytes, entrypoint = "") {
  if (!isLabel(entrypoint)) entrypoint = "";
  let out;
  try {
    out = await generate(bytes, {
      name: NAME_OF_RESULT,
      noTypescript: true,
      instantiation: { tag: "async" },
      asyncMode: { tag: "jspi", val: { imports: [...ASYNC_IMPORTS], exports: asyncExports(entrypoint) } },
    });
  } catch (e) {
    return { error: `jco cannot transpile the component: ${String(e?.payload ?? e?.message ?? e)}` };
  }
  return check(out);
}

// check makes sure that the result of generate is correct and that all its
// names obey the WIT syntax. It gives the result for wasi.js, or {error}.
export function check(out) {
  const bad = (why) => ({ error: `the result of jco is not correct: ${why}` });
  if (!out || !Array.isArray(out.files) || !Array.isArray(out.imports) || !Array.isArray(out.exports)) return bad("it has no files, imports or exports");
  const names = [];
  for (const i of out.imports) names.push(["import", i]);
  for (const e of out.exports) {
    if (!Array.isArray(e) || (e[1] !== "function" && e[1] !== "instance")) return bad(`export ${JSON.stringify(e)} has no kind`);
    names.push(["export", e[0]]);
  }
  for (const [kind, n] of names) {
    if (!witName(n)) {
      return { error: `the component has an ${kind} with a name that does not obey the WIT syntax: ${JSON.stringify(String(n)).slice(0, 200)}` };
    }
  }
  let js = null;
  const modules = {};
  for (const f of out.files) {
    const [file, data] = Array.isArray(f) ? f : [];
    if (!(data instanceof Uint8Array)) return bad(`file ${JSON.stringify(file)} has no bytes`);
    if (file === `${NAME_OF_RESULT}.js`) js = new TextDecoder().decode(data);
    else if (/^component\.core[0-9]*\.wasm$/.test(file)) modules[file] = data;
    else return bad(`unexpected file ${JSON.stringify(String(file)).slice(0, 100)}`);
  }
  if (js === null) return bad(`${NAME_OF_RESULT}.js is missing`);
  const bare = bareImports(js);
  if (bare.length) return bad(`the JavaScript imports the bare module names ${bare.slice(0, 5).join(", ")}`);
  return { js, modules, imports: [...out.imports], exports: out.exports.map(([n, k]) => [n, k]) };
}
