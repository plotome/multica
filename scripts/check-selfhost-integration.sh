#!/usr/bin/env bash
# Read-only acceptance entry point; does not deploy or invoke a real agent.
set -euo pipefail
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
if ! { [ "$#" -eq 0 ] || { [ "$#" -eq 2 ] && [ "$1" = "--current" ]; }; }; then
  echo "usage: bash scripts/check-selfhost-integration.sh [--current deployed-commit]" >&2
  exit 2
fi
# Always audit HEAD, the tree whose tests run below; --target belongs only to
# the standalone static checker and cannot select a different test subject.
python3 "$SCRIPT_DIR/check-selfhost-patches.py" "$@"
python3 "$SCRIPT_DIR/check-selfhost-patches.test.py"
cd "$SCRIPT_DIR/../server"
# Whole packages avoid silently omitting newly added regression test names.
# Do not leak the invoking task's credentials or config-root overrides into
# tests. Preserve the real HOME for the Go cache; test fixtures own their homes.
env -i PATH="$PATH" HOME="$HOME" TMPDIR="${TMPDIR:-/tmp}" \
  "$SCRIPT_DIR/go-test-with-agent-cli-guard.sh" -- \
  go test -race -p 2 -parallel 2 \
  ./internal/daemon ./internal/daemon/execenv ./internal/daemon/repocache ./pkg/agent \
  -count=1 -timeout=10m
