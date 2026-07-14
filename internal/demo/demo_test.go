// Demo-loop tests: the loop must be steerable exactly like a real agent.
// Steering is injected through the OnReady/StepHook test hooks, so every
// directive is admitted at a known point between two checkpoints and the
// tests stay fully deterministic with Interval 0.
package demo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JaydenCJ/steerd"
)

func sockPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "steerd")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "demo.sock")
}

func dialDemo(t *testing.T, path string) *steerd.Client {
	t.Helper()
	c, err := steerd.Dial(path)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	c.Timeout = 5 * time.Second
	t.Cleanup(func() { c.Close() })
	return c
}

func TestRunCompletesAllStepsAndNarratesThem(t *testing.T) {
	var out strings.Builder
	err := Run(&out, Options{Socket: sockPath(t), Steps: 5, Task: "index the wiki"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	s := out.String()
	for _, want := range []string{
		"task: index the wiki",
		"step 1/5 collect: index the wiki",
		"step 4/5 revise: index the wiki",
		"step 5/5 collect: index the wiki", // phases cycle after revise
		"done: 5 steps completed",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("output missing %q:\n%s", want, s)
		}
	}
}

func TestRunAppliesDefaultsForStepsAndTask(t *testing.T) {
	var out strings.Builder
	if err := Run(&out, Options{Socket: sockPath(t)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "done: 8 steps completed") {
		t.Fatalf("default steps should be 8:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "task: summarize the release notes") {
		t.Fatalf("default task missing:\n%s", out.String())
	}
}

func TestRunUsesSingularNounForOneStep(t *testing.T) {
	var out strings.Builder
	if err := Run(&out, Options{Socket: sockPath(t), Steps: 1}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "done: 1 step completed") {
		t.Fatalf("one step must not read \"1 steps\":\n%s", out.String())
	}
}

func TestOnReadyFiresAfterTheSocketExists(t *testing.T) {
	path := sockPath(t)
	var ready bool
	err := Run(&strings.Builder{}, Options{Socket: path, Steps: 1, OnReady: func() {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("socket must exist when OnReady fires: %v", err)
		}
		ready = true
	}})
	if err != nil || !ready {
		t.Fatalf("Run err = %v, ready = %v", err, ready)
	}
}

func TestCancelDirectiveStopsTheDemoWithReason(t *testing.T) {
	path := sockPath(t)
	var out strings.Builder
	err := Run(&out, Options{Socket: path, Steps: 8, StepHook: func(step int) {
		if step != 2 {
			return
		}
		c := dialDemo(t, path)
		if _, err := c.Cancel("saw enough", false); err != nil {
			t.Errorf("Cancel: %v", err)
		}
	}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "cancelled at step 3/8 (reason: saw enough)") {
		t.Fatalf("cancel narration missing:\n%s", s)
	}
	if strings.Contains(s, "done:") || strings.Contains(s, "step 4/8") {
		t.Fatalf("demo must stop at the cancel:\n%s", s)
	}
}

func TestRedirectAppendExtendsTheTask(t *testing.T) {
	path := sockPath(t)
	var out strings.Builder
	err := Run(&out, Options{Socket: path, Steps: 3, Task: "draft the report", StepHook: func(step int) {
		if step != 1 {
			return
		}
		c := dialDemo(t, path)
		if _, err := c.Redirect("cite primary sources", "", false); err != nil {
			t.Errorf("Redirect: %v", err)
		}
	}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, `redirect applied (append): "cite primary sources"`) {
		t.Fatalf("append narration missing:\n%s", s)
	}
	if !strings.Contains(s, "step 2/3 analyze: draft the report; cite primary sources") {
		t.Fatalf("task must carry the appended instruction:\n%s", s)
	}
}

func TestRedirectReplaceSwapsTheTask(t *testing.T) {
	path := sockPath(t)
	var out strings.Builder
	err := Run(&out, Options{Socket: path, Steps: 3, Task: "old task", StepHook: func(step int) {
		if step != 1 {
			return
		}
		c := dialDemo(t, path)
		if _, err := c.Redirect("audit the invoices instead", "replace", false); err != nil {
			t.Errorf("Redirect: %v", err)
		}
	}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, `redirect applied (replace): task is now "audit the invoices instead"`) {
		t.Fatalf("replace narration missing:\n%s", s)
	}
	if !strings.Contains(s, "step 2/3 analyze: audit the invoices instead") {
		t.Fatalf("task must be replaced:\n%s", s)
	}
	if strings.Contains(s, "step 2/3 analyze: old task") {
		t.Fatalf("old task must not survive a replace:\n%s", s)
	}
}

func TestPauseHoldsTheDemoUntilResume(t *testing.T) {
	// The hook admits a pause, then a helper goroutine waits for it to be
	// applied (the demo is now provably blocked at checkpoint 2) before
	// resuming. Synchronisation is pure protocol acks — no sleeps.
	path := sockPath(t)
	resumed := make(chan struct{})
	var out strings.Builder
	err := Run(&out, Options{Socket: path, Steps: 3, StepHook: func(step int) {
		if step != 1 {
			return
		}
		c := dialDemo(t, path)
		ack, err := c.Pause("checking in", false)
		if err != nil {
			t.Errorf("Pause: %v", err)
			close(resumed)
			return
		}
		go func() {
			defer close(resumed)
			if _, err := c.Await(ack.ID); err != nil {
				t.Errorf("Await pause: %v", err)
				return
			}
			if _, err := c.Resume(true); err != nil {
				t.Errorf("Resume: %v", err)
			}
		}()
	}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	<-resumed // join the steering goroutine before the test ends
	if !strings.Contains(out.String(), "done: 3 steps completed") {
		t.Fatalf("demo must finish after the resume:\n%s", out.String())
	}
}

func TestSocketIsRemovedAfterTheRun(t *testing.T) {
	path := sockPath(t)
	if err := Run(&strings.Builder{}, Options{Socket: path, Steps: 1}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("socket file must be gone after Run returns")
	}
}
