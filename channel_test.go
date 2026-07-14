// Agent-side behaviour tests: directive admission, the two-stage
// acknowledgement contract, the pause/resume/redirect/cancel state machine
// at checkpoints, event fan-out, and socket lifecycle. All tests run over
// real Unix sockets in temp dirs; synchronisation is done entirely through
// protocol acknowledgements, so nothing here depends on timing.
package steerd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// clientFromConn frames an already-open connection as a Client, consuming
// the hello. Used when a test needs to write raw bytes before framing.
func clientFromConn(conn net.Conn) (*Client, error) {
	c := &Client{conn: conn, r: bufio.NewReaderSize(conn, 4096), Timeout: 5 * time.Second}
	hello, err := c.Next()
	if err != nil {
		return nil, err
	}
	if hello.Type != "hello" {
		return nil, fmt.Errorf("unexpected first frame type %q", hello.Type)
	}
	c.hello = hello
	return c, nil
}

// sockPath returns a fresh socket path short enough for sun_path.
func sockPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "steerd")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	p := filepath.Join(dir, "s.sock")
	if len(p) >= 100 {
		t.Fatalf("temp socket path too long for a Unix socket: %q", p)
	}
	return p
}

// newChannel starts a channel and registers its cleanup.
func newChannel(t *testing.T, opts Options) *Channel {
	t.Helper()
	ch, err := Listen(sockPath(t), opts)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ch.Close() })
	return ch
}

// testConn is a raw protocol-level connection, deliberately lower-level
// than Client so tests can send exactly the bytes they mean to.
type testConn struct {
	t     *testing.T
	c     *Client
	hello Frame
}

func dialRaw(t *testing.T, path string) *testConn {
	t.Helper()
	c, err := Dial(path)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	c.Timeout = 5 * time.Second // guard against hangs; never hit when green
	t.Cleanup(func() { c.Close() })
	return &testConn{t: t, c: c, hello: c.Hello()}
}

// directive sends an op and returns the admission ack (accepted or, via
// err, rejected).
func (tc *testConn) directive(op Op, d Directive) (Frame, error) {
	tc.t.Helper()
	return tc.c.Send(op, d)
}

func (tc *testConn) mustAccept(op Op, d Directive) Frame {
	tc.t.Helper()
	ack, err := tc.c.Send(op, d)
	if err != nil {
		tc.t.Fatalf("%s should be accepted, got %v", op, err)
	}
	if ack.Stage != StageAccepted {
		tc.t.Fatalf("%s admission ack stage = %q, want accepted", op, ack.Stage)
	}
	return ack
}

func (tc *testConn) mustApply(id string) Frame {
	tc.t.Helper()
	ack, err := tc.c.Await(id)
	if err != nil {
		tc.t.Fatalf("Await(%s): %v", id, err)
	}
	return ack
}

type cpResult struct {
	dec Decision
	err error
}

// goCheckpoint runs one checkpoint concurrently and delivers its result.
func goCheckpoint(ch *Channel, note string) <-chan cpResult {
	out := make(chan cpResult, 1)
	go func() {
		dec, err := ch.Checkpoint(context.Background(), note)
		out <- cpResult{dec, err}
	}()
	return out
}

func TestListenCreatesSocketAndCloseRemovesIt(t *testing.T) {
	path := sockPath(t)
	ch, err := Listen(path, Options{})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("expected a socket at %s, got %v, %v", path, fi, err)
	}
	if err := ch.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket file should be removed on Close, stat err = %v", err)
	}
}

func TestListenReplacesStaleSocketFile(t *testing.T) {
	// A crashed previous run leaves the socket file behind with nobody
	// listening; the next run must be able to take the address over.
	path := sockPath(t)
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("first listen: %v", err)
	}
	// Close the file descriptor without letting Go unlink the path, the
	// same state a SIGKILLed process leaves behind.
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("test bug: stale socket file should still exist: %v", err)
	}
	ch, err := Listen(path, Options{})
	if err != nil {
		t.Fatalf("Listen should replace a stale socket, got %v", err)
	}
	defer ch.Close()
	if _, err := Dial(path); err != nil {
		t.Fatalf("replaced socket should accept connections: %v", err)
	}
}

