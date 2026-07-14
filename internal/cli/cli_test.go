// CLI tests: every subcommand is run in-process through Run against real
// channels on real Unix sockets. Agent loops are either hand-driven (one
// Checkpoint at a time) or busy loops, so waits resolve through protocol
// acknowledgements and never through timing.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/JaydenCJ/steerd"
	"github.com/JaydenCJ/steerd/internal/version"
)

func sockPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "steerd")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "cli.sock")
}

// run invokes the CLI in-process and captures both streams.
func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code = Run(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

// startAgent opens a channel and a busy checkpoint loop that ends on
// cancel or Close. It returns the channel and a join function that yields
// every decision the loop saw.
func startAgent(t *testing.T, opts steerd.Options) (*steerd.Channel, func() []steerd.Decision) {
	t.Helper()
	ch, err := steerd.Listen(sockPath(t), opts)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ch.Close() })
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen []steerd.Decision
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			dec, err := ch.Checkpoint(context.Background(), "")
			if err != nil {
				return
			}
			mu.Lock()
			seen = append(seen, dec)
			mu.Unlock()
			if dec.Cancelled {
				return
			}
		}
	}()
	return ch, func() []steerd.Decision {
		wg.Wait()
		mu.Lock()
		defer mu.Unlock()
		return seen
	}
}

func TestVersionPrintsTheManifestVersion(t *testing.T) {
	code, out, _ := run(t, "version")
	if code != 0 || out != "steerd "+version.Version+"\n" {
		t.Fatalf("code = %d, out = %q", code, out)
	}
}

func TestHelpPrintsUsageAndExitsZero(t *testing.T) {
	code, out, _ := run(t, "help")
	if code != 0 || !strings.Contains(out, "steerd <command>") {
		t.Fatalf("code = %d, out = %q", code, out)
	}
}

