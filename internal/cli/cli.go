// Package cli implements the steerd command-line interface: the controller
// subcommands (pause, resume, redirect, cancel, status, watch), the
// steerable demo loop, and version.
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/JaydenCJ/steerd"
	"github.com/JaydenCJ/steerd/internal/demo"
	"github.com/JaydenCJ/steerd/internal/version"
)

// Exit codes, documented in the README.
const (
	exitOK       = 0
	exitRejected = 1 // the agent refused the directive
	exitUsage    = 2
	exitRuntime  = 3 // connection or I/O failure
)

const usageText = `steerd — steer a running agent loop over its Unix control socket

Usage:
  steerd <command> [flags]

Controller commands (need --socket PATH or STEERD_SOCKET):
  pause      hold the loop at its next checkpoint      [--reason S] [--no-wait]
  resume     release a paused loop                     [--no-wait]
  redirect   inject an instruction  --message S        [--mode append|replace] [--no-wait]
  cancel     stop the loop gracefully                  [--reason S] [--no-wait]
  status     print the agent's current state           [--format text|json]
  watch      stream state changes and steps            [--format text|json]

Other commands:
  demo       run a steerable demo agent loop           [--steps N] [--interval D] [--task S]
  version    print the steerd version

Common flags:
  --socket PATH   agent control socket (or set STEERD_SOCKET)
  --timeout D     give up waiting for an acknowledgement (default 60s, 0 = never)

Exit codes: 0 ok, 1 directive rejected, 2 usage error, 3 connection failure.
`

// Run executes one steerd invocation and returns its exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "pause", "resume", "cancel":
		return runDirective(steerd.Op(cmd), rest, stdout, stderr)
	case "redirect":
		return runRedirect(rest, stdout, stderr)
	case "status":
		return runStatus(rest, stdout, stderr)
	case "watch":
		return runWatch(rest, stdout, stderr)
	case "demo":
		return runDemo(rest, stdout, stderr)
	case "version", "--version", "-V":
		fmt.Fprintf(stdout, "steerd %s\n", version.Version)
		return exitOK
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usageText)
		return exitOK
	default:
		fmt.Fprintf(stderr, "steerd: unknown command %q (try \"steerd help\")\n", cmd)
		return exitUsage
	}
}

// commonFlags holds the flags shared by every controller subcommand.
type commonFlags struct {
	socket  string
	timeout time.Duration
}

func addCommon(fs *flag.FlagSet, cf *commonFlags) {
	fs.StringVar(&cf.socket, "socket", "", "agent control socket path (or STEERD_SOCKET)")
	fs.DurationVar(&cf.timeout, "timeout", 60*time.Second, "acknowledgement wait limit (0 = never)")
}

func parse(fs *flag.FlagSet, args []string, stderr io.Writer) bool {
	fs.SetOutput(stderr)
	return fs.Parse(args) == nil
}

// dial resolves the socket path and opens a client, mapping every failure
// mode to the right exit code via the returned status.
func dial(cf commonFlags, stderr io.Writer) (*steerd.Client, int) {
	path := cf.socket
	if path == "" {
		path = os.Getenv("STEERD_SOCKET")
	}
	if path == "" {
		fmt.Fprintln(stderr, "steerd: no socket: pass --socket PATH or set STEERD_SOCKET")
		return nil, exitUsage
	}
	c, err := steerd.Dial(path)
	if err != nil {
		fmt.Fprintf(stderr, "steerd: %s\n", errText(err))
		return nil, exitRuntime
	}
	c.Timeout = cf.timeout
	return c, exitOK
}

// runDirective handles pause, resume and cancel — the three payload-light
// directives that share the accepted/applied reporting flow.
func runDirective(op steerd.Op, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(string(op), flag.ContinueOnError)
	var cf commonFlags
	addCommon(fs, &cf)
	reason := fs.String("reason", "", "annotation recorded with the directive")
	noWait := fs.Bool("no-wait", false, "return after the accepted ack, do not wait for applied")
	if !parse(fs, args, stderr) {
		return exitUsage
	}
	client, code := dial(cf, stderr)
	if code != exitOK {
		return code
	}
	defer client.Close()
	ack, err := client.Send(op, steerd.Directive{Reason: *reason})
	return reportDirective(op, ack, err, client, *noWait, stdout, stderr)
}

func runRedirect(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("redirect", flag.ContinueOnError)
	var cf commonFlags
	addCommon(fs, &cf)
	message := fs.String("message", "", "instruction to inject (required)")
	mode := fs.String("mode", "append", `how the loop should take it: "append" or "replace"`)
	noWait := fs.Bool("no-wait", false, "return after the accepted ack, do not wait for applied")
	if !parse(fs, args, stderr) {
		return exitUsage
	}
	if *message == "" {
		fmt.Fprintln(stderr, "steerd: redirect requires --message")
		return exitUsage
	}
	if *mode != "append" && *mode != "replace" {
		fmt.Fprintf(stderr, "steerd: unknown redirect mode %q (want \"append\" or \"replace\")\n", *mode)
		return exitUsage
	}
	client, code := dial(cf, stderr)
	if code != exitOK {
		return code
	}
	defer client.Close()
	ack, err := client.Send(steerd.OpRedirect, steerd.Directive{Message: *message, Mode: *mode})
	return reportDirective(steerd.OpRedirect, ack, err, client, *noWait, stdout, stderr)
}

