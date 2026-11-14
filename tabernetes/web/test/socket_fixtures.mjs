// A fake Go side for the unit tests of the socket table of the pod worker
// (web/src/sockets.js, docs/wasi-sockets.md section 8.3.2). The tests call
// the objects of createSockets as a component does, and give the answers of
// the Go side with peer.dispatch.
import { createIdle, createSocketTable, createSockets } from "../src/sockets.js";
import { createPeer } from "../src/wasi_messages.js";
// The io.js of the browser shim, which the bundle of the pod worker uses.
import * as io from "../node_modules/@bytecodealliance/preview2-shim/dist/browser/io.js";

export { io };

// GO is the first ID of the Go side (section 8.3.1).
export const GO = 0x80000000;

// fakeGo gives the pod worker side of one pod worker and the fake Go side:
//   sent        the messages of the pod worker
//   drops       the messages from Go that the dispatcher dropped
//   go(msg)     a message from Go
//   take(type)  removes and gives the sent messages (of type, or all)
export function fakeGo({ idle = createIdle({ pause: () => Promise.resolve() }) } = {}) {
  const sent = [];
  const drops = [];
  const peer = createPeer({ post: (m) => sent.push(m), onDrop: (why) => drops.push(why) });
  const table = createSocketTable({ peer, io, idle });
  const s = createSockets({ idle, io, table });
  return {
    sent,
    drops,
    peer,
    idle,
    table,
    s,
    net: s.instanceNetwork.instanceNetwork(),
    go: (m) => peer.dispatch(m),
    take(type) {
      const out = type ? sent.filter((m) => m.type === type) : [...sent];
      for (let i = sent.length - 1; i >= 0; i--) if (!type || sent[i].type === type) sent.splice(i, 1);
      return out;
    },
  };
}

export const v4 = (a, b, c, d, port) => ({ tag: "ipv4", val: { port, address: [a, b, c, d] } });
export const v6 = (address, port) => ({ tag: "ipv6", val: { port, flowInfo: 0, address, scopeId: 0 } });
export const is = (code) => (e) => e === code;
export const lastFailed = (e) => e?.tag === "last-operation-failed";
export const closed = (e) => e?.tag === "closed";
