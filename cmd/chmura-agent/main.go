// Command chmura-agent is the Chmura cluster agent — it dials out to the
// control plane over mTLS and reconciles the last assigned state.
package main

import (
	"os"

	"github.com/daropotter/chmura/internal/version"
)

func main() {
	if handled, code := version.Run(os.Args[1:], os.Stdout); handled {
		os.Exit(code)
	}
	// No other commands yet.
}
