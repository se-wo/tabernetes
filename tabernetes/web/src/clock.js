// patchWalltime gives Go a wall clock that never repeats. wasm_exec.js reads
// it from Date, in milliseconds. Kubernetes names an event
// <object>.<UnixNano in hex>, so two events of one object in the same
// millisecond got the same name and the second one overwrote the first
// ("Pulled" with the message of "Created"). The resolution of
// performance.now() and a clock that only moves forward keep them apart.
export function patchWalltime(go) {
  let last = 0n;
  go.importObject.gojs["runtime.walltime"] = (sp) => {
    sp >>>= 0;
    let ns = BigInt(Math.round((performance.timeOrigin + performance.now()) * 1e6));
    if (ns <= last) ns = last + 1n;
    last = ns;
    go.mem.setBigInt64(sp + 8, ns / 1000000000n, true);
    go.mem.setInt32(sp + 16, Number(ns % 1000000000n), true);
  };
}

// countBusy counts the time Go runs: every call into the instance goes
// through go._resume (timers, events, calls from JS), and nothing else runs
// on this thread meanwhile. A call can lead to another one inside it; only
// the outer one counts. globalThis.tabernetesBusy() is the total in
// milliseconds, with the call that runs now. The node reports it as its CPU.
export function countBusy(go) {
  const resume = go._resume;
  const idle = globalThis.tabernetesIdle;
  let timer = -1;
  let busy = 0;
  let depth = 0;
  let since = 0;
  let rec = false; // this call goes into tabernetesIdle
  const counted = (fn) => {
    if (depth++ === 0) {
      since = performance.now();
      rec = !!idle && idle.length < 3e6;
      if (rec) idle.push(since);
    }
    try {
      return fn();
    } finally {
      if (--depth === 0) {
        const now = performance.now();
        busy += now - since;
        if (rec) idle.push(now, timer);
        timer = -1;
      }
    }
  };
  go._resume = function () {
    return counted(() => resume.call(this));
  };
  // go.run runs the runtime start, the package init and main up to the
  // first wait in one call that does not go through _resume. With boot
  // marks, terminal.js counts it through this.
  if (idle) go._countedRun = counted;
  // With boot marks: the timer Go asked for before it went idle.
  if (idle) {
    const schedule = go.importObject.gojs["runtime.scheduleTimeoutEvent"];
    go.importObject.gojs["runtime.scheduleTimeoutEvent"] = function (sp) {
      const sp0 = sp >>> 0;
      timer = go.mem.getUint32(sp0 + 8, true) + go.mem.getInt32(sp0 + 12, true) * 4294967296;
      return schedule.call(this, sp);
    };
  }
  globalThis.tabernetesBusy = () => busy + (depth > 0 ? performance.now() - since : 0);
}

// Boot marks (?bootmarks, docs/boot-time.md): performance marks "js:<name>"
// of the page, with the busy time of Go so far, and the Go marks of
// k8s/tabernetes/bootmarks.go. globalThis.tabernetesIdle gets, for every
// call into Go, its start, its end and the timeout in milliseconds that Go
// asked for before it went idle (-1 for none): a flat array of triples.
// Off by default; then bootMark costs one check.
let marking = false;

export function startBootMarks() {
  marking = true;
  globalThis.tabernetesBootMarks = true;
  globalThis.tabernetesIdle = [];
}

export function bootMark(name) {
  if (!marking) return;
  performance.mark(`js:${name}`, { detail: { busy: globalThis.tabernetesBusy?.() ?? 0 } });
}
