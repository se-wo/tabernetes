#!/bin/bash
# run.sh components|runwasi|browser|proxy|opaque|udptable|webrtc
# Proofs of concept for phase 0 of ../wasi-sockets.md. They build in
# components/target and in a temporary directory, and leave the rest of the
# repository alone.
#
#   components  build the Rust test components (wasm32-wasip2)
#   runwasi     run them with the published containerd-shim-wasmtime v0.6.1
#               under a containerd of its own (root, Docker for the images)
#   browser     transpile them in a transpile worker of headless Chromium and
#               run the command components in a pod worker (Playwright)
#   proxy       the two candidates of the HTTP path, 16 instances at the same
#               time and the cancel of 1,000 suspended instances (Playwright)
#   opaque      a pod worker from a data: URL: origin, Module, Blob import
#   udptable    a UDP table next to the in-process network of Go (js/wasm)
#   webrtc      the data channel "udp" between Chromium and pion
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
SHIM_VERSION=containerd-shim-wasmtime/v0.6.1

components() {
	rustup target add wasm32-wasip2 >/dev/null 2>&1 || true
	(cd "$here/components" && cargo build -q --release --target wasm32-wasip2)
}

cmd="${1:-}"
shift || true
case "$cmd" in
components)
	components
	ls -l "$here/components/target/wasm32-wasip2/release/"*.wasm
	;;
runwasi)
	components
	tmp="$(mktemp -d)"
	sock=/run/wasi-sockets-poc/c.sock # a unix socket path has at most 104 bytes
	cleanup() {
		command ctr -a "$sock" task kill -s KILL px >/dev/null 2>&1 || true
		sleep 1
		command ctr -a "$sock" task delete px >/dev/null 2>&1 || true
		[ -n "${ctrd:-}" ] && kill "$ctrd" 2>/dev/null
		sleep 1
		umount -R "$tmp/state"/*/*/*/rootfs 2>/dev/null || true
		rm -rf "$tmp" /run/wasi-sockets-poc
	}
	trap cleanup EXIT
	mkdir -p "$tmp/bin" "$tmp/img" /run/wasi-sockets-poc
	url="https://github.com/containerd/runwasi/releases/download/${SHIM_VERSION}"
	curl -fsSLo "$tmp/shim.tgz" "$url/containerd-shim-wasmtime-x86_64-linux-musl.tar.gz"
	sum="$(curl -fsSL "$url/SHA256SUMS" | awk '/x86_64-linux-musl/ { print $1 }')"
	echo "$sum  $tmp/shim.tgz" | sha256sum -c - >/dev/null
	tar -xzf "$tmp/shim.tgz" -C "$tmp/bin" ./containerd-shim-wasmtime-v1
	cat >"$tmp/config.toml" <<-EOF
		version = 3
		root = "$tmp/root"
		state = "$tmp/state"
		[grpc]
		  address = "$sock"
	EOF
	PATH="$tmp/bin:$PATH" containerd -c "$tmp/config.toml" >"$tmp/containerd.log" 2>&1 &
	ctrd=$!
	for _ in $(seq 20); do
		[ -S "$sock" ] && break
		sleep 0.5
	done
	ctr() { command ctr -a "$sock" "$@" 2> >(grep -v DEPRECATION >&2); }
	for c in cmd exitcode cmdhttp proxy; do
		cp "$here/components/target/wasm32-wasip2/release/$c.wasm" "$tmp/img/"
		printf 'FROM scratch\nCOPY %s.wasm /%s.wasm\nENTRYPOINT ["/%s.wasm"]\n' "$c" "$c" "$c" >"$tmp/img/Dockerfile"
		docker build -q -t "poc/$c:1" "$tmp/img" >/dev/null
	done
	docker save poc/cmd:1 poc/exitcode:1 poc/cmdhttp:1 poc/proxy:1 -o "$tmp/images.tar"
	ctr images import "$tmp/images.tar" >/dev/null
	run() { # run <name> <image> [ctr run flags]: the exit status of the task
		local name=$1 image=$2
		shift 2
		set +e
		# timeout runs the binary ctr, not the function
		timeout 30 ctr -a "$sock" run --rm --runtime io.containerd.wasmtime.v1 \
			"$@" "docker.io/poc/$image:1" "$name" >/dev/null 2>&1
		echo $?
		set -e
	}
	echo "== item 13: exit codes"
	for c in 0 1 3; do echo "std::process::exit($c): status $(run "e$c" cmd --env EXIT_CODE=$c)"; done
	echo "exit-with-code(3): status $(run x3 exitcode --env EXIT_CODE=3)"
	echo "== CR6: a command component with an import of wasi:http"
	echo "status $(run h cmdhttp)"
	echo "== PX33: an IP that the pod cannot bind"
	echo "status $(run f proxy --net-host --env WASMTIME_HTTP_PROXY_SOCKET_ADDR=192.0.2.7:8082)"
	echo "== items 7 and 14: the proxy component on 127.0.0.1:18080"
	ctr run -d --net-host --runtime io.containerd.wasmtime.v1 \
		--env WASMTIME_HTTP_PROXY_SOCKET_ADDR=127.0.0.1:18080 docker.io/poc/proxy:1 px >/dev/null
	sleep 2
	get() { curl -s -i --max-time 5 -H 'Cookie: c=1' -H 'User-Agent: ua/1' "http://127.0.0.1:18080/$1" || echo "(curl exit $?)"; }
	for p in context context headers cookie status/204 sockets error trap; do
		echo "-- /$p"
		get "$p" | tr -d '\r'
	done
	echo "-- /noresponse"
	get noresponse | tr -d '\r'
	sleep 2
	ctr task ls
	# containerd logs the exit status of the task when it deletes the task.
	ctr task delete px >/dev/null || true
	grep -o 'task px exit with non-zero exit code [0-9]*' "$tmp/containerd.log" || true
	grep -o 'error running start function: [^"]*' "$tmp/containerd.log" | sort -u
	;;
