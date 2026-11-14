# Tabernetes: a Kubernetes node that joins a control plane running in a browser.
ARG GO_IMAGE=mirror.gcr.io/library/golang:1.27-trixie
ARG BASE_IMAGE=mirror.gcr.io/library/debian:trixie-slim
ARG KUBERNETES_VERSION=v1.37.2

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS agent
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY agent/go.mod agent/go.sum ./
RUN go mod download
COPY agent/ ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /tabernetes .
# The license files of the Go modules compiled into the agent, and of Go.
RUN set -eu; \
    d=/out/usr/share/doc/tabernetes/licenses/agent; \
    mkdir -p "$d/go"; cp "$(go env GOROOT)/LICENSE" "$(go env GOROOT)/PATENTS" "$d/go/"; \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
      go list -deps -f '{{if not .Standard}}{{with .Module}}{{if not .Main}}{{.Path}} {{.Dir}}{{end}}{{end}}{{end}}' . | sort -u > /tmp/modules; \
    while read -r mod dir; do \
      mkdir -p "$d/$mod"; \
      for f in "$dir"/LICEN[CS]E* "$dir"/COPYING* "$dir"/NOTICE* "$dir"/PATENTS*; do if [ -f "$f" ]; then cp "$f" "$d/$mod/"; fi; done; \
      if [ -z "$(ls -A "$d/$mod")" ]; then echo "no license file in $mod ($dir)" >&2; exit 1; fi; \
    done < /tmp/modules

