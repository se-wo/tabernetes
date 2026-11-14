// Unit tests of web/src/channel.js: a MessagePort that looks like an
// RTCDataChannel. Node.js has MessageChannel, so no browser is necessary.
import { test } from "node:test";
import assert from "node:assert/strict";
import { once } from "node:events";
import { portChannel, openChannel } from "../src/channel.js";

// pair returns two channels on the two ends of one MessageChannel.
function pair(label = "test") {
  const { port1, port2 } = new MessageChannel();
  return [portChannel(port1, label), portChannel(port2, label)];
}

// next resolves with the data of the next message on ch.
const next = (ch) => new Promise((resolve) => (ch.onmessage = (e) => resolve(e.data)));

test("a new channel is open and has the properties of a data channel", () => {
  const [a, b] = pair("api");
  assert.equal(a.label, "api");
  assert.equal(a.readyState, "open");
  assert.equal(a.bufferedAmount, 0);
  assert.equal(a.binaryType, "arraybuffer");
  a.close();
  b.close();
});

test("send moves the bytes to the other end as an ArrayBuffer", async () => {
  const [a, b] = pair();
  const got = next(b);
  const data = new Uint8Array([1, 2, 3, 4]);
  a.send(data);
  const buf = await got;
  assert.ok(buf instanceof ArrayBuffer);
  assert.deepEqual([...new Uint8Array(buf)], [1, 2, 3, 4]);
  // The buffer of a whole array is transferred, not copied.
  assert.equal(data.byteLength, 0);
  a.close();
});

test("send of a view into a larger buffer sends only the view", async () => {
  const [a, b] = pair();
  const got = next(b);
  const big = new Uint8Array([9, 9, 5, 6, 7, 9]);
  a.send(big.subarray(2, 5));
  assert.deepEqual([...new Uint8Array(await got)], [5, 6, 7]);
  // The larger buffer stays with the sender.
  assert.deepEqual([...big], [9, 9, 5, 6, 7, 9]);
  a.close();
});

test("close closes the two ends and send then fails", async () => {
  const [a, b] = pair();
  const closedB = new Promise((resolve) => (b.onclose = resolve));
  let closesA = 0;
  a.onclose = () => closesA++;
  a.close();
  a.close(); // a second close does nothing
  await closedB;
  assert.equal(a.readyState, "closed");
  assert.equal(b.readyState, "closed");
  assert.equal(closesA, 1);
  assert.throws(() => a.send(new Uint8Array(1)), /channel closed/);
  assert.throws(() => b.send(new Uint8Array(1)), /channel closed/);
});

test("openChannel gives the other end of the port to the target", async () => {
  const target = new EventTarget();
  target.postMessage = (msg, transfer) => {
    assert.deepEqual(transfer, [msg.port]);
    target.dispatchEvent(Object.assign(new Event("message"), { data: msg }));
  };
  const received = once(target, "message");
  const ch = openChannel(target, "kubelet:10250");
  const [{ data: msg }] = await received;
  assert.equal(msg.type, "channel");
  assert.equal(msg.label, "kubelet:10250");
  const other = portChannel(msg.port, msg.label);
  const got = next(other);
  ch.send(new TextEncoder().encode("ping"));
  assert.equal(new TextDecoder().decode(await got), "ping");
  ch.close();
});
