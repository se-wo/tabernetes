// Service worker behind the "proxy" command of the Tabernetes terminal.
//
// A tab or the panel next to the terminal on <page>/tabernetes-proxy/<target>/...
// shows a web server inside the WebAssembly of the terminal tab. <target> is
// a port of localhost there (typically kubectl port-forward) or a service,
// <name>.<namespace>:<port>. A web page cannot open a TCP port, so this
// worker hands each request to the terminal tab, which runs it against its
// in-process network and posts the response back.
// The page registers it next to itself: "/tabernetes-proxy/" on a domain root,
// "/<repo>/tabernetes-proxy/" on GitHub Pages.
const scope = new URL(self.registration.scope).pathname;
const TIMEOUT = 60_000;

self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (e) => e.waitUntil(self.clients.claim()));

// The terminal tab says hello when it runs "proxy". The worker may be
// stopped and started again, so this is only the first guess.
let host = null;
self.addEventListener("message", (e) => {
  if (e.data?.type === "tabernetes-host") host = e.source.id;
});

// targetOf returns the target of a path below the scope: a port (number),
// a service ("web.default:80") or "" for none.
const targetOf = (pathname) => {
  const m = pathname.startsWith(scope) &&
    pathname.slice(scope.length).match(/^(?:(\d{1,5})|([a-z0-9-]+\.[a-z0-9-]+:\d{1,5}))(?=\/|$)/);
  if (!m) return "";
  return m[1] ? Number(m[1]) : m[2];
};

self.addEventListener("fetch", (e) => {
  const url = new URL(e.request.url);
  if (url.origin !== self.location.origin) return;
  const target = targetOf(url.pathname);
  if (target) {
    const rest = url.pathname.slice(scope.length + String(target).length);
    // Relative links need the trailing slash: /tabernetes-proxy/80 -> /tabernetes-proxy/80/
    if (!rest) return e.respondWith(Response.redirect(`${url.pathname}/${url.search}`, 302));
    return e.respondWith(forward(e.request, target, rest + url.search));
  }
  // An absolute path ("/style.css") on a proxied page belongs to its target.
  if (e.clientId) {
    e.respondWith((async () => {
      const client = await self.clients.get(e.clientId);
      const t = client && targetOf(new URL(client.url).pathname);
      return t ? forward(e.request, t, url.pathname + url.search) : fetch(e.request);
    })());
  }
});

async function forward(req, target, path) {
  const body = req.method === "GET" || req.method === "HEAD" ? null : await req.arrayBuffer();
  const msg = { type: "tabernetes-proxy", target, method: req.method, path, headers: [...req.headers], body };
  let r;
  try {
    r = await ask(msg);
  } catch (err) {
    return text(502, `proxy ${target}: ${err.message}`);
  }
  if (r.error) return text(502, `proxy ${target}: ${r.error}`);
  const headers = new Headers(r.headers);
  const loc = headers.get("location");
  if (loc) {
    // Redirects to localhost, or to the service, stay in the proxy.
    const svc = typeof target === "string" && target.slice(0, target.lastIndexOf(":"));
    const u = new URL(loc, `http://${svc ? target : `localhost:${target}`}${path}`);
    const [name, ns] = svc ? svc.split(".") : [];
    const local = ["localhost", "127.0.0.1", "[::1]"].includes(u.hostname);
    const same = svc && [name, svc, `${svc}.svc`, `${svc}.svc.cluster.local`].includes(u.hostname);
    if (!svc && local) {
      headers.set("location", `${scope}${u.port || 80}${u.pathname}${u.search}${u.hash}`);
    } else if (same) {
      headers.set("location", `${scope}${name}.${ns}:${u.port || 80}${u.pathname}${u.search}${u.hash}`);
    }
  }
  const nullBody = [101, 204, 205, 304].includes(r.status);
  return new Response(nullBody ? null : r.body, { status: r.status, statusText: r.statusText, headers });
}

// ask sends the request to the terminal tab. Without a known one, every
// window of the site gets it and the first Tabernetes terminal answers.
async function ask(msg) {
  let tabs = [];
  const known = host && (await self.clients.get(host));
  if (known) tabs = [known];
  else {
    const all = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
    tabs = all.filter((c) => !new URL(c.url).pathname.startsWith(scope));
  }
  if (!tabs.length) {
    // The terminal tab is gone without unregistering (crash, closed browser).
    self.registration.unregister();
    throw new Error("the Tabernetes terminal tab is closed");
  }
  const replies = tabs.map((tab) => new Promise((resolve) => {
    const { port1, port2 } = new MessageChannel();
    port1.onmessage = (m) => {
      if (tab.id !== host && !m.data.error) host = tab.id;
      resolve(m.data);
    };
    tab.postMessage(msg, [port2]);
  }));
  const timeout = new Promise((_, reject) => setTimeout(() => reject(new Error(
    "no answer from the Tabernetes terminal tab (is the cluster running?)")), TIMEOUT));
  return Promise.race([...replies, timeout]);
}

function text(status, s) {
  return new Response(s + "\n", { status, headers: { "content-type": "text/plain; charset=utf-8" } });
}
