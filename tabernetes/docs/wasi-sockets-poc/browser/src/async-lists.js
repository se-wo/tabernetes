// The lists of section 8.5: the imports of fact 8, all of wasi:sockets and the
// stream functions that do not block. Names without a version, as jco has them.
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

export const ASYNC_IMPORTS = [
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
];

export const ASYNC_EXPORTS = ["wasi:cli/run#run", "wasi:http/incoming-handler#handle"];
