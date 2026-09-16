#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
#
# Builds every release artifact into dist/.
#
# This lives in a script rather than inline in release.yml so CI can run
# the same code on every push. A release workflow's build recipe is
# otherwise exercised exactly once — at the moment a tag is pushed, which
# is the worst time to discover it is broken.
#
#   VERSION=1.2.3 ./releng/build-release.sh
#
# VERSION is stamped into the binaries via -ldflags and defaults to "dev".

set -euo pipefail

cd "$(dirname "$0")/.."

VERSION="${VERSION:-dev}"
LDFLAGS="-X main.version=${VERSION}"
OUT="${OUT:-dist}"

mkdir -p "$OUT"

# Server: amd64 Linux only (requires CGO for SQLite).
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -ldflags "$LDFLAGS" -o "$OUT/dirq-server-linux-amd64" ./cmd/dirq-server

# Agent and CLI: cross-compiled everywhere, no CGO.
#
# The loop variable is `arch`, not `GOARCH`. Naming it after the
# environment variable it feeds makes `GOARCH=$GOARCH cmd ... $GOARCH`
# read as though the expansion sees the assignment, which it does not —
# it worked only because both held the same value.
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -ldflags "$LDFLAGS" -o "$OUT/dirq-agent-linux-$arch" ./cmd/dirq-agent
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -ldflags "$LDFLAGS" -o "$OUT/dirq-linux-$arch"       ./cmd/dirq
done

CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$LDFLAGS" -o "$OUT/dirq-agent-windows-amd64.exe" ./cmd/dirq-agent
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$LDFLAGS" -o "$OUT/dirq-windows-amd64.exe"       ./cmd/dirq
CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build -ldflags "$LDFLAGS" -o "$OUT/dirq-agent-windows-arm64.exe" ./cmd/dirq-agent

CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags "$LDFLAGS" -o "$OUT/dirq-darwin-amd64" ./cmd/dirq
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags "$LDFLAGS" -o "$OUT/dirq-darwin-arm64" ./cmd/dirq

echo "built $(find "$OUT" -maxdepth 1 -type f | wc -l) artifacts into $OUT/ (version ${VERSION})"
