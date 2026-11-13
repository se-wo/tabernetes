// Candidate B of phase 0, item 12: a wasi:http of Tabernetes for the option
// `http` of WASIShim. No browser Request, Response or Headers object holds
// the data, so no header, method or status code changes on the way.
//
// createHttp({ fetch, limit }) gives { types, outgoingHandler, incomingHandler }
// and two helpers for the pod worker:
//   incoming(msg)        an IncomingRequest from a `request` message
//   outparam()           [ResponseOutparam, Promise of the response]
// fetch({method, url, headers, body}) returns a Promise of
// {status, headers, body} or {error: <wasi:http error-code>}: the Go side.
import { inputStreamCreate, outputStreamCreate, pollableCreate, ioErrorCreate } from "@bytecodealliance/preview2-shim/io";

const enc = new TextEncoder();
const dec = new TextDecoder();
const TOKEN = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/;
const VALUE = /^[\t\x20-\x7E\x80-\xFF]*$/;
const METHODS = ["get", "head", "post", "put", "delete", "connect", "options", "trace", "patch"];
const bytes = (v) => (typeof v === "string" ? enc.encode(v) : new Uint8Array(v));

export function createHttp({ fetch, limit = 16 << 20 } = {}) {
  class Fields {
    #list = [];
    #locked = false;
    static fromList(entries) {
      const f = new Fields();
      for (const [k, v] of entries) f.append(k, v);
      return f;
    }
    static _locked(entries) {
      const f = Fields.fromList(entries);
      f.#locked = true;
      return f;
    }
    _lock() { this.#locked = true; return this; }
    #check(name, value) {
      if (this.#locked) throw { tag: "immutable" };
      if (!TOKEN.test(name)) throw { tag: "invalid-syntax" };
      if (value !== undefined && !VALUE.test(dec.decode(value))) throw { tag: "invalid-syntax" };
    }
    get(name) {
      const n = name.toLowerCase();
      return this.#list.filter(([k]) => k.toLowerCase() === n).map(([, v]) => v.slice());
    }
    has(name) { return this.get(name).length > 0; }
    set(name, values) {
      for (const v of values) this.#check(name, v);
      this.delete(name);
      for (const v of values) this.#list.push([name, bytes(v)]);
    }
    delete(name) {
      this.#check(name);
      const n = name.toLowerCase();
      this.#list = this.#list.filter(([k]) => k.toLowerCase() !== n);
    }
    append(name, value) {
      this.#check(name, value);
      this.#list.push([name, bytes(value)]);
    }
    entries() { return this.#list.map(([k, v]) => [k, v.slice()]); }
    clone() { return Fields.fromList(this.#list); }
  }

  // A body that is one complete block: the input stream gives it, then
  // `closed`.
  function blockStream(data) {
    let off = 0;
    const read = (len) => {
      if (off >= data.length) throw { tag: "closed" };
      const n = Math.min(Number(len), data.length - off);
      const out = data.slice(off, off + n);
      off += n;
      return out;
    };
    return inputStreamCreate({ read, blockingRead: read, subscribe: () => pollableCreate() });
  }

  class FutureTrailers {
    #done = false;
    subscribe() { return pollableCreate(); }
    get() {
      if (this.#done) return { tag: "err", val: undefined };
      this.#done = true;
      return { tag: "ok", val: { tag: "ok", val: undefined } };
    }
  }

  class IncomingBody {
    #data;
    #taken = false;
    constructor(data) { this.#data = data; }
    stream() {
      if (this.#taken) throw undefined;
      this.#taken = true;
      return blockStream(this.#data);
    }
    static finish() { return new FutureTrailers(); }
  }

  class IncomingRequest {
    #m; #path; #scheme; #authority; #headers; #body;
    constructor(msg) {
      this.#m = msg.method;
      this.#path = msg.path;
      this.#scheme = msg.scheme ?? "http";
      this.#authority = msg.authority;
      this.#headers = Fields._locked(msg.headers);
      this.#body = new IncomingBody(msg.body ?? new Uint8Array());
    }
    method() {
      const l = this.#m.toLowerCase();
      return METHODS.includes(l) && l.toUpperCase() === this.#m ? { tag: l } : { tag: "other", val: this.#m };
    }
    pathWithQuery() { return this.#path; }
    scheme() { return this.#scheme === "https" ? { tag: "HTTPS" } : { tag: "HTTP" }; }
    authority() { return this.#authority; }
    headers() { return this.#headers; }
    consume() {
      const b = this.#body;
      if (!b) throw undefined;
      this.#body = null;
      return b;
    }
  }

  class OutgoingBody {
    #chunks = [];
    #size = 0;
    #stream = null;
    #finish;
    _finished = new Promise((r) => (this.#finish = r));
    constructor() {
      const self = this;
      this.#stream = outputStreamCreate({
        checkWrite: () => 1n << 20n,
        write(buf) {
          self.#size += buf.byteLength;
          if (self.#size > limit) throw { tag: "last-operation-failed", val: ioErrorCreate("body larger than the limit") };
          self.#chunks.push(buf.slice());
        },
        flush() {},
        blockingFlush() {},
        subscribe: () => pollableCreate(),
      });
    }
    write() {
      const s = this.#stream;
      if (!s) throw undefined;
      this.#stream = null;
      return s;
    }
    static finish(body, trailers) {
      if (trailers) throw { tag: "internal-error", val: "trailers are not supported" };
      const out = new Uint8Array(body.#size);
      let off = 0;
      for (const c of body.#chunks) { out.set(c, off); off += c.byteLength; }
      body.#finish(out);
    }
  }

  class OutgoingResponse {
    #status = 200;
    #headers;
    #body = null;
    constructor(headers) { this.#headers = headers._lock(); }
    statusCode() { return this.#status; }
    setStatusCode(code) {
      if (!Number.isInteger(code) || code < 100 || code > 999) throw undefined;
      this.#status = code;
    }
    headers() { return this.#headers; }
    body() {
      if (this.#body) throw undefined;
      this.#body = new OutgoingBody();
      return this.#body;
    }
    _parts() { return { status: this.#status, headers: this.#headers.entries(), body: this.#body }; }
  }

  class ResponseOutparam {
    #resolve = null;
    static _create() {
      const p = new ResponseOutparam();
      const done = new Promise((r) => (p.#resolve = r));
      return [p, done];
    }
    static set(param, result) {
      if (!param.#resolve) throw new Error("response outparam already set");
      const resolve = param.#resolve;
      param.#resolve = null;
      if (result.tag === "err") { resolve({ error: result.val }); return; }
      const { status, headers, body } = result.val._parts();
      resolve({ status, headers, body: body ? body._finished : Promise.resolve(new Uint8Array()) });
    }
  }

  class RequestOptions {
    #t = {};
    connectTimeout() { return this.#t.c; }
    setConnectTimeout(d) { this.#t.c = d; }
    firstByteTimeout() { return this.#t.f; }
    setFirstByteTimeout(d) { this.#t.f = d; }
    betweenBytesTimeout() { return this.#t.b; }
    setBetweenBytesTimeout(d) { this.#t.b = d; }
  }

  class OutgoingRequest {
    #method = { tag: "get" };
    #scheme; #path; #authority;
    #headers;
    #body = new OutgoingBody();
    #bodyTaken = false;
    constructor(headers) { this.#headers = headers._lock(); }
    method() { return this.#method; }
    setMethod(m) { this.#method = m; }
    pathWithQuery() { return this.#path; }
    setPathWithQuery(p) { this.#path = p; }
    scheme() { return this.#scheme; }
    setScheme(s) { this.#scheme = s; }
    authority() { return this.#authority; }
    setAuthority(a) { this.#authority = a; }
    headers() { return this.#headers; }
    body() {
      if (this.#bodyTaken) throw undefined;
      this.#bodyTaken = true;
      return this.#body;
    }
    _parts() {
      const m = this.#method.tag === "other" ? this.#method.val : this.#method.tag.toUpperCase();
      const scheme = this.#scheme?.tag === "HTTPS" ? "https" : "http";
      return {
        method: m,
        url: `${scheme}://${this.#authority ?? ""}${this.#path ?? "/"}`,
        headers: this.#headers.entries(),
        body: this.#bodyTaken ? this.#body._finished : Promise.resolve(new Uint8Array()),
      };
    }
  }

  class IncomingResponse {
    #status; #headers; #body;
    constructor(r) {
      this.#status = r.status;
      this.#headers = Fields._locked(r.headers);
      this.#body = new IncomingBody(r.body ?? new Uint8Array());
    }
    status() { return this.#status; }
    headers() { return this.#headers; }
    consume() {
      const b = this.#body;
      if (!b) throw undefined;
      this.#body = null;
      return b;
    }
  }

  class FutureIncomingResponse {
    #result;
    #promise;
    #taken = false;
    constructor(promise) {
      this.#promise = promise.then((r) => {
        this.#result = r.error ? { tag: "err", val: r.error } : { tag: "ok", val: new IncomingResponse(r) };
      });
    }
    subscribe() { return pollableCreate(this.#promise); }
    get() {
      if (!this.#result) return undefined;
      if (this.#taken) return { tag: "err", val: undefined };
      this.#taken = true;
      return { tag: "ok", val: this.#result };
    }
  }

  const outgoingHandler = {
    handle(request) {
      const p = request._parts();
      return new FutureIncomingResponse(p.body.then((body) => fetch({ ...p, body })));
    },
  };

  const types = {
    Fields, IncomingRequest, OutgoingRequest, RequestOptions, ResponseOutparam, IncomingResponse,
    IncomingBody, FutureTrailers, OutgoingResponse, OutgoingBody, FutureIncomingResponse,
    httpErrorCode: () => undefined,
  };
  return {
    http: { types, outgoingHandler, incomingHandler: {} },
    incoming: (msg) => new IncomingRequest(msg),
    outparam: () => ResponseOutparam._create(),
  };
}
