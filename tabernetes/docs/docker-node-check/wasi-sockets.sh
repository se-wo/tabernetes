#!/bin/bash
# shellcheck disable=SC2154 # here, step, N, OUT and crictl_ns come from check.sh
# wasi-sockets.sh: the step wasi-sockets of check.sh. check.sh sources it.
#
# Phase 3 of ../wasi-sockets.md: the tests of section 10.3 for TCP (TC), name
# lookup (NL) and PX5 with "Docker" in the column "Nodes", on the real runwasi
# of node-1. runwasi is the reference for the tab node: the end-to-end tests
# of phase 3 use the results of this step as fixed values. The objects are in
# wasi-sockets.yaml, the modes of wasi-test-echo in
# wasm-images/rust/echo/src/main.rs. One PASS, FAIL or SKIP line for each row
# of section 10.3:
#
#   TC1, TC8                        the echo server through a Service
#   TC2, TC3, TC4, TC5, TC17        a server on 127.0.0.1: a client in the
#                                   same pod, a client in another pod
#   TC15, TC16                      a tcpSocket probe, kubectl port-forward
#   TC6, TC7                        a listener on [::]; SKIP without IPv6
#   TC9                             the ephemeral port of a client
#   TC10                            SKIP: deferred (decision 16)
#   TC11                            a client to a port without a listener
#   TC13                            a client that closes its send direction
#   TC18, TC19, TC20, TC21          the error codes of fact 12
#   TC22                            the socket options
#   NL1, NL2                        the name of a Service
#   PX5                             a proxy component on 127.0.0.1:9000
#
# The step also records facts for the tab node without a result line: the
# error code of an IPv6 connect and of a full backlog (rule 5 of section
# 8.3.2), the addresses of localhost (rule 11), the buffer sizes (TC22) and
# the poll mode (CO17, CO19).
#
# The helpers check, pod_ip and px_get are in wasi-proxy.sh.

# so_logs <pod> [container]: kubectl logs, recorded, without carriage returns.
so_logs() {
	local out
	out="$(tab run "kubectl logs $1${2:+ -c $2}" 2>&1 | tr -d '\r')"
	printf '$ kubectl logs %s%s\n%s\n' "$1" "${2:+ -c $2}" "$out" >>"$OUT/$step.txt"
	printf '%s\n' "$out"
}

# so_exec <command line>: runs it in the busybox pod wasi-sockets-client,
# records it and prints the output.
so_exec() {
	local out
	out="$(pod_exec wasi-sockets-client "$1" 2>&1 | tr -d '\r')"
	printf '$ [wasi-sockets-client] %s\n%s\n' "$1" "$out" >>"$OUT/$step.txt"
	printf '%s\n' "$out"
}

# so_ready <pod>: "<container>=<ready> ..." of the pod.
so_ready() {
	tab run "kubectl get pod $1 -o jsonpath='{range .status.containerStatuses[*]}{.name}={.ready} {end}'" 2>&1 | tail -1 | tr -d '\r' | sed 's/ $//'
}

# so_dns_on_node: waits until each endpoint of the Service kube-dns is ready
# on node-1. The CoreDNS of the tab answers only DNS over TCP. After the
# join, the tab node evicts its CoreDNS pods (handover, tabnode_coredns.go),
# and until then the endpoints are on tab and on node-1. A pod of node-1 then
# sends some UDP queries to the tab, gets no answer and the lookup fails;
# wasmtime gives name-unresolvable for each failure of getaddrinfo. A name
# with fewer dots than ndots:5 goes through the search list, one query for
# each search domain, so the full name of a Service fails more often than
# its short name.
so_dns_on_node() {
	local end=$((SECONDS + 300)) s=""
	while ((SECONDS < end)); do
		s="$(tab run "kubectl get endpointslices -n kube-system -l kubernetes.io/service-name=kube-dns -o jsonpath='{range .items[*].endpoints[*]}{.nodeName}={.conditions.ready} {end}'" 2>&1 | tail -1 | tr -d '\r')"
		if [ -n "$s" ] && ! tr ' ' '\n' <<<"$s" | sed '/^$/d' | grep -qvx 'node-1=true'; then
			echo "kube-dns endpoints: $s" >>"$OUT/$step.txt"
			return 0
		fi
		sleep 5
	done
	echo "kube-dns endpoints after 300 s: $s" >>"$OUT/$step.txt"
	k get pods -n kube-system -l k8s-app=kube-dns -o wide
	return 1
}

