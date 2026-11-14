# Tests of the page

Two kinds of tests are in this directory:

- Unit tests of the JavaScript in `web/src/`: `*.test.mjs` in this directory.
  They run in Node.js with `node --test`, without a browser and without the
  WebAssembly binary.
- End-to-end tests of the page: `e2e/*.test.mjs`. They run the built page in
  headless Chromium and run command lines in the shell of its terminal, on
  the tab node and on a node worker (`addnode`).

## Unit tests

```bash
mise run test-web
```

To add a test, add a file `<name>.test.mjs` here. Import the module of
`../src/` and use `node:test` and `node:assert/strict`, as
`channel.test.mjs` does. Node.js has `MessageChannel`, `EventTarget`, `fetch`,
`TextEncoder` and `crypto.subtle`. A module that needs `document`,
`Worker` or `location` needs a small stub of it in the test.

## End-to-end tests

You need the built page, Node.js and Playwright with its Chromium:

```bash
mise run build && mise run site
(cd web/test/e2e && npm ci && npx playwright install chromium)
node web/test/e2e/run.mjs                        # all e2e/*.test.mjs
node web/test/e2e/run.mjs web/test/e2e/wasi-hello.test.mjs
```

`run.mjs` serves `.build/site` on `127.0.0.1:8080` and starts the page with
`docs/docker-node-check/browser.mjs`. When the terminal is ready, it runs the
test files with `node --test`, one file after the other, and stops the
browser. The console of the page, the log of the control plane and the text of
the terminal go to `.build/e2e/`.

The page needs approximately 1 to 3 minutes to start. To run the tests more
than one time, start the browser yourself. `run.mjs` then uses it:

```bash
python3 -m http.server -d .build/site 8080 &
node docs/docker-node-check/browser.mjs &      # waits for the terminal
node web/test/e2e/run.mjs web/test/e2e/wasi-hello.test.mjs
```

The tests share one page. The first test that needs a node worker starts
one, and the later tests use it.

Settings, as environment variables:

- `E2E_NODES`: the kinds of nodes for `forEachNode`, default `tab,worker`.
- `E2E_KEEP=1`: do not delete the namespaces of the tests.
- `SITE_DIR`, `SITE_PORT`, `CDP_PORT`, `E2E_OUT`: see `run.mjs`.

### Add an end-to-end test

Add a file `e2e/<group>.test.mjs`, one file for each group of requirements.
Use the helpers of `e2e/harness.mjs`:

```js
import { test } from "node:test";
import assert from "node:assert/strict";
import { apply, forEachNode, logs, namespace, nodeName, onNode, podPhase, waitForPod } from "./harness.mjs";

forEachNode(["tab", "worker"], (kind) => {
  test(`CO1: the command component writes its arguments on ${kind}`, async (t) => {
    const ns = await namespace(t);          // deleted after the test
    const node = await nodeName(kind);      // "tab" or a node worker
    await apply({ apiVersion: "v1", kind: "Pod", metadata: { name: "c" }, spec: {
      runtimeClassName: "wasmtime", restartPolicy: "Never", nodeSelector: onNode(node),
      containers: [{ name: "c", image: "ghcr.io/se-wo/tabernetes/wasi-hello:latest" }],
    } }, ns);
    await waitForPod(ns, "c", podPhase("Succeeded", "Failed"));
    assert.match(await logs(ns, "c"), /Hello/);
  });
});
```

The helpers:

| Helper | What it does |
| --- | --- |
| `run(line)`, `sh(line)` | Run a command line in the shell of the tab. `run` gives `{out, rc}`, `sh` gives the output and fails if the exit status is not 0. |
| `kubectl(args)`, `getJSON(args)` | `kubectl <args>`, and `kubectl get <args> -o json` as an object. |
| `apply(manifest, ns)` | `kubectl apply` of YAML text, of an object or of a list of objects. |
| `namespace(t)` | A new namespace for the test `t`, deleted after it. |
| `nodeName(kind)`, `worker()` | The node name of `"tab"` or `"worker"`. `worker()` starts a node worker if the page has none, and waits until it is Ready. |
| `forEachNode(kinds, fn)` | Calls `fn(kind)` for each kind that `E2E_NODES` allows. |
| `onNode(node)` | The `nodeSelector` that puts a pod on the node. |
| `waitForPod(ns, name, test)` | Waits until `test(pod)` is true. `podPhase(...)`, `podReady` and `podDoneOrReady` are tests. On a timeout the error shows the status and the events of the pod. |
| `pod(ns, name)`, `logs(ns, name, container)` | The pod object, and the logs of a container. |
| `waitFor(fn, {timeout, interval, what})` | Calls `fn` until it gives a value that is not false. |
| `tab(args)` | Runs `tab.mjs` with other commands: `screen`, `logs`, `status`. |

`e2e/devtools.mjs` gives `evalOn(node, fn, arg)`: it runs `fn(arg)` on the Go
thread of the tab node (the page) or of a node worker, over the DevTools
port. A test file that uses it calls `closeDevtools` in an `after` hook.

A test that waits 30 seconds or more of real time must use a clock that the
test controls (see `docs/wasi-sockets-plan.md`).
