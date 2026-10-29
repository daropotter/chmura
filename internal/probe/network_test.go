package probe_test

import (
	"bufio"
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/daropotter/chmura/internal/manifest"
	"github.com/daropotter/chmura/internal/probe"
)

func target(t *testing.T, endpoint string) probe.Target {
	t.Helper()
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return probe.Target{Host: u.Hostname(), Ports: map[string]manifest.Port{
		"service": {Number: port, Protocol: u.Scheme},
	}}
}

func httpCheck() manifest.Probe {
	return manifest.Probe{HTTP: &manifest.HTTPCheck{Path: "/ready?full=true", Port: "service"}}
}

func TestHTTPProbeStatuses(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		expected []int
		ok       bool
	}{
		{"default success", 200, nil, true},
		{"default rejects 204", 204, nil, false},
		{"explicit success", 204, []int{200, 204}, true},
		{"unavailable", 503, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/ready" || r.URL.Query().Get("full") != "true" {
					t.Errorf("request = %s %s", r.Method, r.URL.RequestURI())
				}
				w.WriteHeader(tc.status)
			}))
			defer server.Close()
			check := httpCheck()
			check.HTTP.ExpectStatus = tc.expected
			err := (probe.Network{}).Run(context.Background(), target(t, server.URL), check)
			if tc.ok {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var status *probe.StatusError
				if !errors.As(err, &status) || status.StatusCode != tc.status {
					t.Fatalf("probe error = %v, want HTTP status %d", err, tc.status)
				}
			}
		})
	}
}

func TestHTTPProbeNeverFollowsRedirects(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/login", http.StatusFound)
	}))
	defer server.Close()
	for _, expected := range [][]int{nil, {http.StatusFound}} {
		check := httpCheck()
		check.HTTP.ExpectStatus = expected
		err := (probe.Network{}).Run(context.Background(), target(t, server.URL), check)
		if (err == nil) != (expected != nil) {
			t.Errorf("redirect status handling: %v", err)
		}
	}
	if redirected.Load() != 0 {
		t.Fatal("probe followed a redirect away from the application port")
	}
}

func TestHTTPSProbeVerifiesCertificates(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	endpoint := target(t, server.URL)
	if err := (probe.Network{}).Run(context.Background(), endpoint, httpCheck()); err == nil {
		t.Fatal("probe trusted an unknown certificate")
	}
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	if err := (probe.Network{RootCAs: roots}).Run(context.Background(), endpoint, httpCheck()); err != nil {
		t.Fatalf("explicit CA trust failed: %v", err)
	}
	if err := (probe.Network{RootCAs: roots, ServerName: "other.invalid"}).Run(context.Background(), endpoint, httpCheck()); err == nil {
		t.Fatal("probe ignored a certificate name mismatch")
	}
}

func TestHTTPProbeClosesUnusedResponseBody(t *testing.T) {
	canceled, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	if err := (probe.Network{}).Run(context.Background(), target(t, server.URL), httpCheck()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("probe left the response connection open")
	}
}

func TestHTTPProbeCancellationStopsRequest(t *testing.T) {
	entered, stopped, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
			close(stopped)
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	endpoint := target(t, server.URL)
	go func() { done <- (probe.Network{}).Run(ctx, endpoint, httpCheck()) }()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("request stopped before entering handler: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("probe did not reach the handler")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("cancellation error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("probe ignored cancellation")
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled probe left its HTTP request active")
	}
}

func TestHTTPProbeStopsStalledExchange(t *testing.T) {
	for _, protocol := range []string{"http", "https"} {
		for _, mode := range []string{"timeout", "cancellation"} {
			t.Run(protocol+"/"+mode, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					server, client := net.Pipe()
					defer server.Close()
					defer client.Close()
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					peerStopped := make(chan struct{})
					go func() {
						defer close(peerStopped)
						var err error
						if protocol == "http" {
							_, err = http.ReadRequest(bufio.NewReader(server))
						} else {
							_, err = server.Read(make([]byte, 4096))
						}
						if err != nil {
							return
						}
						if mode == "cancellation" {
							cancel()
						}
						_, _ = io.Copy(io.Discard, server)
					}()
					runner := probe.Network{DialContext: func(context.Context, string, string) (net.Conn, error) { return client, nil }}
					check := httpCheck()
					check.Timeout = manifest.Duration(50 * time.Millisecond)
					endpoint := probe.Target{Host: "app.test", Ports: map[string]manifest.Port{"service": {Number: 443, Protocol: protocol}}}
					started := time.Now()
					err := runner.Run(ctx, endpoint, check)
					if mode == "timeout" {
						if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != 50*time.Millisecond {
							t.Errorf("stalled %s timeout: error=%v duration=%v", protocol, err, time.Since(started))
						}
					} else if !errors.Is(err, context.Canceled) {
						t.Errorf("stalled %s cancellation: %v", protocol, err)
					}
					synctest.Wait()
					select {
					case <-peerStopped:
					default:
						t.Error("probe left its stalled connection open")
					}
				})
			})
		}
	}
}

