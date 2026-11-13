#!/bin/bash
# check.sh [step...]
# Joins a Docker node to the control plane in a headless browser tab and runs
# the CKAD checks of issue #56 on it. See ../docker-node-check.md.
#
# Steps, in this order when none are given:
#   images join apply coredns storage metrics ingress sanity leave
# Each step prints PASS, FAIL or SKIP with the reason, and writes what it ran
# and saw to $OUT/<step>.txt. browser.mjs must already run. "leave" removes
# the node, so it comes last.
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
set -uo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
IMAGE="${IMAGE:-ghcr.io/se-wo/tabernetes:latest}"
# The flags of web/src/join.js, without --pull always.
NODE_FLAGS="${NODE_FLAGS:---memory 2g --pids-limit 4096 --security-opt seccomp=unconfined --security-opt systempaths=unconfined --security-opt apparmor=unconfined --device /dev/net/tun}"
IMPORT_IMAGES="${IMPORT_IMAGES:-busybox:latest rancher/local-path-provisioner:v0.0.31}"
OUT="${OUT:-$PWD/out}"
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

step_leave() {
	run docker rm -f "$N"
	wait_for 'kubectl get node node-1' 'NotReady' 120
	# The pods of a lost node go after the 300 s toleration for not-ready.
	if wait_for 'kubectl get pods -n kube-system -l k8s-app=kube-dns -o wide' '1/1 +Running .* tab ' 600; then
		k get pods -n kube-system -o wide
		result PASS "CoreDNS back on tab"
	else
		result FAIL "CoreDNS not back on tab after 10 minutes"
	fi
}

steps=("$@")
[ ${#steps[@]} -gt 0 ] || steps=(images join apply coredns storage metrics ingress sanity leave)
for step in "${steps[@]}"; do
	: >"$OUT/$step.txt"
	"step_$step"
done
