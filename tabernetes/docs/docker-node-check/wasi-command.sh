#!/bin/bash
# shellcheck disable=SC2154 # here, step, N, OUT and crictl_ns come from check.sh
# wasi-command.sh: the step wasi-command of check.sh. check.sh sources it.
#
# Phase 1 of ../wasi-sockets.md: the tests of section 10.3 with "Docker" in
# the column "Nodes", on the real runwasi of node-1. runwasi is the reference
# for the tab node. The objects are in wasi-command.yaml. One PASS or FAIL
# line for each test:
#
#   CO1, CO5, FS1  arguments, environment, a file of a volume, stdin
#   CO18           exit codes 0, 1 and 1 for exit(0), exit(1) and exit(3);
#                  exit-with-code gets the status of CO11
#   CO2            two exports: the world of the first export
#   CO3, CO4       the function of the entrypoint; without it, the status of
#                  phase 0
#   CO11, CO12     an import of WASI 0.3: the status of phase 0 and where the
#                  name of the import shows
#   FS2            a write to a read-only volume fails
#   FS3            a restart shows the file of the run before it

# pod_state <pod>: "<phase> <restarts> <exit code> <reason>" of the first
# container, from its last end if it runs again.
pod_state() {
	local s
	s="$(tab run "kubectl get pod $1 -o jsonpath='{.status.phase} {.status.containerStatuses[0].restartCount} {.status.containerStatuses[0].state.terminated.exitCode} {.status.containerStatuses[0].state.terminated.reason}'" 2>&1 | tail -1)"
	echo "$s" >>"$OUT/$step.txt"
	echo "$s"
}