step_wasi_sockets() {
	wasi_images_ready || return 0
	local c=wasi-sockets-client p out a b s ip ok lport
	node_ns
	if ! so_dns_on_node; then
		result FAIL "CoreDNS does not run only on node-1 after 300 s: pods of node-1 lose DNS queries to the CoreDNS of the tab (DNS over TCP only)"
		return 1
	fi
	apply_file "$here/wasi-sockets.yaml"
	for p in "$c" wasi-echo-tcp wasi-echo-eof wasi-sockets-errors wasi-echo-poll wasi-proxy-lo; do
		if ! wait_for "kubectl get pod $p" 'Running' 240; then
			k get pods -l wasi-check=sockets -o wide
			result FAIL "the pods of wasi-sockets do not run ($p)"
			return 1
		fi
	done
	# The client containers wait 5 and 8 seconds (DELAY_MS).
	for p in wasi-echo-lo wasi-echo-client wasi-sockets-options wasi-lookup wasi-lookup-fqdn wasi-lookup-localhost; do
		wait_for "kubectl get pod $p" 'Completed|Error|Running' 120 >/dev/null
	done
	wait_for 'kubectl get pod wasi-echo-client' 'Completed|Error' 60
	wait_for 'kubectl get pod wasi-echo-lo -o jsonpath={.status.containerStatuses[*].state.terminated.reason}' 'Completed|Error' 60
	k get pods -l wasi-check=sockets -o wide

	# TC1, TC8
	out="$(so_exec 'echo tcp | nc -w 2 wasi-echo-tcp 7')"
	a="$(so_ready wasi-echo-tcp)"
	ok=0
	[ "$out" = "echo tcp" ] && [ "$a" = c=true ] || ok=1
	check "TC1, TC8" "$ok" "nc through the Service wasi-echo-tcp gives '$out' (want echo tcp); the tcpSocket probe on 0.0.0.0:7 gives $a (want c=true)"

	# TC2, TC3, TC4, TC5, TC17. busybox nc writes no text for a refused
	# connection; busybox wget writes "can't connect ... Connection refused"
	# (and "download timed out" for a timeout).
	ip="$(pod_ip wasi-echo-lo)"
	a="$(so_logs wasi-echo-lo client)"
	s="$(so_logs wasi-echo-lo c)"
	b="$(so_exec "wget -qO- -T 3 $ip:7; echo rc=\$?")"
	lport="$(sed -n 's/^local 127\.0\.0\.1:\([0-9]*\) peer 127\.0\.0\.1:7$/\1/p' <<<"$a")"
	ok=0
	grep -qx 'tcp echo on 127.0.0.1:7' <<<"$s" || ok=1
	[ -n "$lport" ] || ok=1
	grep -qx "tcp from Ok(127.0.0.1:$lport) to Ok(127.0.0.1:7)" <<<"$s" || ok=1
	grep -qx 'answer echo lo' <<<"$a" || ok=1
	grep -q 'Connection refused' <<<"$b" || ok=1
	check "TC2, TC3, TC4, TC5, TC17" "$ok" "server: '$(head -1 <<<"$s")' (want tcp echo on 127.0.0.1:7); client in the pod: '$(grep '^local' <<<"$a")', '$(grep '^answer' <<<"$a")'; server sees '$(grep '^tcp from' <<<"$s" | head -1)' (want 127.0.0.1 at the two ends); another pod to $ip:7: '$(head -1 <<<"$b")' (want Connection refused)"

	# TC9: the client without a bind, in its own pod and in the pod of the
	# server.
	a="$(so_logs wasi-echo-client c)"
	b="$(sed -n 's/^local [0-9.]*:\([0-9]*\) peer .*/\1/p' <<<"$a")"
	ok=1
	if [[ "$b $lport" =~ ^[0-9]+\ [0-9]+$ ]] && ((b >= 32768 && b <= 60999 && lport >= 32768 && lport <= 60999)); then ok=0; fi
	check TC9 "$ok" "the local ports are '$b' (another pod) and '$lport' (127.0.0.1) (want 32768 to 60999)"

	result SKIP "TC10: NetworkPolicy on a Docker node is a deferred test (nfqueue, decision 16)"

	# TC11
	b="$(so_logs wasi-echo-client refused)"
	ok=0
	[ "$b" = "error connect: connection-refused" ] || ok=1
	check TC11 "$ok" "a client to 127.0.0.1:9 gives '$b' (want error connect: connection-refused)"

	# TC13, NL1: the client closes its send direction, the server reads the
	# end of the stream and answers.
	s="$(so_logs wasi-echo-eof)"
	ok=0
	grep -qx 'shutdown send: ok' <<<"$a" || ok=1
	grep -qx 'answer echo tc13' <<<"$a" || ok=1
	grep -qx 'eof' <<<"$a" || ok=1
	grep -qx 'tcp eof after 5 bytes: Ok(5)' <<<"$s" || ok=1
	check TC13 "$ok" "client: $(grep -E '^(shutdown|answer|eof)' <<<"$a" | tr '\n' ';'); server: '$(grep '^tcp eof' <<<"$s")' (want shutdown send: ok, answer echo tc13, eof; tcp eof after 5 bytes)"

	# TC15: the probe on 127.0.0.1:7 fails. TC16: port-forward connects in the
	# network namespace of the pod. The job ends with kill.
	a="$(so_ready wasi-echo-lo)"
	s="$(tab run 'kubectl get events --field-selector involvedObject.name=wasi-echo-lo' 2>&1 | grep -c 'Readiness probe failed')"
	out="$(tab run 'kubectl port-forward pod/wasi-echo-lo 17007:7 & sleep 3; echo pf | nc -w 3 127.0.0.1 17007; kill %%' 2>&1 | tr -d '\r')"
	printf '$ kubectl port-forward pod/wasi-echo-lo 17007:7 & ...\n%s\n' "$out" >>"$OUT/$step.txt"
	b="$(tab run 'jobs' 2>&1 | grep -c port-forward)"
	ok=0
	[ "${a%% *}" = c=false ] && [ "$s" -gt 0 ] && grep -qx 'echo pf' <<<"$out" && [ "$b" = 0 ] || ok=1
	check "TC15, TC16" "$ok" "the probe on 127.0.0.1:7 gives ${a%% *} with $s failure events (want c=false and events); port-forward gives '$(grep -x 'echo pf' <<<"$out")' (want echo pf); port-forward jobs after kill: $b (want 0)"

	# TC6, TC7: the GitHub runner has IPv6; a host without IPv6 cannot
	# create the socket.
	out="$(so_logs wasi-sockets-errors)"
	s="$(grep '^tc6 ' <<<"$out" | head -1)"
	if grep -q '^tc6 create ipv6: ' <<<"$s"; then
		result SKIP "TC6, TC7: the host has no IPv6 ('$s')"
	else
		b="$(so_exec "wget -qO- -T 3 $(pod_ip wasi-sockets-errors):7006; echo rc=\$?")"
		ok=0
		[ "$s" = 'tc6 bind [::]:7006: ok listen: ok' ] && grep -q 'Connection refused' <<<"$b" || ok=1
		check "TC6, TC7" "$ok" "'$s' (want ok ok); another pod to the IPv4 pod IP on port 7006: '$(head -1 <<<"$b")' (want Connection refused)"
	fi

	# TC18 to TC21. A second bind without a listener succeeds: wasmtime sets
	# SO_REUSEADDR (rule 3 of section 8.3.2). TC20 needs IPv6.
	ok=0
	for s in 'tc18 bind1 0.0.0.0:7100: ok' 'tc18 bind2 without listen: ok' 'tc18 listen1: ok' 'tc18 bind3 with listen: address-in-use' \
		'tc19 bind 192.0.2.7: address-not-bindable' 'tc21 backlog 0: invalid-argument'; do
		grep -qxF "$s" <<<"$out" || ok=1
	done
	if ! grep -q '^tc20 create ipv6: ' <<<"$out"; then
		grep -qxF 'tc20 ipv6 connect 10.96.0.1:443: invalid-argument' <<<"$out" || ok=1
		grep -qxF 'tc20 ipv6 connect [::ffff:10.96.0.1]:443: invalid-argument' <<<"$out" || ok=1
	fi
	check "TC18, TC19, TC20, TC21" "$ok" "$(grep -E '^tc(18|19|20|21) ' <<<"$out" | tr '\n' ';')"
	echo "facts: $(grep -E '^(rule5|backlog) ' <<<"$out" | tr '\n' ';')" >>"$OUT/$step.txt"

	# TC22: the options other than the buffer sizes must agree with
	# wasmtime 36.0.10. The buffer sizes are recorded (section 11).
	out="$(so_logs wasi-sockets-options)"
	ok=0
	for s in 'option keep-alive-enabled default false, set true: ok read true, set false: ok read false' \
		'option keep-alive-idle-time default 7200000000000, set 30000000000: ok read 30000000000, set 1500000000: ok read 2000000000, set 0: invalid-argument read 2000000000' \
		'option keep-alive-interval default 75000000000, set 5000000000: ok read 5000000000, set 0: invalid-argument read 5000000000' \
		'option keep-alive-count default 9, set 3: ok read 3, set 0: invalid-argument read 3' \
		'option hop-limit default 64, set 42: ok read 42, set 0: invalid-argument read 42'; do
		grep -qxF "$s" <<<"$out" || ok=1
	done
	check TC22 "$ok" "$(grep -vE 'buffer-size' <<<"$out" | tr '\n' ';') buffer sizes (recorded): $(grep -E 'buffer-size' <<<"$out" | tr '\n' ';')"

	# NL1, NL2, and rule 11: localhost.
	a="$(so_logs wasi-lookup | grep '^lookup ')"
	b="$(so_logs wasi-lookup-fqdn | grep '^lookup ')"
	s="$(so_logs wasi-lookup-localhost | grep '^lookup ')"
	echo "facts: $s" >>"$OUT/$step.txt"
	out="$(so_logs wasi-echo-client c | grep '^lookup ')"
	ok=0
	grep -qx 'lookup kubernetes: \[[0-9.]*\]' <<<"$a" || ok=1
	grep -qx 'lookup kubernetes.default.svc.cluster.local: \[[0-9.]*\]' <<<"$b" || ok=1
	grep -qx 'lookup wasi-echo-eof: \[[0-9.]*\]' <<<"$out" || ok=1
	check "NL1, NL2" "$ok" "'$a'; '$b'; '$out'; recorded: '$s'"

	# CO17, CO19: recorded. The client sends 1 second after the connection.
	so_exec "{ sleep 1; echo x; } | nc -w 3 $(pod_ip wasi-echo-poll) 7" >/dev/null
	echo "facts: $(so_logs wasi-echo-poll | grep '^poll ')" >>"$OUT/$step.txt"

	# PX5: the second proxy component of the pod reaches 127.0.0.1:9000,
	# another pod and the httpGet probe do not.
	ip="$(pod_ip wasi-proxy-lo)"
	a="$(so_exec "wget -qO- -T 10 $ip:8080/fetch/127.0.0.1:9000/status/200")"
	b="$(so_exec "wget -qO- -T 5 $ip:9000/status/200; echo rc=\$?")"
	s="$(so_ready wasi-proxy-lo)"
	out="$(tab run 'kubectl get events --field-selector involvedObject.name=wasi-proxy-lo' 2>&1 | grep -c 'Readiness probe failed')"
	ok=0
	grep -q '^status 200' <<<"$a" && grep -q 'refused' <<<"$b" && [ "${s%% *}" = c=false ] && [ "$out" -gt 0 ] || ok=1
	check PX5 "$ok" "the second component gets '$(head -1 <<<"$a")' (want status 200); another pod gets '$(head -1 <<<"$b")' (want Connection refused); the httpGet probe on 9000 gives ${s%% *} with $out failure events (want c=false and events)"

	k get pods -l wasi-check=sockets -o wide
	k delete pods -l wasi-check=sockets --wait=false
}
