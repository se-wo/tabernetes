// wasi-lib.mjs: helpers for the tests with the echo image wasi-test-echo
// (wasm-images/rust/echo): wasi-sockets.test.mjs (TCP, phase 3),
// wasi-udp.test.mjs and wasi-udp-flows.test.mjs (UDP, phase 4).
import { logs, pod, waitFor } from "./harness.mjs";

export const ECHO = "ghcr.io/se-wo/tabernetes/wasi-test-echo:latest";

// echoPod is a pod of the echo image. containers: [name, env object,
// extra fields of the container]. An env value that is not a string is the
// rest of the env entry, for example { valueFrom: ... }.
export function echoPod(name, node, containers, { restartPolicy, labels = {} } = {}) {
  return {
    apiVersion: "v1",
    kind: "Pod",
    metadata: { name, labels: { app: name, ...labels } },
    spec: {
      runtimeClassName: "wasmtime",
      nodeSelector: { "kubernetes.io/hostname": node },
      terminationGracePeriodSeconds: 1,
      ...(restartPolicy ? { restartPolicy } : {}),
      containers: containers.map(([c, env = {}, extra = {}]) => ({
        name: c,
        image: extra.image ?? ECHO,
        env: Object.entries(env).map(([k, v]) => (typeof v === "string" ? { name: k, value: v } : { name: k, ...v })),
        ...Object.fromEntries(Object.entries(extra).filter(([k]) => k !== "image")),
      })),
    },
  };
}

// lines gives the lines of the logs of a container.
export const lines = async (ns, name, c) => (await logs(ns, name, c)).replace(/\r/g, "").split("\n");

// logLine waits until a container logs a line that matches re and resolves
// with the line.
export const logLine = (ns, name, c, re, timeout = 120) =>
  waitFor(async () => (await lines(ns, name, c)).find((l) => re.test(l)),
    { timeout, what: `a line ${re} in the logs of ${ns}/${name}/${c}` });

// exited waits until a container has ended and resolves with its exit code.
export const exited = (ns, name, c, timeout = 180) => waitFor(async () => {
  const p = await pod(ns, name);
  const t = p.status?.containerStatuses?.find((s) => s.name === c)?.state?.terminated;
  return t && { code: t.exitCode };
}, { timeout, what: `the end of ${ns}/${name}/${c}` });
