// The Wasm memories of one browser tab (docs/wasi-sockets.md, section 8.6 and
// open question 2 of section 14).
//
// All Go threads (the page and the node workers) and all their pod workers
// and transpile workers are in one renderer process. They share one limit of
// approximately 125 Wasm memories. The broker of the page (createBroker)
// counts the memories with the numbers of section 14 and gives grants to the
// instances 2 to 16 of proxy containers (PX9):
//
//   - The Go binary of the page counts 2, the Go binary of a node worker 1, a
//     transpile worker 1, the pod worker of a component 1, a module 2.
//   - An instance 2 to 16 of a proxy container asks for a grant (memory-ask).
//     The broker gives it (memory-grant) while more than RESERVE memories
//     are free, else the ask waits in one queue for all containers (FIFO).
//     The pod worker gives the grant back (memory-free) at the end of the
//     instance.
//   - A start never waits for the broker. A wait for a grant has the limit
//     GRANT_WAIT_MS; after it, the pod worker tries to instantiate without a
//     grant, and PX34 handles a failure.
//   - When the browser refuses a memory, the pod worker sends memory-full.
//     If the count is lower than the limit, the broker uses the count as the
//     limit for LEARN_MS.
//   - While asks wait, and after memory-full, the broker sends collect to the
//     pod workers. A pod worker then makes one Wasm memory and drops it: on a
//     full tab this fails, and Chrome collects the garbage of that worker,
//     with the memories of ended instances.
//
// wasi.js takes these messages out of the transport of a pod worker and gives
// them to the broker. They never go to the Go side. The file is part of the
// bundles of the page, of node.worker.js, of wasi.worker.js and of
// component.worker.js, so it imports only wasi_messages.js.
import { examine } from "./wasi_messages.js";

// MAX_INSTANCES is the maximum of instances of one container at the same
// time (PX9). It is also the maximum of grants of one pod worker.
export const MAX_INSTANCES = 16;

// TAB_MEMORIES is the limit of one renderer process in Chromium (section 14).
// No interface of the browser gives it.
export const TAB_MEMORIES = 125;

// RESERVE is the number of free memories that the broker keeps for starts
// of pods, node workers and the first instances of proxy containers.
export const RESERVE = 8;

// GRANT_WAIT_MS is the maximum wait of an instance for a grant.
export const GRANT_WAIT_MS = 2000;

// LEARN_MS is the time for which a limit that the broker learned applies.
// The garbage of pod workers also causes refusals, and their garbage
// collection then frees memories, so a learned limit does not stay.
export const LEARN_MS = 10_000;

// COLLECT_MS is the minimum time between two rounds of collect.
export const COLLECT_MS = 1000;

// The messages of the broker. TO_BROKER go from a pod worker to the broker.
export const TO_BROKER = Object.freeze(["memory-ask", "memory-free", "memory-full"]);
export const FROM_BROKER = Object.freeze(["memory-grant", "collect"]);

// isMemoryError tells if an instantiation failed because the browser cannot
// make one more Wasm memory (section 9.2, item 15). Chrome gives a RangeError
// "Out of memory: Cannot allocate Wasm memory for new instance".
export function isMemoryError(e) {
  return /out of memory|cannot allocate wasm memory|could not allocate memory/i.test(String(e?.message ?? e));
}

// NO_MEMORY is the text for an instantiation that failed with isMemoryError.
export const NO_MEMORY = "the browser tab has no Wasm memory left for a new instance";

// collect makes one Wasm memory and drops it. On a full tab the browser
// first collects the garbage of this worker and then tries again.
export function collect() {
  try {
    new WebAssembly.Memory({ initial: 1 });
  } catch {
    // The tab is full also after the garbage collection.
  }
}

// createBroker gives the broker of the page.
//
//   broker.client({base, send, name})  a user of memories: base is its fixed
//                                      count; send(msg) sends memory-grant
//                                      and collect to a pod worker (null for
//                                      a Go binary or a transpile worker)
//     client.message(msg)              a message of the pod worker
//     client.end()                     frees all its memories and grants
//   broker.stats()                     {used, limit, free, waiting, clients, drops}
//   broker.collect()                   sends collect to all pod workers
//
// The broker examines each message (examine of wasi_messages.js) and trusts
// no number of a pod worker: a pod worker has at most maxGrants grants and
// asks, and a memory-free without a grant is dropped.
export function createBroker({
  limit = TAB_MEMORIES,
  reserve = RESERVE,
  maxGrants = MAX_INSTANCES,
  learnMs = LEARN_MS,
  collectMs = COLLECT_MS,
  now = () => performance.now(),
  setTimer = (fn, ms) => setTimeout(fn, ms)?.unref?.(),
  onDrop = (why, name) => console.warn(`memory broker: dropped a message of ${name || "a pod worker"}: ${why}`),
} = {}) {
  const clients = new Set();
  const queue = []; // one entry for each ask that waits: the client
  let learned = Infinity;
  let learnedUntil = -Infinity;
  let lastCollect = -Infinity;
  let drops = 0;

  const used = () => {
    let n = 0;
    for (const c of clients) n += c.base + c.grants;
    return n;
  };
  const currentLimit = () => (now() < learnedUntil ? Math.min(limit, learned) : limit);
  const free = () => currentLimit() - used();

  const collectAll = (force = false) => {
    const t = now();
    if (!force && t - lastCollect < collectMs) return;
    lastCollect = t;
    for (const c of clients) if (c.send) c.send({ type: "collect" });
  };

  const serve = () => {
    while (queue.length && free() > reserve) {
      const c = queue.shift();
      c.asks--;
      c.grants++;
      c.send({ type: "memory-grant" });
    }
    if (queue.length) collectAll();
  };

  const drop = (c, why) => {
    drops++;
    onDrop(why, c.name);
  };

  return {
    client({ base = 0, send = null, name = "" } = {}) {
      const c = { base, send, name, grants: 0, asks: 0, ended: false };
      clients.add(c);
      return {
        message(msg) {
          if (c.ended) return;
          const why = examine(msg, "w") || (TO_BROKER.includes(msg.type) ? "" : `${msg.type} does not go to the broker`);
          if (why) return drop(c, why);
          if (!c.send) return drop(c, `${msg.type} from a user without messages`);
          if (msg.type === "memory-ask") {
            if (c.grants + c.asks >= maxGrants) return drop(c, `a pod worker has at most ${maxGrants} grants`);
            c.asks++;
            queue.push(c);
          } else if (msg.type === "memory-free") {
            if (c.grants === 0) return drop(c, "memory-free without a grant");
            c.grants--;
          } else {
            // memory-full: the browser refused a memory.
            const u = used();
            if (u < currentLimit()) {
              learned = u;
              learnedUntil = now() + learnMs;
              setTimer(serve, learnMs + 1);
            }
            collectAll(true);
          }
          serve();
        },
        end() {
          if (c.ended) return;
          c.ended = true;
          clients.delete(c);
          for (let i = queue.length - 1; i >= 0; i--) if (queue[i] === c) queue.splice(i, 1);
          serve();
        },
      };
    },
    stats: () => ({ used: used(), limit: currentLimit(), free: free(), waiting: queue.length, clients: clients.size, drops }),
    collect: () => collectAll(true),
  };
}