func TestListenRefusesLiveSocket(t *testing.T) {
	// Two agents must never silently share one steering address.
	path := sockPath(t)
	first, err := Listen(path, Options{})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer first.Close()
	if _, err := Listen(path, Options{}); err == nil {
		t.Fatalf("second Listen on a live socket must fail")
	}
}

func TestHelloFrameAnnouncesProtocolAgentAndState(t *testing.T) {
	ch := newChannel(t, Options{Agent: "indexer", Task: "index the wiki"})
	tc := dialRaw(t, ch.Path())
	h := tc.hello
	if h.Proto != ProtocolVersion || h.Agent != "indexer" || h.Task != "index the wiki" {
		t.Fatalf("hello = %+v", h)
	}
	if h.State != StateRunning || h.Step != 0 || h.PID != os.Getpid() {
		t.Fatalf("hello state/step/pid = %q/%d/%d", h.State, h.Step, h.PID)
	}
}

func TestStatusReportsStateStepNoteAndPending(t *testing.T) {
	ch := newChannel(t, Options{Agent: "a", Task: "t"})
	if _, err := ch.Checkpoint(context.Background(), "step 1/9 collect"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	tc := dialRaw(t, ch.Path())
	tc.mustAccept(OpRedirect, Directive{Message: "m"}) // queued, not yet applied
	st, err := tc.c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != StateRunning || st.Step != 1 || st.Note != "step 1/9 collect" || st.Pending != 1 {
		t.Fatalf("state frame = %+v", st)
	}
}

func TestAcceptedAcksCarryMonotonicSeq(t *testing.T) {
	ch := newChannel(t, Options{})
	tc := dialRaw(t, ch.Path())
	a1 := tc.mustAccept(OpRedirect, Directive{Message: "one"})
	a2 := tc.mustAccept(OpRedirect, Directive{Message: "two"})
	if a1.Seq != 1 || a2.Seq != 2 {
		t.Fatalf("seq = %d, %d; want 1, 2", a1.Seq, a2.Seq)
	}
}

func TestCheckpointWithNoDirectivesContinuesImmediately(t *testing.T) {
	ch := newChannel(t, Options{})
	dec, err := ch.Checkpoint(context.Background(), "")
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if dec.Cancelled || len(dec.Redirects) != 0 || dec.Step != 1 {
		t.Fatalf("empty checkpoint decision = %+v", dec)
	}
}

func TestPauseIsAppliedAtNextCheckpointAndResumeReleasesIt(t *testing.T) {
	ch := newChannel(t, Options{})
	tc := dialRaw(t, ch.Path())

	ack := tc.mustAccept(OpPause, Directive{Reason: "inspect"})
	res := goCheckpoint(ch, "")

	applied := tc.mustApply(ack.ID)
	if applied.Step != 1 || applied.Op != OpPause {
		t.Fatalf("applied ack = %+v", applied)
	}
	// The applied ack is only sent once the pause has taken effect, so the
	// state is observably paused and the checkpoint is still in flight.
	if got := ch.State(); got != StatePaused {
		t.Fatalf("state after applied pause = %q, want paused", got)
	}
	select {
	case r := <-res:
		t.Fatalf("checkpoint must block while paused, returned %+v", r)
	default:
	}

	rack := tc.mustAccept(OpResume, Directive{})
	tc.mustApply(rack.ID)
	r := <-res
	if r.err != nil || r.dec.Cancelled {
		t.Fatalf("resumed checkpoint = %+v", r)
	}
	if got := ch.State(); got != StateRunning {
		t.Fatalf("state after resume = %q, want running", got)
	}
}

func TestPauseWhilePausedIsRejected(t *testing.T) {
	ch := newChannel(t, Options{})
	tc := dialRaw(t, ch.Path())
	ack := tc.mustAccept(OpPause, Directive{})
	res := goCheckpoint(ch, "")
	tc.mustApply(ack.ID)

	_, err := tc.directive(OpPause, Directive{})
	var rej *RejectedError
	if !errors.As(err, &rej) || rej.Reason != "agent is already paused" {
		t.Fatalf("second pause should be rejected with a reason, got %v", err)
	}
	rack := tc.mustAccept(OpResume, Directive{})
	tc.mustApply(rack.ID)
	<-res
}

func TestPauseWhilePausePendingIsRejected(t *testing.T) {
	// The rejection must consider queued directives, not just the current
	// state — otherwise two controllers double-pause and one resume leaks.
	ch := newChannel(t, Options{})
	tc := dialRaw(t, ch.Path())
	tc.mustAccept(OpPause, Directive{})
	_, err := tc.directive(OpPause, Directive{})
	var rej *RejectedError
	if !errors.As(err, &rej) || rej.Reason != "agent is already paused" {
		t.Fatalf("pause on pause-pending should be rejected, got %v", err)
	}
}

func TestResumeWhenNotPausedIsRejected(t *testing.T) {
	ch := newChannel(t, Options{})
	tc := dialRaw(t, ch.Path())
	_, err := tc.directive(OpResume, Directive{})
	var rej *RejectedError
	if !errors.As(err, &rej) || rej.Reason != "agent is not paused" {
		t.Fatalf("resume while running should be rejected, got %v", err)
	}
}

func TestResumeCancelsAPendingPauseBeforeItEverBlocks(t *testing.T) {
	// pause then resume queued before any checkpoint: the loop must sail
	// straight through, and both directives must still be acknowledged.
	ch := newChannel(t, Options{})
	tc := dialRaw(t, ch.Path())
	p := tc.mustAccept(OpPause, Directive{})
	r := tc.mustAccept(OpResume, Directive{})

	dec, err := ch.Checkpoint(context.Background(), "")
	if err != nil || dec.Cancelled {
		t.Fatalf("checkpoint = %+v, %v", dec, err)
	}
	if tc.mustApply(p.ID).Op != OpPause || tc.mustApply(r.ID).Op != OpResume {
		t.Fatalf("both pause and resume must be acked applied")
	}
	if got := ch.State(); got != StateRunning {
		t.Fatalf("state = %q, want running", got)
	}
}

func TestPauseAfterQueuedPauseResumePairIsAccepted(t *testing.T) {
	// Admission replays the queue: pause+resume pending means "will be
	// running", so a third pause is legitimate, not a duplicate.
	ch := newChannel(t, Options{})
	tc := dialRaw(t, ch.Path())
	tc.mustAccept(OpPause, Directive{})
	tc.mustAccept(OpResume, Directive{})
	tc.mustAccept(OpPause, Directive{})
}

func TestRedirectIsDeliveredInTheDecisionWithAppliedAck(t *testing.T) {
	ch := newChannel(t, Options{})
	tc := dialRaw(t, ch.Path())
	ack := tc.mustAccept(OpRedirect, Directive{Message: "skip the appendix"})

	dec, err := ch.Checkpoint(context.Background(), "")
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if len(dec.Redirects) != 1 || dec.Redirects[0].Message != "skip the appendix" {
		t.Fatalf("decision redirects = %+v", dec.Redirects)
	}
	if dec.Redirects[0].Mode != "append" {
		t.Fatalf("default redirect mode = %q, want append", dec.Redirects[0].Mode)
	}
	if applied := tc.mustApply(ack.ID); applied.Step != dec.Step {
		t.Fatalf("applied ack step = %d, want %d", applied.Step, dec.Step)
	}
}

func TestMultipleRedirectsArriveInAcceptanceOrder(t *testing.T) {
	ch := newChannel(t, Options{})
	tc := dialRaw(t, ch.Path())
	tc.mustAccept(OpRedirect, Directive{Message: "first"})
	tc.mustAccept(OpRedirect, Directive{Message: "second", Mode: "replace"})
	tc.mustAccept(OpRedirect, Directive{Message: "third"})

	dec, _ := ch.Checkpoint(context.Background(), "")
	got := make([]string, len(dec.Redirects))
	for i, r := range dec.Redirects {
		got[i] = r.Message
	}
	if strings.Join(got, ",") != "first,second,third" {
		t.Fatalf("redirect order = %v", got)
	}
	if dec.Redirects[1].Mode != "replace" {
		t.Fatalf("mode of second redirect = %q, want replace", dec.Redirects[1].Mode)
	}
}

func TestRedirectWhilePausedIsHeldUntilTheResumedDecision(t *testing.T) {
	// The flagship flow: pause the loop, inject a correction, resume — the
	// checkpoint that was blocked returns carrying the correction.
	ch := newChannel(t, Options{})
	tc := dialRaw(t, ch.Path())
	p := tc.mustAccept(OpPause, Directive{})
	res := goCheckpoint(ch, "")
	tc.mustApply(p.ID)

	rd := tc.mustAccept(OpRedirect, Directive{Message: "use the staging config"})
	tc.mustApply(rd.ID) // applied into the still-blocked decision
	if got := ch.State(); got != StatePaused {
		t.Fatalf("redirect must not unpause, state = %q", got)
	}

	ra := tc.mustAccept(OpResume, Directive{})
	tc.mustApply(ra.ID)
	r := <-res
	if len(r.dec.Redirects) != 1 || r.dec.Redirects[0].Message != "use the staging config" {
		t.Fatalf("resumed decision = %+v", r.dec)
	}
}

func TestCancelStopsTheLoopWithReason(t *testing.T) {
	ch := newChannel(t, Options{})
	tc := dialRaw(t, ch.Path())
	ack := tc.mustAccept(OpCancel, Directive{Reason: "wrong branch"})

	dec, err := ch.Checkpoint(context.Background(), "")
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if !dec.Cancelled || dec.Reason != "wrong branch" {
		t.Fatalf("decision = %+v", dec)
	}
	if tc.mustApply(ack.ID).Step != 1 {
		t.Fatalf("cancel applied ack should carry the step")
	}
	if got := ch.State(); got != StateCancelling {
		t.Fatalf("state = %q, want cancelling", got)
	}
}

func TestCancelWhilePausedUnblocksTheCheckpoint(t *testing.T) {
	ch := newChannel(t, Options{})
	tc := dialRaw(t, ch.Path())
	p := tc.mustAccept(OpPause, Directive{})
	res := goCheckpoint(ch, "")
	tc.mustApply(p.ID)

	c := tc.mustAccept(OpCancel, Directive{Reason: "took too long"})
	tc.mustApply(c.ID)
	r := <-res
	if r.err != nil || !r.dec.Cancelled || r.dec.Reason != "took too long" {
		t.Fatalf("checkpoint after cancel-while-paused = %+v, %v", r.dec, r.err)
	}
}

func TestDirectivesAfterCancelAreRejected(t *testing.T) {
	ch := newChannel(t, Options{})
	tc := dialRaw(t, ch.Path())
	tc.mustAccept(OpCancel, Directive{})

	var rej *RejectedError
	if _, err := tc.directive(OpCancel, Directive{}); !errors.As(err, &rej) ||
		rej.Reason != "cancel already requested" {
		t.Fatalf("second cancel should be rejected, got %v", err)
	}
	if _, err := tc.directive(OpRedirect, Directive{Message: "m"}); !errors.As(err, &rej) ||
		rej.Reason != "agent is cancelling" {
		t.Fatalf("redirect after cancel should be rejected, got %v", err)
	}
}

func TestNothingCanBeQueuedBehindAPendingCancel(t *testing.T) {
	// Queue pause → cancel, then try resume: once a cancel is pending the
	// outcome of the run is decided, so admission refuses further steering
	// immediately rather than letting it be silently outrun by the cancel.
	ch := newChannel(t, Options{})
	tc := dialRaw(t, ch.Path())
	p := tc.mustAccept(OpPause, Directive{})
	ca := tc.mustAccept(OpCancel, Directive{})
	_, err := tc.directive(OpResume, Directive{})
	var rej *RejectedError
	if !errors.As(err, &rej) || rej.Reason != "agent is cancelling" {
		t.Fatalf("resume behind a pending cancel must be rejected, got %v", err)
	}

	// The checkpoint applies the pause and immediately the cancel: the
	// loop never blocks, and both directives resolve as applied.
	dec, err := ch.Checkpoint(context.Background(), "")
	if err != nil || !dec.Cancelled {
		t.Fatalf("decision = %+v, %v", dec, err)
	}
	tc.mustApply(p.ID)
	tc.mustApply(ca.ID)
}

func TestBacklogLimitRejectsExcessDirectives(t *testing.T) {
	ch := newChannel(t, Options{Backlog: 2})
	tc := dialRaw(t, ch.Path())
	tc.mustAccept(OpRedirect, Directive{Message: "one"})
	tc.mustAccept(OpRedirect, Directive{Message: "two"})
	_, err := tc.directive(OpRedirect, Directive{Message: "three"})
	var rej *RejectedError
	if !errors.As(err, &rej) || rej.Reason != "directive queue is full" {
		t.Fatalf("over-backlog directive should be rejected, got %v", err)
	}
}

func TestInvalidDirectiveGetsRejectedAckAndConnectionSurvives(t *testing.T) {
	ch := newChannel(t, Options{})
	tc := dialRaw(t, ch.Path())
	_, err := tc.directive(OpRedirect, Directive{Message: "m", Mode: "sideways"})
	var rej *RejectedError
	if !errors.As(err, &rej) || !strings.Contains(rej.Reason, "unknown redirect mode") {
		t.Fatalf("want validation rejection, got %v", err)
	}
	if _, err := tc.c.Status(); err != nil {
		t.Fatalf("connection should survive a rejected directive: %v", err)
	}
}

func TestGarbageBytesAreRefusedWithoutKillingTheConnection(t *testing.T) {
	ch := newChannel(t, Options{})
	conn, err := net.Dial("unix", ch.Path())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("not a frame\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Re-frame the connection through a Client to read the replies.
	c, err := clientFromConn(conn)
	if err != nil {
		t.Fatalf("hello after garbage: %v", err)
	}
	f, err := c.Next()
	if err != nil || f.Type != "ack" || f.Stage != StageRejected {
		t.Fatalf("garbage should earn a rejected ack, got %+v, %v", f, err)
	}
	if _, err := c.Status(); err != nil {
		t.Fatalf("connection should survive garbage input: %v", err)
	}
}

func TestCheckpointContextCancellationReleasesAPausedLoop(t *testing.T) {
	ch := newChannel(t, Options{})
	tc := dialRaw(t, ch.Path())
	p := tc.mustAccept(OpPause, Directive{})

	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan cpResult, 1)
	go func() {
		dec, err := ch.Checkpoint(ctx, "")
		out <- cpResult{dec, err}
	}()
	tc.mustApply(p.ID) // paused for sure
	cancel()
	r := <-out
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("checkpoint err = %v, want context.Canceled", r.err)
	}
}

