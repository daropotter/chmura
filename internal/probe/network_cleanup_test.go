package probe_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/daropotter/chmura/internal/manifest"
	"github.com/daropotter/chmura/internal/probe"
)

func TestHTTPProbeWaitsForDialCleanup(t *testing.T) {
	for _, protocol := range []string{"http", "https"} {
		for _, mode := range []string{"parent cancellation", "attempt timeout"} {
			t.Run(protocol+"/"+mode, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
					done := make(chan error, 1)
					runner := probe.Network{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
						close(started)
						<-ctx.Done()
						close(canceled)
						<-release
						return nil, ctx.Err()
					}}
					check := httpCheck()
					check.Timeout = manifest.Duration(50 * time.Millisecond)
					endpoint := probe.Target{Host: "app.test", Ports: map[string]manifest.Port{"service": {Number: 8080, Protocol: protocol}}}
					go func() { done <- runner.Run(ctx, endpoint, check) }()
					<-started
					want := context.DeadlineExceeded
					if mode == "parent cancellation" {
						cancel()
						want = context.Canceled
					}
					<-canceled
					synctest.Wait()
					returned := false
					select {
					case err := <-done:
						t.Errorf("HTTP probe returned before dial cleanup: %v", err)
						returned = true
					default:
					}
					close(release)
					if !returned {
						if err := <-done; !errors.Is(err, want) {
							t.Fatalf("probe stop cause=%v, want %v", err, want)
						}
					}
				})
			})
		}
	}
}
