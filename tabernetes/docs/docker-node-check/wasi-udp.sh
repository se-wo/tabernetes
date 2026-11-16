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
#   ND, IC3, UD19      phase 5: UDP between the tab node and node-1, see
#                      udp_nodes
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

# udp_cip <service>: the ClusterIP of a Service in the namespace default.
udp_cip() {
	tab run "kubectl get service $1 -o jsonpath='{.spec.clusterIP}'" | tr -d ' \r\n'
}

# udp_tab <pod> <command line>: runs it in a pod on the tab node with
# kubectl exec, records it and prints the output. The command line has no
# single quotes.
udp_tab() {
	local out
	out="$(tab run "kubectl exec $1 -- sh -c '$2'" 2>&1 | tr -d '\r')"
	printf '$ [%s] %s\n%s\n' "$1" "$2" "$out" >>"$OUT/$step.txt"
	printf '%s\n' "$out"
}

# udp_module <name>: 0 if the kernel of the node has the module (loaded or
# built in).
udp_module() {
	# shellcheck disable=SC2016 # for the shell in the node
	docker exec "$N" sh -c 'grep -q "^$1 " /proc/modules || grep -q "/$1\.ko" "/lib/modules/$(uname -r)/modules.builtin" 2>/dev/null || [ -d "/sys/module/$1" ]' sh "$1"
}

# udp_diagnose <tab pod IP>: records what the page, the agent and the kernel
# of the node show for UDP from node-1 to a tab pod (after a FAIL of ND13).
udp_diagnose() {
	local f="${CONSOLE_LOG:-browser-console.log}" out ns cnt
	# shellcheck disable=SC2016 # for the shell in the node
	cnt='grep -E "^(Ip|Udp):" /proc/net/snmp | tr -s " " | cut -d" " -f1-8,15-18 | tr "\n" " "; ip -s link show tabpods0 | sed -n 4p; ip -s link show tabpods1 | sed -n 4p'
	# shellcheck disable=SC2086 # crictl_ns is a list of flags
	udp_fact "diagnose: counters before: $(udp_lines "$(docker exec "$N" nsenter $crictl_ns sh -c "$cnt" 2>&1)")"
	out="$(udp_exec "echo diag | nc -u -w 2 $1 7; echo rc=\$?")"
	udp_fact "diagnose: busybox nc -u from node-1 to $1:7: $(udp_lines "$out")"
	# shellcheck disable=SC2086 # crictl_ns is a list of flags
	udp_fact "diagnose: counters after: $(udp_lines "$(docker exec "$N" nsenter $crictl_ns sh -c "$cnt" 2>&1)")"
	# TCP takes the same route and the TPROXY rule for TCP: the agent
	# accepts the connection before the tab node decides.
	out="$(udp_exec "echo diag | nc -w 2 $1 7; echo rc=\$?")"
	udp_fact "diagnose: busybox nc (TCP) from node-1 to $1:7: $(udp_lines "$out")"
	udp_fact "diagnose: page console: $(udp_lines "$(grep -ai 'udp' "$f" 2>/dev/null | tail -8)")"
	udp_fact "diagnose: agent: $(udp_lines "$(docker logs "$N" 2>&1 | grep -aiE 'udp channel|pod channel' | tail -8)")"
	# shellcheck disable=SC2016,SC2086 # for the shell in the node; crictl_ns is a list of flags
	for ns in "" "nsenter $crictl_ns"; do
		udp_fact "diagnose: node ${ns:-container}: $(udp_lines "$(docker exec "$N" $ns sh -c 'nft list table inet tabernetes_podlink 2>&1 | grep -E "udp|drop|notrack"; ip route get '"$1"' 2>&1; ip rule 2>&1; ip route show table all 2>&1 | grep -E "local default|tabpods"; for s in all default cni0 tabpods0 tabpods1; do echo "rp_filter $s $(cat /proc/sys/net/ipv4/conf/$s/rp_filter 2>&1)"; done; ip -br link 2>&1 | grep tabpods; (ss -uanp 2>/dev/null || cat /proc/net/udp) | grep -iE "15001|3A97|'"$1"'" | head -5; grep '"$1"' /proc/net/nf_conntrack 2>/dev/null | head -3; iptables -S FORWARD 2>&1 | head -12; nft list ruleset 2>/dev/null | grep -E "queue|drop|reject" | head -12' 2>&1)")"
	done
}

