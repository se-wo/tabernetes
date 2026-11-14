// The wasi:sockets of a pod worker (docs/wasi-sockets.md, decision 3 and
// sections 8.3.2 and 8.5), and the pause of CO17 and CO19 for all calls that
// can suspend.
//
// A command component (and the function of an entrypoint) gets TCP and name
// lookup through the socket messages of section 8.3.2: createSocketTable
// gives the table of the socket IDs and lookup IDs of one pod worker, and
// createSockets({idle, io, table}) gives the objects for WASIShim. UDP uses
// the messages of section 8.3.3 (createUdp).
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
// TCP and UDP streams here use the same count.

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
// them; they have no effect (section 11). The values are those of runwasi
// (wasmtime 36 on Linux, package W3.3a): wasmtime gives the buffer sizes of
// the kernel divided by 2.
const TCP_DEFAULTS = Object.freeze({
  keepAliveEnabled: false,
  keepAliveIdleTime: 7_200_000_000_000n,
  keepAliveInterval: 75_000_000_000n,
  keepAliveCount: 9,
  hopLimit: 64,
  receiveBufferSize: 65_536n,
  sendBufferSize: 8_192n,
});

const SECOND = 1_000_000_000n;
// The smallest buffer sizes that runwasi gives after a set (Linux).
const MIN_RECEIVE_BUFFER = 1_152n;
const MIN_SEND_BUFFER = 2_304n;

// The defaults of the UDP socket options (TC22, fact 7 of package W4.5a).
// They have no effect, as for TCP. The values are those of runwasi on the
// reference runner of package W4.5a; the buffer sizes depend on the host
// (section 11, "Socket options").
export const UDP_DEFAULTS = Object.freeze({
  unicastHopLimit: 64,
  receiveBufferSize: 524_288n,
  sendBufferSize: 106_496n,
});

// The smallest UDP buffer sizes that runwasi gives after a set (fact 7 of
// package W4.5a). The expected values are those of TCP: the minimum of the
// kernel is the same for all sockets.
export const UDP_MIN_RECEIVE_BUFFER = MIN_RECEIVE_BUFFER;
export const UDP_MIN_SEND_BUFFER = MIN_SEND_BUFFER;

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
  for (const t of ["bound", "listening", "connected", "data", "datagram", "credit", "error"]) peer.on(t, route);
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
  // As on runwasi, the idle time goes up to whole seconds.
  setKeepAliveIdleTime(v) {
    if (v <= 0n) throw "invalid-argument";
    this.#options.keepAliveIdleTime = ((v + SECOND - 1n) / SECOND) * SECOND;
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
  // As on runwasi, a small size reads as the minimum of the kernel.
  setReceiveBufferSize(v) {
    if (v === 0n) throw "invalid-argument";
    this.#options.receiveBufferSize = v < MIN_RECEIVE_BUFFER ? MIN_RECEIVE_BUFFER : v;
  }
  sendBufferSize() {
    return this.#options.sendBufferSize;
  }
  setSendBufferSize(v) {
    if (v === 0n) throw "invalid-argument";
    this.#options.sendBufferSize = v < MIN_SEND_BUFFER ? MIN_SEND_BUFFER : v;
  }
}

// UdpOptions keeps the socket options of a UDP socket (TC22). A set to 0
// gives invalid-argument. As on runwasi, a small buffer size reads as the
// minimum of the kernel (fact 7 of package W4.5a).
class UdpOptions {
  #options = { ...UDP_DEFAULTS };
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
    this.#options.receiveBufferSize = udpBufferSize(v, UDP_MIN_RECEIVE_BUFFER);
  }
  sendBufferSize() {
    return this.#options.sendBufferSize;
  }
  setSendBufferSize(v) {
    this.#options.sendBufferSize = udpBufferSize(v, UDP_MIN_SEND_BUFFER);
  }
}

// udpBufferSize gives the value that a UDP buffer size reads after a set of
// v (fact 7 of package W4.5a).
function udpBufferSize(v, min) {
  if (v === 0n) throw "invalid-argument";
  return v < min ? min : v;
}

// The points of UDP that the facts of runwasi (package W4.5a) can change.
// Each one has the expected value of section 8.3.3 and a test in
// udp.test.mjs that names the fact.

