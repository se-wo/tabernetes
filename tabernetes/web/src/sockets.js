// The wasi:sockets of a pod worker (docs/wasi-sockets.md, decision 3 and
// sections 8.3.2 and 8.5), and the pause of CO17 and CO19 for all calls that
// can suspend.
//
// A command component (and the function of an entrypoint) gets TCP and name
// lookup through the socket messages of section 8.3.2: createSocketTable
// gives the table of the socket IDs and lookup IDs of one pod worker, and
// createSockets({idle, io, table}) gives the objects for WASIShim. UDP stays
// a stub until phase 4.
//
// A proxy component keeps the stub (rule 12, PX15): createSockets without a
// table. Each function then gives the result of the proxy world on runwasi
// (section 9.2, item 7):
//
//   create-tcp-socket, create-udp-socket   a socket
//   start-bind, start-connect              access-denied
//   resolve-addresses                      permanent-resolver-failure
//
// The other functions of the stub give the error code of wasmtime for a
// socket that is not bound and not connected. The socket options keep their
// values (TC22).
//
// CO17 and CO19: the functions of section 8.5 that do not wait are in the
// list asyncMode.val.imports (async_lists.js). The pod worker counts the
// calls in sequence that give no data. After IDLE_LIMIT such calls, the next
// call returns a promise that continues after one message on a
// MessageChannel. So a component that polls without a wait still lets the
// pod worker receive messages. The streams of component.js, http.js and the
// TCP streams here use the same count.

import { LIMITS } from "./wasi_messages.js";

// IDLE_LIMIT is the number of calls in sequence without data before a pause
// (CO17).
export const IDLE_LIMIT = 1000;

// nextTask gives a promise that resolves in the next task of the event loop,
// through a message on a MessageChannel (CO19). setTimeout has a minimum of
// 4 ms after nested calls (fact 21); a message has no minimum.
export function nextTask() {
  const { port1, port2 } = new MessageChannel();
  return new Promise((resolve) => {
    port1.onmessage = () => {
      port1.close();
      port2.close();
      resolve();
    };
    port2.postMessage(null);
  });
}

// createIdle gives the count of CO17 for one pod worker.
//
//   idle.call(gotData)   records a call. It gives a promise for the pause of
//                        CO19 when the call is the IDLE_LIMIT-th call in
//                        sequence without data, else null.
//   idle.wrap(fn, test)  gives fn with the count. test(result) tells if the
//                        call gave data; a call that throws gives no data.
//                        At the limit, the result of fn (or its exception)
//                        comes after the pause.
//   idle.count           the calls without data since the last call with
//                        data or the last pause.
export function createIdle({ limit = IDLE_LIMIT, pause = nextTask, onCall } = {}) {
  const idle = {
    count: 0,
    pauses: 0,
    call(gotData) {
      onCall?.();
      if (gotData) {
        idle.count = 0;
        return null;
      }
      if (++idle.count < limit) return null;
      idle.count = 0;
      idle.pauses++;
      return pause();
    },
    wrap(fn, test = () => false) {
      return function counted(...args) {
        let result, error, threw = false;
        try {
          result = fn.apply(this, args);
        } catch (e) {
          threw = true;
          error = e;
        }
        // A promise waits already: it is not a call without data.
        if (!threw && result instanceof Promise) {
          idle.call(true);
          return result;
        }
        const p = idle.call(!threw && test(result));
        if (!p) {
          if (threw) throw error;
          return result;
        }
        return p.then(() => {
          if (threw) throw error;
          return result;
        });
      };
    },
  };
  return idle;
}

// The error codes of wasi:sockets/network are strings: jco gives a thrown
// string to the component as the error result.
const fail = (code) => () => {
  throw code;
};

const FAMILIES = new Set(["ipv4", "ipv6"]);

// The defaults of the socket options. The component can read and change
// them; they have no effect (section 11).
const TCP_DEFAULTS = Object.freeze({
  keepAliveEnabled: false,
  keepAliveIdleTime: 7_200_000_000_000n,
  keepAliveInterval: 75_000_000_000n,
  keepAliveCount: 9,
  hopLimit: 64,
  receiveBufferSize: 131_072n,
  sendBufferSize: 16_384n,
});

const UDP_DEFAULTS = Object.freeze({
  unicastHopLimit: 64,
  receiveBufferSize: 212_992n,
  sendBufferSize: 212_992n,
});

// DEFAULT_BACKLOG is the backlog of listen when the component does not set
// one: the default of wasmtime.
export const DEFAULT_BACKLOG = 128;

// WINDOW is the credit of bytes of each direction of a connection (rule 6,
// TC12). CREDIT_STEP is the number of read bytes after which the pod worker
// sends credit before its buffer is empty.
export const WINDOW = LIMITS.credits;
export const CREDIT_STEP = 64 << 10;

