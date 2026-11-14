#!/bin/bash
# check.sh [step...]
# Joins a Docker node to the control plane in a headless browser tab and runs
# the CKAD checks of issue #56 on it. See ../docker-node-check.md.
#
# Steps, in this order when none are given:
#   images join kubectl apply coredns storage metrics ingress sanity
#   wasi-images wasi-command wasi-proxy wasi-sockets wasi-udp leave
# Each step prints PASS, FAIL or SKIP with the reason, and writes what it ran
# and saw to $OUT/<step>.txt. browser.mjs must already run. "leave" removes
# the node, so it comes last. The steps wasi-* are in wasi-*.sh, with their
# objects in wasi-*.yaml (../wasi-sockets.md, section 10.2).
#
# Env:
#   IMAGE       the node image (default ghcr.io/se-wo/tabernetes:latest)
#   NODE_FLAGS  docker run flags (default: the flags "join" prints). A host
#               with cgroups v1 needs --privileged, see the note.
#   IMPORT=1    import IMPORT_IMAGES with ctr from this host into the node:
#               for images the node cannot pull and its mirror does not serve
#   OOM_WORKAROUND=1  tell containerd in the node not to lower oom_score_adj,
#               for a Docker daemon without CAP_SYS_RESOURCE (nested Docker)
#   OUT         directory for the output (default ./out)
#   OCI         the OCI mirror of the page with the test images for wasi-images
#               (default .build/site/oci, from mise run site)
#   KUBECTL     the kubectl of the step kubectl (default: kubectl
#               KUBECTL_VERSION, downloaded into $OUT/bin)
#   KUBECTL_PORT  the host port of "join --kubectl" (default 6443)
set -uo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
IMAGE="${IMAGE:-ghcr.io/se-wo/tabernetes:latest}"
# The flags of web/src/join.js, without --pull always.
NODE_FLAGS="${NODE_FLAGS:---memory 2g --pids-limit 4096 --security-opt seccomp=unconfined --security-opt systempaths=unconfined --security-opt apparmor=unconfined --device /dev/net/tun}"
IMPORT_IMAGES="${IMPORT_IMAGES:-busybox:latest rancher/local-path-provisioner:v0.0.31}"
OUT="${OUT:-$PWD/out}"
KUBECTL_VERSION="${KUBECTL_VERSION:-v1.37.2}"
KUBECTL_PORT="${KUBECTL_PORT:-6443}"
N=tabernetes-node-1
mkdir -p "$OUT"

tab() { node "$here/tab.mjs" "$@"; }
# run logs a command line and its output to the file of the current step.
run() {
	echo "\$ $*" >>"$OUT/$step.txt"
	"$@" >>"$OUT/$step.txt" 2>&1
}
k() { run tab run "kubectl $*"; }
result() { echo "$1 $step: $2" | tee -a "$OUT/$step.txt" "$OUT/summary.txt"; }
# wait_for '<command line in the tab>' '<regex>' <seconds>: polls until the
# output matches.
wait_for() {
	local end=$((SECONDS + $3)) out=""
	while ((SECONDS < end)); do
		out="$(tab run "$1" 2>&1)"
		if grep -Eq "$2" <<<"$out"; then
			printf '$ %s\n%s\n' "$1" "$out" >>"$OUT/$step.txt"
			return 0
		fi
		sleep 5
	done
	printf '$ %s\n%s\n(no match for /%s/ after %s s)\n' "$1" "$out" "$2" "$3" >>"$OUT/$step.txt"
	return 1
}

# node_ns sets crictl_ns: the nsenter flags for the namespaces of containerd
# in the node. kubectl exec needs the serving certificate of the kubelet;
# crictl exec does not. Without CAP_SYS_ADMIN the agent runs containerd in
# namespaces of its own (node/agent/userns.go), and the stream server of
# crictl exec and attach listens on 127.0.0.1 in there.
crictl_ns=""
node_ns() {
	# shellcheck disable=SC2016 # for the shell in the node
	crictl_ns="$(docker exec "$N" sh -c 'for p in /proc/[0-9]*; do
		[ "$(cat "$p/comm")" = containerd ] || continue
		f="-n -m"
		[ "$(readlink "$p/ns/user")" = "$(readlink /proc/self/ns/user)" ] || f="-U --preserve-credentials $f"
		echo "-t ${p#/proc/} $f"
		break
	done')"
}

