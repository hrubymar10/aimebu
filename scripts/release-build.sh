#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 1 ] || [[ ! "$1" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-(alpha|beta|rc)\.[0-9]+)?$ ]]; then
    echo "usage: scripts/release-build.sh vMAJOR.MINOR.PATCH[-alpha.N|-beta.N|-rc.N]" >&2
    exit 2
fi

tag="$1"
version="${tag#v}"
repo="$(cd "$(dirname "$0")/.." && pwd)"
dist="$repo/dist"
mkdir -p "$dist"
rm -f "$dist"/aimebu_"$version"_*.tar.gz "$dist/checksums.txt"

if command -v sha256sum >/dev/null 2>&1; then
    checksum=(sha256sum)
elif command -v shasum >/dev/null 2>&1; then
    checksum=(shasum -a 256)
else
    echo "release-build: sha256sum or shasum is required" >&2
    exit 1
fi

stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT
for os in darwin linux; do
    for arch in arm64 amd64; do
        archive="aimebu_${version}_${os}_${arch}.tar.gz"
        echo "building $archive"
        (cd "$repo" && CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
            go build -trimpath -ldflags "-s -w -X 'main.version=aimebu $tag'" \
            -o "$stage/aimebu" ./cmd/aimebu)
        tar -czf "$dist/$archive" -C "$stage" aimebu
        (cd "$dist" && "${checksum[@]}" "$archive") >>"$dist/checksums.txt"
    done
done
