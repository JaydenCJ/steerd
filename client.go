package steerd

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"time"
)

// RejectedError is returned when the agent answers a directive with a
// rejected acknowledgement. The directive did not and will not take effect.
type RejectedError struct {
	Op     Op
	Reason string
}

func (e *RejectedError) Error() string {
	return fmt.Sprintf("steerd: %s rejected: %s", e.Op, e.Reason)
}

// Directive carries the optional payload of a steering directive.
type Directive struct {
	Message string // redirect instruction text
	Mode    string // redirect: "append" (default) or "replace"
	Reason  string // pause/cancel annotation
}

// Client is the controller side of a steer/1 socket. It is intentionally
// lock-step — one directive in flight per connection — which is exactly how
// interactive steering behaves. Open one client per concurrent controller.
type Client struct {
	conn    net.Conn
	r       *bufio.Reader
	hello   Frame
	nextID  int
	Timeout time.Duration // per-read guard; 0 means wait forever
}

// Dial connects to the agent socket at path and consumes the hello frame.
func Dial(path string) (*Client, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, fmt.Errorf("steerd: dial: %w", err)
	}
	c := &Client{conn: conn, r: bufio.NewReaderSize(conn, 4096)}
	hello, err := c.Next()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("steerd: reading hello: %w", err)
	}
	if hello.Type != "hello" || hello.Proto != ProtocolVersion {
		conn.Close()
		return nil, fmt.Errorf("steerd: peer is not a %s agent (got type %q, proto %q)",
			ProtocolVersion, hello.Type, hello.Proto)
	}
	c.hello = hello
	return c, nil
}

// Hello returns the hello frame received when the connection was opened.
func (c *Client) Hello() Frame { return c.hello }

// Close closes the underlying connection.
func (c *Client) Close() error { return c.conn.Close() }

// Next reads the next frame from the agent, honouring Timeout.
func (c *Client) Next() (Frame, error) {
	if c.Timeout > 0 {
		if err := c.conn.SetReadDeadline(time.Now().Add(c.Timeout)); err != nil {
			return Frame{}, err
		}
	}
	return DecodeFrame(c.r)
}

func (c *Client) newID() string {
	c.nextID++
	return fmt.Sprintf("d%d", c.nextID)
}

// Send transmits one directive and waits for its admission acknowledgement.
// It returns the accepted ack, or a *RejectedError when the agent refuses.
// The returned frame's ID is what Await needs to observe application.
func (c *Client) Send(op Op, d Directive) (Frame, error) {
	f := Frame{
		Type:    "directive",
		ID:      c.newID(),
		Op:      op,
		Message: d.Message,
		Mode:    d.Mode,
		Reason:  d.Reason,
	}
	if err := EncodeFrame(c.conn, f); err != nil {
		return Frame{}, err
	}
	return c.awaitAck(f.ID, StageAccepted)
}

// Await blocks until the directive identified by id is applied at a
// checkpoint, returning the applied ack (its Step field says which one).
// A directive orphaned by a cancel or by Close resolves as *RejectedError.
func (c *Client) Await(id string) (Frame, error) {
	return c.awaitAck(id, StageApplied)
}

// awaitAck reads frames until the ack for id reaches want (or rejected,
// which resolves any wait). Unrelated frames — events on a connection that
// also subscribed, state replies — are skipped, not errors.
func (c *Client) awaitAck(id string, want Stage) (Frame, error) {
	for {
		f, err := c.Next()
		if err != nil {
			return Frame{}, err
		}
		if f.Type != "ack" || f.ID != id {
			continue
		}
		switch f.Stage {
		case StageRejected:
			return f, &RejectedError{Op: f.Op, Reason: f.Error}
		case want:
			return f, nil
		}
	}
}

// Pause asks the agent to hold at its next checkpoint. With wait set it
// blocks until the pause is applied and returns that ack.
func (c *Client) Pause(reason string, wait bool) (Frame, error) {
	return c.roundTrip(OpPause, Directive{Reason: reason}, wait)
}

// Resume releases a paused (or pause-pending) agent.
func (c *Client) Resume(wait bool) (Frame, error) {
	return c.roundTrip(OpResume, Directive{}, wait)
}

// Redirect injects an instruction into the agent's next decision. Mode ""
// or "append" adds to the current plan; "replace" supersedes it.
func (c *Client) Redirect(message, mode string, wait bool) (Frame, error) {
	return c.roundTrip(OpRedirect, Directive{Message: message, Mode: mode}, wait)
}

// Cancel asks the agent to stop gracefully at its next checkpoint.
func (c *Client) Cancel(reason string, wait bool) (Frame, error) {
	return c.roundTrip(OpCancel, Directive{Reason: reason}, wait)
}

func (c *Client) roundTrip(op Op, d Directive, wait bool) (Frame, error) {
	ack, err := c.Send(op, d)
	if err != nil || !wait {
		return ack, err
	}
	return c.Await(ack.ID)
}

// Status asks for a state frame and returns it.
func (c *Client) Status() (Frame, error) {
	if err := EncodeFrame(c.conn, Frame{Type: "directive", Op: OpStatus}); err != nil {
		return Frame{}, err
	}
	for {
		f, err := c.Next()
		if err != nil {
			return Frame{}, err
		}
		if f.Type == "state" {
			return f, nil
		}
	}
}

// Subscribe turns this connection into an event stream. After it returns,
// call Next repeatedly; every state change and step arrives as an "event"
// frame, ending with "done" when the agent closes the channel.
func (c *Client) Subscribe() error {
	f := Frame{Type: "directive", ID: c.newID(), Op: OpSubscribe}
	if err := EncodeFrame(c.conn, f); err != nil {
		return err
	}
	_, err := c.awaitAck(f.ID, StageAccepted)
	return err
}

// IsTimeout reports whether err is a read-deadline expiry from Next.
func IsTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
