// Command chmura-server is the Chmura control plane, API, and state database.
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
