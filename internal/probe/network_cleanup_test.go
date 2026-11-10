package probe_test

import (
	"context"
	"errors"
	"net"
	"sync"
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

type delayedCloseConn struct {
	net.Conn
	started, release, finished chan struct{}
	once                       sync.Once
}

func (c *delayedCloseConn) Close() error {
	c.once.Do(func() {
		close(c.started)
		<-c.release
		_ = c.Conn.Close()
		close(c.finished)
	})
	return nil
}

func TestHTTPProbeClosesConnectionReturnedAfterCancellation(t *testing.T) {
	for _, protocol := range []string{"http", "https"} {
		t.Run(protocol, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				peer, conn := net.Pipe()
				defer peer.Close()
				defer conn.Close()
				closing, releaseClose, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				releaseCleanup := func() { releaseOnce.Do(func() { close(releaseClose) }) }
				defer releaseCleanup()
				tracked := &delayedCloseConn{Conn: conn, started: closing, release: releaseClose, finished: closed}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
				runner := probe.Network{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					close(started)
					<-ctx.Done()
					close(canceled)
					<-release
					return tracked, nil
				}}
				endpoint := probe.Target{Host: "app.test", Ports: map[string]manifest.Port{"service": {Number: 8080, Protocol: protocol}}}
				done := make(chan error, 1)
				go func() { done <- runner.Run(ctx, endpoint, httpCheck()) }()
				<-started
				cancel()
				<-canceled
				close(release)
				returned := false
				select {
				case <-closing:
				case err := <-done:
					t.Errorf("probe returned before late connection close: %v", err)
					returned = true
				}
				synctest.Wait()
				if !returned {
					select {
					case err := <-done:
						t.Errorf("probe returned before late connection cleanup finished: %v", err)
						returned = true
					default:
					}
				}
				releaseCleanup()
				if !returned {
					if err := <-done; !errors.Is(err, context.Canceled) {
						t.Fatalf("late dial lost cancellation: %v", err)
					}
					select {
					case <-closed:
					default:
						t.Error("probe returned before late connection was closed")
					}
				}
			})
		})
	}
}