# node_crictl runs crictl in the node, in the namespaces of containerd.
node_crictl() {
	[ -n "$crictl_ns" ] || node_ns
	# shellcheck disable=SC2086 # crictl_ns is a list of flags
	docker exec -i "$N" nsenter $crictl_ns crictl "$@"
}

# pod_exec <pod> '<shell command line>': runs it in the first container of a
# pod on node-1, with crictl exec.
pod_exec() {
	local id
	id="$(node_crictl ps -q --label "io.kubernetes.pod.name=$1" | head -1)"
	if [ -z "$id" ]; then
		echo "(no container of the pod $1)"
		return 1
	fi
	node_crictl exec "$id" sh -c "$2" 2>&1
}

# apply_file <file>: deletes the objects of the file and creates them again.
apply_file() {
	run tab run "kubectl delete --ignore-not-found --wait=true -f - <<'EOF'
$(cat "$1")
EOF"
	run tab run "kubectl apply -f - <<'EOF'
$(cat "$1")
EOF"
}

step_images() {
	run sh -c "docker run --rm --entrypoint cat '$IMAGE' /usr/share/tabernetes/images/index.json |
		jq -r '.manifests[].annotations[\"org.opencontainers.image.ref.name\"]'"
	if grep -q local-path-provisioner "$OUT/$step.txt" && grep -q metrics-server "$OUT/$step.txt"; then
		result PASS "the node image has the addon images"
	else
		result FAIL "addon images missing from the node image"
	fi
}

step_join() {
	docker rm -f "$N" >/dev/null 2>&1
	if [ "$(docker info -f '{{.CgroupVersion}}')" = 1 ] && [[ $NODE_FLAGS != *--privileged* ]]; then
		echo "note: cgroups v1 host; without --privileged every pod sandbox fails" | tee -a "$OUT/$step.txt"
	fi
	local j token port line=""
	j="$(tab join)" || {
		result FAIL "join did not start"
		return 1
	}
	token="$(jq -r .token <<<"$j")" port="$(jq -r .port <<<"$j")"
	echo "\$ docker run -d --name $N -p 127.0.0.1:$port:$port/udp $NODE_FLAGS $IMAGE --stun '' <token>" >>"$OUT/$step.txt"
	# shellcheck disable=SC2086 # NODE_FLAGS is a list of flags
	docker run -d --name "$N" -p "127.0.0.1:$port:$port/udp" $NODE_FLAGS "$IMAGE" --stun "" "$token" >>"$OUT/$step.txt" 2>&1
	for _ in $(seq 60); do
		line="$(docker logs "$N" 2>&1 | grep -m1 '^accept ')" && break
		sleep 2
	done
	if [ -z "$line" ]; then
		result FAIL "no accept line, see docker logs $N"
		return 1
	fi
	run tab accept "$line"
	if [ "${OOM_WORKAROUND:-}" = 1 ]; then
		# The agent starts containerd again with the changed config. The
		# node image has no pkill.
		run docker exec "$N" sh -c "printf '%s\n' \"[plugins.'io.containerd.cri.v1.runtime']\" '  restrict_oom_score_adj = true' >>/etc/tabernetes/containerd.toml &&
			for p in /proc/[0-9]*; do [ \"\$(cat \$p/comm)\" = containerd ] && kill \${p#/proc/}; done; true"
	fi
	if ! wait_for 'kubectl get node node-1' ' Ready ' 240; then
		run sh -c "docker logs $N 2>&1 | grep -m3 RunPodSandbox"
		result FAIL "node-1 not Ready, see docker logs $N and /var/log/kubelet.log in it"
		return 1
	fi
	if [ "${IMPORT:-}" = 1 ]; then
		docker cp "$(command -v ctr)" "$N:/usr/local/bin/ctr"
		# shellcheck disable=SC2086 # a list of images
		run sh -c "docker save $IMPORT_IMAGES | docker exec -i $N ctr -n k8s.io images import -"
	fi
	run docker exec "$N" crictl images
	result PASS "node-1 Ready"
}

# get_kubectl sets KUBECTL: the kubectl of KUBECTL_VERSION for this host.
get_kubectl() {
	[ -z "${KUBECTL:-}" ] || return 0
	local arch
	case "$(uname -m)" in
	x86_64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) arch="$(uname -m)" ;;
	esac
	KUBECTL="$OUT/bin/kubectl"
	if [ ! -x "$KUBECTL" ] || ! "$KUBECTL" version --client 2>/dev/null | grep -q "$KUBECTL_VERSION\$"; then
		mkdir -p "$OUT/bin"
		curl -fsSLo "$KUBECTL" "https://dl.k8s.io/release/$KUBECTL_VERSION/bin/linux/$arch/kubectl" && chmod +x "$KUBECTL"
	fi
}

