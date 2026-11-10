#!/bin/bash
# run.sh engine|fakenet|tproxy
# Throwaway proofs of concept for ../networking-policy.md. Each one builds in a
# temporary directory and leaves the repository alone.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
KNP_VERSION=v1.1.3
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

wasmrun() { # Go's node runner passes the whole environment, too long here.
	env -i PATH="$PATH" HOME="$HOME" node "$(go env GOROOT)/lib/wasm/wasm_exec_node.js" "$1"
}

case "${1:-}" in
engine)
	cp "$here/engine/main.go" "$tmp/"
	cd "$tmp"
	go mod init poc >/dev/null 2>&1
	go get "sigs.k8s.io/kube-network-policies@$KNP_VERSION" >/dev/null
	go mod tidy >/dev/null
	go mod vendor
	# The only change js/wasm needs: syscall of js has no IPPROTO_{DSTOPTS,
	# HOPOPTS,ROUTING,FRAGMENT,SCTP}. The numbers are the IANA ones.
	sed -i -e 's/syscall\.IPPROTO_DSTOPTS/60/; s/syscall\.IPPROTO_HOPOPTS/0/' \
		-e 's/syscall\.IPPROTO_ROUTING/43/; s/syscall\.IPPROTO_FRAGMENT/44/; s/syscall\.IPPROTO_SCTP/132/' \
		vendor/sigs.k8s.io/kube-network-policies/pkg/network/packet.go
	GOOS=js GOARCH=wasm go build -mod=vendor -o poc.wasm .
	wasmrun poc.wasm 2>&1 | grep -v '^[IW][0-9]'
	;;
fakenet)
	cp "$here/fakenet/main.go" "$tmp/"
	cd "$tmp"
	go mod init fakenet >/dev/null 2>&1
	GOOS=js GOARCH=wasm go build -o f.wasm .
	wasmrun f.wasm
	;;
tproxy)
	# Linux only, needs unshare, ip, nft and python3. No root needed.
	unshare -Urnm --propagation private sh "$here/tproxy/tproxy.sh" "$here/tproxy/listen.py"
	;;
*)
	echo "usage: $0 engine|fakenet|tproxy" >&2
	exit 2
	;;
esac
