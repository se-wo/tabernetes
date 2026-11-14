// "join" and "accept": connect a real kubelet to the in-browser control plane
// using WebRTC data channels. Signaling is copy & paste, no server involved.
// "join --kubectl" connects kubectl on the Docker machine instead: the agent
// forwards port 6443 to the apiserver of this tab and prints a kubeconfig.

const B = (s) => `\x1b[1m${s}\x1b[0m`;
const DIM = (s) => `\x1b[2m${s}\x1b[0m`;

// Instead of --privileged. See tabernetes/README.md for why each flag is needed.
const NODE_FLAGS = "--pull always --memory 2g --pids-limit 4096 --security-opt seccomp=unconfined --security-opt systempaths=unconfined --security-opt apparmor=unconfined --device /dev/net/tun";

// kubectl mode needs no privileges. No -t: with a TTY, Docker mixes the logs
// of the agent (stderr) into the kubeconfig (stdout).
const KUBECTL_FLAGS = "--pull always";
// The file that the printed command writes the kubeconfig to.
export const KUBECONFIG_FILE = "tabernetes.kubeconfig";

// Messages on the "ctl" data channel (node/agent/ctl.go).
const CTL_BYE = "bye"; // agent -> tab: the agent stops now
const CTL_BYE_SEEN = "bye-seen"; // tab -> agent: the answer
const CTL_CLOSE = "close"; // tab -> agent: rmnode removed the connection

