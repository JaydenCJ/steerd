# steerd examples

Two runnable examples, both fully offline.

## `steer-session.sh` — a scripted steering session

Drives the built-in demo agent through the whole protocol: status, pause,
redirect-while-paused, resume, graceful cancel. Uses only the `steerd`
binary — no code required.

```bash
go build -o steerd ./cmd/steerd
bash examples/steer-session.sh
```

## `embed/` — embedding steerd in your own loop

The smallest realistic integration: ~30 lines of loop code calling
`Checkpoint` between work items. Start it, then steer it from a second
terminal.

```bash
go run ./examples/embed /tmp/agent.sock          # terminal 1
steerd pause  --socket /tmp/agent.sock            # terminal 2
steerd redirect --socket /tmp/agent.sock --message "skip flaky suites"
steerd resume --socket /tmp/agent.sock
steerd cancel --socket /tmp/agent.sock --reason "enough"
```

The integration contract is one method: call `Checkpoint` wherever your
loop finishes a unit of work, apply the returned redirects, and stop when
the decision says `Cancelled`. Everything else — the socket, admission,
acknowledgements, event fan-out — is handled by the channel.