browser | proxy)
	components
	(cd "$here/components" && cargo build -q --target wasm32-wasip2 -p cmd -p proxy)
	cd "$here/browser"
	npm ci --silent 2>/dev/null || npm install --silent
	# jco-transpile has dynamic imports of node:fs for Node.js only.
	npx esbuild src/transpile.worker.js src/pod.worker.js src/proxy.worker.js --bundle --format=esm \
		--target=es2022 --external:'node:*' --outdir=site/assets --log-level=warning
	mkdir -p site/vendor
	cp node_modules/@bytecodealliance/jco-transpile/vendor/js-component-bindgen-component.core*.wasm site/vendor/
	cp index.html site/
	t="$here/components/target/wasm32-wasip2"
	if [ "$cmd" = proxy ]; then
		node proxy-driver.mjs "$t/release/proxy.wasm"
		exit
	fi
	node driver.mjs "$t/release/cmd.wasm" "$t/release/exitcode.wasm" "$t/release/proxy.wasm" \
		"$t/debug/cmd.wasm" "$t/debug/proxy.wasm" "$@"
	;;
opaque)
	cd "$here/browser"
	npm ci --silent 2>/dev/null || npm install --silent
	node opaque-driver.mjs
	;;
webrtc)
	tmp="$(mktemp -d)"
	trap 'rm -rf "$tmp"' EXIT
	cp "$here/webrtc/main.go" "$tmp/"
	(cd "$here/browser" && { npm ci --silent 2>/dev/null || npm install --silent; })
	cd "$tmp"
	go mod init rtc >/dev/null 2>&1
	# The version of node/agent/go.mod.
	go get github.com/pion/webrtc/v4@v4.2.26 >/dev/null 2>&1
	go mod tidy >/dev/null 2>&1
	go build -o rtc .
	node "$here/webrtc/drive.mjs" ./rtc
	;;
udptable)
	tmp="$(mktemp -d)"
	trap 'rm -rf "$tmp"' EXIT
	cp "$here/udptable/main.go" "$tmp/"
	cd "$tmp"
	go mod init udptable >/dev/null 2>&1
	GOOS=js GOARCH=wasm go build -o u.wasm .
	env -i PATH="$PATH" HOME="$HOME" node "$(go env GOROOT)/lib/wasm/wasm_exec_node.js" u.wasm
	;;
*)
	sed -n '2,16p' "$0" >&2
	exit 2
	;;
esac
