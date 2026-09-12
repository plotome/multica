#!/usr/bin/env bash
# Read-only acceptance entry point; does not deploy or invoke a real agent.
set -euo pipefail
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
python3 "$SCRIPT_DIR/check-selfhost-patches.py" "$@"
python3 "$SCRIPT_DIR/check-selfhost-patches.test.py"
cd "$SCRIPT_DIR/../server"
# Whole packages avoid silently omitting newly added regression test names.
"$SCRIPT_DIR/go-test-with-agent-cli-guard.sh" -- \
  go test -race -p 2 -parallel 2 \
  ./internal/daemon ./internal/daemon/execenv ./internal/daemon/repocache ./pkg/agent \
  -count=1 -timeout=10m