FROM ${BASE_IMAGE} AS download
ARG DEBIAN_FRONTEND=noninteractive
ARG TARGETARCH
ARG KUBERNETES_VERSION
ARG CONTAINERD_VERSION=2.4.2
ARG RUNC_VERSION=v1.5.2
ARG CNI_PLUGINS_VERSION=v1.9.1
ARG CRICTL_VERSION=v1.37.0
# The shim of runwasi runs WebAssembly pods (runtimeClassName: wasmtime).
# renovate: github-releases=containerd/runwasi versioning=regex:^containerd-shim-wasmtime/v(?<major>\d+)\.(?<minor>\d+)\.(?<patch>\d+)$
ARG WASMTIME_SHIM_VERSION=containerd-shim-wasmtime/v0.6.1
RUN apt-get update && apt-get install -y --no-install-recommends binutils ca-certificates curl && rm -rf /var/lib/apt/lists/*
WORKDIR /out
# Every download is checked against the sha256 its release publishes. That
# catches broken or swapped files on the way, not a compromised release.
RUN set -eux; \
    get() { \
      name="${1##*/}"; \
      curl -fsSLo "/tmp/${name}" "$1"; \
      sum="$(curl -fsSL "$2" | awk -v f="${name}" 'NF == 1 { print $1; exit } $2 == f || $2 == "*" f { print $1; exit }')"; \
      [ -n "${sum}" ] || { echo "no checksum for ${name} in $2" >&2; exit 1; }; \
      echo "${sum}  /tmp/${name}" | sha256sum -c -; \
    }; \
    mkdir -p usr/local/bin opt/cni/bin; \
    url="https://dl.k8s.io/release/${KUBERNETES_VERSION}/bin/linux/${TARGETARCH}/kubelet"; \
    get "${url}" "${url}.sha256"; \
    mv /tmp/kubelet usr/local/bin/kubelet; \
    url="https://github.com/containerd/containerd/releases/download/v${CONTAINERD_VERSION}/containerd-${CONTAINERD_VERSION}-linux-${TARGETARCH}.tar.gz"; \
    get "${url}" "${url}.sha256sum"; \
    tar -xzf "/tmp/${url##*/}" -C usr/local bin/containerd bin/containerd-shim-runc-v2; \
    url="https://github.com/opencontainers/runc/releases/download/${RUNC_VERSION}"; \
    get "${url}/runc.${TARGETARCH}" "${url}/runc.sha256sum"; \
    mv "/tmp/runc.${TARGETARCH}" usr/local/bin/runc; \
    url="https://github.com/containernetworking/plugins/releases/download/${CNI_PLUGINS_VERSION}/cni-plugins-linux-${TARGETARCH}-${CNI_PLUGINS_VERSION}.tgz"; \
    get "${url}" "${url}.sha256"; \
    tar -xzf "/tmp/${url##*/}" -C opt/cni/bin ./bridge ./host-local ./loopback ./portmap; \
    url="https://github.com/kubernetes-sigs/cri-tools/releases/download/${CRICTL_VERSION}/crictl-${CRICTL_VERSION}-linux-${TARGETARCH}.tar.gz"; \
    get "${url}" "${url}.sha256"; \
    tar -xzf "/tmp/${url##*/}" -C usr/local/bin crictl; \
    case "${TARGETARCH}" in amd64) arch=x86_64 ;; arm64) arch=aarch64 ;; *) echo "no wasmtime shim for ${TARGETARCH}" >&2; exit 1 ;; esac; \
    url="https://github.com/containerd/runwasi/releases/download/${WASMTIME_SHIM_VERSION}"; \
    get "${url}/containerd-shim-wasmtime-${arch}-linux-musl.tar.gz" "${url}/SHA256SUMS"; \
    tar -xzf "/tmp/containerd-shim-wasmtime-${arch}-linux-musl.tar.gz" --no-same-owner -C usr/local/bin ./containerd-shim-wasmtime-v1; \
    rm -f /tmp/*.tar.gz /tmp/*.tgz; \
    # crictl and the CNI plugins ship with symbols: about 12 MB less compressed
    strip usr/local/bin/crictl opt/cni/bin/*; \
    chmod +x usr/local/bin/*; \
    # the pause image of this containerd, for the images stage
    usr/local/bin/containerd config default | sed -n "s/^ *sandbox = [\"']\(.*\)[\"']\$/\1/p" > /pause-image; \
    grep . /pause-image
# The license (and NOTICE) files of the binaries above, from the tags they were
# built from. A NOTICE is optional; anything but 200 or 404 fails the build.
RUN set -eu; \
    fetch() { \
      code="$(curl -sSL -o "$2" -w '%{http_code}' "$1")"; \
      case "${code}" in \
        200) ;; \
        404) rm -f "$2"; if [ "$3" != optional ]; then echo "$1: not found" >&2; return 1; fi ;; \
        *) echo "$1: HTTP ${code}" >&2; return 1 ;; \
      esac; \
    }; \
    lic() { \
      d="/out/usr/share/doc/tabernetes/licenses/$1"; mkdir -p "${d}"; \
      fetch "https://raw.githubusercontent.com/$2/LICENSE" "${d}/LICENSE" required; \
      fetch "https://raw.githubusercontent.com/$2/NOTICE" "${d}/NOTICE" optional; \
    }; \
    lic kubelet "kubernetes/kubernetes/${KUBERNETES_VERSION}"; \
    lic containerd "containerd/containerd/v${CONTAINERD_VERSION}"; \
    lic runc "opencontainers/runc/${RUNC_VERSION}"; \
    lic cni-plugins "containernetworking/plugins/${CNI_PLUGINS_VERSION}"; \
    lic crictl "kubernetes-sigs/cri-tools/${CRICTL_VERSION}"; \
    lic containerd-shim-wasmtime "containerd/runwasi/${WASMTIME_SHIM_VERSION}"; \
    ls -R /out/usr/share/doc/tabernetes/licenses

# pasta from upstream: Debian trixie stays on a snapshot from May 2025. A fixed
# commit, so the hash checks what git fetches. passt is GPL-2.0-or-later: the
# image carries the complete source of this commit (/usr/src), which is all
# that "make" builds from, and its license files.
FROM ${BASE_IMAGE} AS passt
ARG DEBIAN_FRONTEND=noninteractive
ARG PASST_COMMIT=6aa4f88bd9846411edff3ed70460475b538addca
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates gcc git libc6-dev make && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN set -eux; \
    git clone --quiet https://passt.top/passt .; \
    git -c advice.detachedHead=false checkout --quiet "${PASST_COMMIT}"; \
    make; \
    mkdir -p /out/usr/local/bin; \
    # passt.avx2 and pasta.avx2 exist on x86_64 only; pasta is a link to passt
    for f in passt passt.avx2 pasta pasta.avx2; do if [ -e "$f" ]; then cp -d "$f" /out/usr/local/bin/; fi; done; \
    /out/usr/local/bin/pasta --version; \
    mkdir -p /out/usr/src /out/usr/share/doc/tabernetes/licenses/passt; \
    git archive --format=tar.gz --prefix="passt-${PASST_COMMIT}/" -o "/out/usr/src/passt-${PASST_COMMIT}.tar.gz" "${PASST_COMMIT}"; \
    for f in COPYING* LICEN[CS]E*; do if [ -e "$f" ]; then cp -r "$f" /out/usr/share/doc/tabernetes/licenses/passt/; fi; done; \
    if [ -z "$(ls -A /out/usr/share/doc/tabernetes/licenses/passt)" ]; then echo "no license files in passt" >&2; exit 1; fi

# pause, kube-proxy, flannel and CoreDNS: every join needs them, and their
# downloads sometimes stall on Docker Desktop. The agent serves them to
# containerd from this OCI image layout (agent/imagemirror.go). The images come
# from the addon manifests of the control plane, so they always match:
#   docker build --build-context addons=k8s/tabernetes/addons node
# Not the ingress-nginx controller: it is big, and not every join needs it.
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS images
ARG TARGETARCH
ARG KUBERNETES_VERSION
ARG CRANE_VERSION=v0.22.1
RUN CGO_ENABLED=0 go install "github.com/google/go-containerregistry/cmd/crane@${CRANE_VERSION}"
COPY --from=addons *.yaml /addons/
COPY --from=download /pause-image /pause-image
# One crane call per image in a fixed order, so index.json is the same on every build.
RUN set -eux; \
    sed -n 's/^ *image: *//p' /addons/*.yaml | grep -v /ingress-nginx/ | sed "s/KUBERNETES_VERSION/${KUBERNETES_VERSION}/" | cat /pause-image - | sort -u > /images; \
    out=/out/usr/share/tabernetes/images; \
    while read -r image; do \
      crane pull --platform "linux/${TARGETARCH}" --format oci --annotate-ref "${image}" "${out}"; \
    done < /images; \
    cat "${out}/index.json"

FROM ${BASE_IMAGE}
# only while building, not in the image
ARG DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates iproute2 iptables mount nftables util-linux \
 && rm -rf /var/lib/apt/lists/*
COPY --from=download /out/ /
COPY --from=passt /out/ /
COPY --from=images /out/ /
COPY --from=agent /tabernetes /usr/local/bin/tabernetes
COPY --from=agent /out/ /
# What is in this image and under which license: /usr/share/doc/tabernetes/
COPY NOTICE.txt /usr/share/doc/tabernetes/NOTICE.txt
# containerd calls runc through the agent when the node is not privileged
# and the wasmtime shim, see agent/wasm.go
RUN ln -s tabernetes /usr/local/bin/tabernetes-runc \
 && ln -s tabernetes /usr/local/bin/tabernetes-wasmtime
COPY crictl.yaml /etc/crictl.yaml
# containerd needs a real file system for its overlayfs snapshots
VOLUME ["/var/lib/containerd"]
ENTRYPOINT ["/usr/local/bin/tabernetes"]