func TestTCPProbeConnectsAndCloses(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	closed := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			closed <- err
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var buffer [1]byte
		_, err = conn.Read(buffer[:])
		closed <- err
	}()
	check := manifest.Probe{TCP: &manifest.TCPCheck{Port: "service"}}
	if err := (probe.Network{}).Run(context.Background(), target(t, "tcp://"+listener.Addr().String()), check); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if !errors.Is(err, io.EOF) {
			t.Errorf("connection was not closed after success: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TCP probe did not close its connection")
	}
}

func TestNetworkProbeTimeouts(t *testing.T) {
	for _, protocol := range []string{"http", "tcp"} {
		for _, tc := range []struct {
			name                  string
			timeout, parent, want time.Duration
		}{
			{"default", 0, 10 * time.Second, 2 * time.Second},
			{"override", 50 * time.Millisecond, time.Second, 50 * time.Millisecond},
			{"parent deadline", 2 * time.Second, time.Second, time.Second},
		} {
			t.Run(protocol+"/"+tc.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), tc.parent)
					defer cancel()
					dialDone := make(chan struct{})
					runner := probe.Network{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
						defer close(dialDone)
						if network != "tcp" || address != "app.test:8080" {
							t.Errorf("dial = %s %s", network, address)
						}
						<-ctx.Done()
						return nil, ctx.Err()
					}}
					check := httpCheck()
					if protocol == "tcp" {
						check = manifest.Probe{TCP: &manifest.TCPCheck{Port: "service"}}
					}
					check.Timeout = manifest.Duration(tc.timeout)
					endpoint := probe.Target{Host: "app.test", Ports: map[string]manifest.Port{"service": {Number: 8080, Protocol: protocol}}}
					started := time.Now()
					err := runner.Run(ctx, endpoint, check)
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("timeout error = %v", err)
					}
					if got := time.Since(started); got != tc.want {
						t.Errorf("attempt duration = %v, want %v", got, tc.want)
					}
					synctest.Wait()
					select {
					case <-dialDone:
					default:
						t.Error("timed-out dial remained active")
					}
				})
			})
		}
	}
}

func TestNetworkProbeRejectsUnsupportedOrInvalidTargets(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint probe.Target
		check    manifest.Probe
	}{
		{"missing host", probe.Target{Ports: map[string]manifest.Port{"service": {Number: 80, Protocol: "http"}}}, httpCheck()},
		{"missing port", probe.Target{Host: "app.test"}, httpCheck()},
		{"UDP port", probe.Target{Host: "app.test", Ports: map[string]manifest.Port{"service": {Number: 80, Protocol: "udp"}}}, httpCheck()},
		{"process", probe.Target{Host: "app.test"}, manifest.Probe{Process: &manifest.ProcessCheck{Name: "app"}}},
		{"empty check", probe.Target{Host: "app.test"}, manifest.Probe{}},
		{"multiple checks", probe.Target{Host: "app.test"}, manifest.Probe{HTTP: httpCheck().HTTP, TCP: &manifest.TCPCheck{Port: "service"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			runner := probe.Network{DialContext: func(context.Context, string, string) (net.Conn, error) {
				calls.Add(1)
				return nil, errors.New("unexpected dial")
			}}
			if err := runner.Run(context.Background(), tc.endpoint, tc.check); err == nil {
				t.Fatal("invalid or unsupported probe accepted")
			}
			if calls.Load() != 0 {
				t.Fatal("invalid probe attempted network I/O")
			}
		})
	}
}
