// Controller-side tests: Dial semantics, the convenience round-trips
// (Pause/Resume/Redirect/Cancel), rejection surfacing, status, event
// subscription, and read timeouts. The agent under test is either a
// hand-driven Channel or busyAgent, a loop that checkpoints continuously —
// so every wait resolves through protocol acks, never through sleeps.
package steerd

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// busyAgent checkpoints in a tight loop until cancelled or closed, feeding
// every decision to sink. It mirrors how a real agent loop consumes the
// channel and lets convenience methods that wait for "applied" resolve
// without the test having to orchestrate individual checkpoints.
func busyAgent(t *testing.T, ch *Channel, sink chan<- Decision) {
	t.Helper()
	go func() {
		for {
			dec, err := ch.Checkpoint(context.Background(), "")
			if err != nil {
				close(sink)
				return
			}
			if dec.Cancelled || len(dec.Redirects) > 0 {
				sink <- dec
			}
			if dec.Cancelled {
				close(sink)
				return
			}
		}
	}()
}

func newBusyChannel(t *testing.T) (*Channel, chan Decision) {
	t.Helper()
	ch := newChannel(t, Options{Agent: "busy", Task: "spin"})
	sink := make(chan Decision, 16)
	busyAgent(t, ch, sink)
	return ch, sink
}

func TestDialConsumesHelloAndExposesIt(t *testing.T) {
	ch := newChannel(t, Options{Agent: "greeter", Task: "wave"})
	c, err := Dial(ch.Path())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	h := c.Hello()
	if h.Agent != "greeter" || h.Proto != ProtocolVersion {
		t.Fatalf("hello = %+v", h)
	}
}

func TestDialFailsOnMissingSocket(t *testing.T) {
	_, err := Dial(sockPath(t)) // path exists as a dir entry name only
	if err == nil || !strings.Contains(err.Error(), "dial") {
		t.Fatalf("want dial error, got %v", err)
	}
}

func TestDialRejectsPeerThatIsNotASteerAgent(t *testing.T) {
	// Pointing steerd at some other daemon's socket must fail loudly at
	// handshake, not send directives into a foreign protocol.
	path := sockPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		conn.Write([]byte("{\"type\":\"state\",\"step\":1}\n"))
		conn.Close()
	}()
	_, err = Dial(path)
	if err == nil || !strings.Contains(err.Error(), "not a steer/1 agent") {
		t.Fatalf("want handshake error, got %v", err)
	}
}

func TestClientIDsAreSequentialPerConnection(t *testing.T) {
	ch := newChannel(t, Options{})
	c, _ := Dial(ch.Path())
	defer c.Close()
	a1, _ := c.Send(OpRedirect, Directive{Message: "one"})
	a2, _ := c.Send(OpRedirect, Directive{Message: "two"})
	if a1.ID != "d1" || a2.ID != "d2" {
		t.Fatalf("ids = %q, %q; want d1, d2", a1.ID, a2.ID)
	}
}

func TestPauseAndResumeRoundTripAgainstALiveLoop(t *testing.T) {
	ch, _ := newBusyChannel(t)
	c, _ := Dial(ch.Path())
	defer c.Close()

	applied, err := c.Pause("have a look", true)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if applied.Stage != StageApplied || applied.Step < 1 {
		t.Fatalf("pause applied ack = %+v", applied)
	}
	if got := ch.State(); got != StatePaused {
		t.Fatalf("state = %q, want paused", got)
	}
	if _, err := c.Resume(true); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := ch.State(); got != StateRunning {
		t.Fatalf("state = %q, want running", got)
	}
}

func TestRedirectRoundTripDeliversTheInstruction(t *testing.T) {
	ch, sink := newBusyChannel(t)
	c, _ := Dial(ch.Path())
	defer c.Close()

	if _, err := c.Redirect("prefer the v2 endpoint", "replace", true); err != nil {
		t.Fatalf("Redirect: %v", err)
	}
	dec := <-sink
	if len(dec.Redirects) != 1 || dec.Redirects[0].Message != "prefer the v2 endpoint" ||
		dec.Redirects[0].Mode != "replace" {
		t.Fatalf("agent saw %+v", dec.Redirects)
	}
	_ = ch
}