func TestCloseUnderAPausedCheckpointReturnsErrClosed(t *testing.T) {
	ch := newChannel(t, Options{})
	tc := dialRaw(t, ch.Path())
	p := tc.mustAccept(OpPause, Directive{})
	res := goCheckpoint(ch, "")
	tc.mustApply(p.ID)

	if err := ch.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	r := <-res
	if !errors.Is(r.err, ErrClosed) {
		t.Fatalf("paused checkpoint after Close = %v, want ErrClosed", r.err)
	}
}

func TestCheckpointAfterCloseReturnsErrClosed(t *testing.T) {
	ch := newChannel(t, Options{})
	ch.Close()
	if _, err := ch.Checkpoint(context.Background(), ""); !errors.Is(err, ErrClosed) {
		t.Fatalf("want ErrClosed, got %v", err)
	}
}

func TestCloseRejectsPendingDirectives(t *testing.T) {
	// A controller that queued work must learn the agent went away —
	// silence is the one thing the acknowledgement contract forbids.
	ch := newChannel(t, Options{})
	tc := dialRaw(t, ch.Path())
	ack := tc.mustAccept(OpRedirect, Directive{Message: "never applied"})
	go ch.Close()
	_, err := tc.c.Await(ack.ID)
	var rej *RejectedError
	if !errors.As(err, &rej) || rej.Reason != "channel is closed" {
		t.Fatalf("pending directive must resolve rejected on Close, got %v", err)
	}
}

