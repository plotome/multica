#!/usr/bin/env bash
set -euo pipefail
# No database creation, container start, backend deployment or daemon lifecycle.
# Require the existing worktree fixture namespace and a loopback server.
if [[ ! "${DATABASE_URL:-}" =~ ^postgres(ql)?://[^/]+@(127\.0\.0\.1|localhost):[0-9]+/multica_multica_[0-9]+(\?.*)?$ ]]; then
  echo 'device maintenance tests require an existing loopback worktree fixture database' >&2
  exit 64
fi
cd "$(dirname "$0")/../server"
go run ./cmd/migrate up
if [ "${1:-}" = --consumer ]; then
  test -n "${DEVICE_CONSUMER_PYTHON:-}" && test -n "${DEVICE_CONSUMER_SCRIPT:-}"
  go test -race -tags=device_consumer ./internal/devicemaintenance -run TestDeviceMaintenanceConsumerClosedLoop -count=1 -timeout=120s -v
  exit
fi
go test -race ./internal/devicemaintenance ./internal/handler ./cmd/multica ./cmd/server -run 'DeviceMaintenance' -count=1 -timeout=120s -v
