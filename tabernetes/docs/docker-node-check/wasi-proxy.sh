#!/bin/bash
# shellcheck disable=SC2154 # here, step, N, OUT and crictl_ns come from check.sh
# wasi-proxy.sh: the step wasi-proxy of check.sh. check.sh sources it.
#
# Phase 2 of ../wasi-sockets.md: the tests of section 10.3 for the proxy
# components (PX) and the client requests (CR) with "Docker" in the column
# "Nodes", on the real runwasi of node-1. runwasi is the reference for the
# tab node: web/test/e2e/wasi-proxy*.test.mjs has the results of this step
# as fixed values. The objects are in wasi-proxy.yaml. One PASS, FAIL or
# SKIP line for each row of section 10.3:
#
#   PX12            the first 3 requests get REQUEST_ID 0, 1 and 2
#   PX1, PX4, PX16  a Service and a readiness probe on the address of the
#                   variable
#   PX2, PX3        no variable, and a value that does not parse: port 8080
#   PX4, PX32, PX33 [::]:8080, port 0 and a foreign IP
#   PX5             in the step wasi-sockets (wasi-sockets.sh)
#   PX6, PX11, PX13 the context of an instance
#   PX7             a new instance for each request
#   PX8, PX9, PX10  20 requests that wait 1 second
#   PX14            no stdout and stderr of an instance in the log
#   PX15            the results of wasi:sockets
#   PX16, PX28      Cookie, User-Agent and Origin; two Set-Cookie headers
#   PX17            no Content-Type
#   PX21, PX22      a trap
#   PX26            handle sets no response
#   PX27            an error code as the response
#   PX29            PURGE, TRACE and a GET with a body
#   PX30            the status codes 204, 304 and 599
#   PX31            the header names of a response, HTTP/1.1 and HTTP/1.0
#   CR1             a client request to a Service by its name
#   CR1, CR8        SKIP: NetworkPolicy on a Docker node (deferred)
#   CR3, CR4, CR5   no redirects, no added headers, no decompression
#   CR6             a command component with an import of wasi:http
#
# The raw HTTP requests go through busybox nc in the pod wasi-proxy-client.

# in_pod <pod> '<command line>': runs it in the pod and records it.
in_pod() {
	echo "\$ [$1] $2" >>"$OUT/$step.txt"
	pod_exec "$1" "$2" >>"$OUT/$step.txt" 2>&1
}

# px_get <command line>: runs it in the client, records it and prints the
# output without carriage returns.
px_get() {
	local out
	out="$(pod_exec wasi-proxy-client "$1" 2>&1 | tr -d '\r')"
	printf '$ [wasi-proxy-client] %s\n%s\n' "$1" "$out" >>"$OUT/$step.txt"
	printf '%s\n' "$out"
}

# px_raw <host:port> '<printf format of the request>' [seconds]: sends the
# request with nc and prints the response without carriage returns. stdin
# of nc stays open for 1 second (or the seconds) after the request: at the
# end of stdin, nc closes its side of the connection, and hyper (half_close
# false) then drops the request, as for a client that closes the connection.
px_raw() {
	px_get "{ printf '$2'; sleep ${3:-1}; } | nc ${1%:*} ${1##*:}"
}

# header_names: the names of the header lines of a raw response on stdin,
# in lowercase letters, sorted and on one line.
header_names() {
	sed -n '2,/^$/p' | sed '/^$/d' | cut -d: -f1 | tr '[:upper:]' '[:lower:]' | sort | tr '\n' ' ' | sed 's/ $//'
}

# pod_restarts <pod>: "<restarts> <last exit code> <last reason>" of the
# first container.
pod_restarts() {
	local s
	s="$(tab run "kubectl get pod $1 -o jsonpath='{.status.containerStatuses[0].restartCount} {.status.containerStatuses[0].lastState.terminated.exitCode} {.status.containerStatuses[0].lastState.terminated.reason}'" 2>&1 | tail -1)"
	echo "$1: $s" >>"$OUT/$step.txt"
	echo "$s"
}

pod_ip() {
	tab run "kubectl get pod $1 -o jsonpath='{.status.podIP}'" | tr -d ' \r\n'
}

