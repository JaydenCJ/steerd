// Package steerd implements steer/1, an in-band control protocol for
// long-running agent loops, carried over a Unix domain socket.
//
// An agent loop embeds a [Channel] and calls [Channel.Checkpoint] between
// units of work. Any local process can then dial the socket and steer the
// loop while it runs: pause it, inject a corrective instruction, or cancel
// it — without signals, without killing the process, and with explicit
// acknowledgements for every directive.
//
// Every directive is acknowledged twice: once when the agent accepts it
// onto its directive queue ("accepted", the directive is durable in-band)
// and once when it actually takes effect at a checkpoint ("applied", with
// the step number it landed on). Directives the agent cannot honour are
// answered with a "rejected" acknowledgement carrying a reason, never
// dropped silently.
//
// steerd is deliberately not a supervisor and not a chat interface: it
// never starts, restarts, or owns the agent process, and it transports
// control directives, not conversation. The wire format is
// newline-delimited JSON, documented in docs/protocol.md.
package steerd
