# Contributing to steerd

Issues, discussions and pull requests are all welcome.

## Getting started

You need Go ≥1.22; nothing else — the module has zero dependencies.

```bash
git clone https://github.com/JaydenCJ/steerd && cd steerd
go build ./...
go test ./...
bash scripts/smoke.sh
```

`scripts/smoke.sh` builds the binary, starts the steerable demo agent in
the background, and drives a full steering session against it over the
real Unix socket — pause, redirect-while-paused, resume, graceful cancel,
error exit codes; it must finish by printing `SMOKE OK`.

## Before you open a pull request

1. `gofmt -l .` reports nothing (formatting is enforced).
2. `go vet ./...` passes with no findings.
3. `go test ./...` passes (93 deterministic tests, no network, no sleeps).
4. `bash scripts/smoke.sh` prints `SMOKE OK`.
5. Add tests for behavior changes; keep logic in pure, unit-testable
   modules. Synchronise tests through protocol acknowledgements, never
   through timing — a test that needs a sleep is a test with a bug.

## Ground rules

- Keep dependencies at zero; adding one needs strong justification in
  the PR.
- No network calls, ever — steerd speaks only over local Unix domain
  sockets. No telemetry.
- The acknowledgement contract is sacred: every accepted directive must
  resolve as applied or rejected, and on any single connection the
  accepted ack is written before the applied ack. Changes to
  `channel.go` must preserve both invariants and test them.
- Wire-format changes need a matching update to `docs/protocol.md` and
  must ignore unknown fields (forward compatibility).
- Code comments and doc comments are written in English.

## Reporting bugs

Include the output of `steerd version`, the full command you ran, the
`steerd status --format json` output at the time, and — for protocol
issues — a transcript of the frames (`nc -U <socket>` shows them; they
are plain JSON lines).

## Security

Please do not open public issues for security problems; use GitHub's
private vulnerability reporting on this repository instead.