async function deflate(str) {
  const s = new Blob([str]).stream().pipeThrough(new CompressionStream("deflate-raw"));
  return new Uint8Array(await new Response(s).arrayBuffer());
}
async function inflate(bytes) {
  const s = new Blob([bytes]).stream().pipeThrough(new DecompressionStream("deflate-raw"));
  return await new Response(s).text();
}
const b64url = (u8) => btoa(String.fromCharCode(...u8)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
const unb64url = (s) => Uint8Array.from(atob(s.replace(/-/g, "+").replace(/_/g, "/")), (c) => c.charCodeAt(0));

export async function encodeToken(obj) { return b64url(await deflate(JSON.stringify(obj))); }
export async function decodeToken(s) { return JSON.parse(await inflate(unb64url(s.trim()))); }

// dockerCommand returns the docker run command line for a connection, in
// parts for the terminal: the flags, the image and what follows it. kind is
// "node" or "kubectl".
export function dockerCommand(kind, { image, port, name, token }) {
  const udp = `-p 127.0.0.1:${port}:${port}/udp`;
  if (kind === "kubectl") {
    // Only on 127.0.0.1 of the Docker machine: the token is cluster-admin.
    return {
      head: `docker run --rm ${udp} -p 127.0.0.1:6443:6443 --name tabernetes-${name}`,
      flags: KUBECTL_FLAGS,
      image,
      tail: `kubectl ${token} > ${KUBECONFIG_FILE}`,
    };
  }
  return { head: `docker run --rm -it ${udp} --name tabernetes-${name}`, flags: NODE_FLAGS, image, tail: token };
}

function gatheringComplete(pc, timeout) {
  if (pc.iceGatheringState === "complete") return Promise.resolve();
  return new Promise((resolve) => {
    const t = setTimeout(resolve, timeout);
    pc.addEventListener("icegatheringstatechange", () => {
      if (pc.iceGatheringState === "complete") { clearTimeout(t); resolve(); }
    });
  });
}

const STATES = {
  new: ["waiting for accept", "#d29922"],
  connecting: ["connecting", "#d29922"],
  connected: ["connected", "#3fb950"],
  disconnected: ["disconnected", "#d29922"],
  failed: ["failed", "#f85149"],
  closed: ["closed", "#8b949e"],
};

// joinBuiltins returns the join and accept commands. status is an element
// that shows every connection and the state of its WebRTC connection.
// connections is a Map, name -> {remove(write)}, through which rmnode
// (nodes.js) removes kubectl connections.
export function joinBuiltins({ image, stun = "stun:stun.l.google.com:19302", status, connections = new Map() }) {
  const entries = []; // {kind, info, pc, ctl}
  let pending = null;
  let seq = 0;

  const badge = (name, text, color) => {
    const el = document.createElement("span");
    el.className = "tabernetes-node";
    const dot = document.createElement("span");
    dot.textContent = "● ";
    dot.style.color = color;
    el.append(dot, `${name} ${text}`);
    return el;
  };
  const render = () => {
    if (!status) return;
    // The tab node (tabnode.go) is always there: a kubelet inside this tab.
    const tab = badge("tab", "in this tab", "#3fb950");
    if (!entries.length) {
      status.replaceChildren(tab, " · type join to add a Docker node");
      return;
    }
    status.replaceChildren(tab, ...entries.map(({ info, pc }) => {
      const [text, color] = STATES[pc.connectionState] || [pc.connectionState, "#8b949e"];
      return badge(info.name, text, color);
    }));
  };
  render();

  // release gives up what the tab holds for a connection that ended: the
  // ServiceAccount of kubectl (its token stops working), or the Node with
  // its pods, which move to other nodes.
  const release = (entry) => {
    const t = globalThis.tabernetes;
    return entry.kind === "kubectl" ? t.revokeKubectl(entry.info.name) : t.removeNode(entry.info.name);
  };
  // drop takes an entry off the status line.
  const drop = (entry) => {
    const i = entries.indexOf(entry);
    if (i >= 0) entries.splice(i, 1);
    connections.delete(entry.info.name);
    if (pending === entry) pending = null;
    render();
  };
  // later closes the connection after the last message had time to go out.
  const later = (pc) => setTimeout(() => pc.close(), 2000);

  // The agent says bye when it stops (Ctrl-C, docker stop). A node goes
  // at once instead of after the 300 s not-ready toleration. No drain: the
  // containers of the node stop with it anyway, and docker stop gives the
  // agent 10 seconds, too little to wait for PodDisruptionBudgets.
  const onBye = async (entry) => {
    try { entry.ctl.send(CTL_BYE_SEEN); } catch { /* closed meanwhile */ }
    drop(entry);
    later(entry.pc);
    try {
      await release(entry);
      console.info(`${entry.info.name} said bye: ${entry.kind === "kubectl" ? "its token is void" : "node deleted, its pods move"}`);
    } catch (e) {
      console.error(`${entry.info.name} said bye, but removing it failed:`, e);
    }
  };

  // remove is rmnode for a connection of this file.
  const remove = async (entry, write) => {
    if (entry.kind !== "kubectl") {
      write(`rmnode: ${entry.info.name} is a Docker node. Stop its container (docker stop tabernetes-${entry.info.name}): it leaves the cluster at once\n`);
      return 1;
    }
    try { entry.ctl.send(CTL_CLOSE); } catch { /* not open yet or closed */ }
    drop(entry);
    later(entry.pc);
    try {
      await release(entry);
    } catch (e) {
      write(`rmnode: ${entry.info.name}: ${e.message}\n`);
      return 1;
    }
    write(`${entry.info.name} disconnected, its token no longer works\n`);
    return 0;
  };

  // finish hands the answer of the agent to the pending connection.
  const finish = async (answer, term) => {
    let ans;
    try { ans = await decodeToken(answer.trim().replace(/^accept\s+/, "")); } catch { ans = null; }
    if (typeof ans?.sdp !== "string") {
      term.writeln(`That is not the ${B("accept ...")} line of the agent.`);
      return false;
    }
    await pending.pc.setRemoteDescription({ type: "answer", sdp: ans.sdp });
    term.writeln(`Connecting ${pending.info.name}... the status line below the terminal shows the connection.`);
    pending = null;
    render();
    return true;
  };

  // instructions prints what to run on the Docker machine.
  const instructions = async (term, kind, { image, port, name, token }) => {
    const c = dockerCommand(kind, { image, port, name, token });
    const full = `${c.head} ${c.flags} ${c.image} ${c.tail}`;
    if (kind === "kubectl") {
      term.writeln(`Forward kubectl on a machine with docker to this cluster:`);
    } else {
      term.writeln(`Start a node on a machine with docker (Linux, macOS or Windows):`);
    }
    term.writeln("");
    term.writeln(B(`${c.head} \\`));
    term.writeln(B(`  ${c.flags} \\`));
    term.writeln(B(`  ${c.image} \\`));
    term.writeln(B(`  ${c.tail}`));
    term.writeln("");
    if (kind === "kubectl") {
      term.writeln(`Then, in another terminal there: ${B(`kubectl --kubeconfig ${KUBECONFIG_FILE} get nodes`)}`);
      term.writeln(DIM(`The token is cluster-admin, so the port stays on 127.0.0.1. Port 6443 taken? Use`));
      term.writeln(DIM(`-p 127.0.0.1:16443:6443 and add --server https://127.0.0.1:16443 before "kubectl".`));
      term.writeln(DIM(`The kubeconfig works until the agent stops, rmnode ${name}, or a reload of this page.`));
      term.writeln(DIM(`kubectl shares the one thread of this tab with the control plane: busy use slows it.`));
    } else {
      term.writeln(`Docker on another machine in your network? Publish the port on all interfaces and`);
      term.writeln(`add ${B("--advertise <LAN IP of that machine>")} before the token:`);
      term.writeln(DIM(`  docker run ... -p ${port}:${port}/udp ... ${image} --advertise 192.168.x.y <token>`));
      term.writeln(DIM(`Ctrl-C or docker stop there: the node says bye and leaves the cluster at once.`));
    }
    term.writeln("");
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(full).then(() => term.writeln(DIM("(the docker command was copied to your clipboard)")), () => {});
    }
  };

  return {
    async join(args, { term, ask, active }) {
      const unknown = args.filter((a) => a !== "--kubectl");
      if (unknown.length) {
        term.writeln("usage: join [--kubectl]");
        return 2;
      }
      const kind = args.includes("--kubectl") ? "kubectl" : "node";
      if (pending) {
        // The agent never answered (docker run failed, accept line lost): start over.
        term.writeln(DIM(`dropping the pending join for ${pending.info.name}`));
        const old = pending;
        old.pc.close();
        drop(old);
        if (old.kind === "kubectl") release(old).catch((e) => console.error(e));
      }
      const pc = new RTCPeerConnection({ iceServers: stun ? [{ urls: stun }] : [] });
      const ctl = pc.createDataChannel("ctl");
      const open = (label) => new Promise((resolve, reject) => {
        const dc = pc.createDataChannel(label);
        const t = setTimeout(() => reject(new Error("data channel timeout")), 15000);
        dc.onopen = () => { clearTimeout(t); resolve(dc); };
        dc.onerror = (e) => { clearTimeout(t); reject(e.error || new Error("data channel error")); };
      });
      let info;
      try {
        info = kind === "kubectl" ? await globalThis.tabernetes.prepareKubectl() : await globalThis.tabernetes.prepareJoin(open);
      } catch (e) {
        pc.close();
        term.writeln(`join: ${e.message}`);
        return 1;
      }
      const entry = { kind, info, pc, ctl };
      if (kind === "kubectl") {
        // kubectl only opens channels to the apiserver.
        pc.ondatachannel = (e) => {
          if (e.channel.label === "api") globalThis.tabernetes.attachChannel(e.channel, info.name);
          else e.channel.close();
        };
      } else {
        // Channels the node opens; the page names the node (pod channels trust it).
        pc.ondatachannel = (e) => globalThis.tabernetes.attachChannel(e.channel, info.name);
      }
      ctl.onmessage = (e) => { if (e.data === CTL_BYE) onBye(entry); };
      await pc.setLocalDescription(await pc.createOffer());
      await gatheringComplete(pc, 3000);
      if (!active()) { // Ctrl-C while preparing
        pc.close();
        if (kind === "kubectl") release(entry).catch((e) => console.error(e));
        return;
      }
      const port = 50000 + ++seq;
      const token = await encodeToken(kind === "kubectl"
        ? { v: 1, use: kind, n: info.name, t: info.token, ca: info.ca, sdp: pc.localDescription.sdp, p: port }
        : { v: 1, use: kind, n: info.name, ip: info.ip, t: info.token, ca: info.ca, sdp: pc.localDescription.sdp, p: port });
      pc.onconnectionstatechange = () => {
        render();
        if (pc.connectionState !== "failed") return;
        // A failed connection never recovers: free it and its data channels.
        pc.close();
        // Without its tunnel a kubectl token is of no use: void it.
        if (kind === "kubectl" && entries.includes(entry)) {
          drop(entry);
          release(entry).catch((e) => console.error(e));
        }
      };
      pending = entry;
      globalThis.tabernetesLastJoin = { kind, token, port, name: info.name };
      entries.push(entry);
      connections.set(info.name, { remove: (write) => remove(entry, write) });
      render();

      await instructions(term, kind, { image, port, name: info.name, token });
      term.writeln(`The agent prints an ${B("accept ...")} line. Paste it here, or press Ctrl-C and run ${B("accept ...")} later.`);
      for (;;) {
        let answer;
        try { answer = await ask("accept line: "); } catch { return; }
        if (pending !== entry) return; // another join replaced this one
        if (answer.trim() && await finish(answer, term)) return;
      }
    },

    async accept(args, { term }) {
      if (!pending) { term.writeln(`Nothing to accept. Run ${B("join")} first.`); return; }
      if (!args[0]) { term.writeln(`usage: accept <answer>`); return; }
      await finish(args[0], term);
    },
  };
}
