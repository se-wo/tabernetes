// Small components for the tests of the transpile worker, made from the
// WebAssembly text format with wasm-tools of jco.
import { parse } from "@bytecodealliance/jco";

// component gives a component with one import and the exports that names
// lists (each one calls the same core function). functions and data make it
// larger: more core functions and a data segment of that many bytes.
export async function component(names = ["go"], { functions = 1, data = 0 } = {}) {
  const body = "(local.set 0 (i32.add (local.get 0) (i32.const 1)))".repeat(functions > 1 ? 50 : 1);
  const funcs = Array.from({ length: functions }, (_, i) => `(func (export "f${i}") (local i32) ${body})`).join(" ");
  const mem = data ? `(memory ${Math.ceil(data / 65536) + 1}) (data (i32.const 0) "${"x".repeat(data)}")` : "";
  return parse(`(component
  (import "wasi:cli/environment@0.2.3" (instance $env (export "get-arguments" (func (result (list string))))))
  (core module $m ${mem} ${funcs})
  (core instance $i (instantiate $m))
  (func $f (canon lift (core func $i "f0")))
  ${names.map((n) => `(export "${n}" (func $f))`).join("\n  ")}
)`);
}

// withName replaces the last occurrence of the ASCII text from in bytes by
// to, which must have the same length.
export function withName(bytes, from, to) {
  const s = Buffer.from(bytes).toString("latin1");
  const i = s.lastIndexOf(from);
  if (i < 0 || from.length !== to.length) throw new Error(`cannot replace ${from}`);
  return Uint8Array.from(Buffer.from(s.slice(0, i) + to + s.slice(i + from.length), "latin1"));
}
