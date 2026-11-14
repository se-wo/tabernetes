#!/bin/bash
# checks.sh: phase 0, items 7, 8, 14, 17 and 18 on a joined Docker node.
# Run ../run.sh docker; it calls this script after the images are built.
#
# Needs: browser.mjs and the site of ../../docker-node-check.md, a Docker node
# that joined with ../../docker-node-check/check.sh join, and the images
# poc/*:1 in the host's Docker.
#
# Env: N (the container of the node, default tabernetes-node-1), NODE (its
# node name, default node-1).
set -uo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
tabjs="$here/../../docker-node-check/tab.mjs"
N="${N:-tabernetes-node-1}"
NODE="${NODE:-node-1}"
tab() { node "$tabjs" run "$1" 2>&1; }

echo "== item 18: import the test images into the node"
docker cp "$(command -v ctr)" "$N:/usr/local/bin/ctr"
docker save poc/cmd:1 poc/exitcode:1 poc/cmdhttp:1 poc/core:1 poc/proxy:1 poc/echo:1 |
	docker exec -i "$N" ctr -n k8s.io images import - 2>&1 | grep -v DEPRECATION | tail -1

# Objects of an earlier run go first; apply would only change them.
tab 'kubectl delete networkpolicy proxy-egress --ignore-not-found; kubectl delete svc proxy echo-tcp echo-udp --ignore-not-found; kubectl delete pods -l poc=wasi --wait=true' >/dev/null
tab "kubectl apply -f - <<'EOF'
$(sed "s/hostname: node-1/hostname: $NODE/" "$here/manifests.yaml")
EOF" >/dev/null
for _ in $(seq 60); do
	tab 'kubectl get pod client' | grep -q Running && break
	sleep 5
done
if ! tab 'kubectl get pod client' | grep -q Running; then
	echo "the pod client does not run: no checks"
	tab 'kubectl describe pod client | tail -20; kubectl -n kube-flannel get pods -o wide'
	tab "kubectl -n kube-flannel logs --tail 20 \$(kubectl -n kube-flannel get pods --field-selector spec.nodeName=$NODE -o name)"
	docker exec "$N" crictl pods 2>&1 | head -20
	docker exec "$N" sh -c 'tail -40 /var/log/kubelet.log' 2>&1
	docker logs "$N" 2>&1 | tail -40
	exit 1
fi
sleep 20 # the command components end
tab 'kubectl get pods -l poc=wasi -o custom-columns=NAME:.metadata.name,PHASE:.status.phase,RESTARTS:.status.containerStatuses[0].restartCount,EXIT:.status.containerStatuses[0].state.terminated.exitCode,LAST:.status.containerStatuses[0].lastState.terminated.exitCode'
echo "== NL1 and CO3"
tab 'kubectl logs lookup | grep ^lookup; kubectl logs lookup-fqdn | grep ^lookup; kubectl logs core-go'

# kubectl exec needs the serving certificate of the kubelet; crictl exec
# does not. Without CAP_SYS_ADMIN the agent runs containerd in namespaces of
# its own (node/agent/userns.go), and the stream server of crictl exec
# listens on 127.0.0.1 in there: crictl runs in the namespaces of containerd.
# shellcheck disable=SC2016 # for the shell in the node
ns="$(docker exec "$N" sh -c 'for p in /proc/[0-9]*; do
	[ "$(cat "$p/comm")" = containerd ] || continue
	f="-n -m"
	[ "$(readlink "$p/ns/user")" = "$(readlink /proc/self/ns/user)" ] || f="-U --preserve-credentials $f"
	echo "-t ${p#/proc/} $f"
	break
done')"
# shellcheck disable=SC2086 # ns is a list of flags
crictl() { docker exec "$N" nsenter $ns crictl "$@"; }
client=""
for _ in $(seq 60); do
	client="$(crictl ps -q --label io.kubernetes.pod.name=client | head -1)"
	[ -n "$client" ] && break
	sleep 5
done
x() { crictl exec "$client" sh -c "$1" 2>&1; }
if ! x true >/dev/null; then
	echo "crictl exec in the pod client fails: no checks"
	x true
	exit 1
fi
echo "== item 8: the echo images through their Services"
x 'echo tcp | nc -w 2 echo-tcp 7'
x 'echo udp | nc -u -w 2 echo-udp 7'
echo "== item 7: client requests"
x 'mkdir -p /www && echo ok >/www/x && httpd -p 8000 -h /www'
ip="$(x 'hostname -i' | tr -d ' \r\n')"
x "wget -qO- -T 30 proxy:8080/fetch/$ip:8000/x"
x 'wget -qO- -T 30 proxy:8080/fetch/nothere.invalid/x'
x 'wget -qO- -T 30 proxy:8080/fetch/echo-tcp.default.svc.cluster.local:9/x'
echo "== item 7, PX4: [::]:8080 on the IPv4 pod IP"
ip6="$(tab 'kubectl get pod proxy6 -o jsonpath={.status.podIP}' | tr -d ' \r\n')"
x "wget -qO- -T 5 $ip6:8080/context | head -3; echo rc=\$?"
tab 'kubectl logs proxy6 --tail 5'
echo "== item 14: trap, error code, no response"
status() { tab "kubectl get pod proxy -o custom-columns=RESTARTS:.status.containerStatuses[0].restartCount,LAST:.status.containerStatuses[0].lastState.terminated.exitCode,REASON:.status.containerStatuses[0].lastState.terminated.reason --no-headers"; }
for p in trap error noresponse; do
	echo "-- /$p: $(x "wget -qO- -T 5 proxy:8080/$p; echo rc=\$?" | tr '\n' ' ')"
	sleep 5
	status
done
x 'wget -qO- -T 5 proxy:8080/context | grep REQUEST_ID'
echo "== item 17: a client request that NetworkPolicy denies"
tab "kubectl apply -f - <<'EOF'
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: proxy-egress}
spec:
  podSelector: {matchLabels: {app: proxy}}
  policyTypes: [Egress]
  egress:
    - ports: [{port: 53, protocol: UDP}, {port: 53, protocol: TCP}]
EOF" >/dev/null
sleep 5
start=$SECONDS
echo "$(x "wget -qO- -T 700 proxy:8080/fetch/$ip:8000/x; echo rc=\$?" | tr '\n' ' ') after $((SECONDS - start)) s"
tab 'kubectl delete networkpolicy proxy-egress' >/dev/null
