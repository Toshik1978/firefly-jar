// Package app wires the firefly-jar command-line entry point: argument parsing, subcommand dispatch and
// the process exit code.
package app

import (
	"fmt"
	"io"
)

// usage is printed to stderr when no subcommand, or an unrecognized one, is given.
const usage = "usage: firefly-jar <check|auth|accounts> [flags]"

// Run is the CLI entry point invoked by main. Stdin, stdout and stderr are injected so callers, and
// tests, never touch the real console. It returns the process exit code.
func Run(_ []string, _ io.Reader, _, stderr io.Writer) int {
	fmt.Fprintln(stderr, usage)

	return 2
}
