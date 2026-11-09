// addnode and rmnode: nodes in Web Workers (k8s/tabernetes/nodeworker.go).
// Each worker runs the WebAssembly module of the page once more, as a node
// without the control plane, on a thread of its own. It talks to the
// apiserver like a Docker node, over MessageChannels instead of WebRTC.
import { portChannel, openChannel } from "./channel.js";

const MAX_WORKERS = 8; // every worker is a whole Go runtime: 100 MB and more

// nodeBuiltins returns the commands. status is an element for the state of
// the worker nodes under the terminal.
export function nodeBuiltins({ workerURL, wasi, status }) {
  const workers = new Map(); // name -> {worker, state}

  const render = () => {
    if (!status) return;
    status.replaceChildren(...[...workers].map(([name, { state }]) => {
      const el = document.createElement("span");
      el.className = "tabernetes-node";
      const dot = document.createElement("span");
      dot.textContent = "● ";
      dot.style.color = state === "running" ? "#3fb950" : state === "starting" ? "#d29922" : "#f85149";
      el.append(dot, `${name} ${state === "running" ? "in a worker" : state}`);
      return el;
    }));
  };

  async function addOne(write) {
    const module = globalThis.tabernetesModule;
    if (!module) throw new Error("the WebAssembly module is not loaded yet");
    const worker = new Worker(workerURL);
    const config = await globalThis.tabernetes.prepareWorkerNode((label) => Promise.resolve(openChannel(worker, label)));
    const entry = { worker, state: "starting" };
    workers.set(config.name, entry);
    const set = (state) => { entry.state = state; render(); };
    worker.onmessage = (e) => {
      const m = e.data;
      if (m.type === "channel") globalThis.tabernetes.attachChannel(portChannel(m.port, m.label));
      else if (m.type === "ready") set("running");
      else if (m.type === "crash") console.error(`${config.name}:`, m.text);
      else if (m.type === "error" || m.type === "exit") {
        console.error(`${config.name} stopped`, m.error ?? "");
        set("stopped");
      }
    };
    worker.onerror = (e) => { console.error(`${config.name}:`, e.message); set("stopped"); };
    worker.postMessage({ type: "start", module, config, wasi });
    render();
    write(`${config.name} (${config.ip}) starts in a Web Worker\n`);
  }

  return {
    // addnode [n] starts n nodes in Web Workers.
    async addnode(args, { write }) {
      const n = Math.max(1, Number(args[0]) || 1);
      for (let i = 0; i < n; i++) {
        if (workers.size >= MAX_WORKERS) {
          write(`addnode: at most ${MAX_WORKERS} nodes in workers\n`);
          return 1;
        }
        await addOne(write);
      }
      write("They are Ready in a few seconds: k get nodes -w\n");
    },
    // rmnode [--force] <name>... drains nodes in workers (kubectl drain),
    // stops them and deletes them. --force skips the drain.
    async rmnode(args, { write }) {
      const force = args.includes("--force") || args.includes("-f");
      const names = args.filter((a) => a !== "--force" && a !== "-f");
      if (!names.length) {
        write(`usage: rmnode [--force] <name>... (${[...workers.keys()].join(", ") || "no nodes in workers"})\n`);
        return 2;
      }
      let code = 0;
      for (const name of names) {
        const entry = workers.get(name);
        if (!entry) {
          write(`rmnode: ${name}: not a node in a worker\n`);
          code = 1;
          continue;
        }
        if (!force && entry.state === "running") {
          // Evictions respect PodDisruptionBudgets; a blocked drain keeps the
          // node, cordoned, like kubectl drain does.
          const drained = await globalThis.tabernetes.drainNode(name, write);
          if (drained !== 0) {
            write(`rmnode: draining ${name} failed, it stays cordoned (rmnode --force ${name} removes it anyway)\n`);
            code = 1;
            continue;
          }
        }
        entry.worker.terminate();
        workers.delete(name);
        render();
        await globalThis.tabernetes.removeWorkerNode(name);
        write(`${name} stopped and deleted, its pods move to other nodes\n`);
      }
      return code;
    },
  };
}
