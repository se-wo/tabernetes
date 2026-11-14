// The busy time of the instances of a pod worker (docs/wasi-sockets.md,
// AC2). The pod worker sends it in cpu messages; the Go side charges it to the
// container (tabnode_acct.go), so that kubectl top pod shows CPU for a
// component.
//
// An instance is busy from the start of an export call until the call ends,
// less the time in which it is suspended (JSPI). jco makes each import that
// can suspend with new WebAssembly.Suspending(fn): suspending() wraps fn so
// that the meter sees when the instance suspends and when it continues.
//
// A worker thread that runs an instance receives no messages and runs no
// timers. So the meter also sends from the imports that the instance calls
// (tick): a loop of CPU work that calls an import, for example a clock,
// sends a cpu message each second. A loop that calls no import sends its time
// only when it ends (section 11).

// The maximum of the field ms of one cpu message (wasi_messages.js).
const MAX_MS = 3600e3;

// createMeter gives the meter of one pod worker. send(ms) sends a cpu
// message. now() gives the time in ms. every is the interval of the messages
// in ms.
export function createMeter({ send, now = () => performance.now(), every = 1000 }) {
  let busy = 0; // ms not sent yet
  let since = null; // the start of the current busy part, or null
  let calls = 0; // export calls that run or are suspended
  let last = now();
  const m = {
    // begin and end mark an export call (or the instantiation).
    begin() {
      if (calls++ === 0) since = now();
    },
    end() {
      if (calls === 0) return;
      if (--calls === 0 && since !== null) {
        busy += now() - since;
        since = null;
      }
      m.flush();
    },
    // tick sends a cpu message if the last one is older than every.
    tick() {
      if (now() - last >= every) m.flush();
    },
    // flush sends the busy time that the meter did not send yet.
    flush() {
      const t = now();
      if (since !== null) {
        busy += t - since;
        since = t;
      }
      last = t;
      while (busy >= 0.001) {
        const ms = Math.min(busy, MAX_MS);
        busy -= ms;
        send(ms);
      }
    },
    // suspending wraps an import that can suspend. While its promise waits,
    // the instance is not busy.
    suspending(fn) {
      return function (...a) {
        const r = fn.apply(this, a);
        if (since === null || r === null || typeof r?.then !== "function") return r;
        busy += now() - since;
        since = null;
        const resume = () => {
          if (calls > 0 && since === null) since = now();
          m.tick();
        };
        return r.then((v) => (resume(), v), (e) => {
          resume();
          throw e;
        });
      };
    },
    // ticking wraps an import that cannot suspend: each call can send.
    ticking(fn) {
      return function (...a) {
        m.tick();
        return fn.apply(this, a);
      };
    },
  };
  return m;
}

// meterSuspending replaces WebAssembly.Suspending in this worker, so that
// each import that jco makes with it goes through meter.suspending. It gives
// a function that puts back the original.
export function meterSuspending(meter, WA = globalThis.WebAssembly) {
  const Original = WA.Suspending;
  if (typeof Original !== "function") return () => {};
  function Suspending(fn) {
    return new Original(meter.suspending(fn));
  }
  Suspending.prototype = Original.prototype;
  WA.Suspending = Suspending;
  return () => {
    if (WA.Suspending === Suspending) WA.Suspending = Original;
  };
}

// tickImports gives the import object with each function of an interface
// wrapped by meter.ticking. Classes (resources) stay as they are. Each
// interface is a Proxy over the original object, so that each read of a
// name still goes to it: component.js records the names that the generated
// JavaScript reads and does not find.
export function tickImports(meter, imports) {
  const out = {};
  for (const [name, iface] of Object.entries(imports)) {
    if (iface === null || typeof iface !== "object") {
      out[name] = iface;
      continue;
    }
    const wrapped = new Map();
    out[name] = new Proxy(iface, {
      get(t, key, receiver) {
        const v = Reflect.get(t, key, receiver);
        if (typeof v !== "function" || isClass(v)) return v;
        let w = wrapped.get(v);
        if (!w) wrapped.set(v, (w = meter.ticking(v)));
        return w;
      },
    });
  }
  return out;
}

const isClass = (fn) => /^class[\s{]/.test(Function.prototype.toString.call(fn));
