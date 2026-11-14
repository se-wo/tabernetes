// Unit tests of web/src/files.js: the files adapter of components
// (docs/wasi-sockets.md, section 8.10, FS1 to FS3). The tests use the
// in-memory file system and createFilesystem of the browser build of the
// shim, which WASIShim uses with the option browserFilesystem in the pod
// worker. Node.js resolves the package to its Node.js build, so the tests
// import the browser files by their path.
import { test } from "node:test";
import assert from "node:assert/strict";
import { browserFilesystem, tree, READ_ONLY_ERROR } from "../src/files.js";
import { runCommand } from "../src/component.js";
import { createPeer } from "../src/wasi_messages.js";

const shim = new URL("../node_modules/@bytecodealliance/preview2-shim/dist/browser/", import.meta.url);
const { createFilesystem, InMemoryFilesystemAdapter: Memory } = await import(new URL("filesystem.js", shim));

const enc = (s) => new TextEncoder().encode(s);
const dec = (b) => new TextDecoder().decode(b);

// The copy of wasmPreopens for a container with a writable root, a
// read-only volume /config and a writable volume /rw.
const copy = () => [
  { guest: "/", readOnly: false, files: [["etc/hostname", enc("pod\n")]], dirs: ["etc", "tmp"] },
  { guest: "/config", readOnly: true, files: [["file", enc("from a volume")], ["sub/x", enc("x")]], dirs: ["sub"] },
  { guest: "/rw", readOnly: false, files: [], dirs: [] },
];

function filesystem(preopens = copy()) {
  const fs = createFilesystem(browserFilesystem(preopens, Memory));
  const dirs = Object.fromEntries(fs.preopens.getDirectories().map(([d, guest]) => [guest, d]));
  return { fs, dirs };
}

const follow = { symlinkFollow: true };
const read = (dir, path) => {
  const f = dir.openAt(follow, path, {}, { read: true });
  const [bytes] = f.read(1n << 20n, 0n);
  return dec(bytes);
};
const write = (dir, path, text) => {
  const f = dir.openAt(follow, path, { create: true, truncate: true }, { write: true });
  f.write(enc(text), 0n);
};
const code = (fn) => {
  try {
    fn();
  } catch (e) {
    return e;
  }
  return "no error";
};

test("tree gives the FileData of a preopen", () => {
  const t = tree({ files: [["a/b/c", enc("c")], ["top", enc("t")], ["../x", enc("no")], ["a//d", enc("no")]], dirs: ["e", "a/f"] });
  assert.deepEqual(Object.keys(t.dir).sort(), ["a", "e", "top"]);
  assert.deepEqual(Object.keys(t.dir.a.dir).sort(), ["b", "f"]);
  assert.equal(dec(t.dir.a.dir.b.dir.c.source), "c");
  assert.deepEqual(t.dir.e, { dir: {} });
});

test("FS1: the preopens have the guest paths of the copy", () => {
  const { dirs } = filesystem();
  assert.deepEqual(Object.keys(dirs).sort(), ["/", "/config", "/rw"]);
  assert.equal(read(dirs["/config"], "file"), "from a volume");
  assert.equal(read(dirs["/config"], "sub/x"), "x");
  assert.equal(read(dirs["/"], "etc/hostname"), "pod\n");
  assert.equal(dirs["/"].statAt(follow, "tmp").type, "directory");
  const names = [];
  const s = dirs["/config"].readDirectory();
  for (let e; (e = s.readDirectoryEntry());) names.push(`${e.name}:${e.type}`);
  assert.deepEqual(names, ["file:regular-file", "sub:directory"]);
});

test("FS1: without preopens, the component gets no preopened directories", () => {
  const fs = createFilesystem(browserFilesystem(undefined, Memory));
  assert.deepEqual(fs.preopens.getDirectories(), []);
});

test("FS1: a path cannot go out of its preopen", () => {
  const { dirs } = filesystem();
  assert.equal(code(() => read(dirs["/config"], "../etc/hostname")), "not-permitted");
});

