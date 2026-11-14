// The messages between a pod worker of a component and the Go side of the
// tab node (docs/wasi-sockets.md, section 8.3). k8s/tabernetes/tabnode_wasi_msg.go
// has the same table: a new message or field goes into the two files in the
// same pull request (web/test/wasi_messages.test.mjs compares them).
//
// A message is a plain object {type, ...fields}. Bytes are an ArrayBuffer,
// which the sender transfers. Each entry of MESSAGES gives the direction and
// the fields:
//
//   "<direction> <field>:<kind> <field>?:<kind> ..."
//
// direction: w (pod worker to Go), g (Go to pod worker), b (the two).
// A field with "?" is optional. The kinds are in KINDS. An ID kind has a
// suffix that tells the Go side what to do with the ID:
//
//   (none)  the ID must belong to the container of the pod worker (MS2)
//   +       a new ID: the sender makes it, and it then belongs to the container
//   ~       an ID that belongs to the container, or a new ID
//   -       the ID must belong to the container; the message ends it
//
// The pod worker makes the IDs from 1 to 0x7fffffff, the Go side the IDs from
// 0x80000000 to 0xffffffff. So the two sides never make the same ID.
//
// Each side has a dispatcher: a registry of handlers by message type. Later
// packages add their handlers in files of their own (sockets.js, http.js on
// this side; tabnode_wasi_tcp.go, tabnode_wasi_udp.go, tabnode_wasi_proxy.go on
// the Go side). The Go dispatcher examines each message from a pod worker
// before its handler runs (MS1), and drops a message that fails (MS6).

export const MESSAGES = Object.freeze({
  // Start and end of the pod worker.
  "ready": "w",
  "start": "g component:component args:strings env:strings cwd:path preopens?:preopens",
  "exit": "w code:code error?:text",
  // CLI of a command component.
  "stdout": "w bytes:chunk",
  "stderr": "w bytes:chunk",
  "stdin": "g bytes:chunk end:bool",
  "mem": "w size:size",
  "cpu": "w ms:ms",
  // Sockets.
  "bind": "w socket:socket~ protocol:protocol family:family address:ip port:port",
  "bound": "g socket:socket address?:ip port?:port error?:errcode",
  "listen": "w socket:socket backlog:backlog",
  "listening": "g socket:socket error?:errcode",
  "accepted": "g socket:socket accepted:socket+ localAddress:ip localPort:port remoteAddress:ip remotePort:port",
  "connect": "w socket:socket~ protocol:protocol address:ip port:port",
  "connected": "g socket:socket localAddress?:ip localPort?:port remoteAddress?:ip remotePort?:port error?:errcode",
  "data": "b socket:socket bytes:data",
  "datagram": "b socket:socket address:ip port:port bytes:datagram",
  "credit": "b socket:socket kind:credit n:credits",
  "error": "g socket:socket error:errcode",
  "shutdown": "w socket:socket how:how",
  "closed": "b socket:socket- error?:errcode",
  "lookup": "w lookup:lookup+ name:hostname",
  "addresses": "g lookup:lookup- addresses?:ips error?:errcode",
  // HTTP.
  "request": "g request:request+ method:method url:url headers:headers body:body",
  "response": "w request:request- status?:status headers?:headers body?:body error?:errcode",
  "cancel": "g request:request",
  "fetch": "w fetch:fetch+ method:method url:url headers:headers body:body",
  "fetched": "g fetch:fetch- status?:status headers?:headers body?:body error?:errcode",
  "fetch-cancel": "w fetch:fetch-",
});

// The limits of the fields (section 8.4). The Go side has the same numbers.
export const LIMITS = Object.freeze({
  chunk: 1 << 20, // stdout, stderr and stdin: bytes in one message
  data: 256 << 10, // TCP bytes: one credit window
  datagram: 65535,
  body: 16 << 20, // HTTP bodies
  credits: 256 << 10,
  backlog: 4096,
  text: 4096,
  path: 4096,
  hostname: 253,
  url: 8192,
  method: 64,
  headers: 1000, // header lines
  headerName: 256,
  headerValue: 64 << 10,
  ips: 256,
  strings: 65536, // arguments or environment variables
  string: 1 << 20, // one argument or environment variable
  preopens: 256, // preopened directories of start
});