// The IDs of the pod worker (rule 1, section 8.3.1).
const MAX_ID = 0x7fffffff;

// The functions of section 8.5 (the list ASYNC_IMPORTS of async_lists.js),
// by resource. Each one goes through the count of CO17.
const COUNTED = {
  tcp: ["startBind", "finishBind", "startConnect", "finishConnect", "startListen", "finishListen", "accept", "localAddress", "remoteAddress", "shutdown"],
  udp: ["startBind", "finishBind", "stream", "localAddress", "remoteAddress"],
  incoming: ["receive"],
  outgoing: ["checkSend", "send"],
  lookup: ["resolveNextAddress"],
};

// GIVES_DATA tells which calls of the TCP socket give data for the count of
// CO17 when they do not throw. The other calls count as calls without data.
const GIVES_DATA = new Set(["finishBind", "finishListen", "finishConnect", "accept"]);

// The addresses. WASI gives an ip-address as {tag, val: [4 x u8] or
// [8 x u16]} and an ip-socket-address as {tag: "ipv4", val: {port, address}}
// or {tag: "ipv6", val: {port, flowInfo, address, scopeId}}. The messages
// have the text forms.

// ipText gives the text form of an ip-address.
export function ipText({ tag, val }) {
  if (tag === "ipv4") return val.join(".");
  // The longest run of two or more zero groups becomes "::" (RFC 5952).
  let best = -1, bestLen = 1;
  for (let i = 0; i < 8; ) {
    if (val[i] !== 0) {
      i++;
      continue;
    }
    let j = i;
    while (j < 8 && val[j] === 0) j++;
    if (j - i > bestLen) [best, bestLen] = [i, j - i];
    i = j;
  }
  const hex = (a) => a.map((g) => g.toString(16)).join(":");
  if (best < 0) return hex(val);
  return `${hex(val.slice(0, best))}::${hex(val.slice(best + bestLen))}`;
}