# kubectl runs a real kubectl on this host against the tab: "join --kubectl",
# the agent in kubectl mode forwards KUBECTL_PORT and prints the kubeconfig
# on stdout. Then "rmnode" in the tab closes the connection and voids the
# token. It needs only the step join before it.
step_kubectl() {
	local c=tabernetes-kubectl j token port name line="" kc="$OUT/kubectl.kubeconfig" server=()
	get_kubectl
	if ! "$KUBECTL" version --client >/dev/null 2>&1; then
		result FAIL "no kubectl $KUBECTL_VERSION"
		return 1
	fi
	docker rm -f "$c" >/dev/null 2>&1
	j="$(tab join --kubectl)" || {
		result FAIL "join --kubectl did not start"
		return 1
	}
	token="$(jq -r .token <<<"$j")" port="$(jq -r .port <<<"$j")" name="$(jq -r .name <<<"$j")"
	# The node mode refuses a kubectl token.
	run sh -c "docker run --rm '$IMAGE' join '$token' 2>&1; echo exit \$?"
	if ! grep -q 'join --kubectl' "$OUT/$step.txt" || grep -q '^exit 0$' "$OUT/$step.txt"; then
		result FAIL "the node mode took a kubectl token"
		return 1
	fi
	[ "$KUBECTL_PORT" = 6443 ] || server=(--server "https://127.0.0.1:$KUBECTL_PORT")
	echo "\$ docker run -d --name $c -p 127.0.0.1:$port:$port/udp -p 127.0.0.1:$KUBECTL_PORT:6443 $IMAGE --stun '' ${server[*]} kubectl <token>" >>"$OUT/$step.txt"
	docker run -d --name "$c" -p "127.0.0.1:$port:$port/udp" -p "127.0.0.1:$KUBECTL_PORT:6443" \
		"$IMAGE" --stun "" "${server[@]}" kubectl "$token" >>"$OUT/$step.txt" 2>&1
	for _ in $(seq 30); do
		line="$(docker logs "$c" 2>&1 | grep -m1 '^accept ')" && break
		sleep 2
	done
	if [ -z "$line" ]; then
		result FAIL "no accept line, see docker logs $c"
		return 1
	fi
	run tab accept "$line"
	# stdout of the agent is the kubeconfig and nothing else.
	for _ in $(seq 30); do
		docker logs "$c" 2>/dev/null >"$kc"
		grep -q '^current-context:' "$kc" && break
		sleep 2
	done
	run docker logs "$c"
	if grep -q accept "$kc" || ! grep -q '^current-context:' "$kc"; then
		result FAIL "stdout of the agent is not a kubeconfig, see $kc"
		return 1
	fi
	run "$KUBECTL" --kubeconfig "$kc" version
	run "$KUBECTL" --kubeconfig "$kc" get nodes -o wide
	run "$KUBECTL" --kubeconfig "$kc" auth whoami
	run "$KUBECTL" --kubeconfig "$kc" create configmap kubectl-check --from-literal=from=outside
	if ! grep -q "Server Version: $KUBECTL_VERSION" "$OUT/$step.txt" ||
		! grep -q "system:serviceaccount:kube-system:tabernetes-$name" "$OUT/$step.txt" ||
		! wait_for 'kubectl get cm kubectl-check -o jsonpath={.data.from}' '^outside' 30; then
		result FAIL "kubectl on this host did not reach the tab"
		return 1
	fi
	k delete cm kubectl-check
	run tab run "rmnode $name"
	for _ in $(seq 15); do
		[ "$(docker inspect -f '{{.State.Running}}' "$c" 2>/dev/null)" = true ] || break
		sleep 1
	done
	run docker logs --tail 3 "$c"
	if [ "$(docker inspect -f '{{.State.Running}}' "$c")" = true ]; then
		result FAIL "the agent still runs after rmnode $name"
	elif ! wait_for "kubectl get sa -n kube-system tabernetes-$name" 'NotFound|not found' 30; then
		result FAIL "the ServiceAccount of $name is still there"
	else
		result PASS "kubectl $KUBECTL_VERSION on this host reached the tab; rmnode $name stopped the agent and voided the token"
	fi
	docker rm -f "$c" >/dev/null 2>&1
}

