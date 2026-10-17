#!/bin/sh
set -eu

version=${VERSION:-$(sh ./scripts/version.sh)}
out=${OUT_DIR:-dist}
mkdir -p "$out"

for arch in amd64 arm64; do
    name="q-agent_linux_${arch}"
    printf 'building %s\n' "$name"
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
        -ldflags="-s -w -X main.version=$version" -o "$out/$name" ./cmd/q-agent
done
