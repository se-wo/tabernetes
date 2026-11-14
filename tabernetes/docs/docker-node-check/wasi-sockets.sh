#!/bin/bash
# shellcheck disable=SC2154 # here, step, N, OUT and crictl_ns come from check.sh
# wasi-sockets.sh: the step wasi-sockets of check.sh. check.sh sources it.
#
# Phase 3 of ../wasi-sockets.md: TCP and name lookup. The tab node does not
# have them yet (packages W3.1 to W3.3 of ../wasi-sockets-plan.md). Until
# then, this step records the results of runwasi in $OUT/wasi-sockets.txt, as
# phase 0 did (section 9.2, items 7 and 8), and prints SKIP. W3.3 changes the
# records into PASS and FAIL.

step_wasi_sockets() {
	wasi_images_ready || return 0
	local c=wasi-sockets-client
	node_ns
	apply_file "$here/wasi-sockets.yaml"
	if ! wait_for "kubectl get pod $c" 'Running' 240 || ! wait_for 'kubectl get pod wasi-echo-tcp' 'Running' 120; then
		k get pods -l wasi-check=sockets -o wide
		result FAIL "the pods of wasi-sockets do not run"
		return 1
	fi
	sleep 5
	echo "\$ [$c] echo tcp | nc -w 2 wasi-echo-tcp 7" >>"$OUT/$step.txt"
	pod_exec "$c" 'echo tcp | nc -w 2 wasi-echo-tcp 7' >>"$OUT/$step.txt" 2>&1
	k logs wasi-echo-tcp
	wait_for 'kubectl get pod wasi-lookup-fqdn' 'Completed|Error' 60
	k logs wasi-lookup
	k logs wasi-lookup-fqdn
	k delete pods -l wasi-check=sockets --wait=false
	result SKIP "the tab node has no TCP and no name lookup for components yet (phase 3); the results of runwasi are in $OUT/$step.txt"
}
