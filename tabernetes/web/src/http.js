// The wasi:http of Tabernetes (docs/wasi-sockets.md, decision 4 and section
// 8.6) for the option `http` of WASIShim. No browser Request, Response or
// Headers object holds the data, so no header, method or status code changes
// on the way (fact 19). Bodies are complete blocks of a maximum of 16 MiB.
// The base is docs/wasi-sockets-poc/browser/src/http-own.js of phase 0.
//
// createHttp gives the wasi:http of one instance of a proxy component:
//
//   http                      the object for the option http of WASIShim
//   incoming(msg)             an incoming-request from a request message
//   outparam()                [response-outparam, state]: state.result is
//                             undefined until the component sets the
//                             outparam, then {error: code} or {response}
//   result(state, trapped)    the fields of the response message for the
//                             state after handle (section 8.3.1)
//
// The pod worker gives each instance its own createHttp, so a cancel of one
// instance (PX24) can fail each wait of that instance.

// The limit of a request body and a response body (decision 4).
export const BODY_LIMIT = 16 << 20;

// The headers that wasmtime removes from a request that arrives, and that a
// component cannot set (fact 20).
export const FORBIDDEN = new Set([
  "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "proxy-connection",
  "transfer-encoding", "upgrade", "host", "http2-settings",
]);

// The cases of error-code of wasi:http/types 0.2. A case with a record
// payload needs an object: the lowering of jco reads its fields. A case with
// an option payload or without a payload takes undefined.
const RECORD_PAYLOADS = {
  "DNS-error": () => ({ rcode: undefined, infoCode: undefined }),
  "TLS-alert-received": () => ({ alertId: undefined, alertMessage: undefined }),
  "HTTP-request-trailer-size": () => ({ fieldName: undefined, fieldSize: undefined }),
  "HTTP-response-header-size": () => ({ fieldName: undefined, fieldSize: undefined }),
  "HTTP-response-trailer-size": () => ({ fieldName: undefined, fieldSize: undefined }),
};
export const ERROR_CODES = new Set([
  "DNS-timeout", "DNS-error", "destination-not-found", "destination-unavailable", "destination-IP-prohibited",
  "destination-IP-unroutable", "connection-refused", "connection-terminated", "connection-timeout",
  "connection-read-timeout", "connection-write-timeout", "connection-limit-reached", "TLS-protocol-error",
  "TLS-certificate-error", "TLS-alert-received", "HTTP-request-denied", "HTTP-request-length-required",
  "HTTP-request-body-size", "HTTP-request-method-invalid", "HTTP-request-URI-invalid", "HTTP-request-URI-too-long",
  "HTTP-request-header-section-size", "HTTP-request-header-size", "HTTP-request-trailer-section-size",
  "HTTP-request-trailer-size", "HTTP-response-incomplete", "HTTP-response-header-section-size",
  "HTTP-response-header-size", "HTTP-response-body-size", "HTTP-response-trailer-section-size",
  "HTTP-response-trailer-size", "HTTP-response-transfer-coding", "HTTP-response-content-coding",
  "HTTP-response-timeout", "HTTP-upgrade-failed", "HTTP-protocol-error", "loop-detected", "configuration-error",
  "internal-error",
]);

// errorCode gives the error-code variant for the name of a code from the Go
// side, without a change (CR9). A name that wasi:http does not have gives
// internal-error with the name as its text.
export function errorCode(code) {
  if (!ERROR_CODES.has(code)) return { tag: "internal-error", val: String(code) };
  return { tag: code, val: RECORD_PAYLOADS[code]?.() };
}

const METHODS = ["get", "head", "post", "put", "delete", "connect", "options", "trace", "patch"];
const TOKEN = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/;

// methodVariant gives the method variant of wasi:http for a method name.
// Methods are case-sensitive: "get" is a method that is not standard.
export function methodVariant(m) {
  const l = m.toLowerCase();
  return METHODS.includes(l) && l.toUpperCase() === m ? { tag: l } : { tag: "other", val: m };
}

