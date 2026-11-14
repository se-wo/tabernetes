// The wasi:sockets of a pod worker (docs/wasi-sockets.md, decision 3 and
// section 8.5), and the pause of CO17 and CO19 for all calls that can
// suspend.
//
// Phase 1 has no network for components. Until phase 3 (W3.2) replaces this
// stub, each function gives the result of the proxy world on runwasi
// (section 9.2, item 7):
//
//   create-tcp-socket, create-udp-socket   a socket
//   start-bind, start-connect              access-denied
//   resolve-addresses                      permanent-resolver-failure
//
// The other functions give the error code of wasmtime for a socket that is
// not bound and not connected. The socket options keep their values (TC22).
//
// CO17 and CO19: the functions of section 8.5 that do not wait are in the
// list asyncMode.val.imports (async_lists.js). The pod worker counts the
// calls in sequence that give no data. After IDLE_LIMIT such calls, the next
// call returns a promise that continues after one message on a
// MessageChannel. So a component that polls without a wait still lets the
// pod worker receive messages. The streams of component.js and the later
// http.js use the same count.

// IDLE_LIMIT is the number of calls in sequence without data before a pause
// (CO17).
export const IDLE_LIMIT = 1000;

// nextTask gives a promise that resolves in the next task of the event loop,
// through a message on a MessageChannel (CO19). setTimeout has a minimum of
// 4 ms after nested calls (fact 21); a message has no minimum.
export function nextTask() {
  const { port1, port2 } = new MessageChannel();
  return new Promise((resolve) => {
    port1.onmessage = () => {
      port1.close();
      port2.close();
      resolve();
    };
    port2.postMessage(null);
  });
}

// createIdle gives the count of CO17 for one pod worker.
//
//   idle.call(gotData)   records a call. It gives a promise for the pause of
//                        CO19 when the call is the IDLE_LIMIT-th call in
//                        sequence without data, else null.
//   idle.wrap(fn, test)  gives fn with the count. test(result) tells if the
//                        call gave data; a call that throws gives no data.
//                        At the limit, the result of fn (or its exception)
//                        comes after the pause.
//   idle.count           the calls without data since the last call with
//                        data or the last pause.
export function createIdle({ limit = IDLE_LIMIT, pause = nextTask, onCall } = {}) {
  const idle = {
    count: 0,
    pauses: 0,
    call(gotData) {
      onCall?.();
      if (gotData) {
        idle.count = 0;
        return null;
      }
      if (++idle.count < limit) return null;
      idle.count = 0;
      idle.pauses++;
      return pause();
    },
    wrap(fn, test = () => false) {
      return function counted(...args) {
        let result, error, threw = false;
        try {
          result = fn.apply(this, args);
        } catch (e) {
          threw = true;
          error = e;
        }
        // A promise waits already: it is not a call without data.
        if (!threw && result instanceof Promise) {
          idle.call(true);
          return result;
        }
        const p = idle.call(!threw && test(result));
        if (!p) {
          if (threw) throw error;
          return result;
        }
        return p.then(() => {
          if (threw) throw error;
          return result;
        });
      };
    },
  };
  return idle;
}

// The error codes of wasi:sockets/network are strings: jco gives a thrown
// string to the component as the error result.
const fail = (code) => () => {
  throw code;
};

const FAMILIES = new Set(["ipv4", "ipv6"]);

// The defaults of the socket options. The component can read and change
// them; they have no effect (section 11).
const TCP_DEFAULTS = Object.freeze({
  keepAliveEnabled: false,
  keepAliveIdleTime: 7_200_000_000_000n,
  keepAliveInterval: 75_000_000_000n,
  keepAliveCount: 9,
  hopLimit: 64,
  receiveBufferSize: 131_072n,
  sendBufferSize: 16_384n,
});

const UDP_DEFAULTS = Object.freeze({
  unicastHopLimit: 64,
  receiveBufferSize: 212_992n,
  sendBufferSize: 212_992n,
});

// The functions of section 8.5 (the list ASYNC_IMPORTS of async_lists.js),
// by resource. Each one goes through the count of CO17.
const COUNTED = {
  tcp: ["startBind", "finishBind", "startConnect", "finishConnect", "startListen", "finishListen", "accept", "localAddress", "remoteAddress", "shutdown"],
  udp: ["startBind", "finishBind", "stream", "localAddress", "remoteAddress"],
  incoming: ["receive"],
  outgoing: ["checkSend", "send"],
  lookup: ["resolveNextAddress"],
};

