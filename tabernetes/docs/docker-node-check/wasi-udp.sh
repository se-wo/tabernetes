#!/bin/bash
# shellcheck disable=SC2154 # here, step, N, OUT and crictl_ns come from check.sh
# wasi-udp.sh: the step wasi-udp of check.sh. check.sh sources it.
#
# Phase 4 of ../wasi-sockets.md: the tests of section 10.3 for UDP in a pod
# and on one node (UD) with "Docker" in the column "Nodes", on the real
# runwasi of node-1. runwasi is the reference for the tab node: the
# end-to-end tests of phase 4 use the results of this step as fixed values.
# The objects are in wasi-udp.yaml, the UDP modes of wasi-test-echo in
# wasm-images/rust/echo/src/main.rs. One PASS, FAIL or SKIP line for each
# row of section 10.3:
#
#   UD2, UD7, UD15     the echo server through its Service; the answer
#                      comes from the ClusterIP and the port of the Service
#   UD8, UD9           a connected client to a port without a socket
#   UD10, UD11         a connected client to a Service without endpoints
#   UD12               a client to a ClusterIP port without a Service
#   UD13               the echo pod sends to its own Service
#   UD14               a datagram of 65,508 bytes
#   UD20, UD21         a server on 127.0.0.1: a client in the same pod, a
#                      connected client in another pod
#   ND                 SKIP: phase 5 (package W5.3)
#
# The step also records the facts of package W4.5a without a result line
# ("facts: ..."; also on the standard output): the lines of the modes
# udp-errors and udp-options, the queue of UD5 and UD6, busybox nc -u, a
# client without connect and two ClusterIPs with the same endpoint.
#
# The helpers so_logs and so_exec are in wasi-sockets.sh, check and pod_ip in
# wasi-proxy.sh.

# udp_exec <command line>: runs it in the busybox pod wasi-udp-client,
# records it and prints the output.
udp_exec() {
	local out
	out="$(pod_exec wasi-udp-client "$1" 2>&1 | tr -d '\r')"
	printf '$ [wasi-udp-client] %s\n%s\n' "$1" "$out" >>"$OUT/$step.txt"
	printf '%s\n' "$out"
}

# udp_fact <text>: records a fact and prints it.
udp_fact() {
	echo "facts: $1" | tee -a "$OUT/$step.txt"
}

# udp_lines <text>: the lines of a log as one line.
udp_lines() {
	tr '\n' ';' <<<"$1" | sed 's/;$//'
}

# udp_refused <log>: 0 if a connected client got connection-refused at least
# one time and no answer.
udp_refused() {
	grep -q '^error [a-z-]*: connection-refused$' <<<"$1" && ! grep -q '^answer ' <<<"$1"
}