func TestSubscribersReceiveStepPauseRedirectResumeAndDoneEvents(t *testing.T) {
	ch := newChannel(t, Options{})
	ctrl := dialRaw(t, ch.Path())
	watch := dialRaw(t, ch.Path())
	if err := watch.c.Subscribe(); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	nextEvent := func() Frame {
		t.Helper()
		for {
			f, err := watch.c.Next()
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			if f.Type == "event" {
				return f
			}
		}
	}

	p := ctrl.mustAccept(OpPause, Directive{Reason: "look"})
	res := goCheckpoint(ch, "step 1/3 collect")
	ctrl.mustApply(p.ID)
	rd := ctrl.mustAccept(OpRedirect, Directive{Message: "narrow scope"})
	ctrl.mustApply(rd.ID)
	ra := ctrl.mustAccept(OpResume, Directive{})
	ctrl.mustApply(ra.ID)
	<-res
	ch.Close()

	want := []string{"step", "paused", "redirected", "resumed", "done"}
	for i, name := range want {
		f := nextEvent()
		if f.Event != name {
			t.Fatalf("event %d = %q, want %q", i, f.Event, name)
		}
		switch name {
		case "step":
			if f.Note != "step 1/3 collect" {
				t.Fatalf("step event note = %q", f.Note)
			}
		case "paused":
			if f.Reason != "look" {
				t.Fatalf("paused event reason = %q", f.Reason)
			}
		case "redirected":
			if f.Message != "narrow scope" || f.Mode != "append" {
				t.Fatalf("redirected event = %+v", f)
			}
		case "done":
			if f.State != StateDone {
				t.Fatalf("done event state = %q", f.State)
			}
		}
	}
}

