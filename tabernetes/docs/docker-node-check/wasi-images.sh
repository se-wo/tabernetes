#!/bin/bash
# shellcheck disable=SC2154 # here, step, N, OUT and crictl_ns come from check.sh
# wasi-images.sh: the step wasi-images of check.sh. check.sh sources it.
#
# The node has no internet without a proxy, so it cannot pull the test images
# of ../wasi-sockets.md (section 10.1) from ghcr.io. This step imports them
# from the OCI mirror of the page ($OCI) into the containerd of the node, as
# IMPORT=1 does for busybox (section 9.2, item 18). For each image of the
# kind "component" in wasm-images/images.txt, it takes the .wasm from the
# layer of the mirror and builds an image with the same reference and the
# same entrypoint in the Docker of the host. The pods use
# imagePullPolicy: Never.

wasi_images=()

# wasi_images_ready: prints SKIP for the current step if wasi-images did not
# import the test images in this run.
wasi_images_ready() {
	if [ ${#wasi_images[@]} -eq 0 ] && ! grep -q '^PASS wasi-images' "$OUT/summary.txt" 2>/dev/null; then
		result SKIP "the test images are not on node-1: run the step wasi-images first"
		return 1
	fi
}

step_wasi_images() {
	local oci="${OCI:-$here/../../.build/site/oci}" ctx kind ref crate repo name layer missing=""
	if [ ! -d "$oci" ]; then
		result FAIL "no OCI mirror in $oci: run mise run site, or set OCI"
		return 1
	fi
	ctx="$(mktemp -d)"
	while read -r kind ref crate; do
		[ "$kind" = component ] || continue
		repo="${ref%:*}"
		name="${repo##*/}.wasm"
		layer="$(jq -r '.layers[0].digest' "$oci/$repo/manifests/${ref##*:}" 2>>"$OUT/$step.txt")"
		rm -rf "${ctx:?}"/*
		if ! tar -xzf "$oci/blobs/sha256/${layer#sha256:}" -C "$ctx" "$name" 2>>"$OUT/$step.txt"; then
			result FAIL "$ref ($crate) is not in the OCI mirror $oci"
			rm -rf "$ctx"
			return 1
		fi
		printf 'FROM scratch\nCOPY %s /%s\nENTRYPOINT ["/%s"]\n' "$name" "$name" "$name" >"$ctx/Dockerfile"
		run docker build -q -t "$ref" "$ctx"
		wasi_images+=("$ref")
	done <"$here/../../wasm-images/images.txt"
	rm -rf "$ctx"
	docker cp "$(command -v ctr)" "$N:/usr/local/bin/ctr"
	run sh -c "docker save ${wasi_images[*]} | docker exec -i $N ctr -n k8s.io images import -"
	run docker exec "$N" crictl images
	for ref in "${wasi_images[@]}"; do
		grep -q "^${ref%:*} " "$OUT/$step.txt" || missing="$missing $ref"
	done
	if [ -n "$missing" ]; then
		wasi_images=()
		result FAIL "not in the node after the import:$missing"
		return 1
	fi
	result PASS "${#wasi_images[@]} test images imported into node-1"
}