step_wasi_udp() {
	wasi_images_ready || return 0
	local p out a b s ip cip ok
	node_ns
	apply_file "$here/wasi-udp.yaml"
	for p in wasi-udp-client wasi-echo-udp wasi-udp-lo; do
		if ! wait_for "kubectl get pod $p" 'Running' 240; then
			k get pods -l wasi-check=udp -o wide
			result FAIL "the pods of wasi-udp do not run ($p)"
			return 1
		fi
	done
	# The clients wait 10 seconds (DELAY_MS) and then need approximately 5
	# seconds.
	for p in wasi-udp-clients wasi-udp-facts wasi-udp-queue; do
		wait_for "kubectl get pod $p -o jsonpath={.status.phase}" 'Succeeded|Failed' 120 >/dev/null
	done
	wait_for 'kubectl get pod wasi-udp-lo -o jsonpath={.status.containerStatuses[*].state.terminated.reason}' 'Completed|Error' 60
	k get pods -l wasi-check=udp -o wide
	cip="$(tab run "kubectl get service wasi-echo-udp -o jsonpath='{.spec.clusterIP}'" | tr -d ' \r\n')"
	ip="$(pod_ip wasi-echo-udp)"

	# UD2, UD7, UD15: busybox nc and the component, connected and not
	# connected. The answers come from the ClusterIP and port 7.
	out="$(udp_exec 'echo udp | nc -u -w 2 wasi-echo-udp 7; echo rc=$?')"
	a="$(so_logs wasi-udp-clients svc)"
	b="$(so_logs wasi-udp-clients unc)"
	s="$(so_logs wasi-echo-udp c)"
	ok=0
	[ "$(head -1 <<<"$out")" = "echo udp" ] || ok=1
	grep -qx "peer $cip:7" <<<"$a" || ok=1
	[ "$(grep -cx "answer echo svc from $cip:7" <<<"$a")" = 2 ] || ok=1
	grep -qx "answer echo unc from $cip:7" <<<"$b" || ok=1
	grep -qx 'udp echo on 0.0.0.0:7' <<<"$s" || ok=1
	check "UD2, UD7, UD15" "$ok" "nc -u through the Service gives '$(head -1 <<<"$out")' (want echo udp); connected client: $(udp_lines "$(grep -E '^(local|peer|answer|no answer|error)' <<<"$a")"); client without connect: $(udp_lines "$(grep -E '^(local|answer|no answer|error)' <<<"$b")") (want the answers from the ClusterIP $cip:7)"
	udp_fact "the server sees $(udp_lines "$(grep '^udp [0-9]* bytes from' <<<"$s" | head -4)"); the client pod IP is $(pod_ip wasi-udp-clients)"

	# UD8, UD9: the pod IP of the echo pod, port 9. A client without connect
	# gets no error (fact 3).
	a="$(so_logs wasi-udp-clients noport)"
	b="$(so_logs wasi-udp-clients noport-unc)"
	ok=0
	udp_refused "$a" || ok=1
	grep -q '^error ' <<<"$b" && ok=1
	check "UD8, UD9" "$ok" "a connected client to $ip:9: $(udp_lines "$(grep -E '^(peer|answer|no answer|error)' <<<"$a")") (want connection-refused); without connect: $(udp_lines "$(grep -E '^(answer|no answer|error)' <<<"$b")") (want no error)"

	# UD10, UD11: kube-proxy rejects a Service without endpoints.
	a="$(so_logs wasi-udp-clients noep)"
	ok=0
	udp_refused "$a" || ok=1
	check "UD10, UD11" "$ok" "a connected client to the Service wasi-udp-none: $(udp_lines "$(grep -E '^(peer|answer|no answer|error)' <<<"$a")") (want connection-refused)"

	# How kube-proxy rejects a Service without endpoints (UD10, UD11).
	# shellcheck disable=SC2016,SC2086 # for the shell in the node; crictl_ns is a list of flags
	udp_fact "kube-proxy: mode '$(tab run "kubectl -n kube-system get configmap kube-proxy -o jsonpath='{.data.config\.conf}'" 2>&1 | tr -d '\r' | sed -n 's/^mode: *//p')', rules for wasi-udp-none: $(udp_lines "$(docker exec "$N" nsenter $crictl_ns sh -c 'iptables-save 2>/dev/null | grep "wasi-udp-none"; nft list ruleset 2>/dev/null | grep -i "wasi-udp-none"' 2>&1 | head -6)")"

	# UD12: no rule of kube-proxy has the ClusterIP with port 9. The result
	# depends on the host, as for TCP (CR7 in section 9.2).
	a="$(so_logs wasi-udp-clients nosvc)"
	if grep -q '^answer ' <<<"$a" || grep -q '^error [a-z-]*: [^c]' <<<"$a"; then
		check UD12 1 "a connected client to $cip:9: $(udp_lines "$(grep -E '^(peer|answer|no answer|error)' <<<"$a")") (want no answer or connection-refused)"
	elif udp_refused "$a"; then
		check UD12 0 "a connected client to $cip:9 gets connection-refused (this host): $(udp_lines "$(grep -E '^(peer|no answer|error)' <<<"$a")")"
	else
		ok=0
		grep -q '^no answer$' <<<"$a" || ok=1
		check UD12 "$ok" "a connected client to $cip:9 gets no answer and no error (this host): $(udp_lines "$(grep -E '^(peer|no answer|error)' <<<"$a")")"
	fi

	# UD13: the container "self" of the echo pod sends to its own Service.
	a="$(so_logs wasi-echo-udp self)"
	ok=0
	grep -qx "answer echo self from $cip:7" <<<"$a" || ok=1
	check UD13 "$ok" "the echo pod to its own Service: $(udp_lines "$(grep -E '^(local|peer|answer|no answer|error)' <<<"$a")") (want answer echo self from $cip:7)"

	# UD14 and the facts of rules 2 to 7.
	out="$(so_logs wasi-udp-facts errors)"
	ok=0
	grep -qx 'rule5 send 65507: ok' <<<"$out" || ok=1
	grep -qx 'rule5 send 65508: datagram-too-large' <<<"$out" || ok=1
	grep -qx 'rule5 server received: 65507 5' <<<"$out" || ok=1
	check UD14 "$ok" "$(udp_lines "$(grep '^rule5 ' <<<"$out")") (want 65507 ok, 65508 datagram-too-large)"
	while read -r s; do udp_fact "$s"; done < <(grep -v '^rule5 ' <<<"$out")
	while read -r s; do udp_fact "$s"; done < <(so_logs wasi-udp-facts options)

	# UD20, UD21: the server on 127.0.0.1:7.
	s="$(so_logs wasi-udp-lo c)"
	a="$(so_logs wasi-udp-lo client)"
	b="$(so_logs wasi-udp-clients lo)"
	ok=0
	grep -qx 'udp echo on 127.0.0.1:7' <<<"$s" || ok=1
	grep -qx 'answer echo lo from 127.0.0.1:7' <<<"$a" || ok=1
	grep -qx 'local 0.0.0.0:[0-9]*' <<<"$a" || ok=1
	udp_refused "$b" || ok=1
	check "UD20, UD21" "$ok" "server: '$(head -1 <<<"$s")'; client in the pod: $(udp_lines "$(grep -E '^(local|answer|no answer|error)' <<<"$a")"); the server sees '$(grep '^udp [0-9]* bytes from' <<<"$s" | head -1)'; a connected client in another pod to $(pod_ip wasi-udp-lo):7: $(udp_lines "$(grep -E '^(answer|no answer|error)' <<<"$b")") (want connection-refused)"

	# UD5, UD6 on Linux: the number for section 11.
	udp_fact "queue: $(udp_lines "$(so_logs wasi-udp-queue server)"); flood: $(udp_lines "$(so_logs wasi-udp-queue flood)")"

	# Fact 11: one socket, two ClusterIPs with the same endpoint.
	a="$(so_logs wasi-udp-clients clash)"
	udp_fact "two ClusterIPs ($cip, $(tab run "kubectl get service wasi-echo-udp2 -o jsonpath='{.spec.clusterIP}'" | tr -d ' \r\n')) with one endpoint: $(udp_lines "$(grep -E '^(local|answer|no answer|error)' <<<"$a")")"

	# Fact 9: the texts and exit codes of busybox nc -u.
	udp_fact "nc -u answer: $(udp_lines "$(udp_exec 'echo udp | nc -u -w 2 wasi-echo-udp 7; echo rc=$?')")"
	udp_fact "nc -u no answer (192.0.2.1:7): $(udp_lines "$(udp_exec 'echo x | nc -u -w 2 192.0.2.1 7; echo rc=$?')")"
	udp_fact "nc -u refused ($ip:9): $(udp_lines "$(udp_exec "echo x | nc -u -w 2 $ip 9; echo rc=\$?")")"
	udp_fact "nc -u -v refused ($ip:9): $(udp_lines "$(udp_exec "echo x | nc -u -v -w 2 $ip 9; echo rc=\$?")")"
	udp_fact "nc -u no endpoints (wasi-udp-none:7): $(udp_lines "$(udp_exec 'echo x | nc -u -w 2 wasi-udp-none 7; echo rc=$?')")"
	udp_fact "nc -u two lines, one answer each: $(udp_lines "$(udp_exec 'printf "a\nb\n" | nc -u -w 2 wasi-echo-udp 7; echo rc=$?')")"

	result SKIP "ND rows (UDP between nodes): phase 5, package W5.3"

	k get pods -l wasi-check=udp -o wide
	k delete pods -l wasi-check=udp --wait=false
}
