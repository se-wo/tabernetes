// The background jobs of a busybox pod on the tab node end when the pod is
// deleted, with their connections (k8s/tabernetes/jobs.go, podJob). On a
// node, the container runtime kills every process of the container.
//
// The server "sink" writes "--- closed" when its connection closes. The
// client has a background nc whose stdin stays open: only the end of the
// job closes the connection.
import { test } from "node:test";
import assert from "node:assert/strict";
import { apply, kubectl, logs, namespace, TAB_NODE, waitFor, waitForPod } from "./harness.mjs";
import { busyboxPod, podIP, running } from "./wasi-proxy-lib.mjs";

const SINK = "nc -l -p 9000 <&-; echo '--- closed'; sleep 86400";

test("deleting a pod ends its background jobs and their connections", async (t) => {
  const ns = await namespace(t);
  await apply(busyboxPod("sink", TAB_NODE, SINK), ns);
  await waitForPod(ns, "sink", running);
  const ip = await podIP(ns, "sink");
  await apply(busyboxPod("client", TAB_NODE, `sleep 86400 | nc ${ip} 9000 & sleep 86400`), ns);
  await waitForPod(ns, "client", running);
  // The connection is open while the pod runs.
  await new Promise((r) => setTimeout(r, 2000));
  assert.doesNotMatch(await logs(ns, "sink"), /--- closed/);

  await kubectl(`delete pod -n ${ns} client --wait=true --timeout=60s`, { timeout: 90 });
  await waitFor(async () => /--- closed/.test(await logs(ns, "sink")),
    { timeout: 30, what: '"--- closed" in the log of sink' });
});