// createGrants gives the side of a pod worker. It registers the handlers of
// memory-grant and collect on peer (createPeer of wasi_messages.js).
//
//   grants.ask()   a promise: true with a grant, false after waitMs
//   grants.keep()  asks for a grant without a wait and keeps it until the
//                  end of the pod worker
//   grants.free()  gives one grant back
//   grants.full()  tells the broker that the browser refused a memory
//
// An ask that waited too long stays in the queue of the broker. The next ask
// uses it and sends no new memory-ask, so the asks of the pod worker at the
// broker are never more than its waits. The grants and asks of the pod worker
// are never more than maxGrants: a wait over this number gets no grant and
// ends after waitMs.
export function createGrants(peer, { waitMs = GRANT_WAIT_MS, maxGrants = MAX_INSTANCES, setTimer = (fn, ms) => setTimeout(fn, ms), clearTimer = (t) => clearTimeout(t) } = {}) {
  const waiters = [];
  let held = 0;
  let asked = 0; // memory-ask without a grant yet
  const wait = (w) => {
    waiters.push(w);
    if (asked < waiters.length && held + asked < maxGrants) {
      asked++;
      peer.send("memory-ask");
    }
  };
  peer.on("memory-grant", () => {
    if (asked > 0) asked--;
    const w = waiters.shift();
    if (!w) {
      // The grant of an ask that waited too long: give it back.
      peer.send("memory-free");
      return;
    }
    if (w.timer !== null) clearTimer(w.timer);
    held++;
    w.resolve(true);
  });
  peer.on("collect", () => collect());
  return {
    ask() {
      return new Promise((resolve) => {
        const w = { resolve, timer: null };
        w.timer = setTimer(() => {
          const i = waiters.indexOf(w);
          if (i >= 0) waiters.splice(i, 1);
          resolve(false);
        }, waitMs);
        wait(w);
      });
    },
    keep() {
      wait({ resolve() {}, timer: null });
    },
    free() {
      if (held === 0) return;
      held--;
      peer.send("memory-free");
    },
    full: () => peer.send("memory-full"),
    get held() {
      return held;
    },
  };
}

// The broker of a node worker is the broker of the page. remoteBroker gives
// the object of createBroker in the node worker: post(m) sends {type:
// "memory", op, id, ...} to the page, and deliver(m) gives a message of the
// page ({type: "memory", id, msg}) to its client.
export function remoteBroker(post) {
  const sends = new Map();
  let next = 0;
  return {
    client({ base = 0, send = null } = {}) {
      const id = ++next;
      if (send) sends.set(id, send);
      post({ type: "memory", op: "open", id, base, messages: !!send });
      let ended = false;
      return {
        message: (msg) => !ended && post({ type: "memory", op: "message", id, msg }),
        end() {
          if (ended) return;
          ended = true;
          sends.delete(id);
          post({ type: "memory", op: "end", id });
        },
      };
    },
    deliver(m) {
      sends.get(m?.id)?.(m.msg);
    },
  };
}

// serveRemote is the page side of remoteBroker for one node worker. post(m)
// sends a message to the node worker. handle(m) takes the messages of the
// node worker; end() frees all memories of the node worker. The page does
// not trust the numbers of a node worker: base is 0, 1 or 2.
export function serveRemote(broker, post) {
  const clients = new Map();
  return {
    handle(m) {
      if (m?.type !== "memory" || !Number.isInteger(m.id) || m.id < 1) return;
      if (m.op === "open") {
        if (clients.has(m.id) || !Number.isInteger(m.base) || m.base < 0 || m.base > 2) return;
        const send = m.messages ? (msg) => post({ type: "memory", id: m.id, msg }) : null;
        clients.set(m.id, broker.client({ base: m.base, send }));
      } else if (m.op === "message") clients.get(m.id)?.message(m.msg);
      else if (m.op === "end") {
        clients.get(m.id)?.end();
        clients.delete(m.id);
      }
    },
    end() {
      for (const c of clients.values()) c.end();
      clients.clear();
    },
  };
}