const methodName = (v) => (v.tag === "other" ? v.val : v.tag.toUpperCase());

// splitURL gives the authority and the request target of the url of a
// request message: "http://", the value of the host header and the request
// target with no change (section 8.3.1). The host header has no "/", so the
// target starts at the first "/" or "*" after "http://".
export function splitURL(url) {
  const rest = url.startsWith("http://") ? url.slice(7) : url;
  let i = 0;
  while (i < rest.length && rest[i] !== "/" && rest[i] !== "*") i++;
  return { authority: rest.slice(0, i), pathWithQuery: rest.slice(i) || "/" };
}

// A header value of wasmtime (http::HeaderValue::from_bytes): no control
// characters other than HTAB, no DEL.
function validValue(v) {
  for (const b of v) if ((b < 0x20 && b !== 0x09) || b === 0x7f) return false;
  return true;
}

const enc = new TextEncoder();
const bytes = (v) => (typeof v === "string" ? enc.encode(v) : new Uint8Array(v));

// createHttp gives the wasi:http of one instance.
//
//   io      the io.js of the shim: pollableCreate, inputStreamCreate,
//           outputStreamCreate and ioErrorCreate
//   idle    the count of CO17 (sockets.js): reads of bodies without data
//   fetch   fetch({method, url, headers, body}) gives {result, cancel}:
//           result is a promise of the fields of the fetched message, cancel
//           sends fetch-cancel if fetched did not arrive (CR10)
//   wait    wait(promise) gives a promise that also fails when the instance
//           is cancelled (PX24)
//   limit   the body limit
export function createHttp({ io, idle = null, fetch, wait = (p) => p, limit = BODY_LIMIT }) {
  const { pollableCreate, inputStreamCreate, outputStreamCreate, ioErrorCreate } = io;
  const ready = () => pollableCreate();

  class Fields {
    #list = [];
    #immutable = false;
    static fromList(entries) {
      const f = new Fields();
      for (const [k, v] of entries) f.append(k, v);
      return f;
    }
    // _of gives immutable fields with entries from the Go side. They have no
    // check: the Go side examined them, and fact 20 removes the forbidden
    // names.
    static _of(entries) {
      const f = new Fields();
      for (const [k, v] of entries) {
        const name = k.toLowerCase();
        if (!FORBIDDEN.has(name)) f.#list.push([name, bytes(v)]);
      }
      f.#immutable = true;
      return f;
    }
    _lock() {
      this.#immutable = true;
      return this;
    }
    #check(name, values = []) {
      if (!TOKEN.test(name)) throw { tag: "invalid-syntax" };
      if (FORBIDDEN.has(name.toLowerCase())) throw { tag: "forbidden" };
      if (this.#immutable) throw { tag: "immutable" };
      for (const v of values) if (!validValue(bytes(v))) throw { tag: "invalid-syntax" };
    }
    get(name) {
      const n = name.toLowerCase();
      return this.#list.filter(([k]) => k === n).map(([, v]) => v.slice());
    }
    has(name) {
      const n = name.toLowerCase();
      return this.#list.some(([k]) => k === n);
    }
    set(name, values) {
      this.#check(name, values);
      const n = name.toLowerCase();
      this.#list = this.#list.filter(([k]) => k !== n);
      for (const v of values) this.#list.push([n, bytes(v)]);
    }
    delete(name) {
      this.#check(name);
      const n = name.toLowerCase();
      this.#list = this.#list.filter(([k]) => k !== n);
    }
    append(name, value) {
      this.#check(name, [value]);
      this.#list.push([name.toLowerCase(), bytes(value)]);
    }
    entries() {
      return this.#list.map(([k, v]) => [k, v.slice()]);
    }
    clone() {
      const f = new Fields();
      f.#list = this.#list.map(([k, v]) => [k, v.slice()]);
      return f;
    }
  }

  // blockStream gives an input stream of a complete body: its bytes, then
  // closed. read goes through the count of CO17.
  function blockStream(data) {
    let off = 0;
    const take = (len) => {
      if (off >= data.length) throw { tag: "closed" };
      const n = Math.min(Number(len > 0x7fffffffn ? 0x7fffffffn : len), data.length - off);
      const out = data.slice(off, off + n);
      off += n;
      return out;
    };
    const read = idle ? idle.wrap(take, (b) => b.byteLength > 0) : take;
    const size = (b) => (b instanceof Promise ? b.then((x) => BigInt(x.byteLength)) : BigInt(b.byteLength));
    return inputStreamCreate({
      read,
      blockingRead: take,
      skip: (len) => size(read(len)),
      blockingSkip: (len) => BigInt(take(len).byteLength),
      subscribe: ready,
    });
  }

  class FutureTrailers {
    #taken = false;
    subscribe() {
      return ready();
    }
    get() {
      if (this.#taken) return { tag: "err", val: undefined };
      this.#taken = true;
      return { tag: "ok", val: { tag: "ok", val: undefined } };
    }
  }

  class IncomingBody {
    #data;
    #taken = false;
    constructor(data) {
      this.#data = data;
    }
    stream() {
      if (this.#taken) throw undefined;
      this.#taken = true;
      return blockStream(this.#data);
    }
    static finish(_body) {
      return new FutureTrailers();
    }
  }

  class IncomingRequest {
    #method;
    #path;
    #authority;
    #headers;
    #body;
    constructor(msg) {
      const { authority, pathWithQuery } = splitURL(msg.url);
      this.#method = msg.method;
      this.#path = pathWithQuery;
      this.#authority = authority || undefined;
      this.#headers = Fields._of(msg.headers ?? []);
      this.#body = new IncomingBody(new Uint8Array(msg.body ?? 0));
    }
    method() {
      return methodVariant(this.#method);
    }
    pathWithQuery() {
      return this.#path;
    }
    scheme() {
      return { tag: "HTTP" };
    }
    authority() {
      return this.#authority;
    }
    headers() {
      return this.#headers;
    }
    consume() {
      const b = this.#body;
      if (!b) throw undefined;
      this.#body = null;
      return b;
    }
  }

  // OutgoingBody keeps the bytes of a response body or a request body. A
  // write after the limit fails (PX18, PX20); the body then has over set.
  class OutgoingBody {
    #chunks = [];
    #size = 0;
    #taken = false;
    #stream = null;
    #done;
    over = false;
    finished = false;
    dropped = false;
    // _finished resolves with the bytes after finish, and rejects when the
    // component drops the body without finish.
    _finished = new Promise((resolve, reject) => (this.#done = { resolve, reject }));
    constructor() {
      this._finished.catch(() => {});
    }
    write() {
      if (this.#taken) throw undefined;
      this.#taken = true;
      const body = this;
      let failed = false;
      this.#stream = outputStreamCreate({
        checkWrite() {
          if (failed || body.finished) throw { tag: "closed" };
          idle?.call(true);
          return 1n << 20n;
        },
        write(buf) {
          if (failed || body.finished) throw { tag: "closed" };
          idle?.call(true);
          if (body.#size + buf.byteLength > limit) {
            failed = true;
            body.over = true;
            throw { tag: "last-operation-failed", val: ioErrorCreate("the body is larger than 16 MiB") };
          }
          body.#size += buf.byteLength;
          body.#chunks.push(buf.slice());
        },
        flush() {},
        blockingFlush() {},
        subscribe: ready,
      });
      return this.#stream;
    }
    _bytes() {
      const out = new Uint8Array(this.#size);
      let off = 0;
      for (const c of this.#chunks) {
        out.set(c, off);
        off += c.byteLength;
      }
      return out;
    }
    static finish(body, trailers) {
      if (body.finished) throw { tag: "internal-error", val: "the body is finished already" };
      body.finished = true;
      if (trailers !== undefined) {
        body.#done.reject(errorCode("internal-error"));
        throw { tag: "internal-error", val: "trailers are not supported" };
      }
      if (body.over) {
        body.#done.reject(errorCode("HTTP-request-body-size"));
        throw { tag: "HTTP-request-body-size", val: undefined };
      }
      body.#done.resolve(body._bytes());
    }
    [Symbol.dispose]() {
      if (this.finished) return;
      this.dropped = true;
      this.#done.reject(errorCode("internal-error"));
    }
  }

  class OutgoingResponse {
    #status = 200;
    #headers;
    #body = null;
    constructor(headers) {
      this.#headers = headers._lock();
    }
    statusCode() {
      return this.#status;
    }
    setStatusCode(code) {
      if (!Number.isInteger(code) || code < 100 || code > 999) throw undefined;
      this.#status = code;
    }
    headers() {
      return this.#headers;
    }
    body() {
      if (this.#body) throw undefined;
      this.#body = new OutgoingBody();
      return this.#body;
    }
    _parts() {
      return { status: this.#status, headers: this.#headers.entries(), body: this.#body };
    }
  }

  class ResponseOutparam {
    #state = null;
    static _create() {
      const p = new ResponseOutparam();
      const state = { result: undefined };
      p.#state = state;
      return [p, state];
    }
    static set(param, result) {
      const state = param.#state;
      if (!state || state.result) throw new Error("the response-outparam is set already");
      param.#state = null;
      state.result = result.tag === "err" ? { error: result.val?.tag ?? "internal-error" } : { response: result.val._parts() };
    }
  }

  class RequestOptions {
    #t = {};
    connectTimeout() {
      return this.#t.c;
    }
    setConnectTimeout(d) {
      this.#t.c = d;
    }
    firstByteTimeout() {
      return this.#t.f;
    }
    setFirstByteTimeout(d) {
      this.#t.f = d;
    }
    betweenBytesTimeout() {
      return this.#t.b;
    }
    setBetweenBytesTimeout(d) {
      this.#t.b = d;
    }
  }

  class OutgoingRequest {
    #method = { tag: "get" };
    #scheme = undefined;
    #path = undefined;
    #authority = undefined;
    #headers;
    #body = null;
    constructor(headers) {
      this.#headers = headers._lock();
    }
    method() {
      return this.#method;
    }
    setMethod(m) {
      if (m.tag === "other" && !TOKEN.test(m.val)) throw undefined;
      this.#method = m;
    }
    pathWithQuery() {
      return this.#path;
    }
    setPathWithQuery(p) {
      this.#path = p;
    }
    scheme() {
      return this.#scheme;
    }
    setScheme(s) {
      if (s?.tag === "other") throw undefined;
      this.#scheme = s;
    }
    authority() {
      return this.#authority;
    }
    setAuthority(a) {
      this.#authority = a;
    }
    headers() {
      return this.#headers;
    }
    body() {
      if (this.#body) throw undefined;
      this.#body = new OutgoingBody();
      return this.#body;
    }
    // _parts gives the fields of the fetch message without the body, and the
    // body promise. As wasmtime, a request without a scheme uses https, and a
    // request without an authority fails.
    _parts() {
      if (this.#authority === undefined || this.#authority === "") throw { tag: "HTTP-request-URI-invalid" };
      const scheme = this.#scheme?.tag === "HTTP" ? "http" : "https";
      const url = `${scheme}://${this.#authority}${this.#path ?? "/"}`;
      if (url.length > 8192) throw { tag: "HTTP-request-URI-too-long" };
      return {
        method: methodName(this.#method),
        url,
        headers: this.#headers.entries(),
        body: this.#body ? this.#body._finished : Promise.resolve(new Uint8Array(0)),
      };
    }
  }

  class IncomingResponse {
    #status;
    #headers;
    #body;
    constructor(r) {
      this.#status = r.status;
      this.#headers = Fields._of(r.headers ?? []);
      this.#body = new IncomingBody(new Uint8Array(r.body ?? 0));
    }
    status() {
      return this.#status;
    }
    headers() {
      return this.#headers;
    }
    consume() {
      const b = this.#body;
      if (!b) throw undefined;
      this.#body = null;
      return b;
    }
  }

  // FutureIncomingResponse is a client request. The pod worker sends fetch
  // when the request body is complete. If the component drops it before
  // fetched, the pod worker sends fetch-cancel (CR10).
  class FutureIncomingResponse {
    #result = undefined;
    #taken = false;
    #pending;
    #cancel = null;
    #dropped = false;
    constructor(parts) {
      const settle = (r) => {
        this.#cancel = null;
        this.#result = r.status !== undefined ? { tag: "ok", val: new IncomingResponse(r) } : { tag: "err", val: errorCode(r.error ?? "internal-error") };
      };
      this.#pending = parts.body.then(
        (body) => {
          if (this.#dropped) return;
          let f;
          try {
            f = fetch({ method: parts.method, url: parts.url, headers: parts.headers, body });
          } catch {
            settle({ error: "internal-error" });
            return;
          }
          this.#cancel = f.cancel;
          return f.result.then(settle);
        },
        (code) => settle({ error: code?.tag ?? "internal-error" }),
      );
    }
    subscribe() {
      if (this.#result) return ready();
      let done = false;
      const p = wait(this.#pending).then(() => {
        done = true;
      });
      return pollableCreate({ ready: () => done || this.#result !== undefined, wait: () => p });
    }
    get() {
      if (this.#result === undefined) return undefined;
      if (this.#taken) return { tag: "err", val: undefined };
      this.#taken = true;
      return { tag: "ok", val: this.#result };
    }
    [Symbol.dispose]() {
      this.#dropped = true;
      if (this.#cancel) {
        const c = this.#cancel;
        this.#cancel = null;
        c();
      }
    }
  }

  const outgoingHandler = {
    handle(request, _options) {
      return new FutureIncomingResponse(request._parts());
    },
  };

  const types = {
    Fields,
    IncomingRequest,
    OutgoingRequest,
    RequestOptions,
    ResponseOutparam,
    IncomingResponse,
    IncomingBody,
    FutureTrailers,
    OutgoingResponse,
    OutgoingBody,
    FutureIncomingResponse,
    httpErrorCode: () => undefined,
  };

  return {
    http: { types, outgoingHandler, incomingHandler: {} },
    incoming: (msg) => new IncomingRequest(msg),
    outparam: () => ResponseOutparam._create(),
  };
}

// responseFields gives the fields of the response message (section 8.3.1)
// for the state of the response-outparam after handle. trapped tells if
// handle threw.
//
//   - A write of the response body that failed at the limit gives status
//     500, no headers and an empty body (PX20).
//   - A response with a complete body gives the response, also after a trap:
//     runwasi sends a response that the component set before the trap.
//   - A trap gives the error trap (PX21).
//   - No response gives neither a status nor an error (PX26).
//   - An error code gives that error (PX27).
//   - A response body that the component did not finish gives
//     HTTP-response-incomplete: the client of runwasi gets a broken body.
export function responseFields(state, trapped) {
  const r = state.result;
  const response = r?.response;
  if (response?.body?.over) return { status: 500, headers: [], body: new ArrayBuffer(0) };
  if (response && (!response.body || response.body.finished)) {
    const body = response.body ? response.body._bytes() : new Uint8Array(0);
    return {
      status: response.status,
      headers: response.headers.map(([k, v]) => [k, v.buffer.byteLength === v.byteLength ? v.buffer : v.slice().buffer]),
      body: body.buffer,
    };
  }
  if (trapped) return { error: "trap" };
  if (!r) return {};
  if (r.error) return { error: r.error };
  return { error: "HTTP-response-incomplete" };
}
