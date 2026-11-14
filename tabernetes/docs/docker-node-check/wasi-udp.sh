#!/bin/bash
# shellcheck disable=SC2154 # here, step, N, OUT and crictl_ns come from check.sh
# wasi-udp.sh: the step wasi-udp of check.sh. check.sh sources it.
#
# Phases 4 and 5 of ../wasi-sockets.md: UDP in a pod of the Docker node and
# UDP between nodes. The tab node does not have them yet (packages W4.1 to
# W5.3 of ../wasi-sockets-plan.md). Until then, this step records the
# results of runwasi in $OUT/wasi-udp.txt, as phase 0 did (section 9.2, item
# 8), and prints SKIP. W4.5 and W5.3 change the records into PASS and FAIL.

step_wasi_udp() {
	wasi_images_ready || return 0
	local c=wasi-udp-client
	node_ns
	apply_file "$here/wasi-udp.yaml"
	if ! wait_for "kubectl get pod $c" 'Running' 240 || ! wait_for 'kubectl get pod wasi-echo-udp' 'Running' 120; then
		k get pods -l wasi-check=udp -o wide
		result FAIL "the pods of wasi-udp do not run"
		return 1
	fi
	sleep 5
	echo "\$ [$c] echo udp | nc -u -w 2 wasi-echo-udp 7" >>"$OUT/$step.txt"
	pod_exec "$c" 'echo udp | nc -u -w 2 wasi-echo-udp 7' >>"$OUT/$step.txt" 2>&1
	k logs wasi-echo-udp
	k delete pods -l wasi-check=udp --wait=false
	result SKIP "the tab node has no UDP for components yet (phases 4 and 5); the results of runwasi are in $OUT/$step.txt"
}
