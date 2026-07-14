#!/usr/bin/env bash
# A complete scripted steering session against the built-in demo agent.
# Everything happens on your machine over one Unix socket; run it from the
# repository root after `go build -o steerd ./cmd/steerd`.
set -euo pipefail

BIN="${BIN:-./steerd}"
WORKDIR="$(mktemp -d)"
SOCK="$WORKDIR/agent.sock"
DEMO_PID=""
trap 'kill $DEMO_PID 2>/dev/null || true; rm -rf "$WORKDIR"' EXIT

echo "== starting the demo agent (background) =="
"$BIN" demo --socket "$SOCK" --steps 100 --interval 300ms \
  --task "triage the flaky-test backlog" &
DEMO_PID=$!
until [ -S "$SOCK" ]; do sleep 0.05; done

echo
echo "== who is on the other end? =="
"$BIN" status --socket "$SOCK"

sleep 1
echo
echo "== pause it mid-run (blocks until the pause takes effect) =="
"$BIN" pause --socket "$SOCK" --reason "operator wants a look"
"$BIN" status --socket "$SOCK"

echo
echo "== inject a correction while it is held =="
"$BIN" redirect --socket "$SOCK" --no-wait \
  --message "ignore tests quarantined before June"

echo
echo "== resume; the correction is applied in the same decision =="
"$BIN" resume --socket "$SOCK"

sleep 1
echo
echo "== seen enough — cancel gracefully =="
"$BIN" cancel --socket "$SOCK" --reason "session over"
wait "$DEMO_PID"
echo "demo agent exited cleanly"
