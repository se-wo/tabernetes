+++
title = "Tabernetes: Kubernetes in your browser tab"
description = "A real Kubernetes control plane, compiled to WebAssembly: kube-apiserver, kube-controller-manager, kube-scheduler and kubectl run in your browser, with a node inside the tab. Join a real node with Docker and WebRTC."
+++

Tabernetes is a real Kubernetes v1.37 control plane. Not a simulator. kube-apiserver, kube-controller-manager, kube-scheduler and kubectl are the upstream Go code, compiled to WebAssembly into one binary. The only replaced part is etcd: a small in-memory implementation of the etcd v3 API. Everything runs in your browser tab. Reload the page and the cluster is gone.

This version also has a node inside the tab: `kubectl get nodes` shows the node `tab`, and pods run there without Docker. Type `join` and start the printed `docker run` command to add a real node. The container runs a kubelet and containerd and connects to your browser with WebRTC. Then `kubectl get pods -A` shows flannel, kube-proxy and CoreDNS on a real node.

{{< tabernetes >}}

The boot takes around 15 seconds. WebAssembly in the browser is single threaded, so the whole control plane shares one CPU core. Tabernetes was created by Jan-Otto Kröpke ([original page](https://jkroepke.de/tabernetes/), [original source](https://github.com/jkroepke/blog/tree/main/tabernetes)). This page runs a modified version, see the footer.