test("FS2: a read-only preopen gives the error code of runwasi for each write", () => {
  const { dirs } = filesystem();
  const ro = dirs["/config"];
  assert.equal(READ_ONLY_ERROR, "io");
  assert.equal(code(() => write(ro, "x", "no")), "io");
  assert.equal(code(() => write(ro, "file", "no")), "io");
  assert.equal(code(() => ro.openAt(follow, "file", {}, { read: true, write: true })), "io");
  assert.equal(code(() => ro.createDirectoryAt("d")), "io");
  assert.equal(code(() => ro.unlinkFileAt("file")), "io");
  assert.equal(code(() => ro.removeDirectoryAt("sub")), "io");
  assert.equal(code(() => ro.symlinkAt("file", "l")), "io");
  // A descriptor that a read-only preopen opened has no writes either.
  const f = ro.openAt(follow, "file", {}, { read: true });
  assert.equal(code(() => f.write(enc("no"), 0n)), "io");
  assert.equal(code(() => f.writeViaStream(0n)), "io");
  assert.equal(code(() => f.appendViaStream()), "io");
  assert.equal(code(() => f.setSize(0n)), "io");
  assert.equal(ro.getFlags().write, false);
  assert.equal(ro.getFlags().mutateDirectory, false);
  const sub = ro.openAt(follow, "sub", { directory: true }, { read: true });
  assert.equal(code(() => write(sub, "y", "no")), "io");
  // A rename or a link between a read-only and a writable preopen.
  write(dirs["/rw"], "w", "w");
  assert.equal(code(() => dirs["/rw"].renameAt("w", ro, "w")), "io");
  assert.equal(code(() => ro.renameAt("file", dirs["/rw"], "file")), "io");
  assert.equal(code(() => dirs["/rw"].linkAt(follow, "w", ro, "w")), "io");
  // The files did not change.
  assert.equal(read(ro, "file"), "from a volume");
  assert.equal(code(() => read(ro, "x")), "no-entry");
  assert.equal(code(() => read(ro, "w")), "no-entry");
});

test("FS3: writes change only the copy of this pod worker", () => {
  const preopens = copy();
  const first = filesystem(preopens);
  write(first.dirs["/rw"], "x", "written\n");
  write(first.dirs["/"], "etc/hostname", "changed\n");
  first.dirs["/rw"].createDirectoryAt("d");
  write(first.dirs["/rw"], "d/y", "y");
  first.dirs["/rw"].renameAt("x", first.dirs["/"], "tmp/x");
  assert.equal(read(first.dirs["/"], "tmp/x"), "written\n");
  assert.equal(read(first.dirs["/rw"], "d/y"), "y");
  assert.equal(read(first.dirs["/"], "etc/hostname"), "changed\n");
  // A restart gets a new copy from the Go side: the old files.
  const second = filesystem(copy());
  assert.equal(code(() => read(second.dirs["/rw"], "x")), "no-entry");
  assert.equal(code(() => read(second.dirs["/"], "tmp/x")), "no-entry");
  assert.equal(read(second.dirs["/"], "etc/hostname"), "pod\n");
});

test("isSameObject compares the files, not the descriptors", () => {
  const { dirs } = filesystem();
  const a = dirs["/config"].openAt(follow, "file", {}, { read: true });
  const b = dirs["/config"].openAt(follow, "file", {}, { read: true });
  assert.equal(a.isSameObject(b), true);
  assert.equal(a.isSameObject(dirs["/config"]), false);
});

test("runCommand gives WASIShim the files of the start message", async () => {
  let config = null;
  class Shim {
    constructor(c) { config = c; }
    getImportObject() { return {}; }
  }
  const peer = createPeer({ post: () => {} });
  const component = { js: "", modules: {}, imports: [], exports: [["wasi:cli/run@0.2.3", "instance"]] };
  const load = async () => ({ instantiate: async () => ({ "wasi:cli/run@0.2.3": { run: async () => {} } }) });
  const msg = { component, args: [], env: [], cwd: "/", preopens: copy() };
  assert.equal(await runCommand(msg, { peer, WASIShim: Shim, load, Memory }), 0);
  const fs = createFilesystem(config.browserFilesystem);
  assert.deepEqual(fs.preopens.getDirectories().map(([, g]) => g).sort(), ["/", "/config", "/rw"]);
  // Without the class of the in-memory file system: no option.
  assert.equal(await runCommand(msg, { peer, WASIShim: Shim, load }), 0);
  assert.equal(config.browserFilesystem, undefined);
});