const IPV4 = /^(?:(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\.){3}(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)$/;

// parseIP gives the ip-address of a text form, or null.
export function parseIP(text) {
  if (typeof text !== "string") return null;
  if (IPV4.test(text)) return { tag: "ipv4", val: text.split(".").map(Number) };
  if (!text.includes(":") || !/^[0-9a-fA-F:.]+$/.test(text)) return null;
  let s = text;
  let tail = [];
  const last = s.lastIndexOf(":");
  if (s.slice(last + 1).includes(".")) {
    const v4 = s.slice(last + 1);
    if (!IPV4.test(v4)) return null;
    const b = v4.split(".").map(Number);
    tail = [(b[0] << 8) | b[1], (b[2] << 8) | b[3]];
    s = s.slice(0, last + 1);
    if (!s.endsWith("::")) s = s.slice(0, -1);
  }
  const halves = s.split("::");
  if (halves.length > 2) return null;
  const groups = (h) => (h === "" ? [] : h.split(":"));
  const parts = halves.map(groups);
  for (const p of parts) {
    for (const g of p) if (!/^[0-9a-fA-F]{1,4}$/.test(g)) return null;
  }
  const head = parts[0].map((g) => parseInt(g, 16));
  const rest = (parts[1] ?? []).map((g) => parseInt(g, 16));
  const n = head.length + rest.length + tail.length;
  if (halves.length === 1 ? n !== 8 : n > 7) return null;
  const val = [...head, ...new Array(8 - n).fill(0), ...rest, ...tail];
  return { tag: "ipv6", val };
}

const isMapped = (a) => a.tag === "ipv6" && a.val.slice(0, 5).every((g) => g === 0) && a.val[5] === 0xffff;
const isUnspecified = (a) => a.val.every((g) => g === 0);
const isLoopback = (a) => (a.tag === "ipv4" ? a.val[0] === 127 : a.val.slice(0, 7).every((g) => g === 0) && a.val[7] === 1);
const isMulticast = (a) => (a.tag === "ipv4" ? a.val[0] >= 224 && a.val[0] <= 239 : a.val[0] >> 8 === 0xff);
const isBroadcast = (a) => a.tag === "ipv4" && a.val.every((b) => b === 255);

// socketAddress gives the ip-socket-address of an ip-address and a port.
function socketAddress(ip, port) {
  if (ip.tag === "ipv4") return { tag: "ipv4", val: { port, address: [...ip.val] } };
  return { tag: "ipv6", val: { port, flowInfo: 0, address: [...ip.val], scopeId: 0 } };
}

// ipOf gives the ip-address of an ip-socket-address.
const ipOf = (sa) => ({ tag: sa.tag, val: [...sa.val.address] });

// copyAddress gives a copy of an ip-socket-address of the component.
const copyAddress = (sa) => socketAddress(ipOf(sa), sa.val.port);

// checkAddress examines an ip-socket-address of the component for a socket
// of family (fact 12, TC20): the wrong family and an IPv4-mapped IPv6
// address give invalid-argument, as wasmtime does. Multicast and broadcast
// addresses also give invalid-argument (wasmtime: validate_unicast).
function checkAddress(family, sa) {
  if (!sa || !FAMILIES.has(sa.tag)) throw "invalid-argument";
  if (sa.tag !== family) throw "invalid-argument";
  const ip = ipOf(sa);
  if (isMapped(ip) || isMulticast(ip) || isBroadcast(ip)) throw "invalid-argument";
  return ip;
}

// A hostname: labels of letters, digits, "-" and "_", with a dot at the end
// or not (rule 11).
const HOSTNAME = /^[A-Za-z0-9_-]{1,63}(?:\.[A-Za-z0-9_-]{1,63})*\.?$/;

// createSocketTable gives the socket table of one pod worker (rule 1, rule
// 9). It registers the handlers of the socket messages and the lookup
// messages on peer, one time for each pod worker, and routes each message by
// its socket ID or lookup ID.
//
//   table.newID()        a new ID from 1 to 0x7fffffff that is not in use
//   table.add(id, h)     h[type](msg) gets the messages of type for id
//   table.remove(id)     true if id was in the table
//   table.send(type, fields, transfer)
//
// A closed from the Go side removes the ID before its handler runs (rule 9).
// An addresses message ends its lookup ID. Messages for an ID that the table
// does not know are discarded. An accepted message for a listener that the
// table does not know gets closed for the new ID.
export function createSocketTable({ peer, io, idle = null }) {
  const entries = new Map();
  let last = 0;
  const table = {
    io,
    idle,
    newID() {
      for (;;) {
        last = last >= MAX_ID ? 1 : last + 1;
        if (!entries.has(last)) return last;
      }
    },
    add(id, h) {
      entries.set(id, h);
    },
    remove(id) {
      return entries.delete(id);
    },
    has(id) {
      return entries.has(id);
    },
    get size() {
      return entries.size;
    },
    send(type, fields, transfer) {
      peer.send(type, fields, transfer);
    },
  };
  const route = (m) => entries.get(m.socket)?.[m.type]?.(m);
  for (const t of ["bound", "listening", "connected", "data", "credit", "error"]) peer.on(t, route);
  peer.on("accepted", (m) => {
    const h = entries.get(m.socket);
    if (h?.accepted) h.accepted(m);
    else peer.send("closed", { socket: m.accepted });
  });
  peer.on("closed", (m) => {
    const h = entries.get(m.socket);
    if (!h) return;
    entries.delete(m.socket);
    h.closed?.(m);
  });
  peer.on("addresses", (m) => {
    const h = entries.get(m.lookup);
    if (!h) return;
    entries.delete(m.lookup);
    h.addresses?.(m);
  });
  return table;
}

// signal gives the wait of a pollable: wait() resolves at the next wake().
function signal() {
  let waiters = [];
  return {
    wait: () => new Promise((resolve) => waiters.push(resolve)),
    wake() {
      const w = waiters;
      waiters = [];
      for (const fn of w) fn();
    },
  };
}

const CLOSED = Object.freeze({ tag: "closed" });

// Conn is one TCP connection: its two streams and their credits (rules 6 to
// 9). The pod worker makes it after connected or at accepted.
class Conn {
  id;
  #table;
  #idle;
  #chunks = [];
  #size = 0;
  #ended = false;
  #error = null; // the error code of an error message (rule 8)
  #failed = false; // the error went to the component
  #goClosed = null; // closed from the Go side: {error}
  #released = false; // the pod worker sent closed
  #recvShut = false;
  #sendShut = false;
  #unacked = 0;
  #credit = WINDOW;
  #signal = signal();
  constructor(table, idle, id) {
    this.#table = table;
    this.#idle = idle;
    this.id = id;
  }

  // The messages of the Go side.
  data(m) {
    if (this.#released) return;
    const n = m.bytes.byteLength;
    if (n === 0) this.#ended = true;
    else if (this.#recvShut) this.#ack(n, true);
    else {
      this.#chunks.push(new Uint8Array(m.bytes));
      this.#size += n;
    }
    this.#signal.wake();
  }
  credit(m) {
    if (m.kind !== "bytes") return;
    this.#credit += m.n;
    this.#signal.wake();
  }
  error(m) {
    if (this.#error === null && !this.#failed) this.#error = m.error;
    this.#signal.wake();
  }
  closed(m) {
    this.#goClosed = { error: m.error ?? null };
    this.#signal.wake();
  }
  get goClosed() {
    return this.#goClosed !== null;
  }

  // release sends closed for the ID if the Go side did not close it (rule
  // 9). The streams then give closed.
  release() {
    if (this.#released) return;
    this.#released = true;
    if (this.#table.remove(this.id)) this.#table.send("closed", { socket: this.id });
    this.#signal.wake();
  }

  // shutdown sends shutdown (TC13). After "send", writes give closed; after
  // "receive", reads give closed.
  shutdown(how) {
    if (this.#goClosed || this.#released) throw "invalid-state";
    if (how === "receive" || how === "both") {
      this.#recvShut = true;
      // The bytes in the buffer go back to the Go side as credit.
      if (this.#size) this.#ack(this.#size, true);
      this.#chunks = [];
      this.#size = 0;
    }
    if (how === "send" || how === "both") this.#sendShut = true;
    this.#table.send("shutdown", { socket: this.id, how });
    this.#signal.wake();
  }

  // #ack sends credit for n bytes that the component took. It collects
  // the credit up to CREDIT_STEP, or until the buffer is empty.
  #ack(n, now = false) {
    this.#unacked += n;
    if (!now && this.#unacked < CREDIT_STEP && this.#size > 0) return;
    if (this.#released || this.#goClosed) {
      this.#unacked = 0;
      return;
    }
    while (this.#unacked > 0) {
      const k = Math.min(this.#unacked, LIMITS.credits);
      this.#unacked -= k;
      this.#table.send("credit", { socket: this.id, kind: "bytes", n: k });
    }
  }

  #lastFailed(code) {
    return { tag: "last-operation-failed", val: this.#table.io.ioErrorCreate(code) };
  }

  // #check throws the result of a stream call that cannot give bytes:
  // closed from the Go side, an error of rule 8, the end of the stream.
  #check(receive) {
    if (this.#goClosed) throw this.#lastFailed(this.#goClosed.error ?? "connection-aborted");
    if (this.#released || this.#failed) throw CLOSED;
    if (receive ? this.#recvShut : this.#sendShut) throw CLOSED;
  }
  #takeError() {
    if (this.#error === null) return;
    const code = this.#error;
    this.#error = null;
    this.#failed = true;
    throw this.#lastFailed(code);
  }

  // #take gives up to len bytes, or null when no bytes are there.
  #take(len) {
    this.#check(true);
    if (this.#size === 0) {
      this.#takeError();
      if (this.#ended) throw CLOSED;
      return null;
    }
    let n = Number(len > 0x7fffffffn ? 0x7fffffffn : len);
    if (n === 0) return new Uint8Array(0);
    const parts = [];
    let size = 0;
    while (n > 0 && this.#chunks.length) {
      const c = this.#chunks[0];
      if (c.byteLength <= n) {
        parts.push(this.#chunks.shift());
        n -= c.byteLength;
        size += c.byteLength;
      } else {
        parts.push(c.slice(0, n));
        this.#chunks[0] = c.subarray(n);
        size += n;
        n = 0;
      }
    }
    this.#size -= size;
    this.#ack(size);
    if (parts.length === 1) return parts[0];
    const out = new Uint8Array(size);
    let o = 0;
    for (const p of parts) {
      out.set(p, o);
      o += p.byteLength;
    }
    return out;
  }
  #readable() {
    return this.#size > 0 || this.#ended || this.#error !== null || this.#failed || this.#goClosed !== null || this.#released || this.#recvShut;
  }
  #writable() {
    return this.#credit > 0 || this.#error !== null || this.#failed || this.#goClosed !== null || this.#released || this.#sendShut;
  }
  #waitFor(ready) {
    return () => (ready() ? Promise.resolve() : this.#signal.wait());
  }

  // #write sends the bytes as data messages. The caller makes sure that the
  // credit is enough (rule 6).
  #write(buf) {
    this.#check(false);
    this.#takeError();
    const n = buf.byteLength;
    if (n > this.#credit) throw this.#lastFailed("the write is larger than the permit of check-write");
    // Rule 7: the pod worker never sends data with 0 bytes.
    for (let off = 0; off < n; off += LIMITS.data) {
      const part = buf.slice(off, Math.min(n, off + LIMITS.data));
      this.#table.send("data", { socket: this.id, bytes: part.buffer }, [part.buffer]);
    }
    this.#credit -= n;
  }

  // streams gives the input stream and the output stream of the connection.
  streams() {
    const { inputStreamCreate, outputStreamCreate, pollableCreate } = this.#table.io;
    const idle = this.#idle;
    const readable = () => this.#readable();
    const writable = () => this.#writable();
    const take = (len) => this.#take(len) ?? new Uint8Array(0);
    // CO17: a read without bytes counts as a call without data.
    const read = idle.wrap(take, (b) => b.byteLength > 0);
    const blockingRead = (len) => {
      const b = this.#take(len);
      if (b) return b;
      return this.#signal.wait().then(() => blockingRead(len));
    };
    const size = (b) => (b instanceof Promise ? b.then((x) => BigInt(x.byteLength)) : BigInt(b.byteLength));
    const input = inputStreamCreate({
      read,
      blockingRead,
      skip: (len) => size(read(len)),
      blockingSkip: (len) => size(blockingRead(len)),
      subscribe: () => pollableCreate({ ready: readable, wait: this.#waitFor(readable) }),
    });
    const checkWrite = () => {
      this.#check(false);
      this.#takeError();
      return BigInt(Math.min(this.#credit, WINDOW));
    };
    const blockingWriteAndFlush = (buf) => {
      this.#check(false);
      this.#takeError();
      if (buf.byteLength <= this.#credit) return this.#write(buf);
      return this.#signal.wait().then(() => blockingWriteAndFlush(buf));
    };
    const output = outputStreamCreate({
      checkWrite,
      write: (buf) => this.#write(buf),
      blockingWriteAndFlush,
      flush() {},
      blockingFlush() {},
      subscribe: () => pollableCreate({ ready: writable, wait: this.#waitFor(writable) }),
    });
    // CO17: check-write that gives 0 counts as a call without data. The
    // shim examines the result of the handler, so the count wraps the method
    // of the stream: at the limit it gives a promise of the permit.
    const shimCheckWrite = output.checkWrite;
    output.checkWrite = idle.wrap(function () {
      return shimCheckWrite.call(this);
    }, (n) => n > 0n);
    return [input, output];
  }
}

// createTcp gives the TcpSocket and ResolveAddressStream classes and the
// functions createTcpSocket and resolveAddresses that use the socket table.
function createTcp({ table, idle }) {
  const { pollableCreate } = table.io;
  const make = Symbol("make");

  class TcpSocket extends TcpOptions {
    #family;
    #state = "unbound";
    #id = null;
    #answer = null; // the answer of bind, listen or connect
    #bindIP = null; // the ip-address of bind
    #local = null;
    #remote = null;
    #connectIP = null;
    #conn = null;
    #queue = []; // accepted connections: {conn, local, remote}
    #backlog = DEFAULT_BACKLOG;
    #goClosed = null;
    #dropped = false;
    #signal = signal();
    #handler = {
      bound: (m) => this.#onAnswer("bind-started", m),
      listening: (m) => this.#onAnswer("listen-started", m),
      connected: (m) => this.#onAnswer("connect-started", m),
      accepted: (m) => this.#onAccepted(m),
      data: (m) => this.#conn?.data(m),
      credit: (m) => this.#conn?.credit(m),
      error: (m) => this.#conn?.error(m),
      closed: (m) => this.#onClosed(m),
    };
    constructor(family, token, conn) {
      super();
      this.#family = family;
      if (token === make) {
        // A connection that a listener accepted.
        this.#state = "connected";
        this.#conn = conn.conn;
        this.#id = conn.conn.id;
        this.#local = conn.local;
        this.#remote = conn.remote;
      }
    }

    #onAnswer(state, m) {
      if (this.#state === state && this.#answer === null) this.#answer = m;
      this.#signal.wake();
    }
    #onAccepted(m) {
      if (this.#state !== "listening") {
        table.send("closed", { socket: m.accepted });
        return;
      }
      const conn = new Conn(table, idle, m.accepted);
      table.add(m.accepted, conn);
      let local = parseIP(m.localAddress), remote = parseIP(m.remoteAddress);
      // Rule 2, TC17: a listener on 127.0.0.0/8 shows loopback addresses.
      if (isLoopback(this.#bindIP)) {
        local = this.#bindIP;
        remote = this.#bindIP.tag === "ipv4" ? { tag: "ipv4", val: [127, 0, 0, 1] } : this.#bindIP;
      }
      this.#queue.push({ conn, local: socketAddress(local, m.localPort), remote: socketAddress(remote, m.remotePort) });
      this.#signal.wake();
    }
    #onClosed(m) {
      this.#goClosed = { error: m.error ?? null };
      this.#conn?.closed(m);
      this.#signal.wake();
    }

    // #release sends closed for the ID if it is open (rule 9).
    #release() {
      const id = this.#id;
      this.#id = null;
      if (id === null) return;
      if (this.#conn) this.#conn.release();
      else if (table.remove(id)) table.send("closed", { socket: id });
    }
    // #newID gives the socket a new ID. An ID of an earlier bind that
    // failed gets closed first.
    #newID() {
      this.#release();
      this.#goClosed = null;
      this.#id = table.newID();
      table.add(this.#id, this.#handler);
      return this.#id;
    }
    #usable() {
      if (this.#goClosed || this.#dropped) throw "invalid-state";
    }
    // #busy throws for a call while bind, listen or connect is in progress.
    #busy() {
      if (this.#state.endsWith("-started")) throw "concurrency-conflict";
    }

    startBind(_network, localAddress) {
      this.#usable();
      const ip = checkAddress(this.#family, localAddress);
      this.#busy();
      if (this.#state !== "unbound") throw "invalid-state";
      const id = this.#newID();
      this.#bindIP = ip;
      this.#answer = null;
      this.#state = "bind-started";
      table.send("bind", { socket: id, protocol: "tcp", family: this.#family, address: ipText(ip), port: localAddress.val.port });
    }
    finishBind() {
      const m = this.#finish("bind-started");
      if (m.error) {
        // The ID stays open until the drop or the next bind (rule 9).
        this.#state = "unbound";
        this.#bindIP = null;
        throw m.error;
      }
      this.#state = "bound";
      this.#local = socketAddress(this.#bindIP, m.port ?? 0);
    }
    // #finish gives the answer of the operation in state, or throws.
    #finish(state) {
      if (this.#state !== state) throw "not-in-progress";
      if (this.#goClosed) {
        this.#state = "closed";
        throw this.#goClosed.error ?? "connection-aborted";
      }
      if (this.#answer === null) throw "would-block";
      const m = this.#answer;
      this.#answer = null;
      return m;
    }

    startConnect(_network, remoteAddress) {
      this.#usable();
      const ip = checkAddress(this.#family, remoteAddress);
      if (isUnspecified(ip) || remoteAddress.val.port === 0) throw "invalid-argument";
      this.#busy();
      if (this.#state !== "unbound" && this.#state !== "bound") throw "invalid-state";
      const id = this.#state === "bound" ? this.#id : this.#newID();
      this.#connectIP = ip;
      this.#remote = copyAddress(remoteAddress);
      this.#answer = null;
      this.#state = "connect-started";
      table.send("connect", { socket: id, protocol: "tcp", address: ipText(ip), port: remoteAddress.val.port });
    }
    finishConnect() {
      const m = this.#finish("connect-started");
      if (m.error) {
        // As wasmtime: the socket is closed. The ID stays open until the
        // drop (rule 9).
        this.#state = "closed";
        throw m.error;
      }
      // Rule 2: only the local port comes from connected. The remote address
      // is the address of connect.
      let ip;
      if (this.#bindIP && !isUnspecified(this.#bindIP)) ip = this.#bindIP;
      else if (isLoopback(this.#connectIP)) ip = this.#connectIP.tag === "ipv4" ? { tag: "ipv4", val: [127, 0, 0, 1] } : this.#connectIP;
      else ip = parseIP(m.localAddress ?? "") ?? this.#bindIP ?? (this.#family === "ipv4" ? parseIP("0.0.0.0") : parseIP("::"));
      this.#local = socketAddress(ip, m.localPort ?? this.#local?.val.port ?? 0);
      this.#state = "connected";
      this.#conn = new Conn(table, idle, this.#id);
      table.add(this.#id, this.#conn);
      return this.#conn.streams();
    }

    startListen() {
      this.#usable();
      this.#busy();
      if (this.#state !== "bound") throw "invalid-state";
      this.#answer = null;
      this.#state = "listen-started";
      table.send("listen", { socket: this.#id, backlog: this.#backlog });
    }
    finishListen() {
      const m = this.#finish("listen-started");
      if (m.error) {
        this.#state = "bound";
        throw m.error;
      }
      this.#state = "listening";
    }

    accept() {
      if (this.#state !== "listening" || this.#goClosed || this.#dropped) throw "invalid-state";
      const c = this.#queue.shift();
      if (!c) throw "would-block";
      // Rule 4: one more connection for the backlog.
      table.send("credit", { socket: this.#id, kind: "connections", n: 1 });
      const sock = new TcpSocket(this.#family, make, c);
      return [sock, ...c.conn.streams()];
    }

    localAddress() {
      if (this.#local === null) throw "invalid-state";
      return copyAddress(this.#local);
    }
    remoteAddress() {
      if (this.#state !== "connected") throw "invalid-state";
      return copyAddress(this.#remote);
    }
    isListening() {
      return this.#state === "listening";
    }
    addressFamily() {
      return this.#family;
    }
    setListenBacklogSize(n) {
      super.setListenBacklogSize(n);
      if (this.#state === "connect-started" || this.#state === "connected") throw "invalid-state";
      const v = BigInt(n);
      this.#backlog = Number(v > BigInt(LIMITS.backlog) ? BigInt(LIMITS.backlog) : v);
    }
    subscribe() {
      const ready = () => {
        if (this.#goClosed || this.#dropped) return true;
        if (this.#state.endsWith("-started")) return this.#answer !== null;
        if (this.#state === "listening") return this.#queue.length > 0;
        return true;
      };
      return pollableCreate({ ready, wait: () => (ready() ? Promise.resolve() : this.#signal.wait()) });
    }
    shutdown(how) {
      if (this.#state !== "connected") throw "invalid-state";
      this.#conn.shutdown(how);
    }
    // The drop of the socket (rule 9). A drop of a stream does not close
    // the socket.
    [Symbol.dispose]() {
      if (this.#dropped) return;
      this.#dropped = true;
      for (const c of this.#queue) c.conn.release();
      this.#queue = [];
      this.#release();
      this.#signal.wake();
    }
  }

  class ResolveAddressStream {
    #list = null;
    #error = null;
    #id = null;
    #signal = signal();
    constructor(token, list, id) {
      if (token !== make) throw new TypeError("ResolveAddressStream has no constructor");
      this.#list = list;
      this.#id = id;
    }
    _answer(m) {
      this.#id = null;
      if (m.error) this.#error = m.error;
      else this.#list = (m.addresses ?? []).map(parseIP).filter(Boolean);
      this.#signal.wake();
    }
    resolveNextAddress() {
      if (this.#error !== null) {
        const e = this.#error;
        this.#error = null;
        this.#list = [];
        throw e;
      }
      if (this.#list === null) throw "would-block";
      return this.#list.shift();
    }
    subscribe() {
      const ready = () => this.#list !== null || this.#error !== null;
      return pollableCreate({ ready, wait: () => (ready() ? Promise.resolve() : this.#signal.wait()) });
    }
    // Rule 11: addresses for a dropped stream is discarded.
    [Symbol.dispose]() {
      if (this.#id !== null) table.remove(this.#id);
      this.#id = null;
    }
  }

  // resolveAddresses (rule 11): an IP literal gives the address without a
  // message, as wasmtime does. A name that is not correct gives
  // invalid-argument. Each other name sends lookup.
  function resolveAddresses(_network, name) {
    const literal = typeof name === "string" && name.startsWith("[") && name.endsWith("]") ? parseIP(name.slice(1, -1)) : parseIP(name);
    if (literal) return new ResolveAddressStream(make, [literal], null);
    if (typeof name !== "string" || name.length < 1 || name.length > LIMITS.hostname || !HOSTNAME.test(name)) throw "invalid-argument";
    const id = table.newID();
    const stream = new ResolveAddressStream(make, null, id);
    table.add(id, { addresses: (m) => stream._answer(m) });
    table.send("lookup", { lookup: id, name });
    return stream;
  }

  for (const n of COUNTED.tcp) {
    const fn = TcpSocket.prototype[n];
    TcpSocket.prototype[n] = idle.wrap(fn, GIVES_DATA.has(n) ? () => true : () => false);
  }
  ResolveAddressStream.prototype.resolveNextAddress = idle.wrap(ResolveAddressStream.prototype.resolveNextAddress, (a) => a !== undefined);

  const createTcpSocket = (family) => {
    if (!FAMILIES.has(family)) throw "not-supported";
    return new TcpSocket(family);
  };
  return { TcpSocket, ResolveAddressStream, createTcpSocket, resolveAddresses: idle.wrap(resolveAddresses, () => true) };
}

// TcpOptions keeps the socket options of a TCP socket (TC22).
class TcpOptions {
  #options = { ...TCP_DEFAULTS };
  setListenBacklogSize(n) {
    if (n === 0n || n === 0) throw "invalid-argument";
  }
  keepAliveEnabled() {
    return this.#options.keepAliveEnabled;
  }
  setKeepAliveEnabled(v) {
    this.#options.keepAliveEnabled = !!v;
  }
  keepAliveIdleTime() {
    return this.#options.keepAliveIdleTime;
  }
  setKeepAliveIdleTime(v) {
    if (v <= 0n) throw "invalid-argument";
    this.#options.keepAliveIdleTime = v;
  }
  keepAliveInterval() {
    return this.#options.keepAliveInterval;
  }
  setKeepAliveInterval(v) {
    if (v <= 0n) throw "invalid-argument";
    this.#options.keepAliveInterval = v;
  }
  keepAliveCount() {
    return this.#options.keepAliveCount;
  }
  setKeepAliveCount(v) {
    if (v === 0) throw "invalid-argument";
    this.#options.keepAliveCount = v;
  }
  hopLimit() {
    return this.#options.hopLimit;
  }
  setHopLimit(v) {
    if (v === 0) throw "invalid-argument";
    this.#options.hopLimit = v;
  }
  receiveBufferSize() {
    return this.#options.receiveBufferSize;
  }
  setReceiveBufferSize(v) {
    if (v === 0n) throw "invalid-argument";
    this.#options.receiveBufferSize = v;
  }
  sendBufferSize() {
    return this.#options.sendBufferSize;
  }
  setSendBufferSize(v) {
    if (v === 0n) throw "invalid-argument";
    this.#options.sendBufferSize = v;
  }
}

// createSockets gives the object `sockets` for WASIShim: instanceNetwork,
// network, ipNameLookup, tcp, tcpCreateSocket, udp and udpCreateSocket.
// idle is the count of createIdle.
//
// With table (createSocketTable), TCP and name lookup use the messages of
// section 8.3.2; io is the io.js of the shim (the table has it too).
// Without table, the result is the stub of the proxy world; pollableCreate
// (or io.pollableCreate) is the function of the io.js of the shim, so that
// subscribe gives a pollable of the shim.
export function createSockets({ idle = createIdle(), io = null, table = null, pollableCreate = (io ?? table?.io).pollableCreate }) {
  const ready = () => pollableCreate();

  class Network {}
  const network = new Network();

  class ResolveAddressStream {
    resolveNextAddress() {
      return undefined;
    }
    subscribe() {
      return ready();
    }
  }

  class TcpSocket extends TcpOptions {
    #family;
    constructor(family) {
      super();
      this.#family = family;
    }
    startBind() {
      throw "access-denied";
    }
    finishBind() {
      throw "not-in-progress";
    }
    startConnect() {
      throw "access-denied";
    }
    finishConnect() {
      throw "not-in-progress";
    }
    startListen() {
      throw "invalid-state";
    }
    finishListen() {
      throw "not-in-progress";
    }
    accept() {
      throw "invalid-state";
    }
    localAddress() {
      throw "invalid-state";
    }
    remoteAddress() {
      throw "invalid-state";
    }
    isListening() {
      return false;
    }
    addressFamily() {
      return this.#family;
    }
    subscribe() {
      return ready();
    }
    shutdown() {
      throw "invalid-state";
    }
  }

  class IncomingDatagramStream {
    receive() {
      return [];
    }
    subscribe() {
      return ready();
    }
  }

  class OutgoingDatagramStream {
    checkSend() {
      return 0n;
    }
    send() {
      return 0n;
    }
    subscribe() {
      return ready();
    }
  }

  class UdpSocket {
    #family;
    #options = { ...UDP_DEFAULTS };
    constructor(family) {
      this.#family = family;
    }
    startBind() {
      throw "access-denied";
    }
    finishBind() {
      throw "not-in-progress";
    }
    stream() {
      throw "invalid-state";
    }
    localAddress() {
      throw "invalid-state";
    }
    remoteAddress() {
      throw "invalid-state";
    }
    addressFamily() {
      return this.#family;
    }
    unicastHopLimit() {
      return this.#options.unicastHopLimit;
    }
    setUnicastHopLimit(v) {
      if (v === 0) throw "invalid-argument";
      this.#options.unicastHopLimit = v;
    }
    receiveBufferSize() {
      return this.#options.receiveBufferSize;
    }
    setReceiveBufferSize(v) {
      if (v === 0n) throw "invalid-argument";
      this.#options.receiveBufferSize = v;
    }
    sendBufferSize() {
      return this.#options.sendBufferSize;
    }
    setSendBufferSize(v) {
      if (v === 0n) throw "invalid-argument";
      this.#options.sendBufferSize = v;
    }
    subscribe() {
      return ready();
    }
  }

  // The count of CO17 on the prototypes: in this stub, no call gives data.
  const count = (cls, names) => {
    for (const n of names) cls.prototype[n] = idle.wrap(cls.prototype[n]);
  };
  count(UdpSocket, COUNTED.udp);
  count(IncomingDatagramStream, COUNTED.incoming);
  count(OutgoingDatagramStream, COUNTED.outgoing);

  const create = (Cls) => (family) => {
    if (!FAMILIES.has(family)) throw "not-supported";
    return new Cls(family);
  };

  const udp = {
    udp: { UdpSocket, IncomingDatagramStream, OutgoingDatagramStream },
    udpCreateSocket: { createUdpSocket: create(UdpSocket) },
  };

  if (table) {
    const t = createTcp({ table, idle });
    return {
      instanceNetwork: { instanceNetwork: () => network },
      network: { Network },
      ipNameLookup: { ResolveAddressStream: t.ResolveAddressStream, resolveAddresses: t.resolveAddresses },
      tcp: { TcpSocket: t.TcpSocket },
      tcpCreateSocket: { createTcpSocket: t.createTcpSocket },
      ...udp,
    };
  }

  count(TcpSocket, COUNTED.tcp);
  count(ResolveAddressStream, COUNTED.lookup);
  return {
    instanceNetwork: { instanceNetwork: () => network },
    network: { Network },
    ipNameLookup: {
      ResolveAddressStream,
      // runwasi does not allow name lookup in the proxy world (fact 6).
      resolveAddresses: idle.wrap(fail("permanent-resolver-failure")),
    },
    tcp: { TcpSocket },
    tcpCreateSocket: { createTcpSocket: create(TcpSocket) },
    ...udp,
  };
}
