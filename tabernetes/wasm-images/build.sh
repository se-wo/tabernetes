#!/bin/bash
# build.sh <outdir>: build the WebAssembly images of images.txt into the OCI
# mirror <outdir> that the page reads same-origin (see ocimirror/main.go for
# the layout). Publish <outdir> next to tabernetes.js as oci/, or point the
# data-oci attribute of the terminal element at it.
set -euo pipefail
out=$(mkdir -p "$1" && cd "$1" && pwd)
cd "$(dirname "$0")"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
go build -o "$tmp/ocimirror" ./ocimirror

while read -r kind ref src; do
  case "$kind" in
  "" | \#*) continue ;;
  image | artifact)
    [ -e "$tmp/$src.wasm" ] || GOOS=wasip1 GOARCH=wasm go build -trimpath -ldflags=-s -o "$tmp/$src.wasm" "./$src"
    flags=()
    [ "$kind" = image ] && flags=(-gzip)
    "$tmp/ocimirror" "$kind" -out "$out" -ref "$ref" -wasm "$tmp/$src.wasm" "${flags[@]}"
    ;;
  mirror)
    if "$tmp/ocimirror" pull -out "$out" -ref "$ref"; then continue; fi
    if [ -n "$src" ] && [ -n "${RUNWASI_SRC:-}" ]; then
      echo "building $src from $RUNWASI_SRC instead" >&2
      cargo build --quiet --release --target wasm32-wasip1 --manifest-path "$RUNWASI_SRC/Cargo.toml" -p "$src"
      "$tmp/ocimirror" image -out "$out" -ref "$ref" -wasm "$RUNWASI_SRC/target/wasm32-wasip1/release/$src.wasm"
    else
      echo "warning: $ref is not in the mirror" >&2
    fi
    ;;
  *) echo "images.txt: unknown kind $kind" >&2; exit 1 ;;
  esac
done < images.txt
