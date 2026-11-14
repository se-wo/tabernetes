// The lists of asyncMode for jco (docs/wasi-sockets.md, section 8.5) and the
// version of jco. They are part of the key of the transpile cache (decision
// 2), so wasi.js and transpile.worker.js both import them from here. Names
// have no version, as jco matches them.

// JCO_VERSION must agree with @bytecodealliance/jco in web/package.json
// (web/test/transpile.test.mjs makes sure of this).
export const JCO_VERSION = "1.42.0";

const sockets = [];
for (const m of ["start-bind", "finish-bind", "start-connect", "finish-connect", "start-listen", "finish-listen", "accept", "local-address", "remote-address", "shutdown"]) {
  sockets.push(`wasi:sockets/tcp#[method]tcp-socket.${m}`);
}
for (const m of ["start-bind", "finish-bind", "stream", "local-address", "remote-address"]) {
  sockets.push(`wasi:sockets/udp#[method]udp-socket.${m}`);
}
sockets.push("wasi:sockets/udp#[method]incoming-datagram-stream.receive");
sockets.push("wasi:sockets/udp#[method]outgoing-datagram-stream.check-send");
sockets.push("wasi:sockets/udp#[method]outgoing-datagram-stream.send");
sockets.push("wasi:sockets/ip-name-lookup#resolve-addresses");
sockets.push("wasi:sockets/ip-name-lookup#[method]resolve-address-stream.resolve-next-address");

// The imports that can suspend: those of fact 8, the stream functions that
// do not block (for the count of CO17) and all of wasi:sockets.
export const ASYNC_IMPORTS = Object.freeze([
  "wasi:io/poll#poll",
  "wasi:io/poll#[method]pollable.block",
  "wasi:io/streams#[method]input-stream.blocking-read",
  "wasi:io/streams#[method]input-stream.blocking-skip",
  "wasi:io/streams#[method]output-stream.blocking-flush",
  "wasi:io/streams#[method]output-stream.blocking-write-and-flush",
  "wasi:io/streams#[method]output-stream.blocking-write-zeroes-and-flush",
  "wasi:io/streams#[method]output-stream.blocking-splice",
  "wasi:io/streams#[method]input-stream.read",
  "wasi:io/streams#[method]input-stream.skip",
  "wasi:io/streams#[method]output-stream.check-write",
  "wasi:io/streams#[method]output-stream.write",
  "wasi:io/streams#[method]output-stream.flush",
  "wasi:io/streams#[method]output-stream.write-zeroes",
  "wasi:io/streams#[method]output-stream.splice",
  ...sockets,
]);

// The exports that can suspend. asyncExports adds the function that the
// entrypoint names (CO3).
export const ASYNC_EXPORTS = Object.freeze(["wasi:cli/run#run", "wasi:http/incoming-handler#handle"]);

export function asyncExports(entrypoint) {
  return entrypoint && !ASYNC_EXPORTS.includes(entrypoint) ? [...ASYNC_EXPORTS, entrypoint] : [...ASYNC_EXPORTS];
}
