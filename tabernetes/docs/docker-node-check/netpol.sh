#!/bin/bash
# shellcheck disable=SC2154 # here, step, N, OUT and crictl_ns come from check.sh
# netpol.sh: the step netpol of check.sh. check.sh sources it.
#
# Phase 0 of ../networking-policy.md and decision 16 of ../wasi-sockets.md:
# does kube-network-policies (nfqueue) run on the rootless Docker node, and
# does it deny? One PASS, FAIL or SKIP line for each item:
#
#   kernel    nft_queue and nfnetlink_queue loaded or built in (SKIP if one
#             is missing)
#   pod       the pod of kube-network-policies on node-1 is Ready and does
#             not restart (SKIP if not: the answer of phase 0 is a fact)
#   ruleset   a rule with "queue" to the queue 98 in the network namespace
#             of the node (FAIL only if the pod is Ready)
#   allow     a client connects to a server on node-1 without a policy
#   deny      the same client does not connect within 10 seconds after a
#             NetworkPolicy that denies ingress to the server; SKIP if
#             kube-network-policies does not run
#
# The facts are lines "facts: ..." (also on the standard output). The objects
# are in netpol.yaml, in the namespace netpol-check, which the step deletes at
# the end.

# netpol_fact <text>: records a fact and prints it.
netpol_fact() {
	echo "facts: $1" | tee -a "$OUT/$step.txt"
}

# netpol_mods <command prefix...>: the state of nft_queue and
# nfnetlink_queue as "name=loaded|builtin|missing", for this host or, with
# "docker exec $N", for the node.
netpol_mods() {
	local m s out=""
	for m in nft_queue nfnetlink_queue; do
		# shellcheck disable=SC2016 # for the shell of the target
		s="$("$@" sh -c 'm=$1
			if grep -q "^$m " /proc/modules 2>/dev/null; then echo loaded
			elif grep -q "/$m\.ko" "/lib/modules/$(uname -r)/modules.builtin" 2>/dev/null; then echo builtin
			elif [ -d "/sys/module/$m" ]; then echo builtin
			else echo missing; fi' sh "$m" 2>/dev/null)"
		out="$out $m=${s:-unknown}"
	done
	echo "${out# }"
}

# netpol_connect: one connection of the client to the server, "ok" on
# success.
netpol_connect() {
	local out
	out="$(pod_exec netpol-client "echo hi | nc -w 2 $1 8080" 2>&1 | tr -d '\r')"
	printf '$ [netpol-client] nc -w 2 %s 8080\n%s\n' "$1" "$out" >>"$OUT/$step.txt"
	grep -qx ok <<<"$out"
}

