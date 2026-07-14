// Command steerd steers a running agent loop over its Unix control socket:
// pause, resume, redirect, cancel, status, watch — plus a steerable demo
// loop for trying the protocol without writing any code.
package main

import (
	"os"

	"github.com/JaydenCJ/steerd/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