// UDP_CAST_CODE is the error code of a multicast or a broadcast address in
// start-bind (wasmtime: validate_unicast).
export const UDP_CAST_CODE = "invalid-argument";
// UDP_BROADCAST_CODE is the error code of the broadcast address in stream
// and send (fact 5): runwasi does not set SO_BROADCAST. A multicast address
// is permitted there (UDP_MULTICAST_REMOTE); the tab node drops the
// datagram, because it has no receivers.
export const UDP_BROADCAST_CODE = "access-denied";
export const UDP_MULTICAST_REMOTE = true;
// UDP_IPV6_REMOTE_CODE is the error code of an IPv6 remote address in stream
// and send (fact 5, rule 4): an IPv6 socket has no socket in the UDP table.
// An IPv6 multicast address is permitted, as on runwasi. The pod worker
// keeps such a remote address and drops such a datagram itself, without a
// message: nothing can receive it.
export const UDP_IPV6_REMOTE_CODE = "remote-unreachable";
// UDP_SEND_PERMIT is the result of check-send in wasmtime (fact 6): a fixed
// permit, not a count of bytes. After a send that would block, check-send
// gives 0 until the pollable of the stream is ready.
export const UDP_SEND_PERMIT = 16;

// unpermitted gives the result of a send of more datagrams than the last
// check-send permitted, or of a send without check-send: wasmtime traps
// (udp.wit). jco makes a trap of a thrown Error.
function unpermitted(n, permit) {
  return new Error(`unpermitted: send of ${n} datagrams, check-send permitted ${permit ?? "nothing"}`);
}

const LOOPBACK4 = Object.freeze({ tag: "ipv4", val: Object.freeze([127, 0, 0, 1]) });
const sameIP = (a, b) => a.tag === b.tag && a.val.every((g, i) => g === b.val[i]);
const sameAddress = (a, b) => a.tag === b.tag && a.val.port === b.val.port && sameIP(ipOf(a), ipOf(b));

// udpAddress examines an ip-socket-address of the component for a UDP socket
// of family (fact 12, rule 4) and gives its ip-address. A remote address (of
// stream and send) must also have an IP and a port that are not 0. These
// answers have no message.
function udpAddress(family, sa, remote) {
  if (!sa || !FAMILIES.has(sa.tag) || sa.tag !== family) throw "invalid-argument";
  const ip = ipOf(sa);
  if (isMapped(ip)) throw "invalid-argument";
  if (!remote) {
    if (isMulticast(ip) || isBroadcast(ip)) throw UDP_CAST_CODE;
    return ip;
  }
  if (isUnspecified(ip) || sa.val.port === 0) throw "invalid-argument";
  if (isBroadcast(ip)) throw UDP_BROADCAST_CODE;
  if (isMulticast(ip)) {
    if (!UDP_MULTICAST_REMOTE) throw UDP_CAST_CODE;
    return ip;
  }
  if (ip.tag === "ipv6") throw UDP_IPV6_REMOTE_CODE;
  return ip;
}

// Udp is the state of one UDP socket and its streams (section 8.3.3): the
// socket ID, the queue of received datagrams (UD5) and the credits of
// datagram bytes (rule 6).
class Udp {
  family;
  state = "unbound"; // unbound, bind-started or bound
  id = null;
  answer = null; // the bound message of start-bind
  bindIP = null;
  local = null;
  podIP = null; // from bound (rule 3)
  remote = null; // the ip-socket-address of stream: the socket is connected
  queue = []; // {data, ip, port}
  size = 0;
  unacked = 0;
  credit = 0; // datagram bytes that the pod worker can send
  error = null; // a pending error of rule 7
  goClosed = null; // closed from the Go side: {error}
  dropped = false;
  streams = 0; // the streams of stream that the component did not drop
  signal = signal();
  #table;
  constructor(table, family) {
    this.#table = table;
    this.family = family;
  }

