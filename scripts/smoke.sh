#!/usr/bin/env bash
# End-to-end smoke test for steerd: builds the binary, starts the steerable
# demo agent in the background, and drives a full steering session against
# it over the real Unix socket — pause, inject a correction, resume, cancel
# — asserting on real CLI output and exit codes at every stage.
# No network, idempotent, finishes in seconds.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKDIR="$(mktemp -d)"
DEMO_PID=""
WATCH_PID=""
cleanup() {
  [ -n "$DEMO_PID" ] && kill "$DEMO_PID" 2>/dev/null || true
  [ -n "$WATCH_PID" ] && kill "$WATCH_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

fail() {
  echo "SMOKE FAIL: $*" >&2
  exit 1
}

# wait_for <description> <predicate...>: poll a condition briefly.
wait_for() {
  local what="$1"; shift
  for _ in $(seq 1 100); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 0.05
  done
  fail "timed out waiting for $what"
}

BIN="$WORKDIR/steerd"
SOCK="$WORKDIR/agent.sock"
DEMO_LOG="$WORKDIR/demo.log"
WATCH_LOG="$WORKDIR/watch.log"

echo "1. build"
(cd "$ROOT" && go build -o "$BIN" ./cmd/steerd) || fail "go build failed"

echo "2. version matches manifest"
"$BIN" version | grep -qx "steerd 0.1.0" || fail "version mismatch"

echo "3. start the demo agent in the background"
"$BIN" demo --socket "$SOCK" --steps 500 --interval 25ms \
  --task "summarize the incident report" > "$DEMO_LOG" &
DEMO_PID=$!
wait_for "the control socket" test -S "$SOCK"

echo "4. status reports a running agent"
STATUS="$("$BIN" status --socket "$SOCK")"
echo "$STATUS" | grep -q "agent    steerd-demo" || fail "agent name missing"
echo "$STATUS" | grep -q "state    running" || fail "state should be running"
echo "$STATUS" | grep -q "task     summarize the incident report" || fail "task missing"

echo "5. watch subscribes to the event stream"
"$BIN" watch --socket "$SOCK" > "$WATCH_LOG" &
WATCH_PID=$!
wait_for "the watch banner" grep -q "watching steerd-demo" "$WATCH_LOG"

echo "6. resume while running is cleanly rejected (exit 1)"
set +e
"$BIN" resume --socket "$SOCK" 2>"$WORKDIR/rejected.err"
[ $? -eq 1 ] || fail "rejected directive should exit 1"
set -e
grep -q "resume rejected: agent is not paused" "$WORKDIR/rejected.err" \
  || fail "rejection reason missing"

echo "7. pause is acknowledged twice and takes effect"
OUT="$("$BIN" pause --socket "$SOCK" --reason "operator check")"
echo "$OUT" | grep -q "pause: accepted (seq " || fail "accepted ack missing"
echo "$OUT" | grep -q "pause: applied at step " || fail "applied ack missing"
"$BIN" status --socket "$SOCK" | grep -q "state    paused" || fail "agent should be paused"

echo "8. redirect is queued while paused (--no-wait)"
"$BIN" redirect --socket "$SOCK" --no-wait \
  --message "focus on the timeline section" \
  | grep -q "redirect: accepted" || fail "redirect not accepted"

echo "9. resume releases the loop and delivers the correction"
"$BIN" resume --socket "$SOCK" | grep -q "resume: applied at step " \
  || fail "resume not applied"
wait_for "the redirect to reach the loop" \
  grep -q 'redirect applied (append): "focus on the timeline section"' "$DEMO_LOG"

echo "10. cancel stops the demo gracefully"
"$BIN" cancel --socket "$SOCK" --reason "smoke test done" \
  | grep -q "cancel: applied at step " || fail "cancel not applied"
wait "$DEMO_PID" || fail "demo should exit 0 after cancel"
DEMO_PID=""
grep -q "cancelled at step .* (reason: smoke test done)" "$DEMO_LOG" \
  || fail "cancel narration missing from demo output"

echo "11. watch saw the whole story and exited on done"
wait "$WATCH_PID" || fail "watch should exit 0 after the done event"
WATCH_PID=""
for event in paused redirected resumed cancelling done; do
  grep -q "^$event " "$WATCH_LOG" || fail "watch log missing $event event"
done

echo "12. usage and connection errors use distinct exit codes"
set +e
"$BIN" redirect --socket "$SOCK" >/dev/null 2>&1
[ $? -eq 2 ] || fail "missing --message should exit 2"
"$BIN" status --socket "$WORKDIR/nobody-home.sock" >/dev/null 2>&1
[ $? -eq 3 ] || fail "unreachable socket should exit 3"
set -e

echo "13. socket file is gone after the agent shut down"
test ! -e "$SOCK" || fail "socket file should be removed"

echo "SMOKE OK"
