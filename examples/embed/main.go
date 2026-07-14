// Command embed is the smallest realistic integration of steerd into an
// agent loop: open a channel, checkpoint between work items, honour
// redirects and cancellation. Run it, then steer it from another terminal:
//
//	go run ./examples/embed /tmp/agent.sock
//	steerd pause  --socket /tmp/agent.sock
//	steerd redirect --socket /tmp/agent.sock --message "skip flaky suites"
//	steerd resume --socket /tmp/agent.sock
//	steerd cancel --socket /tmp/agent.sock --reason "enough"
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/JaydenCJ/steerd"
)

func main() {
	socket := "/tmp/agent.sock"
	if len(os.Args) > 1 {
		socket = os.Args[1]
	}

	ch, err := steerd.Listen(socket, steerd.Options{
		Agent: "embed-example",
		Task:  "run the integration suites",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer ch.Close()
	fmt.Printf("steer me with: steerd status --socket %s\n", socket)

	suites := []string{"auth", "billing", "search", "export", "webhooks"}
	notes := []string{}
	for i, suite := range suites {
		// One checkpoint per unit of work. This is the whole integration:
		// the loop blocks here while paused and learns about redirects
		// and cancellation from the returned decision.
		dec, err := ch.Checkpoint(context.Background(),
			fmt.Sprintf("suite %d/%d %s", i+1, len(suites), suite))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		for _, r := range dec.Redirects {
			if r.Mode == "replace" {
				notes = []string{r.Message}
			} else {
				notes = append(notes, r.Message)
			}
			fmt.Printf("  operator says: %s\n", r.Message)
		}
		if dec.Cancelled {
			fmt.Printf("stopping early (reason: %s)\n", dec.Reason)
			return
		}

		fmt.Printf("running %s suite (notes: %v)\n", suite, notes)
		time.Sleep(2 * time.Second) // pretend work
	}
	fmt.Println("all suites finished")
}
