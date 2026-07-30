// Package doctor runs local diagnostic checks and reports their results.
package doctor

import (
	"fmt"
	"io"
)

// Status is the outcome of a single check.
type Status int

const (
	OK Status = iota
	Warn
	Fail
)

// Result is what a check reports.
type Result struct {
	Name   string
	Status Status
	Detail string
}

// Check is a single diagnostic.
type Check func() Result

// Run executes checks in order and collects their results.
func Run(checks []Check) []Result {
	results := make([]Result, 0, len(checks))
	for _, c := range checks {
		results = append(results, c())
	}
	return results
}

// ExitCode is 1 if any check failed, else 0. A warning is advisory —
// degradation over binary failure — and does not fail the command.
func ExitCode(results []Result) int {
	for _, r := range results {
		if r.Status == Fail {
			return 1
		}
	}
	return 0
}

// Report writes a human-readable report of results to w.
func Report(w io.Writer, results []Result) {
	warns, fails := 0, 0
	for _, r := range results {
		switch r.Status {
		case Warn:
			warns++
		case Fail:
			fails++
		}
		fmt.Fprintf(w, "  %s  %-18s %s\n", symbol(r.Status), r.Name, r.Detail)
	}

	fmt.Fprintln(w)
	switch {
	case fails > 0:
		fmt.Fprintf(w, "%d problem(s), %d warning(s).\n", fails, warns)
	case warns > 0:
		fmt.Fprintf(w, "%d warning(s).\n", warns)
	default:
		fmt.Fprintln(w, "Everything looks OK.")
	}
}

func symbol(s Status) string {
	switch s {
	case OK:
		return "✓"
	case Warn:
		return "!"
	default:
		return "✗"
	}
}

// ContainerRuntime checks whether a container runtime is available on PATH.
// lookPath is injected for testing; pass exec.LookPath in production.
func ContainerRuntime(lookPath func(string) (string, error)) Check {
	return func() Result {
		for _, name := range []string{"docker", "podman"} {
			if _, err := lookPath(name); err == nil {
				return Result{Name: "container runtime", Status: OK, Detail: name + " found"}
			}
		}
		return Result{
			Name:   "container runtime",
			Status: Warn,
			Detail: "no docker or podman on PATH — needed for `chmura init` and local dev",
		}
	}
}
