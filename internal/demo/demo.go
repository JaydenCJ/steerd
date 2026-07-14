// Package demo implements a small, deterministic, steerable agent loop.
// It exists so the protocol can be tried end-to-end — and demonstrated in
// scripts/smoke.sh — without writing any integration code.
package demo

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/JaydenCJ/steerd"
)

// phases is the fixed work cycle the demo pretends to run.
var phases = [4]string{"collect", "analyze", "draft", "revise"}

// Options configures one demo run. The two hooks exist for deterministic
// tests: OnReady fires once the socket is accepting controllers, StepHook
// after each step's output line (so a directive sent inside the hook is
// applied at the very next checkpoint).
type Options struct {
	Socket   string
	Steps    int
	Interval time.Duration
	Task     string

	OnReady  func()
	StepHook func(step int)
}

// Run executes the demo loop, writing its narration to w. It returns nil
// both when the loop finishes and when a controller cancels it; only setup
// or channel failures are errors.
func Run(w io.Writer, o Options) error {
	if o.Steps <= 0 {
		o.Steps = 8
	}
	if o.Task == "" {
		o.Task = "summarize the release notes"
	}
	ch, err := steerd.Listen(o.Socket, steerd.Options{Agent: "steerd-demo", Task: o.Task})
	if err != nil {
		return err
	}
	defer ch.Close()

	fmt.Fprintf(w, "demo agent listening on %s\n", o.Socket)
	fmt.Fprintf(w, "task: %s\n", o.Task)
	if o.OnReady != nil {
		o.OnReady()
	}

	task := o.Task
	for i := 1; i <= o.Steps; i++ {
		phase := phases[(i-1)%len(phases)]
		note := fmt.Sprintf("step %d/%d %s", i, o.Steps, phase)
		dec, err := ch.Checkpoint(context.Background(), note)
		if err != nil {
			return err
		}
		for _, r := range dec.Redirects {
			if r.Mode == "replace" {
				task = r.Message
				fmt.Fprintf(w, "redirect applied (replace): task is now %q\n", task)
			} else {
				task = task + "; " + r.Message
				fmt.Fprintf(w, "redirect applied (append): %q\n", r.Message)
			}
		}
		if dec.Cancelled {
			reason := dec.Reason
			if reason == "" {
				reason = "none given"
			}
			fmt.Fprintf(w, "cancelled at step %d/%d (reason: %s)\n", i, o.Steps, reason)
			return nil
		}
		fmt.Fprintf(w, "step %d/%d %s: %s\n", i, o.Steps, phase, task)
		if o.StepHook != nil {
			o.StepHook(i)
		}
		if o.Interval > 0 {
			time.Sleep(o.Interval)
		}
	}
	noun := "steps"
	if o.Steps == 1 {
		noun = "step"
	}
	fmt.Fprintf(w, "done: %d %s completed\n", o.Steps, noun)
	return nil
}
