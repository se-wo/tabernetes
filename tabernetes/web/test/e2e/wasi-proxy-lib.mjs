// wasi-proxy-lib.mjs: helpers of wasi-proxy.test.mjs and
// wasi-proxy-cr.test.mjs (docs/wasi-sockets.md, section 10.3, rows PX and
// CR). The pods of a test file share one namespace, so that the e2e job
// starts each pod one time.
import { after } from "node:test";
import { randomBytes } from "node:crypto";
import { kubectl, onNode, pod, run, waitFor } from "./harness.mjs";

export const PROXY = "ghcr.io/se-wo/tabernetes/wasi-test-proxy:latest";

// shq quotes a string for the shell of the tab and for sh in a pod.
export const shq = (s) => `'${String(s).replace(/'/g, `'\\''`)}'`;

// sharedNamespace gives the name of one namespace for all tests of a file.
// Call it at the top level of the file: it deletes the namespace after the
// last test. create() creates it.
export function sharedNamespace(prefix) {
  const ns = `${prefix}-${randomBytes(3).toString("hex")}`;
  if (!process.env.E2E_KEEP) after(() => kubectl(`delete namespace ${ns} --wait=false`).catch(() => {}));
  return { ns, create: () => kubectl(`create namespace ${ns}`) };
}

// proxyPod is a pod of the proxy test image. env sets
// WASMTIME_HTTP_PROXY_SOCKET_ADDR if addr is not undefined.
export function proxyPod(name, node, { addr, labels = {}, extra = {} } = {}) {
  return {
    apiVersion: "v1",
    kind: "Pod",
    metadata: { name, labels: { app: name, ...labels } },
    spec: {
      runtimeClassName: "wasmtime",
      nodeSelector: onNode(node),
      containers: [{
        name: "c",
        image: PROXY,
        env: addr === undefined ? [] : [{ name: "WASMTIME_HTTP_PROXY_SOCKET_ADDR", value: addr }],
        ...extra,
      }],
    },
  };
}

// busyboxPod is a pod with busybox: the client, or a server with a shell
// command line.
export function busyboxPod(name, node, script = "sleep 86400", env = []) {
  return {
    apiVersion: "v1",
    kind: "Pod",
    metadata: { name, labels: { app: name } },
    spec: {
      nodeSelector: onNode(node),
      terminationGracePeriodSeconds: 1,
      containers: [{ name: "c", image: "busybox", command: ["sh", "-c", script], env }],
    },
  };
}

export const service = (name, port) => ({
  apiVersion: "v1",
  kind: "Service",
  metadata: { name },
  spec: { selector: { app: name }, ports: [{ port }] },
});

// inPod runs a shell command line in the first container of a pod with
// kubectl exec and resolves with {out, rc}. out has no carriage returns.
export async function inPod(ns, name, line, opts) {
  const { out, rc } = await run(`kubectl exec -n ${ns} ${name} -- sh -c ${shq(line)}`, opts);
  return { out: out.replace(/\r/g, ""), rc };
}

// raw sends a raw HTTP request with nc from a pod and resolves with the
// response without carriage returns. req is a printf format: "\\r\\n" for
// the end of a line. stdin of nc stays open for wait seconds after the
// request: at the end of stdin, nc closes its side of the connection, and
// net/http of Go and hyper (half_close false) then drop the request, as for
// a client that closes the connection (PX24).
export async function raw(ns, client, hostPort, req, wait = 1) {
  const [host, port] = hostPort.split(":");
  return (await inPod(ns, client, `{ printf ${shq(req)}; sleep ${wait}; } | nc ${host} ${port}`)).out;
}

// headerNames gives the names of the header lines of a raw response in
// lowercase letters, sorted.
export function headerNames(resp) {
  const lines = resp.split("\n");
  const end = lines.indexOf("", 1);
  return lines.slice(1, end < 0 ? lines.length : end).map((l) => l.split(":")[0].toLowerCase()).sort();
}

// status gives the status code of a raw response, or "" for no response.
export const status = (resp) => resp.split("\n")[0].split(" ")[1] ?? "";

// header gives the values of a header of a raw response.
export function header(resp, name) {
  const lines = resp.split("\n");
  const end = lines.indexOf("", 1);
  return lines.slice(1, end < 0 ? lines.length : end)
    .filter((l) => l.toLowerCase().startsWith(`${name.toLowerCase()}:`))
    .map((l) => l.slice(name.length + 1).trim());
}

// body gives the body of a raw response.
export function body(resp) {
  const i = resp.indexOf("\n\n");
  return i < 0 ? "" : resp.slice(i + 2);
}

// podIP waits for the IP of a pod.
export const podIP = (ns, name) =>
  waitFor(async () => (await pod(ns, name)).status?.podIP, { what: `the IP of ${ns}/${name}` });

// restarts gives "<restarts> <last exit code> <last reason>" of the first
// container, as pod_restarts of docs/docker-node-check/wasi-proxy.sh.
export function restarts(p) {
  const s = p.status.containerStatuses?.[0];
  const t = s?.lastState?.terminated;
  return `${s?.restartCount} ${t?.exitCode} ${t?.reason}`;
}

// state gives "<phase> <restarts> <exit code> <reason>", as pod_state of
// docs/docker-node-check/wasi-command.sh.
export function state(p) {
  const s = p.status.containerStatuses?.[0];
  const t = s?.state?.terminated;
  return `${p.status.phase} ${s?.restartCount} ${t?.exitCode} ${t?.reason}`;
}

// running is a test for waitForPod: the first container runs.
export const running = (p) => !!p.status?.containerStatuses?.[0]?.state?.running;
