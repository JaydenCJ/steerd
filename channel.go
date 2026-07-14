package steerd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
)

// ErrClosed is returned by Checkpoint after the channel has been closed.
var ErrClosed = errors.New("steerd: channel is closed")

// Options configures the agent side of a steering channel.
type Options struct {
	// Agent is the display name announced in hello and state frames.
	// Defaults to "agent".
	Agent string

	// Task is a one-line description of what the loop is doing overall,
	// reported in hello and state frames. Optional.
	Task string

	// Backlog caps how many directives may be queued between two
	// checkpoints; further directives are rejected with "directive queue
	// is full". Defaults to 64.
	Backlog int
}

// Redirect is one injected instruction, delivered inside a Decision in the
// order the directives were accepted.
type Redirect struct {
	Message string // the instruction text
	Mode    string // "append" (default) or "replace"
}

// Decision is what a checkpoint resolves to once all queued directives
// have been applied. The loop owns the interpretation: apply the redirects
// to its plan, and stop cleanly if Cancelled is set.
type Decision struct {
	Step      int        // the checkpoint's step number (1-based)
	Redirects []Redirect // injected instructions, in arrival order
	Cancelled bool       // true once a cancel directive has been applied
	Reason    string     // the cancel directive's reason, if any
}

// pending is a directive admitted to the queue, waiting for a checkpoint.
type pending struct {
	frame Frame
	seq   int
	w     *frameWriter // where the applied/rejected ack goes; may be dead
}

// frameWriter serialises frame writes to one connection. Acks, state
// frames and events for a connection may come from different goroutines.
type frameWriter struct {
	mu   sync.Mutex
	conn net.Conn
}

func (w *frameWriter) send(f Frame) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return EncodeFrame(w.conn, f)
}

// Channel is the agent side of a steer/1 socket. Create one with Listen,
// call Checkpoint between units of work, and Close it when the loop ends.
// All methods are safe for concurrent use.
type Channel struct {
	opts Options
	path string
	ln   net.Listener
	wake chan struct{} // buffered(1); poked whenever the queue changes

	mu     sync.Mutex
	state  State
	step   int
	note   string
	seq    int
	queue  []pending
	subs   map[*frameWriter]struct{}
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// Listen creates the socket at path and starts accepting controllers.
// A stale socket file with no listener behind it is silently replaced;
// a live socket (another process is listening) is an error.
func Listen(path string, opts Options) (*Channel, error) {
	if opts.Agent == "" {
		opts.Agent = "agent"
	}
	if opts.Backlog <= 0 {
		opts.Backlog = 64
	}
	ln, err := net.Listen("unix", path)
	if err != nil && isAddrInUse(err) && socketIsStale(path) {
		if rmErr := os.Remove(path); rmErr == nil {
			ln, err = net.Listen("unix", path)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("steerd: listen: %w", err)
	}
	c := &Channel{
		opts:  opts,
		path:  path,
		ln:    ln,
		wake:  make(chan struct{}, 1),
		state: StateRunning,
		subs:  make(map[*frameWriter]struct{}),
		conns: make(map[net.Conn]struct{}),
	}
	c.wg.Add(1)
	go c.acceptLoop()
	return c, nil
}

func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}

// socketIsStale reports whether nothing answers on the socket at path,
// which is what a crashed previous run leaves behind.
func socketIsStale(path string) bool {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return true
	}
	conn.Close()
	return false
}

// Path returns the socket path the channel is listening on.
func (c *Channel) Path() string { return c.path }

// Step returns the number of checkpoints reached so far.
func (c *Channel) Step() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.step
}

// State returns the current lifecycle state.
func (c *Channel) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *Channel) acceptLoop() {
	defer c.wg.Done()
	for {
		conn, err := c.ln.Accept()
		if err != nil {
			return // listener closed
		}
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			conn.Close()
			return
		}
		c.conns[conn] = struct{}{}
		c.wg.Add(1)
		c.mu.Unlock()
		go c.handleConn(conn)
	}
}

func (c *Channel) handleConn(conn net.Conn) {
	defer c.wg.Done()
	w := &frameWriter{conn: conn}
	defer func() {
		c.mu.Lock()
		delete(c.conns, conn)
		delete(c.subs, w)
		c.mu.Unlock()
		conn.Close()
	}()

	if w.send(c.helloFrame()) != nil {
		return
	}
	r := bufio.NewReaderSize(conn, 4096)
	for {
		f, err := DecodeFrame(r)
		switch {
		case err == nil:
			c.dispatch(f, w)
		case errors.Is(err, ErrFrameTooLarge), errors.Is(err, ErrMalformedFrame):
			// The stream is still frame-aligned; refuse and carry on.
			if w.send(rejectedAck(Frame{}, err.Error())) != nil {
				return
			}
		default:
			return // EOF, closed connection, or truncated stream
		}
	}
}

