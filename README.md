# Tabernetes (modified version)

A real Kubernetes control plane in a browser tab: kube-apiserver,
kube-controller-manager, kube-scheduler and kubectl, compiled to WebAssembly
into one binary, with a terminal around it. This repository publishes a
modified version of it.

Live page: <https://se-wo.github.io/tabernetes/>

## Credit

Tabernetes was created by **Jan-Otto Kröpke**
([jkroepke](https://github.com/jkroepke)), © 2025-2026 Jan-Otto Kröpke.

- Original work: <https://jkroepke.de/tabernetes/>
- Original source: <https://github.com/jkroepke/blog/tree/main/tabernetes>
- License: [CC BY-NC-SA 4.0](https://creativecommons.org/licenses/by-nc-sa/4.0/)

This repository is a fork of the original author's blog repository. It is
not run or endorsed by the original author. The blog itself (posts, pages,
theme, Hugo configuration) has been removed; only Tabernetes is kept.

## Changes

This is a modified version of Tabernetes. The changes to the original work:

- A Kubernetes node inside the browser tab (node `tab`): a kubelet in the
  WebAssembly binary runs pods without Docker.
- WebAssembly pods: a WASI runtime in Web Workers runs WebAssembly container
  images on the tab node, and runwasi runs them on the Docker node.
- Publishing of only Tabernetes with GitHub Pages: a standalone page
  (`tabernetes/site/`) instead of the Hugo blog, the workflow
  `.github/workflows/pages.yaml`, the node image under the owner of this
  repository, and a footer with the attribution and the licenses.

The full history of the changes is in the Git log of this repository.

## License

Tabernetes, its own code and text, is licensed under the
[Creative Commons Attribution-NonCommercial-ShareAlike 4.0 International
License](https://creativecommons.org/licenses/by-nc-sa/4.0/) (CC BY-NC-SA 4.0).
The full legal code is in [LICENSE](LICENSE).

The original repository contains the CC BY-NC-SA 4.0 legal code
(`content/LICENCE`), while the footer of its blog named CC BY-NC 4.0. This
repository uses CC BY-NC-SA 4.0, the stricter of the two; complying with it
also complies with CC BY-NC 4.0.

What that means if you use or share this work:

- **Attribution**: credit Jan-Otto Kröpke as the creator, link the original
  work and the license, and say that you changed it, if you did.
- **NonCommercial**: no commercial use.
- **ShareAlike**: adaptations must be shared under the same license
  (CC BY-NC-SA 4.0).
- **No warranties**: the work is provided as is, see section 5 of the
  license (Disclaimer of Warranties and Limitation of Liability).

Third-party software keeps its own license and is not covered by the
Creative Commons license: Kubernetes (Apache License 2.0), the Go standard
library (BSD-3-Clause), mvdan.cc/sh (BSD-3-Clause), the other Go modules
compiled into the binary, and the JavaScript packages of the terminal
(xterm.js and fzstd, MIT). The site build collects the list and the license
texts into `THIRD_PARTY_LICENSES.txt` next to the page
(`tabernetes/site/third-party.mjs`); the footer of the page links it.

## Build

The tasks live in `tabernetes/mise.toml`. [mise](https://mise.jdx.dev/)
installs Go and Node.js. See [tabernetes/README.md](tabernetes/README.md) for
how Tabernetes works.

```bash
cd tabernetes
mise run build       # the WebAssembly binary and the terminal, into .build/assets/
mise run site        # the page, into .build/site/
python3 -m http.server -d .build/site 8080   # http://localhost:8080/
```

All URLs of the page are relative, so it works under any path.

## Publishing

`.github/workflows/pages.yaml` builds the page on every push to `main` and
deploys it with GitHub Pages to `https://<owner>.github.io/<repository>/`.
`.github/workflows/tabernetes-image.yaml` builds the node image that `join`
prints and pushes it to `ghcr.io/<owner>/tabernetes:latest`.

One-time setup in the repository settings:

1. Settings → Pages → Build and deployment → Source: **GitHub Actions**.
2. Actions → "tabernetes image" → Run workflow (on `main`), once, so that the
   node image exists.
3. The package `tabernetes` on the owner's profile (Packages) → Package
   settings → Change visibility → **Public**, so that `docker run` can pull
   it without a login.
4. Actions → "Pages" → Run workflow, or push to `main`.