const u32 = (v) => Number.isInteger(v) && v >= 0 && v <= 0xffffffff;
const id = (v) => Number.isInteger(v) && v >= 1 && v <= 0xffffffff;
const str = (max) => (v) => typeof v === "string" && v.length <= max;
const buf = (max) => (v) => v instanceof ArrayBuffer && v.byteLength <= max;
const oneOf = (...a) => (v) => a.includes(v);
const IPV4 = /^(?:(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\.){3}(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)$/;
// ip accepts the text forms of IPv4 and IPv6 addresses, without a zone.
export function ip(v) {
  if (typeof v !== "string" || v.length > 45) return false;
  if (IPV4.test(v)) return true;
  if (!/^[0-9a-fA-F:.]+$/.test(v) || !v.includes(":")) return false;
  let s = v;
  let groups = 0;
  const last = s.lastIndexOf(":");
  if (s.slice(last + 1).includes(".")) {
    if (!IPV4.test(s.slice(last + 1))) return false;
    s = s.slice(0, last + 1) + "0:0";
  }
  const halves = s.split("::");
  if (halves.length > 2) return false;
  for (const h of halves) {
    if (h === "") continue;
    for (const g of h.split(":")) {
      if (!/^[0-9a-fA-F]{1,4}$/.test(g)) return false;
      groups++;
    }
  }
  return halves.length === 2 ? groups <= 7 : groups === 8;
}

// KINDS gives the test of each kind of field. The ID kinds are "socket",
// "request", "fetch" and "lookup".
export const KINDS = Object.freeze({
  socket: id,
  request: id,
  fetch: id,
  lookup: id,
  bool: (v) => typeof v === "boolean",
  code: (v) => Number.isInteger(v) && v >= 0 && v <= 255,
  size: (v) => Number.isSafeInteger(v) && v >= 0,
  ms: (v) => typeof v === "number" && Number.isFinite(v) && v >= 0 && v <= 3600e3,
  port: (v) => Number.isInteger(v) && v >= 0 && v <= 65535,
  backlog: (v) => Number.isInteger(v) && v >= 1 && v <= LIMITS.backlog,
  credits: (v) => Number.isInteger(v) && v >= 0 && v <= LIMITS.credits,
  status: (v) => Number.isInteger(v) && v >= 100 && v <= 999,
  protocol: oneOf("tcp", "udp"),
  family: oneOf("ipv4", "ipv6"),
  how: oneOf("receive", "send", "both"),
  credit: oneOf("bytes", "datagram-bytes", "connections"),
  errcode: (v) => typeof v === "string" && v.length <= 64 && /^[a-z][a-z0-9-]*$/.test(v),
  text: str(LIMITS.text),
  path: (v) => str(LIMITS.path)(v) && v.startsWith("/"),
  hostname: (v) => str(LIMITS.hostname)(v) && v.length > 0,
  url: (v) => str(LIMITS.url)(v) && v.length > 0,
  method: (v) => str(LIMITS.method)(v) && /^[!#$%&'*+.^_`|~0-9A-Za-z-]+$/.test(v),
  ip,
  ips: (v) => Array.isArray(v) && v.length <= LIMITS.ips && v.every(ip),
  strings: (v) => Array.isArray(v) && v.length <= LIMITS.strings && v.every(str(LIMITS.string)),
  headers: (v) => Array.isArray(v) && v.length <= LIMITS.headers && v.every((h) =>
    Array.isArray(h) && h.length === 2 && str(LIMITS.headerName)(h[0]) && h[0].length > 0 && buf(LIMITS.headerValue)(h[1])),
  chunk: buf(LIMITS.chunk),
  data: buf(LIMITS.data),
  datagram: buf(LIMITS.datagram),
  body: buf(LIMITS.body),
  // The copy of the files of the container (wasmPreopens of tabnode_wasi.go,
  // section 8.10): [{guest, readOnly, files: [[path, Uint8Array]], dirs:
  // [path]}]. Only Go sends it; the size limit is wasmSnapshotLimit there.
  preopens: (v) => Array.isArray(v) && v.length <= LIMITS.preopens && v.every((p) =>
    p !== null && typeof p === "object" && KINDS.path(p.guest) && typeof p.readOnly === "boolean" &&
    Array.isArray(p.files) && Array.isArray(p.dirs)),
  // The output of the transpile cache (wasi.js): only Go sends it.
  component: (v) => v !== null && typeof v === "object" && typeof v.js === "string" && v.modules !== null && typeof v.modules === "object",
});

// SPECS is MESSAGES in parsed form: {type: {dir, fields: [{name, kind, id, optional}]}}.
// id is the suffix of an ID kind ("", "+", "~" or "-"), or null.
export const SPECS = Object.freeze(Object.fromEntries(Object.entries(MESSAGES).map(([type, s]) => {
  const [dir, ...fs] = s.split(" ");
  if (!["w", "g", "b"].includes(dir)) throw new Error(`message ${type}: direction ${dir}`);
  const fields = fs.map((f) => {
    const m = /^([a-zA-Z]+)(\??):([a-z]+)([+~-]?)$/.exec(f);
    if (!m || !KINDS[m[3]]) throw new Error(`message ${type}: field ${f}`);
    const isID = ["socket", "request", "fetch", "lookup"].includes(m[3]);
    if (m[4] && !isID) throw new Error(`message ${type}: field ${f}: only an ID has a suffix`);
    return Object.freeze({ name: m[1], kind: m[3], id: isID ? m[4] : null, optional: m[2] === "?" });
  });
  return [type, Object.freeze({ dir, fields: Object.freeze(fields) })];
})));

// examine gives "" for a correct message that goes in the direction dir ("w"
// or "g"), else the reason. It examines the type, the direction, each field
// and its size. It does not know which IDs belong to the container: the Go
// side examines that.
export function examine(msg, dir) {
  if (msg === null || typeof msg !== "object" || Array.isArray(msg)) return "the message is not an object";
  const spec = typeof msg.type === "string" && Object.hasOwn(SPECS, msg.type) ? SPECS[msg.type] : null;
  if (!spec) return `unknown message type ${JSON.stringify(String(msg.type)).slice(0, 80)}`;
  if (spec.dir !== "b" && spec.dir !== dir) return `${msg.type} does not go from ${dir === "w" ? "the pod worker to Go" : "Go to the pod worker"}`;
  for (const k of Object.keys(msg)) {
    if (k !== "type" && !spec.fields.some((f) => f.name === k)) return `${msg.type}: unknown field ${JSON.stringify(k).slice(0, 80)}`;
  }
  for (const f of spec.fields) {
    const v = msg[f.name];
    if (v === undefined) {
      if (f.optional) continue;
      return `${msg.type}: field ${f.name} is missing`;
    }
    if (!KINDS[f.kind](v)) return `${msg.type}: field ${f.name} is not a correct ${f.kind}`;
  }
  return "";
}

// createPeer gives the dispatcher of the pod worker. post(msg, transfer)
// sends a message to the Go side. onDrop(reason, msg) gets each message from
// the Go side that fails examine, or that has no handler.
//
//   peer.on(type, fn)        fn(msg) handles the messages of type from Go
//   peer.dispatch(msg)       give a message from Go to its handler
//   peer.send(type, fields, transfer)
//
// send throws for a message that examine rejects: that is an error of the
// code in the pod worker, not of the component.
export function createPeer({ post, onDrop = () => {} }) {
  const handlers = new Map();
  return {
    on(type, fn) {
      const spec = SPECS[type];
      if (!spec || spec.dir === "w") throw new Error(`the pod worker cannot receive ${type}`);
      if (handlers.has(type)) throw new Error(`${type} has a handler already`);
      handlers.set(type, fn);
    },
    dispatch(msg) {
      const why = examine(msg, "g");
      if (why) return onDrop(why, msg);
      const fn = handlers.get(msg.type);
      if (!fn) return onDrop(`no handler for ${msg.type}`, msg);
      return fn(msg);
    },
    send(type, fields = {}, transfer = []) {
      const msg = { type, ...fields };
      const why = examine(msg, "w");
      if (why) throw new Error(`the pod worker cannot send this message: ${why}`);
      post(msg, transfer);
    },
  };
}
