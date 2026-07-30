// Command chmura is the Chmura CLI — the primary interface to an installation.
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