func TestEventsFanOutToEverySubscriber(t *testing.T) {
	ch := newChannel(t, Options{})
	w1 := dialRaw(t, ch.Path())
	w2 := dialRaw(t, ch.Path())
	if err := w1.c.Subscribe(); err != nil {
		t.Fatalf("Subscribe w1: %v", err)
	}
	if err := w2.c.Subscribe(); err != nil {
		t.Fatalf("Subscribe w2: %v", err)
	}
	if _, err := ch.Checkpoint(context.Background(), "n"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	for _, w := range []*testConn{w1, w2} {
		f, err := w.c.Next()
		if err != nil || f.Event != "step" || f.Step != 1 {
			t.Fatalf("subscriber missed the step event: %+v, %v", f, err)
		}
	}
}

func TestStepCounterAdvancesAcrossCheckpoints(t *testing.T) {
	ch := newChannel(t, Options{})
	for i := 1; i <= 3; i++ {
		dec, err := ch.Checkpoint(context.Background(), "")
		if err != nil || dec.Step != i {
			t.Fatalf("checkpoint %d: step = %d, err = %v", i, dec.Step, err)
		}
	}
	if ch.Step() != 3 {
		t.Fatalf("Step() = %d, want 3", ch.Step())
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	ch := newChannel(t, Options{})
	if err := ch.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := ch.Close(); err != nil {
		t.Fatalf("second Close must be a no-op, got %v", err)
	}
}

func TestDefaultOptionsAreApplied(t *testing.T) {
	ch := newChannel(t, Options{})
	tc := dialRaw(t, ch.Path())
	if tc.hello.Agent != "agent" {
		t.Fatalf("default agent name = %q, want %q", tc.hello.Agent, "agent")
	}
	// Default backlog is 64: the 65th queued directive is refused.
	for i := 0; i < 64; i++ {
		tc.mustAccept(OpRedirect, Directive{Message: "m"})
	}
	if _, err := tc.directive(OpRedirect, Directive{Message: "m"}); err == nil {
		t.Fatalf("default backlog should cap at 64")
	}
}