func (c *Channel) helloFrame() Frame {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Frame{
		Type:  "hello",
		Proto: ProtocolVersion,
		Agent: c.opts.Agent,
		PID:   os.Getpid(),
		Task:  c.opts.Task,
		State: c.state,
		Step:  c.step,
	}
}

func (c *Channel) stateFrame() Frame {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Frame{
		Type:    "state",
		Agent:   c.opts.Agent,
		PID:     os.Getpid(),
		Task:    c.opts.Task,
		State:   c.state,
		Step:    c.step,
		Note:    c.note,
		Pending: len(c.queue),
	}
}

func rejectedAck(d Frame, reason string) Frame {
	return Frame{Type: "ack", ID: d.ID, Op: d.Op, Stage: StageRejected, Error: reason}
}

func (c *Channel) dispatch(f Frame, w *frameWriter) {
	if err := f.validateDirective(); err != nil {
		w.send(rejectedAck(f, err.Error()))
		return
	}
	switch f.Op {
	case OpStatus:
		w.send(c.stateFrame())
	case OpSubscribe:
		// The accepted ack is written while c.mu is held so that no event
		// broadcast can reach this subscriber before its acknowledgement.
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			w.send(rejectedAck(f, "channel is closed"))
			return
		}
		c.subs[w] = struct{}{}
		w.send(Frame{Type: "ack", ID: f.ID, Op: f.Op, Stage: StageAccepted})
		c.mu.Unlock()
	default:
		c.admit(f, w)
	}
}

// admit validates a mutating directive against the projected channel state
// and either queues it (accepted ack, with its admission seq) or refuses it
// (rejected ack, with a reason). Controllers get feedback immediately;
// nothing waits for the next checkpoint to be told "no".
//
// The accepted ack is written while c.mu is still held: a checkpoint can
// only see the queued directive after the lock is released, which is what
// guarantees the wire-order invariant "accepted always precedes applied"
// on every connection.
func (c *Channel) admit(f Frame, w *frameWriter) {
	c.mu.Lock()
	if reason := c.admitError(f); reason != "" {
		c.mu.Unlock()
		w.send(rejectedAck(f, reason))
		return
	}
	c.seq++
	p := pending{frame: f, seq: c.seq, w: w}
	c.queue = append(c.queue, p)
	w.send(Frame{Type: "ack", ID: f.ID, Op: f.Op, Stage: StageAccepted, Seq: p.seq})
	c.mu.Unlock()
	c.poke()
}

// admitError returns the rejection reason for f, or "" to accept.
// Callers hold c.mu.
func (c *Channel) admitError(f Frame) string {
	if c.closed || c.state == StateDone {
		return "channel is closed"
	}
	if c.state == StateCancelling || c.cancelPending() {
		if f.Op == OpCancel {
			return "cancel already requested"
		}
		return "agent is cancelling"
	}
	if len(c.queue) >= c.opts.Backlog {
		return "directive queue is full"
	}
	switch f.Op {
	case OpPause:
		if c.projectedPaused() {
			return "agent is already paused"
		}
	case OpResume:
		if !c.projectedPaused() {
			return "agent is not paused"
		}
	}
	return ""
}

// projectedPaused replays the queue over the current state, so that
// pause/resume admission stays coherent even when several directives are
// waiting for the same checkpoint. Callers hold c.mu.
func (c *Channel) projectedPaused() bool {
	paused := c.state == StatePaused
	for _, p := range c.queue {
		switch p.frame.Op {
		case OpPause:
			paused = true
		case OpResume:
			paused = false
		}
	}
	return paused
}

// cancelPending reports whether a cancel is queued. Callers hold c.mu.
func (c *Channel) cancelPending() bool {
	for _, p := range c.queue {
		if p.frame.Op == OpCancel {
			return true
		}
	}
	return false
}