// reportDirective prints the two acknowledgement stages and maps errors to
// exit codes. A rejection is a clean, expected outcome — exit 1, reason on
// stderr — not a crash.
func reportDirective(op steerd.Op, ack steerd.Frame, err error, client *steerd.Client, noWait bool, stdout, stderr io.Writer) int {
	if err != nil {
		return directiveErr(err, stderr)
	}
	fmt.Fprintf(stdout, "%s: accepted (seq %d)\n", op, ack.Seq)
	if noWait {
		return exitOK
	}
	applied, err := client.Await(ack.ID)
	if err != nil {
		return directiveErr(err, stderr)
	}
	fmt.Fprintf(stdout, "%s: applied at step %d\n", op, applied.Step)
	return exitOK
}

func directiveErr(err error, stderr io.Writer) int {
	var rej *steerd.RejectedError
	if errors.As(err, &rej) {
		fmt.Fprintf(stderr, "steerd: %s\n", errText(rej))
		return exitRejected
	}
	if steerd.IsTimeout(err) {
		fmt.Fprintln(stderr, "steerd: timed out waiting for an acknowledgement (is the loop checkpointing?)")
		return exitRuntime
	}
	fmt.Fprintf(stderr, "steerd: %s\n", errText(err))
	return exitRuntime
}

// errText renders an error for the CLI's "steerd: " prefix. Library errors
// already carry the package prefix (idiomatic for importers), so strip it
// here rather than printing "steerd: steerd: …".
func errText(err error) string {
	return strings.TrimPrefix(err.Error(), "steerd: ")
}

func runStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	var cf commonFlags
	addCommon(fs, &cf)
	format := fs.String("format", "text", `output format: "text" or "json"`)
	if !parse(fs, args, stderr) {
		return exitUsage
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "steerd: unknown format %q (want \"text\" or \"json\")\n", *format)
		return exitUsage
	}
	client, code := dial(cf, stderr)
	if code != exitOK {
		return code
	}
	defer client.Close()
	st, err := client.Status()
	if err != nil {
		return directiveErr(err, stderr)
	}
	if *format == "json" {
		b, _ := json.Marshal(st)
		fmt.Fprintln(stdout, string(b))
		return exitOK
	}
	fmt.Fprintf(stdout, "agent    %s (pid %d)\n", st.Agent, st.PID)
	fmt.Fprintf(stdout, "state    %s\n", st.State)
	fmt.Fprintf(stdout, "step     %d\n", st.Step)
	if st.Task != "" {
		fmt.Fprintf(stdout, "task     %s\n", st.Task)
	}
	if st.Note != "" {
		fmt.Fprintf(stdout, "note     %s\n", st.Note)
	}
	fmt.Fprintf(stdout, "pending  %d\n", st.Pending)
	return exitOK
}

func runWatch(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	var cf commonFlags
	fs.StringVar(&cf.socket, "socket", "", "agent control socket path (or STEERD_SOCKET)")
	// Watches are long-lived, so unlike the other commands the default is
	// to wait forever rather than 60 s.
	fs.DurationVar(&cf.timeout, "timeout", 0, "stop watching after this long with no events (0 = never)")
	format := fs.String("format", "text", `output format: "text" or "json"`)
	if !parse(fs, args, stderr) {
		return exitUsage
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "steerd: unknown format %q (want \"text\" or \"json\")\n", *format)
		return exitUsage
	}
	client, code := dial(cf, stderr)
	if code != exitOK {
		return code
	}
	defer client.Close()
	if err := client.Subscribe(); err != nil {
		return directiveErr(err, stderr)
	}
	h := client.Hello()
	fmt.Fprintf(stdout, "watching %s (state %s, step %d)\n", h.Agent, h.State, h.Step)
	for {
		f, err := client.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return exitOK // agent went away after done
			}
			return directiveErr(err, stderr)
		}
		if f.Type != "event" {
			continue
		}
		if *format == "json" {
			b, _ := json.Marshal(f)
			fmt.Fprintln(stdout, string(b))
		} else {
			fmt.Fprintln(stdout, formatEvent(f))
		}
		if f.Event == "done" {
			return exitOK
		}
	}
}

// formatEvent renders one event frame as a stable, grep-friendly line.
func formatEvent(f steerd.Frame) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-11s step=%d", f.Event, f.Step)
	if f.Note != "" {
		fmt.Fprintf(&b, " note=%q", f.Note)
	}
	if f.Message != "" {
		fmt.Fprintf(&b, " message=%q mode=%s", f.Message, f.Mode)
	}
	if f.Reason != "" {
		fmt.Fprintf(&b, " reason=%q", f.Reason)
	}
	return b.String()
}

func runDemo(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	socket := fs.String("socket", "", "control socket path to create (or STEERD_SOCKET)")
	steps := fs.Int("steps", 8, "number of work steps to simulate")
	interval := fs.Duration("interval", 200*time.Millisecond, "pause between steps (0 = flat out)")
	task := fs.String("task", "", "what the demo agent pretends to work on")
	if !parse(fs, args, stderr) {
		return exitUsage
	}
	path := *socket
	if path == "" {
		path = os.Getenv("STEERD_SOCKET")
	}
	if path == "" {
		fmt.Fprintln(stderr, "steerd: no socket: pass --socket PATH or set STEERD_SOCKET")
		return exitUsage
	}
	err := demo.Run(stdout, demo.Options{
		Socket:   path,
		Steps:    *steps,
		Interval: *interval,
		Task:     *task,
	})
	if err != nil {
		fmt.Fprintf(stderr, "steerd: demo: %v\n", err)
		return exitRuntime
	}
	return exitOK
}
