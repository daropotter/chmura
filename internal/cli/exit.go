package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// Exit codes, per docs/cli.md. Codes 3 (state conflict), 4 (degraded under
// --fail-on-degraded), and 130 (Ctrl+C) are part of the contract but are only
// produced once the operations that raise them exist; they arrive with them.
const (
	ExitOK    = 0 // success
	ExitError = 1 // execution error
	ExitUsage = 2 // bad arguments, configuration, or a missing confirmation
)

// usageError marks an error as an argument/usage problem rather than an
// execution failure, so it maps to ExitUsage. Flag-parsing errors are wrapped
// in it via the root command's FlagErrorFunc.
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

// Execute runs the chmura CLI and returns the process exit code. It renders its
// own error output, so the caller only forwards the code to os.Exit.
func Execute(args []string, stdout, stderr io.Writer) int {
	root := NewRootCmd()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	// We classify and print errors ourselves; keep cobra from doing either.
	root.SilenceErrors = true
	root.SilenceUsage = true

	err := root.Execute()
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err.Error())
	}
	return classify(err)
}

// classify maps an error returned by cobra to a contract exit code. Argument
// and usage problems are ExitUsage; anything else is an execution error.
func classify(err error) int {
	if err == nil {
		return ExitOK
	}
	var ue *usageError
	if errors.As(err, &ue) {
		return ExitUsage
	}
	// cobra reports an unknown subcommand and a NoArgs violation with the same
	// "unknown command ..." message; both are usage problems.
	if strings.HasPrefix(err.Error(), "unknown command") {
		return ExitUsage
	}
	return ExitError
}
