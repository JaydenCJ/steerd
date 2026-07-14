# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-07-13

### Added

- The steer/1 wire protocol: newline-delimited JSON frames over a Unix
  domain socket, 64 KiB frame cap with resynchronisation after oversized
  or malformed input, hello handshake with protocol check, and forward-
  compatible field handling (`docs/protocol.md`).
- Agent-side `Channel` with a one-method integration contract:
  `Checkpoint` applies queued directives in admission order, blocks while
  paused, and returns a `Decision` carrying injected redirects and
  graceful cancellation.
- Two-stage acknowledgements for every directive — `accepted` at
  admission (with queue seq) and `applied` at the checkpoint it landed on
  (with step) — plus immediate `rejected` acks with reasons; accepted is
  guaranteed to precede applied on the wire, and orphaned directives are
  resolved (never dropped) on cancel or shutdown.
- Admission control against the projected state: duplicate pauses,
  stray resumes, directives behind a pending cancel, and queue overflow
  (configurable backlog, default 64) are refused with precise reasons.
- Controller `Client` with lock-step round-trips (`Pause`, `Resume`,
  `Redirect`, `Cancel`, `Status`, `Subscribe`), typed rejection errors,
  and optional read deadlines.
- `steerd` CLI: pause / resume / redirect / cancel with `--no-wait`,
  status and watch in text or JSON, `STEERD_SOCKET` fallback, and
  distinct exit codes (0 ok, 1 rejected, 2 usage, 3 connection).
- Event stream for subscribers: step, paused, resumed, redirected,
  cancelling, done — consumed by `steerd watch`.
- Stale-socket takeover at listen time (probe dial, replace only if
  dead) and socket removal on close.
- A deterministic, steerable demo loop (`steerd demo`) and runnable
  examples (`examples/steer-session.sh`, `examples/embed/`).
- 93 deterministic offline tests (protocol, channel, client, CLI, demo —
  synchronised purely through acknowledgements) and `scripts/smoke.sh`.

[0.1.0]: https://github.com/JaydenCJ/steerd/releases/tag/v0.1.0
