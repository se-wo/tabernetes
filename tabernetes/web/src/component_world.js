// The names of the imports and exports of a component, from its bytes, and
// the world that runwasi finds for it (docs/wasi-sockets.md, fact 5 and
// section 8.2).
//
// jco gives its own lists: names without a version, and the exports sorted
// by name with camelCase aliases. runwasi uses the names with their versions
// and the sequence of the exports in the component. So the transpile worker
// reads the two lists from the component itself. It reads only the import
// section and the export section of the outer component; it does not read
// types, core modules or nested components.

// The section IDs of the component binary format.
const SECTION_IMPORT = 10;
const SECTION_EXPORT = 11;

// The sorts of the component binary format (sortidx), for the export list.
const SORTS = { 0x01: "function", 0x02: "value", 0x03: "type", 0x04: "component", 0x05: "instance" };

class Reader {
  constructor(bytes, start = 0, end = bytes.length) {
    this.b = bytes;
    this.p = start;
    this.end = end;
  }
  byte() {
    if (this.p >= this.end) throw new Error("unexpected end of the component");
    return this.b[this.p++];
  }
  u32() {
    let v = 0, shift = 0;
    for (;;) {
      const x = this.byte();
      v += (x & 0x7f) * 2 ** shift;
      if (!(x & 0x80)) break;
      shift += 7;
      if (shift > 28) throw new Error("an integer of the component is too long");
    }
    if (v > 0xffffffff) throw new Error("an integer of the component is too large");
    return v;
  }
  // s33 reads a signed integer of up to 33 bits (a value type).
  s33() {
    let shift = 0, x;
    do {
      x = this.byte();
      shift += 7;
      if (shift > 35) throw new Error("an integer of the component is too long");
    } while (x & 0x80);
  }
  name() {
    const n = this.u32();
    if (n > this.end - this.p) throw new Error("a name goes past the end of its section");
    const s = new TextDecoder("utf-8", { fatal: true }).decode(this.b.subarray(this.p, this.p + n));
    this.p += n;
    return s;
  }
  // externName reads importname' or exportname': a kind byte and a name.
  externName() {
    const k = this.byte();
    if (k !== 0x00 && k !== 0x01) throw new Error(`unknown kind 0x${k.toString(16)} of an import or export name`);
    return this.name();
  }
  // externDesc reads and discards an externdesc.
  externDesc() {
    const k = this.byte();
    switch (k) {
      case 0x00: // core module: 0x11 typeidx
        if (this.byte() !== 0x11) throw new Error("unknown core sort in an externdesc");
        this.u32();
        return;
      case 0x01: case 0x04: case 0x05: // func, component, instance: typeidx
        this.u32();
        return;
      case 0x02: { // value: valuebound
        const b = this.byte();
        if (b === 0x00) this.u32();
        else if (b === 0x01) this.s33();
        else throw new Error("unknown value bound");
        return;
      }
      case 0x03: { // type: typebound
        const b = this.byte();
        if (b === 0x00) this.u32();
        else if (b !== 0x01) throw new Error("unknown type bound");
        return;
      }
      default:
        throw new Error(`unknown externdesc 0x${k.toString(16)}`);
    }
  }
}

// componentNames gives {imports: [name], exports: [[name, kind]]} of the outer
// component, in the sequence of the component. kind is "function",
// "instance" or another sort of the binary format. It throws for bytes that
// are not a correct component.
export function componentNames(bytes) {
  const b = bytes instanceof Uint8Array ? bytes : new Uint8Array(bytes);
  if (b.length < 8 || b[0] !== 0x00 || b[1] !== 0x61 || b[2] !== 0x73 || b[3] !== 0x6d || b[6] !== 0x01 || b[7] !== 0x00) {
    throw new Error("the bytes are not a component");
  }
  const imports = [], exports = [];
  const r = new Reader(b, 8);
  while (r.p < r.end) {
    const id = r.byte();
    const size = r.u32();
    if (size > r.end - r.p) throw new Error(`section ${id} goes past the end of the component`);
    const end = r.p + size;
    if (id === SECTION_IMPORT || id === SECTION_EXPORT) {
      const s = new Reader(b, r.p, end);
      const n = s.u32();
      for (let i = 0; i < n; i++) {
        const name = s.externName();
        if (id === SECTION_IMPORT) {
          s.externDesc();
          imports.push(name);
        } else {
          let sort = s.byte();
          if (sort === 0x00) sort = 0x100 + s.byte(); // a core sort
          s.u32();
          if (s.byte() === 0x01) s.externDesc(); // the optional type of the export
          exports.push([name, SORTS[sort] ?? "core"]);
        }
      }
      if (s.p !== end) throw new Error(`section ${id} has bytes after its last item`);
    }
    r.p = end;
  }
  return { imports, exports };
}

// selectWorld gives the world that runwasi finds (instance.rs,
// ComponentTarget::new): the first export with the prefix
// wasi:http/incoming-handler gives "proxy", the first export with the prefix
// wasi:cli/run gives "command". A component without the two gives "" (CO3:
// runwasi calls the function that the entrypoint names). exports is the list
// of componentNames, in the sequence of the component.
export function selectWorld(exports) {
  for (const [name] of exports) {
    if (name.startsWith("wasi:http/incoming-handler")) return "proxy";
    if (name.startsWith("wasi:cli/run")) return "command";
  }
  return "";
}
