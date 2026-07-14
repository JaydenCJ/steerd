package steerd

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ProtocolVersion identifies the wire protocol spoken over the socket.
// It is announced in the hello frame sent to every new connection.
const ProtocolVersion = "steer/1"

// MaxFrameSize is the maximum length in bytes of a single encoded frame,
// including the trailing newline. Frames beyond this limit are rejected on
// both ends; the reader resynchronises at the next newline so one oversized
// frame does not poison the connection.
const MaxFrameSize = 64 * 1024

// ErrFrameTooLarge is returned when an encoded frame exceeds MaxFrameSize.
var ErrFrameTooLarge = errors.New("steerd: frame exceeds 64 KiB limit")

// ErrMalformedFrame wraps decode failures that leave the stream intact
// (bad JSON, missing type). The reader may report them and keep going.
var ErrMalformedFrame = errors.New("steerd: malformed frame")

// Op is a directive operation a controller can send to an agent.
type Op string

// The steer/1 directive operations.
const (
	OpPause     Op = "pause"     // hold the loop at its next checkpoint
	OpResume    Op = "resume"    // release a paused (or pause-pending) loop
	OpRedirect  Op = "redirect"  // inject an instruction into the next decision
	OpCancel    Op = "cancel"    // stop the loop gracefully
	OpStatus    Op = "status"    // request a state frame (answered immediately)
	OpSubscribe Op = "subscribe" // turn this connection into an event stream
)

// State is the lifecycle state of the agent side of a channel.
type State string

// The channel lifecycle states.
const (
	StateRunning    State = "running"    // between checkpoints, making progress
	StatePaused     State = "paused"     // held inside Checkpoint, waiting
	StateCancelling State = "cancelling" // cancel applied, loop unwinding
	StateDone       State = "done"       // channel closed
)

// Stage is the acknowledgement stage attached to ack frames.
type Stage string

// The acknowledgement stages. Every queued directive produces exactly one
// "accepted" or "rejected" ack, and every accepted directive later produces
// exactly one "applied" or "rejected" ack.
const (
	StageAccepted Stage = "accepted" // validated and queued in-band
	StageApplied  Stage = "applied"  // took effect at a checkpoint
	StageRejected Stage = "rejected" // refused, with a reason
)

// Frame is the single wire unit of steer/1: one JSON object per line.
// Which fields are meaningful depends on Type; empty fields are omitted on
// the wire. See docs/protocol.md for the field matrix per frame type.
type Frame struct {
	Type string `json:"type"` // "hello", "directive", "ack", "state", "event"

	// hello fields (agent → controller, once per connection).
	Proto string `json:"proto,omitempty"`
	PID   int    `json:"pid,omitempty"`

	// directive fields (controller → agent).
	ID      string `json:"id,omitempty"`      // controller-chosen, echoed in acks
	Op      Op     `json:"op,omitempty"`      // also echoed in acks
	Message string `json:"message,omitempty"` // redirect payload
	Mode    string `json:"mode,omitempty"`    // redirect: "append" (default) or "replace"
	Reason  string `json:"reason,omitempty"`  // pause/cancel annotation

	// ack fields (agent → controller).
	Stage Stage  `json:"stage,omitempty"`
	Seq   int    `json:"seq,omitempty"`   // queue admission order, on accepted acks
	Error string `json:"error,omitempty"` // on rejected acks

	// state and event fields (agent → controller).
	Agent   string `json:"agent,omitempty"`
	State   State  `json:"state,omitempty"`
	Step    int    `json:"step,omitempty"`
	Task    string `json:"task,omitempty"`
	Note    string `json:"note,omitempty"`    // latest checkpoint annotation
	Pending int    `json:"pending,omitempty"` // queued directives not yet applied
	Event   string `json:"event,omitempty"`   // "step", "paused", "resumed", …
}

// EncodeFrame writes f to w as one newline-terminated JSON line. It fails
// with ErrFrameTooLarge before writing anything if the encoding would
// exceed MaxFrameSize, so a too-large frame never corrupts the stream.
func EncodeFrame(w io.Writer, f Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("steerd: encode frame: %w", err)
	}
	if len(b)+1 > MaxFrameSize {
		return ErrFrameTooLarge
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// DecodeFrame reads the next frame from r. On ErrFrameTooLarge the rest of
// the offending line has already been discarded, so the caller may report
// the error and keep reading. Any other error is terminal for the stream.
func DecodeFrame(r *bufio.Reader) (Frame, error) {
	line, err := readFrameLine(r)
	if err != nil {
		return Frame{}, err
	}
	var f Frame
	if err := json.Unmarshal(line, &f); err != nil {
		return Frame{}, fmt.Errorf("%w: %v", ErrMalformedFrame, err)
	}
	if f.Type == "" {
		return Frame{}, fmt.Errorf("%w: missing type", ErrMalformedFrame)
	}
	return f, nil
}

// readFrameLine reads one newline-terminated line, enforcing MaxFrameSize
// without ever buffering more than the limit. When the limit is hit it
// drains the remainder of the line so the stream stays frame-aligned.
func readFrameLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > MaxFrameSize {
			if err == nil {
				return nil, ErrFrameTooLarge // newline seen; already aligned
			}
			for errors.Is(err, bufio.ErrBufferFull) {
				chunk, err = r.ReadSlice('\n')
			}
			if err != nil {
				return nil, err // stream died while draining
			}
			return nil, ErrFrameTooLarge
		}
		switch {
		case err == nil:
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(line) > 0:
			return nil, io.ErrUnexpectedEOF // truncated final frame
		default:
			return nil, err
		}
	}
}

// validateDirective checks that f is a well-formed steer/1 directive.
// It returns a human-readable reason suitable for a rejected ack.
func (f Frame) validateDirective() error {
	if f.Type != "directive" {
		return fmt.Errorf("unexpected frame type %q (want \"directive\")", f.Type)
	}
	switch f.Op {
	case OpPause, OpResume, OpCancel, OpStatus, OpSubscribe:
		if f.Message != "" || f.Mode != "" {
			return fmt.Errorf("op %q takes no message or mode", f.Op)
		}
	case OpRedirect:
		if f.Message == "" {
			return errors.New(`op "redirect" requires a non-empty message`)
		}
		if f.Mode != "" && f.Mode != "append" && f.Mode != "replace" {
			return fmt.Errorf(`unknown redirect mode %q (want "append" or "replace")`, f.Mode)
		}
	case "":
		return errors.New("directive is missing an op")
	default:
		return fmt.Errorf("unknown op %q", f.Op)
	}
	if f.Op != OpStatus && f.Op != OpSubscribe && f.ID == "" {
		return fmt.Errorf("op %q requires an id", f.Op)
	}
	return nil
}