# apply moves the local-path provisioner to node-1 and creates the test objects.
step_apply() {
	k -n local-path-storage rollout restart deploy/local-path-provisioner
	k -n local-path-storage rollout status deploy/local-path-provisioner --timeout=120s
	run tab run "kubectl apply -f - <<'EOF'
$(cat "$here/manifests.yaml")
EOF"
	if wait_for 'kubectl get pod bb' 'Running' 120; then
		result PASS "test objects created"
	else
		k describe pod bb
		result FAIL "pod bb not running (image pull? see IMPORT)"
	fi
}

step_coredns() {
	if wait_for 'kubectl get pods -n kube-system -l k8s-app=kube-dns -o wide' '1/1 +Running .* node-1' 180 &&
		! tab run 'kubectl get pods -n kube-system -l k8s-app=kube-dns -o wide' | grep -q ' tab '; then
		k get pods -n kube-system -o wide
		k exec bb -- nslookup kubernetes.default.svc.cluster.local
		run tab run 'nslookup kubernetes.default'
		result PASS "CoreDNS runs on node-1"
	else
		k get pods -n kube-system -o wide
		result FAIL "CoreDNS did not move to node-1"
	fi
}

step_storage() {
	k -n local-path-storage get pods -o wide
	if wait_for 'kubectl get pvc data' 'Bound' 180 && wait_for 'kubectl get pod writer' 'Running' 120; then
		k get pv -o wide
		k exec writer -- cat /data/x
		run docker exec "$N" sh -c 'ls /opt/local-path-provisioner/; cat /opt/local-path-provisioner/*/x'
		result PASS "PVC bound on node-1, writer wrote /data/x"
	else
		k describe pvc data
		result FAIL "PVC not bound or writer not running"
	fi
}

step_metrics() {
	k -n kube-system get pod -l k8s-app=metrics-server -o wide
	if wait_for 'kubectl top node' 'node-1 +[0-9]+m' 120; then
		k top pod -A
		result PASS "kubectl top node shows node-1"
	else
		k -n kube-system logs deploy/metrics-server --tail 30
		result FAIL "no metrics for node-1"
	fi
}

step_ingress() {
	wait_for 'kubectl get pods -l app=web -o wide' 'Running' 120
	k get ingress web
	if wait_for "curl -s -H 'Host: demo.local' 10.0.0.1:30080" '^web-' 60; then
		result PASS "the Ingress on tab reaches a backend on node-1"
	else
		result FAIL "the Ingress did not reach the backend"
	fi
	if ! docker exec "$N" crictl images | grep -q ingress-nginx/controller; then
		result SKIP "the real controller is not on node-1: its image comes from registry.k8s.io"
	fi
}

step_sanity() {
	if wait_for 'kubectl get pod sanity' '1/1 +Running' 120; then
		k logs sanity
		result PASS "securityContext, memory limit and probe on the real kubelet"
	else
		k describe pod sanity
		result FAIL "pod sanity not Ready"
	fi
}

# leave stops the node like Ctrl-C or docker stop would: the agent says bye,
# and the tab deletes the node and its pods at once, without the 300 s
# toleration for not-ready.
step_leave() {
	local start=$SECONDS
	run docker stop "$N"
	run sh -c "docker logs $N 2>&1 | tail -5"
	if ! wait_for 'kubectl get node node-1' 'NotFound|not found' 30; then
		result FAIL "node-1 is still there after docker stop: no bye?"
	elif wait_for 'kubectl get pods -n kube-system -l k8s-app=kube-dns -o wide' '1/1 +Running .* tab ' 180; then
		k get pods -n kube-system -o wide
		result PASS "node-1 left at once, CoreDNS back on tab after $((SECONDS - start)) s"
	else
		k get pods -n kube-system -o wide
		result FAIL "node-1 left, but CoreDNS is not back on tab after 3 minutes"
	fi
	docker rm -f "$N" >/dev/null 2>&1
}

for f in "$here"/wasi-*.sh; do
	# shellcheck source=/dev/null
	. "$f"
done

steps=("$@")
[ ${#steps[@]} -gt 0 ] || steps=(images join kubectl apply coredns storage metrics ingress sanity
	wasi-images wasi-command wasi-proxy wasi-sockets wasi-udp leave)
for step in "${steps[@]}"; do
	: >"$OUT/$step.txt"
	"step_${step//-/_}"
done
