// Package version reports the Chmura build version, shared by all artifacts.
package version

import (
	"fmt"
	"io"
)

// Version is the build version. The default is a fallback for local builds;
// release builds override it via -ldflags "-X .../version.Version=<v>".
var Version = "0.0.0-dev"

// Run handles a version request common to every binary. If args ask for the
// version — the --version flag anywhere, or the "version" subcommand first —
// it writes the version to stdout and reports handled=true with exit code 0.
// Otherwise it reports handled=false and writes nothing, leaving the caller to
// handle the args.
func Run(args []string, stdout io.Writer) (handled bool, exitCode int) {
	if !requested(args) {
		return false, 0
	}
	fmt.Fprintln(stdout, Version)
	return true, 0
}

func requested(args []string) bool {
	if len(args) > 0 && args[0] == "version" {
		return true
	}
	for _, a := range args {
		if a == "--version" {
			return true
		}
	}
	return false
}
