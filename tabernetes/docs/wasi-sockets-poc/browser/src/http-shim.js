// Candidate A of phase 0, item 12: the HTTP of preview2-shim with objects
// that have the functions of Request, Response and Headers, but not the rules
// of the browser. The shim makes `new Headers()` and `new Response()` itself
// (dist/browser/http.js, lines 365, 620, 662, 669 and 691), so the pod worker
// must replace the globals. method() gives {tag: method.toLowerCase()} (line
// 581), and a failure of fetch becomes internal-error (lines 739 to 744):
// both need a wrapper.
//
// createShimHttp({ fetch }) gives { handle(msg, handler) } and installs the
// globals. handle() returns {status, headers, body} or {error} or {none}.
import { types, handleIncomingRequest } from "@bytecodealliance/preview2-shim/http";

class LaxHeaders {
  #l = [];
  constructor(init) {
    if (!init) return;
    for (const [k, v] of Array.isArray(init) || typeof init.entries !== "function" ? init : init.entries()) this.append(k, v);
  }
  append(k, v) { this.#l.push([String(k).toLowerCase(), String(v)]); }
  set(k, v) { this.delete(k); this.append(k, v); }
  delete(k) { const n = String(k).toLowerCase(); this.#l = this.#l.filter(([x]) => x !== n); }
  get(k) { const n = String(k).toLowerCase(); const v = this.#l.filter(([x]) => x === n).map(([, y]) => y); return v.length ? v.join(", ") : null; }
  has(k) { return this.get(k) !== null; }
  forEach(cb) { for (const [k, v] of this.#l) cb(v, k, this); }
  entries() { return this.#l.map(([k, v]) => [k, v])[Symbol.iterator](); }
  [Symbol.iterator]() { return this.entries(); }
}

const toBytes = (b) => (b == null ? null : b instanceof Uint8Array ? b : typeof b === "string" ? new TextEncoder().encode(b) : new Uint8Array(b));

class LaxResponse {
  constructor(body, init = {}) {
    this.status = init.status ?? 200;
    this.headers = init.headers instanceof LaxHeaders ? init.headers : new LaxHeaders(init.headers);
    this._bytes = body instanceof ReadableStream ? null : toBytes(body);
    this._stream = body instanceof ReadableStream ? body : null;
  }
  get body() {
    if (this._stream) return this._stream;
    return this._bytes ? new Blob([this._bytes]).stream() : null;
  }
  async arrayBuffer() { return this._bytes ? this._bytes.buffer : new ArrayBuffer(0); }
  clone() { return this; }
}

export function createShimHttp({ fetch }) {
  globalThis.Headers = LaxHeaders;
  globalThis.Response = LaxResponse;

  // The method: a String whose toLowerCase() keeps a method that is not
  // standard, marked with a NUL character.
  const STANDARD = ["GET", "HEAD", "POST", "PUT", "DELETE", "CONNECT", "OPTIONS", "TRACE", "PATCH"];
  const method = types.IncomingRequest.prototype.method;
  types.IncomingRequest.prototype.method = function () {
    const m = method.call(this);
    return m.tag.startsWith("\0") ? { tag: "other", val: m.tag.slice(1) } : m;
  };
  const methodOf = (m) => {
    const s = new String(m);
    s.toLowerCase = () => (STANDARD.includes(m) ? m.toLowerCase() : `\0${m}`);
    return s;
  };

  // An error code as the response: the shim makes a 500 of it.
  const errors = new WeakMap();
  const set = types.ResponseOutparam.set;
  types.ResponseOutparam.set = (param, result) => {
    if (result.tag === "err") errors.set(param, result.val);
    return set(param, result);
  };

  // Client requests: globalThis.fetch to the Go side. The error code goes in
  // the message of the Error, and a wrapper of get() takes it out again.
  globalThis.fetch = async (url, init) => {
    const headers = [];
    init.headers?.forEach((v, k) => headers.push([k, new TextEncoder().encode(v)]));
    const r = await fetch({ method: init.method, url: String(url), headers, body: toBytes(init.body) ?? new Uint8Array() });
    if (r.error) throw new Error(`\0${JSON.stringify(r.error)}`);
    return new LaxResponse(r.body, { status: r.status, headers: r.headers.map(([k, v]) => [k, new TextDecoder().decode(v)]) });
  };
  const get = types.FutureIncomingResponse.prototype.get;
  types.FutureIncomingResponse.prototype.get = function () {
    const r = get.call(this);
    const e = r?.tag === "ok" && r.val.tag === "err" ? r.val.val : null;
    if (e?.tag === "internal-error" && e.val?.startsWith("\0")) r.val.val = JSON.parse(e.val.slice(1));
    return r;
  };

  return {
    http: undefined, // the default of WASIShim: the shim
    async handle(msg, handler) {
      const dec = new TextDecoder();
      const req = {
        method: methodOf(msg.method),
        url: `http://${msg.authority}${msg.path}`,
        headers: new LaxHeaders(msg.headers.map(([k, v]) => [k, dec.decode(v)])),
        body: msg.body?.length ? new Blob([msg.body]).stream() : null,
        clone() { return { ...this, arrayBuffer: async () => msg.body?.buffer ?? new ArrayBuffer(0) }; },
      };
      let param;
      let resp;
      try {
        resp = await handleIncomingRequest(req, (r, out) => { param = out; return handler(r, out); });
      } catch (e) {
        if (/without setting its response/.test(e?.message)) return { none: true };
        throw e;
      }
      if (errors.has(param)) return { error: errors.get(param) };
      const headers = [];
      resp.headers.forEach((v, k) => headers.push([k, new TextEncoder().encode(v)]));
      return { status: resp.status, headers, body: resp._bytes ?? new Uint8Array() };
    },
  };
}
