#!/usr/bin/env bash
set -euo pipefail

task_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$task_root"
task_cache="${TMPDIR:-/tmp}/mole-performance-go-cache"
export GOCACHE="${GOCACHE:-$task_cache}"
task_binary="${1:-/tmp/mole-performance-20261007}"

# Integration tests require permission to bind localhost TCP sockets.
# Stop on failure; never install a binary whose full gate did not pass.
go test -buildvcs=false -race ./...
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -p '*_test.py'
if [[ "$(go env GOOS)" == "darwin" ]]; then
  go build -buildvcs=false -trimpath -ldflags='-s -w -linkmode external' -o "$task_binary" .
else
  go build -buildvcs=false -trimpath -ldflags='-s -w' -o "$task_binary" .
fi
printf 'Verified binary: %s\n' "$task_binary"
