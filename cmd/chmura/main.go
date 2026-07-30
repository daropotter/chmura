// Command chmura is the Chmura CLI — the primary interface to an installation.
package main

import (
	"os"

	"github.com/daropotter/chmura/internal/cli"
)

func main() {
	os.Exit(cli.Execute(os.Args[1:], os.Stdout, os.Stderr))
}
