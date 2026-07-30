// Command chmura-dev runs Chmura in a local development profile.
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
