// Unit tests of web/src/join.js: the tokens of "join" and "join --kubectl",
// the bye of an agent and rmnode of a kubectl connection. RTCPeerConnection
// and the API of the WebAssembly binary are stubs.
import { test } from "node:test";
import assert from "node:assert/strict";
import { joinBuiltins, dockerCommand, decodeToken, encodeToken, KUBECONFIG_FILE } from "../src/join.js";

// FakeChannel records what the page sends on a data channel.
class FakeChannel {
  constructor(label) { this.label = label; this.sent = []; this.onmessage = null; }
  send(m) { this.sent.push(m); }
  close() { this.closed = true; }
}

class FakePeer {
  constructor() {
    this.channels = [];
    this.connectionState = "new";
    this.iceGatheringState = "complete";
    FakePeer.last = this;
  }
  createDataChannel(label) { const c = new FakeChannel(label); this.channels.push(c); return c; }
  async createOffer() { return { type: "offer", sdp: "offer-sdp" }; }
  async setLocalDescription(d) { this.localDescription = d; }
  async setRemoteDescription(d) { this.remoteDescription = d; }
  addEventListener() {}
  close() { this.connectionState = "closed"; }
  get ctl() { return this.channels.find((c) => c.label === "ctl"); }
}
globalThis.RTCPeerConnection = FakePeer;

// The calls of join.js to the WebAssembly binary.
const calls = [];
globalThis.tabernetes = {
  prepareJoin: async () => ({ name: "node-1", ip: "10.0.0.2", token: "bootstrap-token", ca: "CA" }),
  prepareKubectl: async () => ({ name: "kubectl-1", token: "eyJ.sa.token", ca: "CA" }),
  removeNode: async (name) => { calls.push(["removeNode", name]); },
  revokeKubectl: async (name) => { calls.push(["revokeKubectl", name]); },
  attachChannel: (dc, from) => { calls.push(["attachChannel", dc.label, from]); },
};

// run runs a builtin like the terminal does, with ask answering the accept line.
async function run(fn, args) {
  const lines = [];
  const term = { writeln: (s) => lines.push(s) };
  const answer = "accept " + await encodeToken({ v: 1, sdp: "answer-sdp" });
  const code = await fn(args, { term, ask: async () => answer, active: () => true });
  return { code, out: lines.join("\n") };
}

const tick = () => new Promise((r) => setTimeout(r, 0));

test("join makes a node token, and the bye of the node removes it", async () => {
  const connections = new Map();
  const b = joinBuiltins({ image: "img", connections });
  const { out } = await run(b.join, []);
  const pc = FakePeer.last;
  assert.equal(pc.remoteDescription.sdp, "answer-sdp");
  const tok = await decodeToken(globalThis.tabernetesLastJoin.token);
  assert.equal(tok.use, "node");
  assert.equal(tok.n, "node-1");
  assert.equal(tok.ip, "10.0.0.2");
  assert.match(out, /docker run --rm -it -p 127\.0\.0\.1:50001:50001\/udp/);

  // rmnode does not remove a Docker node; it says how.
  const written = [];
  assert.equal(await connections.get("node-1").remove((s) => written.push(s)), 1);
  assert.match(written.join(""), /docker stop tabernetes-node-1/);
  assert.deepEqual(pc.ctl.sent, []);

  calls.length = 0;
  pc.ctl.onmessage({ data: "bye" });
  await tick();
  assert.deepEqual(pc.ctl.sent, ["bye-seen"]);
  assert.deepEqual(calls, [["removeNode", "node-1"]]);
  assert.equal(connections.has("node-1"), false);
});

test("join --kubectl makes a kubectl token, and rmnode voids it", async () => {
  const connections = new Map();
  const b = joinBuiltins({ image: "img", connections });
  const { out } = await run(b.join, ["--kubectl"]);
  const pc = FakePeer.last;
  const tok = await decodeToken(globalThis.tabernetesLastJoin.token);
  assert.equal(tok.use, "kubectl");
  assert.equal(tok.n, "kubectl-1");
  assert.equal(tok.t, "eyJ.sa.token");
  assert.equal(tok.ip, undefined);
  assert.match(out, /-p 127\.0\.0\.1:6443:6443/);
  assert.match(out, new RegExp(`kubectl \\S+ > ${KUBECONFIG_FILE.replace(".", "\\.")}`));

  // Only channels to the apiserver.
  calls.length = 0;
  const other = new FakeChannel("kubelet:10250");
  pc.ondatachannel({ channel: other });
  pc.ondatachannel({ channel: new FakeChannel("api") });
  assert.equal(other.closed, true);
  assert.deepEqual(calls, [["attachChannel", "api", "kubectl-1"]]);

  calls.length = 0;
  const written = [];
  assert.equal(await connections.get("kubectl-1").remove((s) => written.push(s)), 0);
  assert.deepEqual(pc.ctl.sent, ["close"]);
  assert.deepEqual(calls, [["revokeKubectl", "kubectl-1"]]);
  assert.equal(connections.has("kubectl-1"), false);
  assert.match(written.join(""), /kubectl-1 disconnected/);
});

test("the bye of a kubectl agent voids its token", async () => {
  const b = joinBuiltins({ image: "img" });
  await run(b.join, ["--kubectl"]);
  calls.length = 0;
  FakePeer.last.ctl.onmessage({ data: "bye" });
  await tick();
  assert.deepEqual(FakePeer.last.ctl.sent, ["bye-seen"]);
  assert.deepEqual(calls, [["revokeKubectl", "kubectl-1"]]);
});

test("join takes only --kubectl", async () => {
  const b = joinBuiltins({ image: "img" });
  const { code, out } = await run(b.join, ["--keep"]);
  assert.equal(code, 2);
  assert.match(out, /usage: join \[--kubectl\]/);
});

test("dockerCommand", () => {
  const k = dockerCommand("kubectl", { image: "img", port: 50002, name: "kubectl-1", token: "T" });
  // stdout must stay the kubeconfig: no TTY, no privileges.
  assert.doesNotMatch(`${k.head} ${k.flags}`, /\s-\w*t\b|--tty|--privileged|--security-opt|--device/);
  assert.match(k.head, /-p 127\.0\.0\.1:6443:6443/);
  assert.equal(k.tail, `kubectl T > ${KUBECONFIG_FILE}`);
  const n = dockerCommand("node", { image: "img", port: 50001, name: "node-1", token: "T" });
  assert.equal(n.tail, "T");
  assert.match(n.flags, /--device \/dev\/net\/tun/);
});
