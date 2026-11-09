// portChannel makes a MessagePort look like the part of an RTCDataChannel
// that tunnel.go uses (label, readyState, bufferedAmount, binaryType, send,
// close, onmessage, onclose). A node in a Web Worker (nodes.js,
// node.worker.js) then talks to the control plane exactly like a Docker node
// does over WebRTC. null on the port means the other side closed.
export function portChannel(port, label) {
  const ch = {
    label,
    readyState: "open",
    bufferedAmount: 0,
    binaryType: "arraybuffer",
    onmessage: null,
    onclose: null,
    send(data) {
      if (ch.readyState !== "open") throw new Error("channel closed");
      // Go hands over a fresh Uint8Array each time: move its buffer.
      const buf = data.byteOffset === 0 && data.byteLength === data.buffer.byteLength
        ? data.buffer
        : data.buffer.slice(data.byteOffset, data.byteOffset + data.byteLength);
      port.postMessage(buf, [buf]);
    },
    close() {
      if (ch.readyState === "closed") return;
      port.postMessage(null);
      closed();
    },
  };
  const closed = () => {
    if (ch.readyState === "closed") return;
    ch.readyState = "closed";
    port.close();
    ch.onclose?.();
  };
  port.onmessage = (e) => {
    if (e.data === null) closed();
    else ch.onmessage?.({ data: e.data });
  };
  return ch;
}

// openChannel creates a channel over a fresh MessageChannel and hands its
// other end to target (a Worker or the worker's own scope) to attach.
export function openChannel(target, label) {
  const { port1, port2 } = new MessageChannel();
  target.postMessage({ type: "channel", label, port: port2 }, [port2]);
  return portChannel(port1, label);
}
