# The steer/1 wire protocol

steer/1 is the in-band control protocol steerd speaks over a Unix domain
socket. It is small on purpose: newline-delimited JSON objects ("frames"),
one object per line, at most 64 KiB per frame including the newline. Any
language that can open a Unix socket and encode JSON can steer an agent —
`nc -U` and `jq` are a working client.

## Roles and connections

The **agent** owns the socket (it listens); **controllers** dial it. Any
number of controllers may be connected at once. Each connection is
independent: directives are answered on the connection that sent them, and
a connection that sent `subscribe` additionally receives event frames.

On connect, the agent immediately sends a `hello` frame:

```json
{"type":"hello","proto":"steer/1","agent":"steerd-demo","pid":4711,
 "task":"summarize the release notes","state":"running","step":3}
```

A controller must check `proto` before sending anything; dialing some
other daemon's socket must fail at handshake, not mid-session.

## Frame types

| `type` | Direction | Purpose |
|---|---|---|
| `hello` | agent → controller | handshake, once per connection |
| `directive` | controller → agent | one steering operation |
| `ack` | agent → controller | admission / application / rejection of a directive |
| `state` | agent → controller | reply to a `status` directive |
| `event` | agent → controller | pushed to subscribers on every state change and step |

## Directives

```json
{"type":"directive","id":"d1","op":"pause","reason":"operator check"}
{"type":"directive","id":"d2","op":"redirect","message":"skip the appendix","mode":"append"}
{"type":"directive","id":"d3","op":"cancel","reason":"wrong branch"}
{"type":"directive","id":"d4","op":"resume"}
{"type":"directive","op":"status"}
{"type":"directive","op":"subscribe"}
```

`id` is chosen by the controller and echoed in every ack for that
directive; it is required for all ops except `status` and `subscribe`.
`redirect` requires a non-empty `message`; `mode` is `append` (default) or
`replace` — the distinction is advisory, interpreted by the loop.

## The acknowledgement contract

Every mutating directive (`pause`, `resume`, `redirect`, `cancel`)
resolves through exactly one of these sequences, never silently:

1. **accepted → applied** — admitted to the queue (`seq` = admission
   order), then took effect at a checkpoint (`step` = which one).
2. **accepted → rejected** — admitted, but the run ended (cancel or agent
   shutdown) before it could apply.
3. **rejected** — refused at admission with a `reason`.

```json
{"type":"ack","id":"d1","op":"pause","stage":"accepted","seq":5}
{"type":"ack","id":"d1","op":"pause","stage":"applied","seq":5,"step":12}
{"type":"ack","id":"d9","op":"resume","stage":"rejected","error":"agent is not paused"}
```

Wire-order invariant: on any single connection the `accepted` ack is
written before the `applied` ack for the same directive.

Admission rules (checked against the *projected* state, i.e. the current
state with the queue replayed over it):

| Directive | Rejected when | Reason string |
|---|---|---|
| any | channel closed | `channel is closed` |
| any but cancel | cancel applied or pending | `agent is cancelling` |
| `cancel` | cancel already applied or pending | `cancel already requested` |
| any | queue at capacity (default 64) | `directive queue is full` |
| `pause` | already paused or pause pending | `agent is already paused` |
| `resume` | not paused and no pause pending | `agent is not paused` |

## Checkpoint semantics

The agent applies queued directives only at checkpoints, in admission
order. `pause` holds the loop *inside* the checkpoint; directives arriving
while paused are applied immediately (still in order) without releasing
it — so a `redirect` sent during a pause is carried by the same decision
the loop receives when it is resumed. `cancel` resolves the checkpoint at
once, even while paused.

## Events

Subscribers receive one `event` frame per step and per state change:

```json
{"type":"event","event":"step","step":13,"state":"running","note":"step 13/200 draft"}
{"type":"event","event":"paused","step":13,"state":"paused","reason":"operator check"}
{"type":"event","event":"redirected","step":13,"state":"paused","message":"skip the appendix","mode":"append"}
{"type":"event","event":"resumed","step":13,"state":"running"}
{"type":"event","event":"cancelling","step":14,"state":"cancelling","reason":"wrong branch"}
{"type":"event","event":"done","step":14,"state":"done"}
```

`done` is always the final frame; after it the agent closes the socket.

## Robustness rules

- A malformed line or an oversized frame earns a `rejected` ack (empty
  `id`) and the connection stays open; the reader resynchronises at the
  next newline.
- Unknown fields in any frame are ignored (forward compatibility).
- A stale socket file (previous agent crashed) is detected by a probe
  dial and replaced at listen time; a live socket is never taken over.