// createSockets gives the object `sockets` for WASIShim: instanceNetwork,
// network, ipNameLookup, tcp, tcpCreateSocket, udp and udpCreateSocket.
// idle is the count of createIdle. pollableCreate is the function of the
// io.js of the shim, so that subscribe gives a pollable of the shim.
export function createSockets({ idle = createIdle(), pollableCreate }) {
  const ready = () => pollableCreate();

  class Network {}
  const network = new Network();

  class ResolveAddressStream {
    resolveNextAddress() {
      return undefined;
    }
    subscribe() {
      return ready();
    }
  }

  class TcpSocket {
    #family;
    #options = { ...TCP_DEFAULTS };
    constructor(family) {
      this.#family = family;
    }
    startBind() {
      throw "access-denied";
    }
    finishBind() {
      throw "not-in-progress";
    }
    startConnect() {
      throw "access-denied";
    }
    finishConnect() {
      throw "not-in-progress";
    }
    startListen() {
      throw "invalid-state";
    }
    finishListen() {
      throw "not-in-progress";
    }
    accept() {
      throw "invalid-state";
    }
    localAddress() {
      throw "invalid-state";
    }
    remoteAddress() {
      throw "invalid-state";
    }
    isListening() {
      return false;
    }
    addressFamily() {
      return this.#family;
    }
    setListenBacklogSize(n) {
      if (n === 0n || n === 0) throw "invalid-argument";
    }
    keepAliveEnabled() {
      return this.#options.keepAliveEnabled;
    }
    setKeepAliveEnabled(v) {
      this.#options.keepAliveEnabled = !!v;
    }
    keepAliveIdleTime() {
      return this.#options.keepAliveIdleTime;
    }
    setKeepAliveIdleTime(v) {
      if (v <= 0n) throw "invalid-argument";
      this.#options.keepAliveIdleTime = v;
    }
    keepAliveInterval() {
      return this.#options.keepAliveInterval;
    }
    setKeepAliveInterval(v) {
      if (v <= 0n) throw "invalid-argument";
      this.#options.keepAliveInterval = v;
    }
    keepAliveCount() {
      return this.#options.keepAliveCount;
    }
    setKeepAliveCount(v) {
      if (v === 0) throw "invalid-argument";
      this.#options.keepAliveCount = v;
    }
    hopLimit() {
      return this.#options.hopLimit;
    }
    setHopLimit(v) {
      if (v === 0) throw "invalid-argument";
      this.#options.hopLimit = v;
    }
    receiveBufferSize() {
      return this.#options.receiveBufferSize;
    }
    setReceiveBufferSize(v) {
      if (v === 0n) throw "invalid-argument";
      this.#options.receiveBufferSize = v;
    }
    sendBufferSize() {
      return this.#options.sendBufferSize;
    }
    setSendBufferSize(v) {
      if (v === 0n) throw "invalid-argument";
      this.#options.sendBufferSize = v;
    }
    subscribe() {
      return ready();
    }
    shutdown() {
      throw "invalid-state";
    }
  }

  class IncomingDatagramStream {
    receive() {
      return [];
    }
    subscribe() {
      return ready();
    }
  }

  class OutgoingDatagramStream {
    checkSend() {
      return 0n;
    }
    send() {
      return 0n;
    }
    subscribe() {
      return ready();
    }
  }

  class UdpSocket {
    #family;
    #options = { ...UDP_DEFAULTS };
    constructor(family) {
      this.#family = family;
    }
    startBind() {
      throw "access-denied";
    }
    finishBind() {
      throw "not-in-progress";
    }
    stream() {
      throw "invalid-state";
    }
    localAddress() {
      throw "invalid-state";
    }
    remoteAddress() {
      throw "invalid-state";
    }
    addressFamily() {
      return this.#family;
    }
    unicastHopLimit() {
      return this.#options.unicastHopLimit;
    }
    setUnicastHopLimit(v) {
      if (v === 0) throw "invalid-argument";
      this.#options.unicastHopLimit = v;
    }
    receiveBufferSize() {
      return this.#options.receiveBufferSize;
    }
    setReceiveBufferSize(v) {
      if (v === 0n) throw "invalid-argument";
      this.#options.receiveBufferSize = v;
    }
    sendBufferSize() {
      return this.#options.sendBufferSize;
    }
    setSendBufferSize(v) {
      if (v === 0n) throw "invalid-argument";
      this.#options.sendBufferSize = v;
    }
    subscribe() {
      return ready();
    }
  }

  // The count of CO17 on the prototypes: in this stub, no call gives data.
  const count = (cls, names) => {
    for (const n of names) cls.prototype[n] = idle.wrap(cls.prototype[n]);
  };
  count(TcpSocket, COUNTED.tcp);
  count(UdpSocket, COUNTED.udp);
  count(IncomingDatagramStream, COUNTED.incoming);
  count(OutgoingDatagramStream, COUNTED.outgoing);
  count(ResolveAddressStream, COUNTED.lookup);

  const create = (Cls) => (family) => {
    if (!FAMILIES.has(family)) throw "not-supported";
    return new Cls(family);
  };

  return {
    instanceNetwork: { instanceNetwork: () => network },
    network: { Network },
    ipNameLookup: {
      ResolveAddressStream,
      // runwasi does not allow name lookup in the proxy world (fact 6).
      resolveAddresses: idle.wrap(fail("permanent-resolver-failure")),
    },
    tcp: { TcpSocket },
    tcpCreateSocket: { createTcpSocket: create(TcpSocket) },
    udp: { UdpSocket, IncomingDatagramStream, OutgoingDatagramStream },
    udpCreateSocket: { createUdpSocket: create(UdpSocket) },
  };
}