step_netpol() {
	local host node pod line restarts restarts2 ruleset running=1 ip start t="" why=""
	node_ns

	# Kernel.
	host="$(netpol_mods)"
	node="$(netpol_mods docker exec "$N")"
	netpol_fact "kernel host $(uname -r): $host"
	netpol_fact "kernel node $(docker exec "$N" uname -r 2>&1): $node"
	if [[ "$host $node" == *missing* ]]; then
		why="no nft_queue or nfnetlink_queue in the kernel"
		result SKIP "kernel: not all of nft_queue and nfnetlink_queue are loaded or built in ($host)"
	else
		result PASS "kernel: $host"
	fi

	# The pod of kube-network-policies.
	if wait_for 'kubectl get pods -n kube-system -l app=kube-network-policies -o wide' '1/1 +Running .* node-1' 120; then
		# A pod that cannot bind its queue exits after the start: look again.
		line="$(tab run 'kubectl get pods -n kube-system -l app=kube-network-policies -o wide --no-headers' | tr -d '\r' | grep ' node-1 ' | head -1)"
		restarts="$(awk '{print $4}' <<<"$line")"
		sleep 20
		line="$(tab run 'kubectl get pods -n kube-system -l app=kube-network-policies -o wide --no-headers' | tr -d '\r' | grep ' node-1 ' | head -1)"
		restarts2="$(awk '{print $4}' <<<"$line")"
		[[ "$line" =~ 1/1\ +Running ]] && [ "$restarts" = "$restarts2" ] || running=0
	else
		running=0
		line="$(tab run 'kubectl get pods -n kube-system -l app=kube-network-policies -o wide --no-headers' | tr -d '\r' | grep ' node-1 ' | head -1)"
	fi
	pod="$(awk '{print $1}' <<<"$line")"
	netpol_fact "kube-network-policies on node-1: ${line:-no pod}"
	if [ -n "$pod" ]; then
		netpol_fact "log: $(tab run "kubectl logs -n kube-system $pod --tail 8" 2>&1 | tr -d '\r' | tr '\n' ';' | sed 's/;$//')"
		if [ "$(awk '{print $4}' <<<"$line")" != 0 ]; then
			netpol_fact "previous log: $(tab run "kubectl logs -n kube-system $pod --previous --tail 8" 2>&1 | tr -d '\r' | tr '\n' ';' | sed 's/;$//')"
		fi
	fi
	if [ "$running" = 1 ]; then
		result PASS "pod: kube-network-policies is Ready on node-1 ($pod, restarts ${restarts2:-?})"
	else
		# Not FAIL: the result of phase 0 is a fact (decision 16).
		why="kube-network-policies is not Ready${why:+: $why}"
		result SKIP "pod: $why (${line:-no pod})"
	fi

	# The ruleset in the network namespace of the node (hostNetwork pods).
	# shellcheck disable=SC2086 # crictl_ns is a list of flags
	ruleset="$(docker exec "$N" nsenter $crictl_ns nft list ruleset 2>&1)"
	printf '$ nft list ruleset\n%s\n' "$ruleset" >>"$OUT/$step.txt"
	if grep -Eq 'queue.*(num|to) 98' <<<"$ruleset"; then
		netpol_fact "ruleset: $(grep -Em3 'queue.*(num|to) 98' <<<"$ruleset" | sed 's/^[[:space:]]*//' | tr '\n' ';' | sed 's/;$//')"
		result PASS "ruleset: a rule with queue 98 in the network namespace of the node"
	else
		netpol_fact "ruleset: no rule with queue 98; tables: $(grep '^table' <<<"$ruleset" | tr '\n' ';' | sed 's/;$//')"
		if [ "$running" = 1 ]; then
			result FAIL "ruleset: kube-network-policies is Ready, but no rule with queue 98 is in the network namespace of the node"
			why="no rule with queue 98"
		else
			result SKIP "ruleset: no rule with queue 98 ($why)"
		fi
		running=0
	fi

	# The deny test.
	apply_file "$here/netpol.yaml"
	if ! wait_for 'kubectl get pod -n netpol-check netpol-server' 'Running' 120 ||
		! wait_for 'kubectl get pod -n netpol-check netpol-client' 'Running' 120; then
		k get pods -n netpol-check -o wide
		result FAIL "allow: the pods of netpol-check do not run"
		k delete namespace netpol-check --wait=false
		return 1
	fi
	ip="$(tab run "kubectl get pod -n netpol-check netpol-server -o jsonpath='{.status.podIP}'" | tr -d ' \r\n')"
	if netpol_connect "$ip" || netpol_connect "$ip"; then
		result PASS "allow: netpol-client connects to netpol-server $ip:8080 without a policy"
	else
		result FAIL "allow: netpol-client does not connect to netpol-server $ip:8080 without a policy"
	fi
	if [ "$running" != 1 ]; then
		result SKIP "deny: ${why:-kube-network-policies does not run on node-1} (see the facts)"
		k delete namespace netpol-check --wait=false
		return 0
	fi
	run tab run "kubectl apply -f - <<'EOF'
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: netpol-deny, namespace: netpol-check}
spec:
  podSelector: {matchLabels: {app: netpol-server}}
  policyTypes: [Ingress]
EOF"
	start=$SECONDS
	while ((SECONDS - start < 10)); do
		if ! netpol_connect "$ip"; then
			t=$((SECONDS - start))
			break
		fi
		sleep 1
	done
	if [ -n "$t" ] && ! netpol_connect "$ip"; then
		netpol_fact "the first denied connection after the policy: ${t} s (each try waits 2 s)"
		result PASS "deny: netpol-client does not connect after the NetworkPolicy netpol-deny (after ${t} s)"
	else
		result FAIL "deny: netpol-client still connects 10 s after the NetworkPolicy netpol-deny"
	fi
	k delete namespace netpol-check --wait=false
}