# udp_nodes: phase 5, UDP between the tab node and node-1 (package W5.3).
# One PASS, FAIL or SKIP line for each row of section 10.3 with "Docker":
#
#   ND3, ND4, ND5     a tab pod sends datagrams of 65,507 bytes to a pod on
#                     node-1, which gets them whole
#   ND13, ND14, ND15  a pod on node-1 sends to a tab pod and to its Service;
#                     the answers come from the pod IP and the ClusterIP
#   ND16, ND18        a tab pod sends to a pod on node-1 and to a Service
#                     with only that endpoint; it gets the answers
#   Rule 7            a connected tab client to a port without a socket on
#                     node-1 gets no connection-refused (section 11)
#   UD19              nslookup over UDP: a tab pod to the pod IP of CoreDNS
#                     on node-1, the default server in a tab pod and in the
#                     terminal; a pod on node-1 to CoreDNS on the tab node if
#                     it runs there (else a fact)
#   Host              nc -u in the terminal of the tab node to a pod on
#                     node-1 (SKIP without nft_socket and without answer)
#   IC3               ping from a tab pod to a pod on node-1
#   ND6, ND9          policies on tab pods: the engine of the tab node
#   ND10, ND17        a policy on a pod of node-1: kube-network-policies on
#                     node-1 (SKIP with a host kernel without nft_queue)
#
# ND3 and ND12 are also unit tests (W5.1a, W5.2a), ND19 is a unit test
# (W5.2a). A deny test waits at most 10 seconds for "no answer".
udp_nodes() {
	local p dock dsvc tabip tsvc inip polip cl dc ok out a b c s k8s dns cd1 cdt n
	for p in wasi-nd-dock wasi-nd-tab wasi-nd-in wasi-nd-pol wasi-nd-bb wasi-nd-ok wasi-nd-eg; do
		if ! wait_for "kubectl get pod $p" 'Running' 240; then
			k get pods -l wasi-check=udp -o wide
			result FAIL "ND rows: the pods of phase 5 do not run ($p)"
			return 1
		fi
	done
	dock="$(pod_ip wasi-nd-dock)" tabip="$(pod_ip wasi-nd-tab)" inip="$(pod_ip wasi-nd-in)" polip="$(pod_ip wasi-nd-pol)"
	dsvc="$(udp_cip wasi-nd-dock)" tsvc="$(udp_cip wasi-nd-tab)" k8s="$(udp_cip kubernetes)"

	# The clients of the echo image get the IPs as literals. The clients of
	# a Service wait 2 seconds for its endpoint.
	run tab run "kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata: {name: wasi-nd-client, labels: {wasi-check: udp}}
spec:
  runtimeClassName: wasmtime
  nodeSelector: {kubernetes.io/hostname: tab}
  restartPolicy: Never
  containers:
    - name: flood
      image: ghcr.io/se-wo/tabernetes/wasi-test-echo:latest
      env: [{name: MODE, value: udp-flood}, {name: CONNECT, value: '$dock:7'}, {name: COUNT, value: '3'}, {name: SIZE, value: '65507'}]
    - name: pod
      image: ghcr.io/se-wo/tabernetes/wasi-test-echo:latest
      env: [{name: MODE, value: udp-client}, {name: CONNECT, value: '$dock:7'}, {name: CONNECTED, value: '1'}, {name: COUNT, value: '2'}, {name: INTERVAL_MS, value: '500'}, {name: LINE, value: nd16}]
    - name: svc
      image: ghcr.io/se-wo/tabernetes/wasi-test-echo:latest
      env: [{name: MODE, value: udp-client}, {name: CONNECT, value: '$dsvc:7'}, {name: CONNECTED, value: '1'}, {name: COUNT, value: '2'}, {name: INTERVAL_MS, value: '500'}, {name: LINE, value: nd18}, {name: DELAY_MS, value: '2000'}]
    - name: noport
      image: ghcr.io/se-wo/tabernetes/wasi-test-echo:latest
      env: [{name: MODE, value: udp-client}, {name: CONNECT, value: '$dock:9'}, {name: CONNECTED, value: '1'}, {name: COUNT, value: '3'}, {name: INTERVAL_MS, value: '500'}, {name: WAIT_MS, value: '1000'}, {name: LINE, value: noport}]
---
apiVersion: v1
kind: Pod
metadata: {name: wasi-nd-docker, labels: {wasi-check: udp}}
spec:
  runtimeClassName: wasmtime
  nodeSelector: {kubernetes.io/hostname: node-1}
  restartPolicy: Never
  containers:
    - name: pod
      image: ghcr.io/se-wo/tabernetes/wasi-test-echo:latest
      imagePullPolicy: Never
      env: [{name: MODE, value: udp-client}, {name: CONNECT, value: '$tabip:7'}, {name: CONNECTED, value: '1'}, {name: COUNT, value: '2'}, {name: INTERVAL_MS, value: '500'}, {name: LINE, value: nd13}]
    - name: svc
      image: ghcr.io/se-wo/tabernetes/wasi-test-echo:latest
      imagePullPolicy: Never
      env: [{name: MODE, value: udp-client}, {name: CONNECT, value: '$tsvc:7'}, {name: CONNECTED, value: '1'}, {name: COUNT, value: '2'}, {name: INTERVAL_MS, value: '500'}, {name: LINE, value: nd15}, {name: DELAY_MS, value: '2000'}]
    - name: in
      image: ghcr.io/se-wo/tabernetes/wasi-test-echo:latest
      imagePullPolicy: Never
      env: [{name: MODE, value: udp-client}, {name: CONNECT, value: '$inip:7'}, {name: CONNECTED, value: '1'}, {name: COUNT, value: '2'}, {name: INTERVAL_MS, value: '500'}, {name: WAIT_MS, value: '1000'}, {name: LINE, value: nd9}]
EOF"
	for p in wasi-nd-client wasi-nd-docker; do
		wait_for "kubectl get pod $p -o jsonpath={.status.phase}" 'Succeeded|Failed' 180 >/dev/null
	done
	k get pods -l wasi-check=udp -o wide
	cl="$(pod_ip wasi-nd-client)" dc="$(pod_ip wasi-nd-docker)"

	# ND3, ND4, ND5: 14 + 65,507 = 65,521 bytes on the channel udp. The
	# answers of the echo pod are too large and get lost.
	a="$(so_logs wasi-nd-client flood)"
	s="$(so_logs wasi-nd-dock c)"
	n="$(grep -c "^udp 65507 bytes from $cl:" <<<"$s")"
	ok=0
	grep -qx 'sent 3 errors 0' <<<"$a" || ok=1
	[ "$n" -ge 1 ] || ok=1
	check "ND3, ND4, ND5" "$ok" "the tab pod $cl sends 3 datagrams of 65,507 bytes to $dock:7 on node-1: '$(udp_lines "$a")'; the pod on node-1 gets $n of them whole (want at least 1); other sizes from the tab pod: '$(grep "bytes from $cl:" <<<"$s" | grep -v '^udp 65507 ' | head -3 | tr '\n' ';')'"

	# ND13, ND14, ND15: the pod on node-1 to the tab pod and to its Service.
	a="$(so_logs wasi-nd-docker pod)"
	b="$(so_logs wasi-nd-docker svc)"
	s="$(so_logs wasi-nd-tab c)"
	ok=0
	grep -qx "answer echo nd13 from $tabip:7" <<<"$a" || ok=1
	grep -qx "answer echo nd15 from $tsvc:7" <<<"$b" || ok=1
	grep -q '^error ' <<<"$a$b" && ok=1
	grep -q "^udp 4 bytes from $dc:" <<<"$s" || ok=1
	check "ND13, ND14, ND15" "$ok" "the pod $dc on node-1 to the tab pod $tabip:7: $(udp_lines "$(grep -E '^(answer|no answer|error)' <<<"$a")"); to its Service $tsvc:7: $(udp_lines "$(grep -E '^(answer|no answer|error)' <<<"$b")") (want the answers from $tabip:7 and $tsvc:7); the tab pod sees '$(grep "bytes from $dc:" <<<"$s" | head -1)'"
	[ "$ok" = 0 ] || udp_diagnose "$tabip"

	# ND16, ND18: the tab pod to the pod on node-1 and to its Service; busybox
	# nc -u too.
	a="$(so_logs wasi-nd-client pod)"
	b="$(so_logs wasi-nd-client svc)"
	out="$(udp_tab wasi-nd-bb "echo bb | nc -u -w 2 $dock 7; echo rc=\$?")"
	ok=0
	grep -qx "answer echo nd16 from $dock:7" <<<"$a" || ok=1
	grep -qx "answer echo nd18 from $dsvc:7" <<<"$b" || ok=1
	grep -q '^error ' <<<"$a$b" && ok=1
	[ "$(head -1 <<<"$out")" = "echo bb" ] || ok=1
	check "ND16, ND18" "$ok" "the tab pod $cl to $dock:7 on node-1: $(udp_lines "$(grep -E '^(answer|no answer|error)' <<<"$a")"); to the Service $dsvc:7: $(udp_lines "$(grep -E '^(answer|no answer|error)' <<<"$b")"); busybox nc -u: '$(udp_lines "$out")' (want the answers)"

	# Rule 7 of section 8.9.1, section 11: no connection-refused between
	# nodes.
	c="$(so_logs wasi-nd-client noport)"
	ok=0
	grep -q '^error ' <<<"$c" && ok=1
	grep -q '^answer ' <<<"$c" && ok=1
	grep -qx 'no answer' <<<"$c" || ok=1
	check "Rule 7, section 11" "$ok" "a connected tab client to $dock:9 on node-1: $(udp_lines "$(grep -E '^(peer|answer|no answer|error)' <<<"$c")") (want no answer and no connection-refused)"

	# UD19: nslookup over UDP between nodes. CoreDNS runs on node-1 after the
	# step join.
	dns="$(tab run "kubectl get pods -n kube-system -l k8s-app=kube-dns -o jsonpath='{range .items[*]}{.spec.nodeName}={.status.podIP} {end}'" 2>&1 | tail -1 | tr -d '\r')"
	cd1="$(tr ' ' '\n' <<<"$dns" | sed -n 's/^node-1=//p' | head -1)"
	cdt="$(tr ' ' '\n' <<<"$dns" | sed -n 's/^tab=//p' | head -1)"
	a="$(udp_tab wasi-nd-bb "nslookup kubernetes.default.svc.cluster.local $cd1; echo rc=\$?")"
	b="$(udp_tab wasi-nd-bb "nslookup kubernetes.default; echo rc=\$?")"
	c="$(tab run 'nslookup kubernetes.default; echo rc=$?' 2>&1 | tr -d '\r')"
	printf '$ nslookup kubernetes.default\n%s\n' "$c" >>"$OUT/$step.txt"
	ok=0
	[ -n "$cd1" ] || ok=1
	for out in "$a" "$b" "$c"; do
		grep -qx "Address: *$k8s" <<<"$out" && grep -qx 'rc=0' <<<"$out" || ok=1
	done
	check "UD19 (phase 5)" "$ok" "CoreDNS: $dns; a tab pod to CoreDNS $cd1 on node-1: $(udp_lines "$(grep -E '^(Address: |rc=|;;)' <<<"$a")"); the default server in a tab pod: $(udp_lines "$(grep -E '^(Server:|Address: |rc=|;;)' <<<"$b")"); in the terminal: $(udp_lines "$(grep -E '^(Server:|Address: |rc=|;;)' <<<"$c")") (want Address: $k8s, rc=0)"
	if [ -n "$cdt" ]; then
		a="$(udp_exec "nslookup kubernetes.default.svc.cluster.local $cdt; echo rc=\$?")"
		ok=0
		grep -qx "Address: *$k8s" <<<"$a" && grep -qx 'rc=0' <<<"$a" || ok=1
		check "UD19 (phase 5, node-1 to the tab node)" "$ok" "a pod on node-1 to CoreDNS $cdt on the tab node: $(udp_lines "$(grep -E '^(Address: |rc=|;;)' <<<"$a")") (want Address: $k8s, rc=0)"
	else
		udp_fact "no CoreDNS pod on the tab node now ($dns): a pod on node-1 to the CoreDNS of the tab node is not tested here; wasi-udp-nodes.test.mjs has a node worker to the CoreDNS of the tab node"
	fi

	# Host traffic: the terminal of the tab node has the node IP as source.
	out="$(tab run "echo host | nc -u -w 2 $dock 7; echo rc=\$?" 2>&1 | tr -d '\r')"
	printf '$ echo host | nc -u -w 2 %s 7\n%s\n' "$dock" "$out" >>"$OUT/$step.txt"
	s="$(so_logs wasi-nd-dock c)"
	a="the terminal of the tab node to $dock:7 on node-1: '$(udp_lines "$out")'; the pod sees '$(grep '^udp 5 bytes from' <<<"$s" | grep -v "from $cl:" | tail -1)'"
	if [ "$(head -1 <<<"$out")" = "echo host" ]; then
		result PASS "Host traffic: $a"
	elif ! udp_module nft_socket; then
		result SKIP "Host traffic: the kernel of the node has no nft_socket ($a)"
	else
		result FAIL "Host traffic: $a (want echo host)"
	fi

	# IC3, the part on a Docker node.
	out="$(udp_tab wasi-nd-bb "ping -c 1 $dock 2>&1; echo rc=\$?")"
	ok=0
	grep -qx "ping: $dock is on node node-1: ICMP between nodes is not available" <<<"$out" || ok=1
	grep -qx 'rc=1' <<<"$out" || ok=1
	check IC3 "$ok" "ping -c 1 $dock in a tab pod: $(udp_lines "$out") (want ping: $dock is on node node-1: ICMP between nodes is not available, rc=1)"

	# ND6: a policy denies all egress of the tab pod wasi-nd-eg. The engine
	# of the tab node decides (the source node examines the egress).
	a="$(udp_tab wasi-nd-eg "echo eg | nc -u -w 2 $dock 7; echo rc=\$?")"
	b="$(udp_tab wasi-nd-eg "echo eg | nc -u -w 2 $dock 7; echo rc=\$?")"
	s="$(so_logs wasi-nd-dock c)"
	p="$(pod_ip wasi-nd-eg)"
	ok=0
	[ "$a$b" = "rc=0rc=0" ] || ok=1
	grep -q "from $p:" <<<"$s" && ok=1
	check ND6 "$ok" "the denied tab pod $p to $dock:7 on node-1, two times: '$(udp_lines "$a")', '$(udp_lines "$b")' (want rc=0 without an answer); the pod on node-1 sees '$(grep "from $p:" <<<"$s" | head -1)' (want nothing)"

	# ND9: a policy denies all ingress of the tab pod wasi-nd-in. The engine
	# of the tab node decides (the destination node examines the ingress).
	a="$(so_logs wasi-nd-docker in)"
	s="$(so_logs wasi-nd-in c)"
	ok=0
	grep -qE '^(answer|error) ' <<<"$a" && ok=1
	grep -qx 'no answer' <<<"$a" || ok=1
	grep -q "from $dc:" <<<"$s" && ok=1
	check ND9 "$ok" "the pod $dc on node-1 to the denied tab pod $inip:7: $(udp_lines "$(grep -E '^(answer|no answer|error)' <<<"$a")") (want no answer); the tab pod sees '$(grep "from $dc:" <<<"$s" | head -1)' (want nothing)"

	# ND10, ND17: the policy wasi-nd-pol on node-1 permits only wasi-nd-ok.
	# kube-network-policies (nfqueue) examines the datagrams of the
	# transparent sockets of the agent (uid 65534). The engine of the tab node
	# also evaluates the ingress of the destination (one call, W4.2b).
	if ! udp_module nft_queue || ! udp_module nfnetlink_queue; then
		result SKIP "ND10, ND17: host kernel without nft_queue"
	else
		a="$(udp_tab wasi-nd-ok "echo ok | nc -u -w 2 $polip 7; echo rc=\$?")"
		[ "$(head -1 <<<"$a")" = "echo ok" ] || a="$(udp_tab wasi-nd-ok "echo ok | nc -u -w 2 $polip 7; echo rc=\$?")"
		b="$(udp_tab wasi-nd-bb "echo bb | nc -u -w 2 $polip 7; echo rc=\$?")"
		c="$(udp_tab wasi-nd-bb "echo bb | nc -u -w 2 $polip 7; echo rc=\$?")"
		p="$(pod_ip wasi-nd-bb)"
		s="$(so_logs wasi-nd-pol c)"
		ok=0
		[ "$(head -1 <<<"$a")" = "echo ok" ] || ok=1
		[ "$b$c" = "rc=0rc=0" ] || ok=1
		grep -q "from $p:" <<<"$s" && ok=1
		check "ND10, ND17" "$ok" "the permitted tab pod to $polip:7 on node-1: '$(udp_lines "$a")' (want echo ok); the second tab pod $p, two times: '$(udp_lines "$b")', '$(udp_lines "$c")' (want rc=0 without an answer); the pod on node-1 sees '$(grep "from $p:" <<<"$s" | head -1)' (want nothing)"
	fi
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

	# UD10, UD11: kube-proxy rejects a Service without endpoints with ICMP
	# "port unreachable" (fact 17). The ICMP does not arrive on every host, as
	# for CR7 and UD12 (section 9.2): PASS for connection-refused and for no
	# answer, and record which one.
	a="$(so_logs wasi-udp-clients noep)"
	if udp_refused "$a"; then
		check "UD10, UD11" 0 "a connected client to the Service wasi-udp-none gets connection-refused (this host): $(udp_lines "$(grep -E '^(peer|no answer|error)' <<<"$a")")"
	else
		ok=0
		grep -qE '^(answer|error) ' <<<"$a" && ok=1
		grep -q '^no answer$' <<<"$a" || ok=1
		check "UD10, UD11" "$ok" "a connected client to the Service wasi-udp-none gets no answer and no error (this host; fact 17 expects connection-refused): $(udp_lines "$(grep -E '^(peer|answer|no answer|error)' <<<"$a")")"
	fi

	# How kube-proxy rejects a Service without endpoints (UD10, UD11).
	# shellcheck disable=SC2016,SC2086 # for the shell in the node; crictl_ns is a list of flags
	udp_fact "kube-proxy: mode '$(tab run "kubectl -n kube-system get configmap kube-proxy -o jsonpath='{.data.config\.conf}'" 2>&1 | tr -d '\r' | sed -n 's/^mode: *//p')', rules for wasi-udp-none: $(udp_lines "$(docker exec "$N" nsenter $crictl_ns sh -c 'iptables -t filter -L KUBE-SERVICES -v -n 2>/dev/null | grep "wasi-udp-none"; iptables -t filter -S FORWARD 2>/dev/null | grep -i kube; nft list ruleset 2>/dev/null | grep -i "wasi-udp-none"' 2>&1 | head -10)")"

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
	# W4.3: the second datagram of a connected socket takes the error of the
	# first one. stdout and stderr separately.
	udp_fact "nc -u refused, two datagrams ($ip:9): $(udp_lines "$(udp_exec "{ echo x; sleep 1; echo y; } | nc -u -w 2 $ip 9 2>/tmp/nc.err; echo rc=\$?; echo stderr:; cat /tmp/nc.err")")"
	udp_fact "nc -u -v refused, two datagrams ($ip:9): $(udp_lines "$(udp_exec "{ echo x; sleep 1; echo y; } | nc -u -v -w 2 $ip 9 2>/tmp/nc.err; echo rc=\$?; echo stderr:; cat /tmp/nc.err")")"
	udp_fact "nc -u no endpoints (wasi-udp-none:7): $(udp_lines "$(udp_exec 'echo x | nc -u -w 2 wasi-udp-none 7; echo rc=$?')")"
	udp_fact "nc -u two lines, one answer each: $(udp_lines "$(udp_exec 'printf "a\nb\n" | nc -u -w 2 wasi-echo-udp 7; echo rc=$?')")"

	udp_nodes

	k get pods -l wasi-check=udp -o wide
	k delete pods -l wasi-check=udp --wait=false
}
