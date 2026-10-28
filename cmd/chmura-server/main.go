// Command chmura-server is the Chmura control plane, API, and state database.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/daropotter/chmura/internal/servercmd"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := servercmd.Execute(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