step_wasi_command() {
	wasi_images_ready || return 0
	local id out s logs where
	node_ns
	apply_file "$here/wasi-command.yaml"

	# CO1, CO5, FS1. The component waits for a line on stdin; crictl attach
	# gives it the line. Then the component ends, and the attach with it.
	if ! wait_for 'kubectl get pod wasi-cmd' 'Running|Completed' 240; then
		k describe pod wasi-cmd
	fi
	id="$(node_crictl ps -a -q --label io.kubernetes.pod.name=wasi-cmd | head -1)"
	echo "\$ crictl attach -i $id <<<'line from stdin'" >>"$OUT/$step.txt"
	# shellcheck disable=SC2086 # crictl_ns is a list of flags
	printf 'line from stdin\n' | timeout 60 docker exec -i "$N" nsenter $crictl_ns crictl attach -i "$id" >>"$OUT/$step.txt" 2>&1
	wait_for "kubectl get pod wasi-cmd" 'Completed|Error' 60
	logs="$(tab run 'kubectl logs wasi-cmd' 2>&1)"
	printf '$ kubectl logs wasi-cmd\n%s\n' "$logs" >>"$OUT/$step.txt"
	s="$(pod_state wasi-cmd)"
	out=""
	grep -qx 'arg first arg' <<<"$logs" || out="$out, argument 'first arg'"
	grep -qx 'arg second' <<<"$logs" || out="$out, argument 'second'"
	grep -qx 'env GREETING=hello component' <<<"$logs" || out="$out, env GREETING"
	grep -qx 'file /config/file: from a volume' <<<"$logs" || out="$out, file of the volume"
	grep -qx 'stdin line from stdin' <<<"$logs" || out="$out, stdin"
	[ "$s" = "Succeeded 0 0 Completed" ] || out="$out, state '$s'"
	if [ -z "$out" ]; then
		result PASS "CO1, CO5, FS1: arguments, environment, file of a volume and stdin"
	else
		result FAIL "CO1, CO5, FS1: missing or wrong:${out#,}"
	fi

	# The other pods ran at the same time as wasi-cmd. They end in a few
	# seconds; wasi-fs-rw starts again after 10 seconds.
	wait_for 'kubectl get pod wasi-fs-rw' 'wasi-fs-rw +[0-9/]+ +[A-Za-z]+ +[1-9]' 120
	k get pods -l wasi-check=command -o wide

	# CO18
	out="$(pod_state wasi-exit0 | cut -d' ' -f3) $(pod_state wasi-exit1 | cut -d' ' -f3) $(pod_state wasi-exit3 | cut -d' ' -f3)"
	s="$(pod_state wasi-exitcode)"
	if [ "$out" = "0 1 1" ] && [ "$s" = "Failed 0 137 Error" ]; then
		result PASS "CO18: exit(0), exit(1), exit(3) give $out; exit-with-code gives 137 Error"
	else
		result FAIL "CO18: exit(0), exit(1), exit(3) give '$out' (want 0 1 1); exit-with-code gives '$s' (want Failed 0 137 Error)"
	fi

	# CO2. With wasi:cli/run first, runwasi selects the command world. Its
	# linker has no wasi:http, so the import of wasi:http/types for the second
	# export fails as for CR6. With wasi:http/incoming-handler first, runwasi
	# selects the proxy world and serves the requests.
	s="$(pod_state wasi-both)"
	k logs wasi-both
	if wait_for 'kubectl get pod wasi-bothrev' 'wasi-bothrev +1/1 +Running' 60; then
		out="ready"
	else
		out="not ready"
		k describe pod wasi-bothrev
	fi
	if [ "$s" = "Failed 0 137 Error" ] && [ "$out" = ready ]; then
		result PASS "CO2: wasi:cli/run first gives the command world (137 Error, no wasi:http); wasi:http/incoming-handler first gives the proxy world (ready)"
	else
		result FAIL "CO2: wasi:cli/run first gives '$s' (want Failed 0 137 Error); wasi:http/incoming-handler first is $out (want ready)"
	fi

	# CO3, CO4
	logs="$(tab run 'kubectl logs wasi-core-go' 2>&1)"
	printf '$ kubectl logs wasi-core-go\n%s\n' "$logs" >>"$OUT/$step.txt"
	s="$(pod_state wasi-core-go)"
	out="$(pod_state wasi-core-start)"
	k logs wasi-core-start
	if [ "$s" = "Succeeded 0 0 Completed" ] && grep -qx 'go was called' <<<"$logs" && [ "$out" = "Failed 0 137 Error" ]; then
		result PASS "CO3, CO4: the entrypoint #go calls go; without it, 137 Error"
	else
		result FAIL "CO3, CO4: #go gives '$s' (want Succeeded 0 0 Completed and 'go was called'); without it '$out' (want Failed 0 137 Error)"
	fi

	# CO11, CO12. The name of the import: in the log of the pod, in the
	# message of the container status or only in the log of the node.
	s="$(pod_state wasi-wasi03)"
	where=""
	tab run 'kubectl logs wasi-wasi03' 2>&1 | tee -a "$OUT/$step.txt" | grep -q 'monotonic-clock@0.3.0' && where="$where pod log,"
	tab run "kubectl get pod wasi-wasi03 -o jsonpath='{.status.containerStatuses[0].state.terminated.message}'" 2>&1 |
		tee -a "$OUT/$step.txt" | grep -q 'monotonic-clock@0.3.0' && where="$where status message,"
	docker logs "$N" 2>&1 | grep -m3 'monotonic-clock@0.3.0' | tee -a "$OUT/$step.txt" | grep -q . && where="$where node log,"
	where="${where%,}"
	if [ "$s" = "Failed 0 137 Error" ]; then
		result PASS "CO11, CO12: a WASI 0.3 import gives 137 Error; the name of the import is in:${where:- no text}"
	else
		result FAIL "CO11, CO12: a WASI 0.3 import gives '$s' (want Failed 0 137 Error)"
	fi

	# FS2
	logs="$(tab run 'kubectl logs wasi-fs-ro' 2>&1)"
	printf '$ kubectl logs wasi-fs-ro\n%s\n' "$logs" >>"$OUT/$step.txt"
	if grep -q '^write /ro/x: Err(' <<<"$logs"; then
		result PASS "FS2: a write to a read-only volume fails: $(grep '^write ' <<<"$logs")"
	else
		result FAIL "FS2: a write to a read-only volume: '$(grep '^write ' <<<"$logs")'"
	fi

	# FS3
	logs="$(tab run 'kubectl logs wasi-fs-rw' 2>&1)"
	printf '$ kubectl logs wasi-fs-rw\n%s\n' "$logs" >>"$OUT/$step.txt"
	s="$(pod_state wasi-fs-rw)"
	if [ "$(cut -d' ' -f2 <<<"$s")" -ge 1 ] 2>/dev/null && grep -qx 'old /rw/x: written' <<<"$logs" && grep -q '^write /rw/x: Ok' <<<"$logs"; then
		result PASS "FS3: after a restart, the component reads the file of the run before it"
	else
		result FAIL "FS3: state '$s', log: $(grep -E '^(old|write) ' <<<"$logs" | tr '\n' ' ')"
	fi
	k delete pods -l wasi-check=command --wait=false
}
