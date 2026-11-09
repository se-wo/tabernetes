// proxy shows a web server of the in-browser cluster in a panel next to the
// terminal (or in a new tab): what listens on localhost:<port> of the wasm
// (kubectl port-forward), or a service. A service worker (web/proxy-sw.js,
// published as tabernetes-proxy/sw.js next to the page) catches the requests
// of the panel and posts them here; Go runs them against its in-process
// network. Requests to a service go through the service proxy of the tab,
// each on a connection of its own, so a reload may show another replica.
const B = (s) => `\x1b[1m${s}\x1b[0m`;
const DIM = (s) => `\x1b[2m${s}\x1b[0m`;

const USAGE = [
  "usage: proxy <port> | svc/<service>[:<port>] [-n <namespace>] [--tab]",
  "       proxy --close",
  "Shows a web server of the cluster in a panel next to the terminal",
  "(--tab: in a new browser tab), for example:",
  `  ${B("kubectl create deploy web --image=nginxdemos/hello --replicas=3")}`,
  `  ${B("kubectl expose deploy web --port 80")}`,
  `  ${B("proxy svc/web")}            ${DIM("every reload: the next replica")}`,
  `  ${B("kubectl port-forward deploy/web 8080:80 &")}`,
  `  ${B("proxy 8080")}               ${DIM("localhost:8080 of the wasm: one pod")}`,
];

// parseArgs reads the arguments of proxy: {target, label, service, namespace,
// tab, close} or {error}.
function parseArgs(args) {
  const o = { namespace: "default", tab: false, close: false };
  const rest = [];
  for (let i = 0; i < args.length; i++) {
    const a = args[i];
    if (a === "-n" || a === "--namespace") {
      if (i + 1 >= args.length) return { error: `${a} needs a namespace` };
      o.namespace = args[++i];
    } else if (a.startsWith("--namespace=")) o.namespace = a.slice(12);
    else if (a === "--tab" || a === "-t") o.tab = true;
    else if (a === "--close" || a === "close") o.close = true;
    else if (a.startsWith("-")) return { error: `unknown flag ${a}` };
    else rest.push(a);
  }
  if (o.close) return rest.length ? { error: "--close takes no target" } : o;
  if (rest.length !== 1) return { error: "" };
  const t = rest[0];
  if (/^\d{1,5}$/.test(t)) {
    const port = Number(t);
    if (port < 1 || port > 65535) return { error: `invalid port ${t}` };
    return { ...o, target: port, label: `localhost:${port}` };
  }
  const m = t.match(/^(?:svc|service|services)\/([a-z0-9]([-a-z0-9]*[a-z0-9])?)(?::([a-z0-9-]+))?$/);
  if (!m) return { error: `invalid target ${t}` };
  if (!/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(o.namespace)) return { error: `invalid namespace ${o.namespace}` };
  return { ...o, service: m[1], port: m[3] || "" };
}

// The panel next to the terminal: a bar (target, reload, new tab, close)
// above an iframe. The page may bring its own markup (site/terminal.html);
// otherwise it is created next to the terminal.
function createPanel(terminal) {
  let panel = document.getElementById("tabernetes-panel");
  if (!panel) {
    panel = document.createElement("div");
    panel.id = "tabernetes-panel";
    panel.className = "tabernetes-panel";
    panel.hidden = true;
    terminal.after(panel);
  }
  panel.replaceChildren();
  const bar = document.createElement("div");
  bar.className = "tabernetes-panel-bar";
  const button = (text, title) => {
    const b = document.createElement("button");
    b.type = "button";
    b.textContent = text;
    b.title = title;
    return b;
  };
  const reload = button("↻", "Reload (a service answers from its next replica)");
  const label = document.createElement("span");
  label.className = "tabernetes-panel-label";
  const open = document.createElement("a");
  open.textContent = "↗";
  open.title = "Open in a new tab";
  open.target = "_blank";
  const close = button("✕", "Close the panel (proxy --close)");
  bar.append(reload, label, open, close);
  const frame = document.createElement("iframe");
  frame.name = "tabernetes-panel";
  frame.title = "Web server of the cluster";
  panel.append(bar, frame);

  const show = (url, text) => {
    label.textContent = text;
    label.title = url;
    open.href = url;
    frame.src = url;
    panel.hidden = false;
  };
  const hide = () => {
    panel.hidden = true;
    frame.src = "about:blank";
  };
  reload.addEventListener("click", () => {
    try {
      frame.contentWindow.location.reload();
    } catch {
      frame.src = open.href;
    }
  });
  close.addEventListener("click", hide);
  return { show, hide, open, isOpen: () => !panel.hidden };
}

