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
