#!/usr/bin/env bash
# Chạy lệnh `go` trong container golang:1.25 (máy không cần cài Go).
# Ví dụ: scripts/docker-go.sh test ./internal/host/...
set -euo pipefail
cd "$(dirname "$0")/.."
docker volume create ainovel-gomod >/dev/null
exec docker run --rm \
  -v ainovel-gomod:/go/pkg/mod \
  -v "$PWD":/src -w /src \
  -e GOWORK=off -e GOFLAGS=-buildvcs=false \
  golang:1.25 go "$@"