func TestNoArgumentsIsAUsageError(t *testing.T) {
	code, _, errOut := run(t)
	if code != 2 || !strings.Contains(errOut, "Usage:") {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func TestUnknownCommandIsAUsageError(t *testing.T) {
	code, _, errOut := run(t, "reboot")
	if code != 2 || !strings.Contains(errOut, `unknown command "reboot"`) {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func TestMissingSocketIsAUsageError(t *testing.T) {
	t.Setenv("STEERD_SOCKET", "")
	code, _, errOut := run(t, "pause")
	if code != 2 || !strings.Contains(errOut, "pass --socket PATH or set STEERD_SOCKET") {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func TestUnreachableSocketExitsThree(t *testing.T) {
	code, _, errOut := run(t, "status", "--socket", filepath.Join(t.TempDir(), "nope.sock"))
	if code != 3 || !strings.Contains(errOut, "dial") {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func TestRedirectWithoutMessageIsAUsageError(t *testing.T) {
	code, _, errOut := run(t, "redirect", "--socket", "irrelevant")
	if code != 2 || !strings.Contains(errOut, "redirect requires --message") {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func TestRedirectWithUnknownModeIsAUsageError(t *testing.T) {
	// Caught locally, before dialing — the socket path never has to exist.
	code, _, errOut := run(t, "redirect", "--socket", "irrelevant",
		"--message", "hi", "--mode", "prepend")
	if code != 2 || !strings.Contains(errOut, `unknown redirect mode "prepend"`) {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func TestStatusTextShowsAgentStateStepAndTask(t *testing.T) {
	ch, err := steerd.Listen(sockPath(t), steerd.Options{Agent: "worker-7", Task: "sort the backlog"})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ch.Close() })
	if _, err := ch.Checkpoint(context.Background(), "step 1/4 collect"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	code, out, _ := run(t, "status", "--socket", ch.Path())
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	for _, want := range []string{
		"agent    worker-7",
		"state    running",
		"step     1",
		"task     sort the backlog",
		"note     step 1/4 collect",
		"pending  0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("status output missing %q:\n%s", want, out)
		}
	}
}

func TestStatusJSONIsMachineReadable(t *testing.T) {
	ch, err := steerd.Listen(sockPath(t), steerd.Options{Agent: "j"})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ch.Close() })

	code, out, _ := run(t, "status", "--socket", ch.Path(), "--format", "json")
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	var f steerd.Frame
	if err := json.Unmarshal([]byte(out), &f); err != nil {
		t.Fatalf("status --format json must emit valid JSON: %v\n%s", err, out)
	}
	if f.Type != "state" || f.State != steerd.StateRunning || f.Agent != "j" {
		t.Fatalf("frame = %+v", f)
	}
}

func TestBadFormatIsAUsageError(t *testing.T) {
	code, _, errOut := run(t, "status", "--socket", "irrelevant", "--format", "yaml")
	if code != 2 || !strings.Contains(errOut, `unknown format "yaml"`) {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func TestSocketFallsBackToEnvironmentVariable(t *testing.T) {
	ch, err := steerd.Listen(sockPath(t), steerd.Options{Agent: "envy"})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ch.Close() })
	t.Setenv("STEERD_SOCKET", ch.Path())

	code, out, _ := run(t, "status")
	if code != 0 || !strings.Contains(out, "agent    envy") {
		t.Fatalf("code = %d, out = %q", code, out)
	}
}

func TestPauseReportsBothAcknowledgementStages(t *testing.T) {
	ch, join := startAgent(t, steerd.Options{})
	code, out, _ := run(t, "pause", "--socket", ch.Path())
	if code != 0 {
		t.Fatalf("code = %d, out = %q", code, out)
	}
	if !strings.Contains(out, "pause: accepted (seq 1)") {
		t.Fatalf("accepted line missing:\n%s", out)
	}
	if !strings.Contains(out, "pause: applied at step ") {
		t.Fatalf("applied line missing:\n%s", out)
	}
	if got := ch.State(); got != steerd.StatePaused {
		t.Fatalf("state = %q, want paused", got)
	}
	// Release and stop the loop so the test leaves nothing spinning.
	if code, _, _ := run(t, "resume", "--socket", ch.Path()); code != 0 {
		t.Fatalf("resume code = %d", code)
	}
	if code, _, _ := run(t, "cancel", "--socket", ch.Path()); code != 0 {
		t.Fatalf("cancel code = %d", code)
	}
	join()
}

func TestNoWaitReturnsAfterAcceptanceOnly(t *testing.T) {
	// No checkpoint ever runs: --no-wait must still exit 0 with just the
	// accepted line, proving it does not wait for application.
	ch, err := steerd.Listen(sockPath(t), steerd.Options{})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ch.Close() })

	code, out, _ := run(t, "pause", "--socket", ch.Path(), "--no-wait")
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out, "pause: accepted") || strings.Contains(out, "applied") {
		t.Fatalf("no-wait output = %q", out)
	}
}

func TestRejectedDirectiveExitsOneWithReasonOnStderr(t *testing.T) {
	ch, err := steerd.Listen(sockPath(t), steerd.Options{})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ch.Close() })

	code, _, errOut := run(t, "resume", "--socket", ch.Path())
	if code != 1 {
		t.Fatalf("rejected directive must exit 1, got %d", code)
	}
	if !strings.Contains(errOut, "resume rejected: agent is not paused") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestRedirectAndCancelReachTheLoop(t *testing.T) {
	ch, join := startAgent(t, steerd.Options{})
	code, out, _ := run(t, "redirect", "--socket", ch.Path(),
		"--message", "focus on cmd/ only", "--mode", "replace")
	if code != 0 || !strings.Contains(out, "redirect: applied at step ") {
		t.Fatalf("code = %d, out = %q", code, out)
	}
	code, out, _ = run(t, "cancel", "--socket", ch.Path(), "--reason", "budget spent")
	if code != 0 || !strings.Contains(out, "cancel: applied at step ") {
		t.Fatalf("code = %d, out = %q", code, out)
	}

	var sawRedirect, sawCancel bool
	for _, dec := range join() {
		for _, r := range dec.Redirects {
			if r.Message == "focus on cmd/ only" && r.Mode == "replace" {
				sawRedirect = true
			}
		}
		if dec.Cancelled && dec.Reason == "budget spent" {
			sawCancel = true
		}
	}
	if !sawRedirect || !sawCancel {
		t.Fatalf("loop saw redirect=%v cancel=%v", sawRedirect, sawCancel)
	}
}

// syncWriter signals readyCh the first time a line is written, letting a
// test wait until watch has provably subscribed before driving the agent.
type syncWriter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	ready chan struct{}
	once  sync.Once
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	w.once.Do(func() { close(w.ready) })
	return n, err
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestWatchStreamsEventsUntilDone(t *testing.T) {
	ch, err := steerd.Listen(sockPath(t), steerd.Options{Agent: "watched"})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ch.Close() })

	out := &syncWriter{ready: make(chan struct{})}
	codeCh := make(chan int, 1)
	go func() {
		var errBuf bytes.Buffer
		codeCh <- Run([]string{"watch", "--socket", ch.Path()}, out, &errBuf)
	}()

	<-out.ready // the "watching …" banner is printed after Subscribe succeeds
	if _, err := ch.Checkpoint(context.Background(), "step 1/1 collect"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	ch.Close() // emits the final done event; watch must exit 0

	if code := <-codeCh; code != 0 {
		t.Fatalf("watch exit code = %d\n%s", code, out.String())
	}
	s := out.String()
	for _, want := range []string{
		"watching watched (state running, step 0)",
		`step        step=1 note="step 1/1 collect"`,
		"done        step=1",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("watch output missing %q:\n%s", want, s)
		}
	}
}

func TestWatchJSONEmitsOneFramePerLine(t *testing.T) {
	ch, err := steerd.Listen(sockPath(t), steerd.Options{})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ch.Close() })

	out := &syncWriter{ready: make(chan struct{})}
	codeCh := make(chan int, 1)
	go func() {
		var errBuf bytes.Buffer
		codeCh <- Run([]string{"watch", "--socket", ch.Path(), "--format", "json"}, out, &errBuf)
	}()
	<-out.ready
	if _, err := ch.Checkpoint(context.Background(), "n1"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	ch.Close()
	if code := <-codeCh; code != 0 {
		t.Fatalf("watch exit code = %d", code)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) < 3 { // banner + step event + done event
		t.Fatalf("want at least 3 lines, got %q", lines)
	}
	var f steerd.Frame
	if err := json.Unmarshal([]byte(lines[1]), &f); err != nil || f.Event != "step" {
		t.Fatalf("line 2 must be the step event frame: %v, %q", err, lines[1])
	}
}

func TestDemoCommandRunsToCompletion(t *testing.T) {
	path := sockPath(t)
	code, out, _ := run(t, "demo", "--socket", path, "--steps", "3", "--interval", "0", "--task", "demo it")
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	for _, want := range []string{
		fmt.Sprintf("demo agent listening on %s", path),
		"step 3/3 draft: demo it",
		"done: 3 steps completed",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("demo output missing %q:\n%s", want, out)
		}
	}
}

func TestDemoWithoutSocketIsAUsageError(t *testing.T) {
	t.Setenv("STEERD_SOCKET", "")
	code, _, errOut := run(t, "demo")
	if code != 2 || !strings.Contains(errOut, "pass --socket PATH or set STEERD_SOCKET") {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func TestBadFlagIsAUsageError(t *testing.T) {
	code, _, errOut := run(t, "pause", "--sockets", "typo")
	if code != 2 || !strings.Contains(errOut, "flag provided but not defined") {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}
