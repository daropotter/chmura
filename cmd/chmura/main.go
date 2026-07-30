// Command chmura is the Chmura CLI — the primary interface to an installation.
package main

import (
	"os"

	"github.com/daropotter/chmura/internal/cli"
)

func main() {
	if err := cli.NewRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}