// poke nudges a Checkpoint that is blocked in the paused wait.
func (c *Channel) poke() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Checkpoint marks one unit of work as finished and applies every queued
// directive. It blocks while the channel is paused and returns as soon as
// the loop should act: the Decision carries injected redirects in arrival
// order, and Cancelled once a cancel has been applied.
//
// note annotates this checkpoint ("step 3/8 analyze"); it is shown in
// state frames and step events. Checkpoint returns ctx.Err() if the
// context ends while paused, and ErrClosed if the channel is closed
// underneath a paused loop.
func (c *Channel) Checkpoint(ctx context.Context, note string) (Decision, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return Decision{}, ErrClosed
	}
	c.step++
	c.note = note
	dec := Decision{Step: c.step}
	c.mu.Unlock()

	c.broadcast(Frame{Type: "event", Event: "step", Step: dec.Step, State: c.State(), Note: note})

	for {
		acks, events := c.drain(&dec)
		for _, a := range acks {
			a.w.send(a.frame)
		}
		for _, e := range events {
			c.broadcast(e)
		}

		c.mu.Lock()
		paused := c.state == StatePaused
		closed := c.closed
		c.mu.Unlock()
		if dec.Cancelled {
			return dec, nil
		}
		if closed {
			return dec, ErrClosed
		}
		if !paused {
			return dec, nil
		}
		select {
		case <-ctx.Done():
			return dec, ctx.Err()
		case <-c.wake:
			// Queue changed (or channel closed); drain again.
		}
	}
}

// addressedAck pairs an ack frame with the connection that must receive it.
type addressedAck struct {
	w     *frameWriter
	frame Frame
}

// drain applies every queued directive to dec and returns the acks and
// events to deliver. Delivery happens outside the lock so a slow
// controller can never stall admission.
func (c *Channel) drain(dec *Decision) (acks []addressedAck, events []Frame) {
	c.mu.Lock()
	defer c.mu.Unlock()
	step := c.step
	applied := func(p pending) addressedAck {
		return addressedAck{p.w, Frame{
			Type: "ack", ID: p.frame.ID, Op: p.frame.Op,
			Stage: StageApplied, Seq: p.seq, Step: step,
		}}
	}
	for len(c.queue) > 0 {
		p := c.queue[0]
		c.queue = c.queue[1:]
		switch p.frame.Op {
		case OpPause:
			c.state = StatePaused
			acks = append(acks, applied(p))
			events = append(events, Frame{Type: "event", Event: "paused", Step: step, State: StatePaused, Reason: p.frame.Reason})
		case OpResume:
			if c.state != StatePaused {
				// Defensive: admission replays the queue so an admitted
				// resume always finds a paused state here today. If that
				// invariant is ever broken, resolve — never drop — the ack.
				acks = append(acks, addressedAck{p.w, rejectedAck(p.frame, "agent is not paused")})
				continue
			}
			c.state = StateRunning
			acks = append(acks, applied(p))
			events = append(events, Frame{Type: "event", Event: "resumed", Step: step, State: StateRunning})
		case OpRedirect:
			mode := p.frame.Mode
			if mode == "" {
				mode = "append"
			}
			dec.Redirects = append(dec.Redirects, Redirect{Message: p.frame.Message, Mode: mode})
			acks = append(acks, applied(p))
			events = append(events, Frame{Type: "event", Event: "redirected", Step: step, State: c.state, Message: p.frame.Message, Mode: mode})
		case OpCancel:
			c.state = StateCancelling
			dec.Cancelled = true
			dec.Reason = p.frame.Reason
			acks = append(acks, applied(p))
			events = append(events, Frame{Type: "event", Event: "cancelling", Step: step, State: StateCancelling, Reason: p.frame.Reason})
			// Admission refuses everything once a cancel is pending, so a
			// cancel is always the last queued directive; if that invariant
			// ever breaks, resolve the leftovers — never drop their acks.
			for _, rest := range c.queue {
				acks = append(acks, addressedAck{rest.w, rejectedAck(rest.frame, "agent is cancelling")})
			}
			c.queue = nil
		}
	}
	return acks, events
}

// broadcast delivers an event frame to every subscriber, dropping the ones
// whose connections have died.
func (c *Channel) broadcast(f Frame) {
	c.mu.Lock()
	targets := make([]*frameWriter, 0, len(c.subs))
	for w := range c.subs {
		targets = append(targets, w)
	}
	c.mu.Unlock()
	for _, w := range targets {
		if w.send(f) != nil {
			c.mu.Lock()
			delete(c.subs, w)
			c.mu.Unlock()
		}
	}
}

// Close ends the channel: pending directives are rejected, subscribers get
// a final "done" event, the listener and every connection are shut down,
// and the socket file is removed. Close is idempotent.
func (c *Channel) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.state = StateDone
	orphans := c.queue
	c.queue = nil
	step := c.step
	c.mu.Unlock()

	for _, p := range orphans {
		p.w.send(rejectedAck(p.frame, "channel is closed"))
	}
	c.broadcast(Frame{Type: "event", Event: "done", Step: step, State: StateDone})
	c.poke() // release a Checkpoint blocked in the paused wait

	err := c.ln.Close()
	c.mu.Lock()
	for conn := range c.conns {
		conn.Close()
	}
	c.mu.Unlock()
	c.wg.Wait()
	return err
}