func TestCancelRoundTripStopsTheLoop(t *testing.T) {
	ch, sink := newBusyChannel(t)
	c, _ := Dial(ch.Path())
	defer c.Close()

	applied, err := c.Cancel("done early", true)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if applied.Stage != StageApplied {
		t.Fatalf("cancel ack stage = %q, want applied", applied.Stage)
	}
	var last Decision
	for dec := range sink { // sink closes once the loop exits
		last = dec
	}
	if !last.Cancelled || last.Reason != "done early" {
		t.Fatalf("loop's final decision = %+v", last)
	}
	if got := ch.State(); got != StateCancelling {
		t.Fatalf("state = %q, want cancelling", got)
	}
}

func TestNoWaitSendReturnsAfterAcceptance(t *testing.T) {
	// No checkpoint ever runs here, so a wait=false round-trip must still
	// return promptly with just the accepted ack.
	ch := newChannel(t, Options{})
	c, _ := Dial(ch.Path())
	defer c.Close()
	ack, err := c.Pause("later", false)
	if err != nil {
		t.Fatalf("Pause no-wait: %v", err)
	}
	if ack.Stage != StageAccepted {
		t.Fatalf("stage = %q, want accepted", ack.Stage)
	}
}

func TestRejectionSurfacesAsRejectedError(t *testing.T) {
	ch := newChannel(t, Options{})
	c, _ := Dial(ch.Path())
	defer c.Close()
	_, err := c.Resume(false)
	var rej *RejectedError
	if !errors.As(err, &rej) {
		t.Fatalf("want *RejectedError, got %v", err)
	}
	if rej.Op != OpResume || rej.Reason != "agent is not paused" {
		t.Fatalf("rejection = %+v", rej)
	}
	if !strings.Contains(rej.Error(), "resume rejected: agent is not paused") {
		t.Fatalf("error string = %q", rej.Error())
	}
}

func TestStatusReturnsTheStateFrame(t *testing.T) {
	ch := newChannel(t, Options{Agent: "statler", Task: "heckle"})
	if _, err := ch.Checkpoint(context.Background(), "warming up"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	c, _ := Dial(ch.Path())
	defer c.Close()
	st, err := c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Agent != "statler" || st.Step != 1 || st.Note != "warming up" {
		t.Fatalf("state = %+v", st)
	}
}

func TestSubscribeStreamsEventsThroughNext(t *testing.T) {
	ch := newChannel(t, Options{})
	c, _ := Dial(ch.Path())
	defer c.Close()
	c.Timeout = 5 * time.Second
	if err := c.Subscribe(); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if _, err := ch.Checkpoint(context.Background(), "first"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	f, err := c.Next()
	if err != nil || f.Event != "step" || f.Note != "first" {
		t.Fatalf("event = %+v, %v", f, err)
	}
}

func TestAwaitTimesOutWhenTheLoopNeverCheckpoints(t *testing.T) {
	// The one legitimate use of a deadline: a controller must not hang
	// forever on an agent that stopped checkpointing. Outcome is fully
	// deterministic — the applied ack can never arrive.
	ch := newChannel(t, Options{})
	c, _ := Dial(ch.Path())
	defer c.Close()
	ack, err := c.Send(OpPause, Directive{})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	c.Timeout = 25 * time.Millisecond
	_, err = c.Await(ack.ID)
	if !IsTimeout(err) {
		t.Fatalf("want timeout, got %v", err)
	}
	_ = ch
}

func TestIsTimeoutIsFalseForOtherErrors(t *testing.T) {
	if IsTimeout(errors.New("boom")) || IsTimeout(nil) {
		t.Fatalf("IsTimeout must only match net timeout errors")
	}
}
