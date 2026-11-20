#!/usr/bin/env bash
set -euo pipefail
# No database creation, container start, backend deployment or production lifecycle.
# --legacy-native alone exercises fresh test daemon profiles/launchd labels.
# Require the existing worktree fixture namespace and a loopback server.
if [[ ! "${DATABASE_URL:-}" =~ ^postgres(ql)?://[^/]+@(127\.0\.0\.1|localhost):[0-9]+/multica_multica_[0-9]+(\?.*)?$ ]]; then
  echo 'device maintenance tests require an existing loopback worktree fixture database' >&2
  exit 64
fi
cd "$(dirname "$0")/../server"
go run ./cmd/migrate up
if [ "${1:-}" = --legacy-native ]; then
  test -n "${NATIVE_OLD_CLI:-}" && test -n "${NATIVE_OLD_SHA256:-}"
  go test -tags=legacy_native ./cmd/server -run TestNativeOldDaemonExit -count=1 -timeout=600s -v
  exit
fi
if [ "${1:-}" = --consumer ]; then
  test -n "${DEVICE_CONSUMER_PYTHON:-}" && test -n "${DEVICE_CONSUMER_SCRIPT:-}"
  go test -race -tags=device_consumer ./internal/devicemaintenance -run TestDeviceMaintenanceConsumerClosedLoop -count=1 -timeout=120s -v
  exit
fi
go test -race ./internal/devicemaintenance ./internal/handler ./cmd/multica ./cmd/server -run 'DeviceMaintenance' -count=1 -timeout=120s -v
