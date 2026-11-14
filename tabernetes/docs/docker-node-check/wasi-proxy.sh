#!/bin/bash
# shellcheck disable=SC2154 # here, step, N, OUT and crictl_ns come from check.sh
# wasi-proxy.sh: the step wasi-proxy of check.sh. check.sh sources it.
#
# Phase 2 of ../wasi-sockets.md: the proxy components and the client requests.
# The tab node does not have them yet (packages W2.1 to W2.3 of
# ../wasi-sockets-plan.md). Until then, this step records the results of
# runwasi in $OUT/wasi-proxy.txt, as phase 0 did (section 9.2, items 7 and
# 14), and prints SKIP. W2.3 changes the records into PASS and FAIL.

# in_pod <pod> '<command line>': runs it in the pod and records it.
in_pod() {
	echo "\$ [$1] $2" >>"$OUT/$step.txt"
	pod_exec "$1" "$2" >>"$OUT/$step.txt" 2>&1
}

step_wasi_proxy() {
	wasi_images_ready || return 0
	local ip p c=wasi-proxy-client
	node_ns
	apply_file "$here/wasi-proxy.yaml"
	if ! wait_for "kubectl get pod $c" 'Running' 240 || ! wait_for 'kubectl get pod wasi-proxy' 'Running' 120; then
		k get pods -l wasi-check=proxy -o wide
		result FAIL "the pods of wasi-proxy do not run"
		return 1
	fi
	sleep 5
	for p in context headers cookie status/204 status/599 sockets; do
		in_pod "$c" "wget -S -qO- -T 10 wasi-proxy:8080/$p 2>&1"
	done
	ip="$(pod_exec "$c" 'hostname -i' | tr -d ' \r\n')"
	in_pod "$c" 'mkdir -p /www && echo ok >/www/x && httpd -p 8000 -h /www'
	in_pod "$c" "wget -qO- -T 30 wasi-proxy:8080/fetch/$ip:8000/x"
	in_pod "$c" 'wget -qO- -T 30 wasi-proxy:8080/fetch/nothere.invalid/x'
	for p in trap error noresponse; do
		in_pod "$c" "wget -qO- -T 10 wasi-proxy:8080/$p; echo rc=\$?"
		sleep 5
		k get pod wasi-proxy -o "custom-columns=RESTARTS:.status.containerStatuses[0].restartCount,LAST:.status.containerStatuses[0].lastState.terminated.exitCode,REASON:.status.containerStatuses[0].lastState.terminated.reason"
	done
	ip="$(tab run "kubectl get pod wasi-proxy6 -o jsonpath='{.status.podIP}'" | tr -d ' \r\n')"
	in_pod "$c" "wget -qO- -T 10 $ip:8080/context | head -3"
	k get pods -l wasi-check=proxy -o wide
	k logs wasi-cmdhttp
	k delete pods -l wasi-check=proxy --wait=false
	result SKIP "the tab node has no proxy components yet (phase 2); the results of runwasi are in $OUT/$step.txt"
}
