package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestServeRejectsTLS12(t *testing.T) {
	certificate, roots := guaranteeCertificate(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, listener, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}), certificate)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("shutdown error = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("server did not stop")
		}
	})
	endpoint := "https://" + listener.Addr().String()
	for _, maximum := range []uint16{tls.VersionTLS13, tls.VersionTLS12} {
		transport := &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs: roots, MinVersion: maximum, MaxVersion: maximum,
		}}
		t.Cleanup(transport.CloseIdleConnections)
		client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
		response, err := client.Get(endpoint)
		if response != nil {
			response.Body.Close()
		}
		if maximum == tls.VersionTLS13 {
			if err != nil || response.StatusCode != http.StatusOK {
				t.Fatalf("TLS 1.3 probe failed: %v", err)
			}
		} else if err == nil {
			t.Error("server accepted a TLS 1.2-only client")
		}
	}
}

func TestServeDrainsInFlightRequest(t *testing.T) {
	certificate, roots := guaranteeCertificate(t)
	synctest.Test(t, func(t *testing.T) {
		listener := &guaranteeListener{connections: make(chan net.Conn), closed: make(chan struct{})}
		defer listener.Close()
		server, client := net.Pipe()
		defer server.Close()
		defer client.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		releaseHandler := func() { once.Do(func() { close(release) }) }
		defer releaseHandler()
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			close(entered)
			<-release
			_, _ = io.WriteString(w, "completed")
		})
		served := make(chan error, 1)
		go func() { served <- Serve(ctx, listener, handler, certificate) }()
		type result struct {
			body   string
			status int
			err    error
		}
		finished := make(chan result, 1)
		go func() {
			// Close the raw pipe after consuming the response; TLS close-notify
			// would otherwise need a concurrent reader on this unbuffered pipe.
			defer client.Close()
			select {
			case listener.connections <- server:
			case <-listener.closed:
				finished <- result{err: net.ErrClosed}
				return
			}
			conn := tls.Client(client, &tls.Config{
				RootCAs: roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS13,
			})
			request, err := http.NewRequest(http.MethodGet, "https://127.0.0.1/work", nil)
			if err != nil {
				finished <- result{err: err}
				return
			}
			if err := request.Write(conn); err != nil {
				finished <- result{err: err}
				return
			}
			response, err := http.ReadResponse(bufio.NewReader(conn), request)
			if err != nil {
				finished <- result{err: err}
				return
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			finished <- result{body: string(body), status: response.StatusCode, err: err}
		}()
		select {
		case <-entered:
		case got := <-finished:
			t.Fatalf("request failed before entering the handler: %v", got.err)
		case <-time.After(5 * time.Second):
			t.Fatal("request did not enter the handler")
		}
		cancel()
		// In-memory connections and channels let Wait establish that shutdown
		// has reached its blocked state before the active handler is released.
		synctest.Wait()
		select {
		case err := <-served:
			t.Fatalf("server stopped before the active request completed: %v", err)
		default:
		}
		select {
		case got := <-finished:
			t.Fatalf("active request ended before its handler was released: %v", got.err)
		default:
		}
		releaseHandler()
		select {
		case got := <-finished:
			if got.err != nil || got.status != http.StatusOK || got.body != "completed" {
				t.Errorf("in-flight request was not drained: %#v", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("in-flight request did not finish")
		}
		select {
		case err := <-served:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("shutdown error = %v, want context cancellation", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("server did not finish draining")
		}
	})
}

func guaranteeCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	fixture := httptest.NewTLSServer(http.NotFoundHandler())
	defer fixture.Close()
	roots := x509.NewCertPool()
	roots.AddCert(fixture.Certificate())
	return fixture.TLS.Certificates[0], roots
}

// guaranteeListener keeps the drain test's I/O inside the synctest bubble.
type guaranteeListener struct {
	connections chan net.Conn
	closed      chan struct{}
	once        sync.Once
}

func (l *guaranteeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.connections:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *guaranteeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *guaranteeListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 443}
}