// The worker and its scope are relative to the page, so the site works under
// any base path (GitHub Pages: /<repo>/tabernetes-proxy/).
export function proxyBuiltins({
  scope = new URL("tabernetes-proxy/", document.baseURI).pathname,
  worker = `${scope}sw.js`,
  status,
  terminal,
} = {}) {
  const sw = navigator.serviceWorker;
  if (sw) {
    sw.addEventListener("message", async (e) => {
      if (e.data?.type !== "tabernetes-proxy") return;
      const [reply] = e.ports;
      const { method, path, headers, body } = e.data;
      const target = e.data.target ?? e.data.port;
      try {
        const r = await globalThis.tabernetes.proxy(target, method, path, headers, body ? new Uint8Array(body) : null);
        reply.postMessage(r, [r.body.buffer]);
      } catch (err) {
        reply.postMessage({ error: err.message });
      }
    });
    sw.startMessages();
  }

  // The worker only serves the tabs of this page. When the page goes away,
  // so does the worker. Tabs it still controls keep it until they close.
  let registration = null;
  addEventListener("pagehide", () => {
    registration?.unregister();
    registration = null;
  });
  async function activeWorker() {
    registration ||= await sw.register(worker, { scope });
    const reg = registration;
    if (reg.active) return reg.active;
    const w = reg.installing || reg.waiting;
    await new Promise((resolve) => w.addEventListener("statechange", () => w.state === "activated" && resolve()));
    return reg.active;
  }
  // ready makes sure the worker runs and knows this tab.
  const ready = async () => (await activeWorker()).postMessage({ type: "tabernetes-host" });

  const urlOf = (target) => new URL(`${scope}${target}/`, location.href).href;

  // openTab opens a target in a new tab. window.open needs the click or key
  // press that started it: open the tab first, the worker is ready before
  // the tab sends its request.
  async function openTab(target) {
    const tab = window.open("about:blank", "_blank");
    try {
      await ready();
    } catch (err) {
      tab?.close();
      throw err;
    }
    if (tab) tab.location.href = urlOf(target);
    return tab;
  }

  let panel = null;
  async function openPanel(target, text) {
    if (!terminal) return openTab(target);
    await ready();
    panel ||= createPanel(terminal);
    panel.show(urlOf(target), text);
    return true;
  }

  // The status line links every port of "kubectl port-forward" while it
  // listens. Go calls this when the list changes (tabernetes/forwards.go).
  // A click shows the port in the panel, a middle or Ctrl click in a tab.
  globalThis.tabernetesForwards = (list) => {
    if (!status) return;
    status.replaceChildren(...list.map(({ port, target }) => {
      const label = `⇄ localhost:${port} → ${target}`;
      if (!sw) {
        const el = document.createElement("span");
        el.className = "tabernetes-port";
        el.textContent = label;
        return el;
      }
      const a = document.createElement("a");
      a.className = "tabernetes-port";
      a.href = urlOf(port);
      a.target = "_blank";
      a.textContent = label;
      a.title = `Show localhost:${port} of this tab (kubectl port-forward to ${target}); Ctrl-click: in a new tab`;
      a.addEventListener("click", (e) => {
        if (e.ctrlKey || e.metaKey || e.shiftKey) {
          e.preventDefault();
          openTab(port).catch((err) => console.error("tabernetes proxy:", err));
          return;
        }
        e.preventDefault();
        openPanel(port, `localhost:${port} → ${target}`).catch((err) => console.error("tabernetes proxy:", err));
      });
      return a;
    }));
    // A middle click opens the link without the click handler: have the
    // worker ready for it.
    if (list.length && sw) ready().catch(() => {});
  };

  return {
    async proxy(args, { term }) {
      const o = parseArgs(args);
      if (o.error !== undefined) {
        if (o.error) term.writeln(`proxy: ${o.error}`);
        for (const l of USAGE) term.writeln(l);
        return 2;
      }
      if (o.close) {
        panel?.hide();
        return 0;
      }
      if (!sw) {
        term.writeln("proxy: this browser offers no service workers here (private mode?)");
        return 1;
      }
      let { target, label } = o;
      if (o.service) {
        try {
          target = await globalThis.tabernetes.proxyService(o.service, o.namespace, o.port);
        } catch (err) {
          term.writeln(`proxy: ${err.message ?? err}`);
          return 1;
        }
        label = `svc/${o.service}${o.namespace === "default" ? "" : ` -n ${o.namespace}`} → ${target}`;
      }
      const url = urlOf(target);
      try {
        if (o.tab) {
          const tab = await openTab(target);
          term.writeln(`${tab ? "Opened" : "Open"} ${url}`);
        } else {
          await openPanel(target, label);
          term.writeln(`${label} next to the terminal (${B("proxy --close")} closes it, ${B("--tab")} opens a tab)`);
        }
      } catch (err) {
        term.writeln(`proxy: ${err.message}`);
        return 1;
      }
      term.writeln(DIM(o.service
        ? "Each request goes to the next ready replica of the service; works while this tab is open."
        : `It shows localhost:${target} of this tab and works while this tab is open.`));
      return 0;
    },
  };
}