# listen_ports <pod>: the TCP ports in the state LISTEN in the network
# namespace of the first container, from /proc/<pid>/net/tcp of the node.
listen_ports() {
	local id pid
	id="$(node_crictl ps -q --label "io.kubernetes.pod.name=$1" | head -1)"
	pid="$(node_crictl inspect "$id" 2>/dev/null | jq -r '.info.pid // empty')"
	echo "$1: container $id, pid $pid" >>"$OUT/$step.txt"
	[ -n "$pid" ] || return 0
	docker exec "$N" cat "/proc/$pid/net/tcp" "/proc/$pid/net/tcp6" 2>>"$OUT/$step.txt" |
		awk '$4 == "0A" { split($2, a, ":"); print a[2] }' | sort -u |
		while read -r hex; do echo $((16#$hex)); done | tr '\n' ' ' | sed 's/ $//'
}

# check <row> <condition result 0/1> <text>: one PASS or FAIL line.
check() {
	if [ "$2" = 0 ]; then
		result PASS "$1: $3"
	else
		result FAIL "$1: $3"
	fi
}

step_wasi_proxy() {
	wasi_images_ready || return 0
	local ip out s a b names c=wasi-proxy-client p ok
	node_ns
	apply_file "$here/wasi-proxy.yaml"
	if ! wait_for "kubectl get pod $c" 'Running' 240 || ! wait_for 'kubectl get pod wasi-proxy' 'Running' 120 ||
		! wait_for 'kubectl get pod wasi-proxy-cr' 'Running' 60; then
		k get pods -l wasi-check=proxy -o wide
		result FAIL "the pods of wasi-proxy do not run"
		return 1
	fi
	sleep 5
	k get pods -l wasi-check=proxy -o wide

	# PX12 first: the first requests of the container.
	out="$(for p in 1 2 3; do px_get 'wget -qO- -T 10 wasi-proxy:8080/context'; done)"
	s="$(grep '^env REQUEST_ID=' <<<"$out" | cut -d= -f2 | tr '\n' ' ')"
	check PX12 "$([ "$s" = "0 1 2 " ] && echo 0 || echo 1)" "3 requests get REQUEST_ID '${s% }' (want 0 1 2)"

	# PX7: each response shows the counter 1.
	s="$(grep '^counter ' <<<"$out" | tr '\n' ' ')"
	check PX7 "$([ "$s" = "counter 1 counter 1 counter 1 " ] && echo 0 || echo 1)" "3 responses show '${s% }' (want counter 1 each time)"

	# PX1, PX4, PX16: the Service, and the readiness probe on 0.0.0.0:9090.
	a="$(px_get 'wget -qO- -T 10 wasi-proxy:8080/status/200; echo rc=$?' | tail -1)"
	if wait_for 'kubectl get pod wasi-proxy-probe' 'wasi-proxy-probe +1/1 +Running' 90; then b=ready; else b="not ready"; fi
	check "PX1, PX4, PX16" "$([ "$a" = rc=0 ] && [ "$b" = ready ] && echo 0 || echo 1)" "wget through the Service gives $a (want rc=0); the httpGet probe on 0.0.0.0:9090 is $b"

	# PX2, PX3: port 8080.
	a="$(px_get "wget -qO- -T 10 $(pod_ip wasi-proxy):8080/status/200; echo rc=\$?" | tail -1)"
	b="$(px_get "wget -qO- -T 10 $(pod_ip wasi-proxy-bad):8080/status/200; echo rc=\$?" | tail -1)"
	check "PX2, PX3" "$([ "$a $b" = "rc=0 rc=0" ] && echo 0 || echo 1)" "no variable: $a; 'not-an-address': $b on port 8080 (want rc=0 rc=0)"

	# PX4, PX32, PX33. PX4 needs IPv6 in the node: the GitHub runner has it,
	# a host without IPv6 gives "Address family not supported by protocol"
	# (section 9.2).
	a="$(px_get "wget -qO- -T 10 $(pod_ip wasi-proxy6):8080/status/200; echo rc=\$?" | tail -1) restarts $(pod_restarts wasi-proxy6 | cut -d' ' -f1)"
	ip="$(pod_ip wasi-proxy-port0)"
	p="$(listen_ports wasi-proxy-port0)"
	ok=1
	if [[ "$p" =~ ^[0-9]+$ ]] && ((p >= 32768 && p <= 60999)); then
		b="$(px_get "wget -qO- -T 10 $ip:$p/status/200; echo rc=\$?" | tail -1)"
		[ "$b" = rc=0 ] && ok=0
	else
		b="no single port"
	fi
	wait_for 'kubectl get pod wasi-proxy-foreign' 'wasi-proxy-foreign +0/1 +[A-Za-z]+ +[1-9]' 120
	s="$(pod_restarts wasi-proxy-foreign)"
	out="$(tab run 'kubectl logs wasi-proxy-foreign --previous' 2>&1)"
	printf '$ kubectl logs wasi-proxy-foreign --previous\n%s\n' "$out" >>"$OUT/$step.txt"
	docker logs "$N" 2>&1 | grep -m3 'os error 99' >>"$OUT/$step.txt"
	[ "$a" = "rc=0 restarts 0" ] && [ "${s#* }" = "137 Error" ] || ok=1
	check "PX4, PX32, PX33" "$ok" "[::]:8080: $a (want rc=0 restarts 0); port 0 listens on '$p': $b (want a port of 32768 to 60999, rc=0); 192.0.2.7:8080: restarts, exit code, reason '$s' (want 137 Error)"

	# PX6, PX11, PX13: the context of an instance on 0.0.0.0:9090, with the
	# two variables of runwasi in the container spec.
	out="$(px_get "wget -qO- -T 10 $(pod_ip wasi-proxy-probe):9090/context")"
	ok=0
	grep -qx 'args \[\]' <<<"$out" || ok=1
	grep -qx 'preopens 0' <<<"$out" || ok=1
	grep -q '^env REQUEST_ID=[0-9]' <<<"$out" || ok=1
	grep -q '^env HOSTNAME=wasi-proxy-probe$' <<<"$out" || ok=1
	grep -q '^env WASMTIME_HTTP' <<<"$out" && ok=1
	check "PX6, PX11, PX13" "$ok" "$(grep -E '^(args|preopens) ' <<<"$out" | tr '\n' ' ')REQUEST_ID $(grep -c '^env REQUEST_ID=' <<<"$out"), WASMTIME_HTTP_* $(grep -c '^env WASMTIME_HTTP' <<<"$out") (want args [], preopens 0, REQUEST_ID 1, WASMTIME_HTTP_* 0)"

	# PX8, PX9, PX10: no maximum on runwasi, so all responses arrive in
	# approximately 1 second. busybox time writes "real 0m 1.04s".
	# shellcheck disable=SC2016 # for the shell in the pod
	out="$(px_get 'time sh -c "for i in \$(seq 20); do wget -qO- -T 30 wasi-proxy:8080/wait/1000 >/dev/null & done; wait" 2>&1')"
	s="$(sed -n 's/^real[[:space:]]*0m *\([0-9.]*\)s$/\1/p' <<<"$out")"
	check "PX8, PX9, PX10" "$(awk -v t="${s:-99}" 'BEGIN { exit !(t >= 1 && t < 1.9) }' && echo 0 || echo 1)" "20 requests that wait 1 second take ${s:-?} s (want 1 to 1.9 s)"

	# PX14
	px_get 'wget -qO- -T 10 wasi-proxy:8080/stdio' >/dev/null
	out="$(tab run 'kubectl logs wasi-proxy' 2>&1)"
	printf '$ kubectl logs wasi-proxy\n%s\n' "$out" >>"$OUT/$step.txt"
	check PX14 "$(grep -q 'from the instance' <<<"$out" && echo 1 || echo 0)" "kubectl logs shows $(grep -c 'from the instance' <<<"$out") lines of the instance (want 0)"

	# PX15
	out="$(px_get 'wget -qO- -T 30 wasi-proxy:8080/sockets')"
	ok=0
	for s in 'tcp create: ok' 'udp create: ok'; do grep -qx "$s" <<<"$out" || ok=1; done
	for s in 'tcp start-bind' 'tcp start-connect' 'udp start-bind'; do grep -q "^$s: Err(.*name: \"access-denied\"" <<<"$out" || ok=1; done
	grep -q '^resolve.*name: "permanent-resolver-failure"' <<<"$out" || ok=1
	check PX15 "$ok" "$(tr '\n' ';' <<<"$out")"

	# PX16, PX28
	out="$(px_raw wasi-proxy:8080 'GET /headers HTTP/1.1\r\nHost: wasi-proxy\r\nCookie: c=1\r\nUser-Agent: px28\r\nOrigin: http://example.org\r\nConnection: close\r\n\r\n')"
	ok=0
	for s in 'cookie: c=1' 'user-agent: px28' 'origin: http://example.org'; do grep -qix "$s" <<<"$out" || ok=1; done
	b="$(px_raw wasi-proxy:8080 'GET /cookie HTTP/1.1\r\nHost: wasi-proxy\r\nConnection: close\r\n\r\n' | grep -ic '^set-cookie: [ab]=[12]$')"
	[ "$b" = 2 ] || ok=1
	check "PX16, PX28" "$ok" "Cookie, User-Agent and Origin arrive: $(grep -icE '^(cookie|user-agent|origin): ' <<<"$out") of 3; Set-Cookie lines: $b (want 2)"

	# PX17: /headers sets no headers.
	s="$(header_names <<<"$out")"
	check PX17 "$(grep -qw content-type <<<"$s" && echo 1 || echo 0)" "the header names are '$s' (want no content-type)"

	# PX21, PX22
	a="$(px_raw wasi-proxy:8080 'GET /trap HTTP/1.1\r\nHost: wasi-proxy\r\nConnection: close\r\n\r\n')"
	a=${#a}
	b="$(px_get 'wget -qO- -T 10 wasi-proxy:8080/status/200; echo rc=$?' | tail -1)"
	s="$(pod_restarts wasi-proxy)"
	check "PX21, PX22" "$([ "$a $b ${s%% *}" = "0 rc=0 0" ] && echo 0 || echo 1)" "a trap gives $a bytes of response, then $b, restarts ${s%% *} (want 0 bytes, rc=0, restarts 0)"

	# PX27
	a="$(px_raw wasi-proxy:8080 'GET /error HTTP/1.1\r\nHost: wasi-proxy\r\nConnection: close\r\n\r\n')"
	a=${#a}
	b="$(px_get 'wget -qO- -T 10 wasi-proxy:8080/status/200; echo rc=$?' | tail -1)"
	s="$(pod_restarts wasi-proxy)"
	check PX27 "$([ "$a $b ${s%% *}" = "0 rc=0 0" ] && echo 0 || echo 1)" "an error code gives $a bytes of response, then $b, restarts ${s%% *} (want 0 bytes, rc=0, restarts 0)"

	# PX26: the container exits with 139, kubelet starts it again, and
	# REQUEST_ID starts at 0 again.
	px_get "wget -qO- -T 10 $(pod_ip wasi-proxy-nores):8080/context >/dev/null 2>&1" >/dev/null
	a="$(px_raw "$(pod_ip wasi-proxy-nores):8080" 'GET /noresponse HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n')"
	a=${#a}
	wait_for 'kubectl get pod wasi-proxy-nores' 'wasi-proxy-nores +1/1 +Running +1' 90
	s="$(pod_restarts wasi-proxy-nores)"
	b="$(px_get "wget -qO- -T 10 $(pod_ip wasi-proxy-nores):8080/context" | grep '^env REQUEST_ID=')"
	check PX26 "$([ "$a|$s|$b" = "0|1 139 Error|env REQUEST_ID=0" ] && echo 0 || echo 1)" "no response gives $a bytes; restarts, exit code, reason '$s'; then $b (want 0 bytes, 1 139 Error, REQUEST_ID=0)"

	# PX29
	a="$(px_raw wasi-proxy:8080 'PURGE /echo HTTP/1.0\r\nHost: wasi-proxy\r\n\r\n' | grep -i '^x-method:')"
	b="$(px_raw wasi-proxy:8080 'TRACE /echo HTTP/1.0\r\nHost: wasi-proxy\r\n\r\n' | grep -i '^x-method:')"
	out="$(px_raw wasi-proxy:8080 'GET /echo HTTP/1.0\r\nHost: wasi-proxy\r\nContent-Length: 9\r\n\r\nget body\n')"
	s="$(grep -i '^x-method:' <<<"$out") $(grep -i '^x-body-length:' <<<"$out") $(tail -1 <<<"$out")"
	check PX29 "$(grep -qix 'x-method: other:PURGE' <<<"$a" && grep -qix 'x-method: TRACE' <<<"$b" && [ "$(tr '[:upper:]' '[:lower:]' <<<"$s")" = "x-method: get x-body-length: 9 get body" ] && echo 0 || echo 1)" "PURGE: '$a'; TRACE: '$b'; GET with a body: '$s'"

	# PX30
	s=""
	for p in 204 304 599; do
		s="$s $(px_raw wasi-proxy:8080 "GET /status/$p HTTP/1.1\r\nHost: wasi-proxy\r\nConnection: close\r\n\r\n" | head -1 | cut -d' ' -f2)"
	done
	check PX30 "$([ "$s" = " 204 304 599" ] && echo 0 || echo 1)" "/status/204, 304, 599 give${s} (want 204 304 599)"

	# PX31: the header names, the reference of the tab node. /cookie has no
	# content-length, /status/204 has no body.
	a="$(px_raw wasi-proxy:8080 'GET /cookie HTTP/1.1\r\nHost: wasi-proxy\r\nConnection: close\r\n\r\n')"
	b="$(px_raw wasi-proxy:8080 'GET /cookie HTTP/1.0\r\nHost: wasi-proxy\r\n\r\n')"
	out="$(px_raw wasi-proxy:8080 'GET /status/204 HTTP/1.1\r\nHost: wasi-proxy\r\nConnection: close\r\n\r\n')"
	s="HTTP/1.1 /cookie: $(header_names <<<"$a"); HTTP/1.0 /cookie: $(header_names <<<"$b"); HTTP/1.1 /status/204: $(header_names <<<"$out")"
	echo "status lines: $(head -1 <<<"$a"), $(head -1 <<<"$b"), $(head -1 <<<"$out")" >>"$OUT/$step.txt"
	# Requests without a host header: runwasi gives no response. The bytes of
	# the response, for HTTP/1.0 and HTTP/1.1.
	a="$(px_raw wasi-proxy:8080 'GET /cookie HTTP/1.0\r\n\r\n')"
	b="$(px_raw wasi-proxy:8080 'GET /cookie HTTP/1.1\r\nConnection: close\r\n\r\n')"
	s="$s; no host header: HTTP/1.0 ${#a} bytes, HTTP/1.1 ${#b} bytes ($(head -1 <<<"$b"))"
	check PX31 "$([ "$s" = "HTTP/1.1 /cookie: connection date set-cookie set-cookie transfer-encoding; HTTP/1.0 /cookie: date set-cookie set-cookie; HTTP/1.1 /status/204: connection date; no host header: HTTP/1.0 0 bytes, HTTP/1.1 0 bytes ()" ] && echo 0 || echo 1)" "$s"

	# CR1: by the name of the Service.
	out="$(px_get 'wget -qO- -T 30 wasi-proxy:8080/fetch/wasi-proxy-cr:8000/cr1')"
	check CR1 "$(grep -q '^status 302 ' <<<"$out" && echo 0 || echo 1)" "a client request to wasi-proxy-cr:8000 gives '$out' (want status 302)"

	result SKIP "CR1, CR8: NetworkPolicy on a Docker node is a deferred test (nfqueue, decision 16)"

	# CR3, CR4, CR5
	out="$(px_get 'wget -qO- -T 30 wasi-proxy:8080/fetchfull/wasi-proxy-cr:8000/cr345')"
	a="$(tab run 'kubectl logs wasi-proxy-cr' 2>&1 | tr -d '\r')"
	printf '$ kubectl logs wasi-proxy-cr\n%s\n' "$a" >>"$OUT/$step.txt"
	# The request lines of the last request: from its request line to the
	# empty line.
	names="$(awk '/^GET \/cr345 / { n = "" ; on = 1; next } on && /^$/ { on = 0 } on { split($0, f, ":"); n = n tolower(f[1]) " " } END { print n }' <<<"$a" | sed 's/ $//')"
	ok=0
	grep -qx 'status 302' <<<"$out" || ok=1
	grep -qix 'h location: /elsewhere' <<<"$out" || ok=1
	grep -qx 'body-hex 1f8b08000000000002034bcecf2d284a2d2e4e4d5148ca4fa9e4020008dbea6710000000' <<<"$out" || ok=1
	[ "$names" = host ] || ok=1
	check "CR3, CR4, CR5" "$ok" "the component gets '$(grep -E '^(status|body-len)' <<<"$out" | tr '\n' ' ')' (want status 302, body-len 36 and the gzip bytes); the server gets the headers '$names' (want host)"

	# CR6
	s="$(pod_state_proxy wasi-cmdhttp)"
	k logs wasi-cmdhttp
	check CR6 "$([ "$s" = "Failed 0 137 Error" ] && echo 0 || echo 1)" "a command component with wasi:http gives '$s' (want Failed 0 137 Error)"

	k get pods -l wasi-check=proxy -o wide
	k delete pods -l wasi-check=proxy --wait=false
}

# pod_state_proxy <pod>: as pod_state of wasi-command.sh.
pod_state_proxy() {
	local s
	s="$(tab run "kubectl get pod $1 -o jsonpath='{.status.phase} {.status.containerStatuses[0].restartCount} {.status.containerStatuses[0].state.terminated.exitCode} {.status.containerStatuses[0].state.terminated.reason}'" 2>&1 | tail -1)"
	echo "$1: $s" >>"$OUT/$step.txt"
	echo "$s"
}