  // The messages of the Go side.
  handler = {
    bound: (m) => {
      if (this.state === "bind-started" && this.answer === null) {
        this.answer = m;
        // Rule 6: the credit starts after bound.
        if (!m.error) this.credit = WINDOW;
      }
      this.signal.wake();
    },
    datagram: (m) => {
      const ip = parseIP(m.address);
      const data = new Uint8Array(m.bytes);
      // Rule 4: a datagram from another source crossed the connect.
      if (!ip || (this.remote && !this.#fromRemote(ip, m.port))) this.#ack(data.byteLength, true);
      else {
        this.queue.push({ data, ip, port: m.port });
        this.size += data.byteLength;
      }
      this.signal.wake();
    },
    credit: (m) => {
      if (m.kind !== "datagram-bytes") return;
      this.credit += m.n;
      this.signal.wake();
    },
    // Rule 7: one pending error for the next send. A second error before
    // that send does not add a second throw.
    error: (m) => {
      if (this.error === null) this.error = m.error;
      this.signal.wake();
    },
    closed: (m) => {
      this.goClosed = { error: m.error ?? null };
      this.id = null;
      this.signal.wake();
    },
  };

  // shown gives the address that the component sees for a source ip and
  // port (rule 3): the pod IP shows as 127.0.0.1 for a socket that binds to
  // 127.0.0.0/8, and as the remote IP for a socket that connects to
  // 127.0.0.0/8 (127.0.0.1 for the usual connect).
  shown(ip, port) {
    let shown = ip;
    if (this.podIP && sameIP(ip, this.podIP)) {
      const remoteIP = this.remote && ipOf(this.remote);
      if (remoteIP && isLoopback(remoteIP)) shown = remoteIP;
      else if (this.bindIP && isLoopback(this.bindIP)) shown = this.bindIP.tag === "ipv4" ? LOOPBACK4 : this.bindIP;
    }
    return socketAddress(shown, port);
  }
  #fromRemote(ip, port) {
    return sameAddress(this.shown(ip, port), this.remote);
  }

  // newID gives the socket a new ID. An ID of an earlier bind that failed
  // gets closed first (TCP rule 9).
  newID() {
    this.release();
    this.goClosed = null;
    this.id = this.#table.newID();
    this.#table.add(this.id, this.handler);
    return this.id;
  }
  // release sends closed for the ID if the Go side did not close it.
  release() {
    const id = this.id;
    this.id = null;
    if (id !== null && this.#table.remove(id)) this.#table.send("closed", { socket: id });
  }
  send(type, fields, transfer) {
    this.#table.send(type, { socket: this.id, ...fields }, transfer);
  }

  // connect sends connect for the remote address ip and port (rule 4). The
  // address 0.0.0.0 (:: for ipv6) with the port 0 removes the remote address.
  connect(sa) {
    // An IPv6 socket can only connect to a multicast address: the pod worker
    // keeps it without a message (UDP_IPV6_REMOTE_CODE).
    if (this.family === "ipv6") {
      this.remote = sa && copyAddress(sa);
      return;
    }
    if (sa === null) {
      this.remote = null;
      this.send("connect", { protocol: "udp", address: this.family === "ipv4" ? "0.0.0.0" : "::", port: 0 });
      return;
    }
    this.remote = copyAddress(sa);
    this.send("connect", { protocol: "udp", address: ipText(ipOf(sa)), port: sa.val.port });
    // The datagrams of other sources in the queue: wasmtime discards them
    // at receive. The pod worker discards them now and gives the credit back.
    const keep = [];
    let dropped = 0;
    for (const d of this.queue) {
      if (this.#fromRemote(d.ip, d.port)) keep.push(d);
      else dropped += d.data.byteLength;
    }
    this.queue = keep;
    this.size -= dropped;
    if (dropped) this.#ack(dropped, true);
  }

  // check throws for a call of a stream after closed from the Go side or
  // after the drop of the socket.
  check() {
    if (this.goClosed) throw this.goClosed.error ?? "connection-aborted";
    if (this.dropped) throw "invalid-state";
  }
  // takeError throws the pending error of rule 7, one time.
  takeError() {
    if (this.error === null) return;
    const code = this.error;
    this.error = null;
    throw code;
  }

  // #ack sends credit for n datagram bytes (rule 6). It collects the credit
  // up to CREDIT_STEP, or until the queue is empty.
  #ack(n, now = false) {
    this.unacked += n;
    if (!now && this.unacked < CREDIT_STEP && this.size > 0) return;
    if (this.id === null) {
      this.unacked = 0;
      return;
    }
    while (this.unacked > 0) {
      const k = Math.min(this.unacked, LIMITS.credits);
      this.unacked -= k;
      this.send("credit", { kind: "datagram-bytes", n: k });
    }
  }

  // receive gives up to max datagrams from the queue (rules 3 and 4). As on
  // runwasi, it never takes the pending error of rule 7.
  receive(max) {
    this.check();
    if (max === 0n) return [];
    const out = [];
    let taken = 0;
    while (BigInt(out.length) < max && this.queue.length) {
      const d = this.queue.shift();
      this.size -= d.data.byteLength;
      taken += d.data.byteLength;
      out.push({ data: d.data, remoteAddress: this.shown(d.ip, d.port) });
    }
    if (taken) this.#ack(taken);
    return out;
  }

  // writable tells if a datagram of 65,507 bytes fits into the credit
  // (rule 6).
  writable() {
    return this.credit >= LIMITS.datagram;
  }

  // sendOne sends one datagram of the component (rules 4 and 5), or throws
  // its error code.
  sendOne({ data, remoteAddress }) {
    const n = data.byteLength;
    // UD14: the pod worker gives the error code.
    if (n > LIMITS.datagram) throw "datagram-too-large";
    let sa;
    if (remoteAddress === undefined || remoteAddress === null) sa = this.remote;
    else if (!this.remote || sameAddress(remoteAddress, this.remote)) sa = remoteAddress;
    if (!sa) throw "invalid-argument";
    const ip = udpAddress(this.family, sa, true);
    // Rule 7: a send that takes the pending error sends nothing.
    this.takeError();
    // IPv6 multicast: nothing can receive the datagram.
    if (ip.tag === "ipv6") return true;
    // Rule 6: without credit, the send would block.
    if (n > this.credit) return false;
    const bytes = new Uint8Array(data).buffer;
    this.send("datagram", { address: ipText(ip), port: sa.val.port, bytes }, [bytes]);
    this.credit -= n;
    return true;
  }
}

// createUdp gives the UdpSocket, IncomingDatagramStream and
// OutgoingDatagramStream classes and the function createUdpSocket that use
// the socket table (section 8.3.3).
function createUdp({ table, idle }) {
  const { pollableCreate } = table.io;
  const make = Symbol("make");
  const poll = (u, ready) => pollableCreate({ ready, wait: () => (ready() ? Promise.resolve() : u.signal.wait()) });

  class UdpSocket extends UdpOptions {
    #u;
    constructor(family) {
      super();
      this.#u = new Udp(table, family);
    }
    #usable() {
      if (this.#u.goClosed || this.#u.dropped) throw "invalid-state";
    }

    startBind(_network, localAddress) {
      const u = this.#u;
      this.#usable();
      const ip = udpAddress(u.family, localAddress, false);
      if (u.state === "bind-started") throw "concurrency-conflict";
      if (u.state !== "unbound") throw "invalid-state";
      u.newID();
      u.bindIP = ip;
      u.answer = null;
      u.state = "bind-started";
      u.send("bind", { protocol: "udp", family: u.family, address: ipText(ip), port: localAddress.val.port });
    }
    finishBind() {
      const u = this.#u;
      if (u.state !== "bind-started") throw "not-in-progress";
      if (u.goClosed) {
        u.state = "unbound";
        throw u.goClosed.error ?? "connection-aborted";
      }
      if (u.answer === null) throw "would-block";
      const m = u.answer;
      u.answer = null;
      if (m.error) {
        // The ID stays open until the drop or the next bind (TCP rule 9).
        u.state = "unbound";
        u.bindIP = null;
        throw m.error;
      }
      u.state = "bound";
      u.podIP = parseIP(m.address ?? "");
      u.local = socketAddress(u.bindIP, m.port ?? 0);
    }

    // stream (rule 4) does not wait for the Go side. As wasmtime, it traps
    // when the streams of an earlier call are not dropped, and an address
    // that is not correct removes the remote address before the error.
    stream(remoteAddress) {
      const u = this.#u;
      if (u.streams > 0) throw new Error("UDP streams not dropped yet");
      this.#usable();
      if (u.state !== "bound") throw "invalid-state";
      if (remoteAddress === undefined || remoteAddress === null) {
        if (u.remote) u.connect(null);
      } else {
        try {
          udpAddress(u.family, remoteAddress, true);
        } catch (e) {
          if (u.remote) u.connect(null);
          throw e;
        }
        u.connect(remoteAddress);
      }
      u.streams = 2;
      return [new IncomingDatagramStream(make, u), new OutgoingDatagramStream(make, u)];
    }

    localAddress() {
      this.#usable();
      if (this.#u.local === null) throw "invalid-state";
      return copyAddress(this.#u.local);
    }
    remoteAddress() {
      this.#usable();
      if (this.#u.remote === null) throw "invalid-state";
      return copyAddress(this.#u.remote);
    }
    addressFamily() {
      return this.#u.family;
    }
    subscribe() {
      const u = this.#u;
      return poll(u, () => u.goClosed !== null || u.dropped || u.state !== "bind-started" || u.answer !== null);
    }
    // The drop of the socket (TCP rule 9).
    [Symbol.dispose]() {
      const u = this.#u;
      if (u.dropped) return;
      u.dropped = true;
      u.release();
      u.queue = [];
      u.size = 0;
      u.signal.wake();
    }
  }

  class IncomingDatagramStream {
    #u;
    #dropped = false;
    constructor(token, u) {
      if (token !== make) throw new TypeError("IncomingDatagramStream has no constructor");
      this.#u = u;
    }
    receive(maxResults) {
      return this.#u.receive(BigInt(maxResults));
    }
    subscribe() {
      const u = this.#u;
      return poll(u, () => u.queue.length > 0 || u.goClosed !== null || u.dropped);
    }
    [Symbol.dispose]() {
      if (this.#dropped) return;
      this.#dropped = true;
      this.#u.streams--;
    }
  }

  class OutgoingDatagramStream {
    #u;
    #dropped = false;
    #permit = null; // the permit of the last check-send
    #waiting = false; // the last send would block
    constructor(token, u) {
      if (token !== make) throw new TypeError("OutgoingDatagramStream has no constructor");
      this.#u = u;
    }
    // checkSend gives the permit of wasmtime (fact 6): 16, or 0 after a send
    // that would block until the credit permits a datagram of 65,507 bytes.
    // A pending error of rule 7 does not change it.
    checkSend() {
      const u = this.#u;
      u.check();
      if (this.#waiting) {
        if (!u.writable()) return 0n;
        this.#waiting = false;
      }
      this.#permit ??= UDP_SEND_PERMIT;
      return BigInt(this.#permit);
    }
    // send gives the number of datagrams that went. An error on the first
    // datagram throws; an error on a later one ends the list (udp.wit).
    send(datagrams) {
      const u = this.#u;
      u.check();
      if (datagrams.length === 0) return 0n;
      const permit = this.#permit;
      this.#permit = null;
      if (permit === null || datagrams.length > permit) throw unpermitted(datagrams.length, permit);
      let count = 0;
      for (const d of datagrams) {
        let sent;
        try {
          sent = u.sendOne(d);
        } catch (e) {
          if (count > 0 && typeof e === "string") break;
          throw e;
        }
        if (!sent) {
          this.#waiting = true;
          break;
        }
        count++;
      }
      return BigInt(count);
    }
    subscribe() {
      const u = this.#u;
      return poll(u, () => !this.#waiting || u.writable() || u.goClosed !== null || u.dropped);
    }
    [Symbol.dispose]() {
      if (this.#dropped) return;
      this.#dropped = true;
      this.#u.streams--;
    }
  }

  // CO17: finish-bind without an error, receive with datagrams and
  // check-send with more than 0 give data.
  for (const n of COUNTED.udp) {
    UdpSocket.prototype[n] = idle.wrap(UdpSocket.prototype[n], n === "finishBind" ? () => true : () => false);
  }
  IncomingDatagramStream.prototype.receive = idle.wrap(IncomingDatagramStream.prototype.receive, (r) => r.length > 0);
  OutgoingDatagramStream.prototype.checkSend = idle.wrap(OutgoingDatagramStream.prototype.checkSend, (n) => n > 0n);
  OutgoingDatagramStream.prototype.send = idle.wrap(OutgoingDatagramStream.prototype.send);

  const createUdpSocket = (family) => {
    if (!FAMILIES.has(family)) throw "not-supported";
    return new UdpSocket(family);
  };
  return { UdpSocket, IncomingDatagramStream, OutgoingDatagramStream, createUdpSocket };
}

// createSockets gives the object `sockets` for WASIShim: instanceNetwork,
// network, ipNameLookup, tcp, tcpCreateSocket, udp and udpCreateSocket.
// idle is the count of createIdle.
//
// With table (createSocketTable), TCP and name lookup use the messages of
// section 8.3.2 and UDP the messages of section 8.3.3; io is the io.js of
// the shim (the table has it too).
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

  class UdpSocket extends UdpOptions {
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
    const u = createUdp({ table, idle });
    return {
      instanceNetwork: { instanceNetwork: () => network },
      network: { Network },
      ipNameLookup: { ResolveAddressStream: t.ResolveAddressStream, resolveAddresses: t.resolveAddresses },
      tcp: { TcpSocket: t.TcpSocket },
      tcpCreateSocket: { createTcpSocket: t.createTcpSocket },
      udp: { UdpSocket: u.UdpSocket, IncomingDatagramStream: u.IncomingDatagramStream, OutgoingDatagramStream: u.OutgoingDatagramStream },
      udpCreateSocket: { createUdpSocket: u.createUdpSocket },
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
