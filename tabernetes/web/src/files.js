// The files of a component (docs/wasi-sockets.md, section 8.10): the option
// browserFilesystem of WASIShim over the copy that wasmPreopens of
// tabnode_wasi.go makes for the container. The Go side sends this copy in the
// field preopens of the message start:
//
//   [{guest: "/", readOnly: false, files: [["etc/hosts", Uint8Array]], dirs: ["etc"]},
//    {guest: "/config", readOnly: true, files: [...], dirs: [...]}]
//
// It is the same copy that a module gets (wasi.worker.js):
//
//   FS1  the same guest paths as for a module;
//   FS2  a preopen with readOnly has no writes. A write gives the error code
//        "io", as runwasi gives for a read-only volume ("I/O error (os
//        error 29)" in Rust);
//   FS3  writes change only the copy in this pod worker, and end with it;
//   FS4  the Go side fails a copy of more than 64 MiB before the pod worker
//        starts, with the same text as for a module.
//
// The files are in the in-memory file system of the shim
// (InMemoryFilesystemAdapter of its browser build: component.worker.js gives
// the class). Each descriptor of the shim goes through Descriptor below,
// which knows the preopen of the descriptor.

// The error code of a write to a read-only preopen (FS2).
export const READ_ONLY_ERROR = "io";

// tree gives the FileData of the shim for one preopen of wasmPreopens:
// {dir: {name: {dir: {...}} | {source: Uint8Array}}}. A path with "" , "."
// or ".." as a part is not a path of the copy: tree ignores it.
export function tree({ files = [], dirs = [] }) {
  const root = { dir: {} };
  const ok = (parts) => parts.length > 0 && parts.every((p) => p !== "" && p !== "." && p !== "..");
  const dirOf = (parts) => {
    let d = root;
    for (const p of parts) {
      let e = d.dir[p];
      if (!e || !e.dir) e = d.dir[p] = { dir: {} };
      d = e;
    }
    return d;
  };
  for (const path of dirs) {
    const parts = String(path).split("/");
    if (ok(parts)) dirOf(parts);
  }
  for (const [path, data] of files) {
    const parts = String(path).split("/");
    if (!ok(parts)) continue;
    const bytes = data instanceof Uint8Array ? data : new Uint8Array(data ?? 0);
    dirOf(parts.slice(0, -1)).dir[parts.at(-1)] = { source: bytes };
  }
  return root;
}

// The methods that change the file system. On a read-only preopen they throw
// READ_ONLY_ERROR.
const WRITES = new Set([
  "writeViaStream", "appendViaStream", "setSize", "setTimes", "write", "createDirectoryAt",
  "setTimesAt", "removeDirectoryAt", "symlinkAt", "unlinkFileAt", "linkAt", "renameAt",
]);

// Descriptor is a descriptor of the shim (BrowserFilesystemDescriptor) on a
// preopen. It gives each call to the descriptor of the in-memory file
// system. openAt gives a Descriptor of the same preopen.
class Descriptor {
  #inner;
  #readOnly;
  constructor(inner, readOnly) {
    this.#inner = inner;
    this.#readOnly = readOnly;
  }

  get readOnly() { return this.#readOnly; }

  // The descriptor of the in-memory file system. renameAt, linkAt and
  // isSameObject give it to the in-memory file system for the second
  // descriptor: that file system reads a private field of it.
  static inner(d) { return d.#inner; }

  #write() {
    if (this.#readOnly) throw READ_ONLY_ERROR;
  }

  getFlags() {
    const f = this.#inner.getFlags();
    return this.#readOnly ? { ...f, write: false, mutateDirectory: false } : f;
  }

  openAt(pathFlags, path, openFlags, flags) {
    if (this.#readOnly && (flags?.write || flags?.mutateDirectory || openFlags?.create || openFlags?.truncate)) {
      throw READ_ONLY_ERROR;
    }
    return new Descriptor(this.#inner.openAt(pathFlags, path, openFlags, flags), this.#readOnly);
  }

  linkAt(oldPathFlags, oldPath, other, newPath) {
    this.#write();
    other.#write();
    return this.#inner.linkAt(oldPathFlags, oldPath, other.#inner, newPath);
  }

  renameAt(oldPath, other, newPath) {
    this.#write();
    other.#write();
    return this.#inner.renameAt(oldPath, other.#inner, newPath);
  }

  isSameObject(other) {
    return other instanceof Descriptor && this.#inner.isSameObject(other.#inner);
  }

  readDirectory() { return this.#inner.readDirectory(); }
}

// The other methods go to the descriptor of the in-memory file system; the
// methods in WRITES first examine the preopen.
for (const name of [
  "readViaStream", "writeViaStream", "appendViaStream", "advise", "syncData", "getType", "setSize",
  "setTimes", "read", "write", "sync", "createDirectoryAt", "stat", "statAt", "setTimesAt",
  "readlinkAt", "removeDirectoryAt", "symlinkAt", "unlinkFileAt", "metadataHash", "metadataHashAt",
  "lockShared", "lockExclusive", "tryLockShared", "tryLockExclusive", "unlock",
]) {
  const write = WRITES.has(name);
  Object.defineProperty(Descriptor.prototype, name, {
    configurable: true,
    writable: true,
    value: function (...a) {
      if (write && this.readOnly) throw READ_ONLY_ERROR;
      return Descriptor.inner(this)[name](...a);
    },
  });
}

// FilesAdapter is the BrowserFilesystemAdapter of the pod worker. A
// capability is {root, readOnly}: root is the FileData of tree. Memory is
// the class InMemoryFilesystemAdapter of the shim.
export class FilesAdapter {
  #memory;
  constructor(Memory) {
    this.#memory = new Memory();
  }

  getRoot({ root, readOnly }) {
    return new Descriptor(this.#memory.getRoot(root), !!readOnly);
  }
}

// browserFilesystem gives the option browserFilesystem of WASIShim for the
// field preopens of the message start. Without preopens, the component gets
// no preopened directories. If two preopens have the same guest path, the
// last one wins, as a later mount over an earlier one.
export function browserFilesystem(preopens, Memory) {
  const caps = {};
  for (const p of preopens ?? []) caps[p.guest] = { root: tree(p), readOnly: !!p.readOnly };
  return { adapter: new FilesAdapter(Memory), preopens: caps };
}
