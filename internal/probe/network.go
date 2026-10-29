// Package probe executes individual application health-check attempts.
package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/daropotter/chmura/internal/manifest"
)

// Target is an instance host and its declared application ports, supplied by a runtime adapter.
type Target struct {
	Host  string
	Ports map[string]manifest.Port
}

// Network executes HTTP and TCP attempts. Its zero value uses direct networking
// and normal TLS verification. Optional CA roots and a server name provide explicit
// application trust. DialContext, if supplied, must honor context cancellation.
// Configure these fields before use and keep them immutable while attempts run.
type Network struct {
	DialContext func(context.Context, string, string) (net.Conn, error)
	RootCAs     *x509.CertPool
	ServerName  string
}

// StatusError reports an HTTP response outside the declared success statuses.
type StatusError struct {
	StatusCode int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("HTTP probe returned unexpected status %d", e.StatusCode)
}

// Run executes one validated HTTP or TCP check. It resolves omitted timeout and
// status settings through the shared manifest defaults. Scheduling, thresholds
// and restart decisions belong to the caller.
func (n Network) Run(ctx context.Context, target Target, check manifest.Probe) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if check.Process != nil || check.Exec != nil || (check.HTTP == nil) == (check.TCP == nil) {
		return errors.New("network probe must select exactly one of http or tcp")
	}
	if target.Host == "" || strings.ContainsAny(target.Host, " /?#@\t\r\n") {
		return errors.New("probe target must be an instance host without URL components")
	}
	effective := (&manifest.Health{Check: &check}).Effective().Check
	if effective.Timeout <= 0 {
		return errors.New("probe timeout must be positive")
	}
	name := ""
	protocols := []string{"tcp", "http", "https"}
	if check.HTTP != nil {
		name = check.HTTP.Port
		protocols = []string{"http", "https"}
	} else {
		name = check.TCP.Port
	}
	port, exists := target.Ports[name]
	if !exists || port.Number < 1 || port.Number > 65535 || !slices.Contains(protocols, port.Protocol) {
		return fmt.Errorf("probe port %q is missing or incompatible", name)
	}
	address := net.JoinHostPort(target.Host, strconv.Itoa(port.Number))
	ctx, cancel := context.WithTimeout(ctx, time.Duration(effective.Timeout))
	defer cancel()
	dial := n.DialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	if effective.TCP != nil {
		conn, err := dial(ctx, "tcp", address)
		if err != nil {
			return attemptError(ctx, "TCP probe", err)
		}
		_ = conn.Close()
		return contextError(ctx)
	}
	u, err := url.ParseRequestURI(effective.HTTP.Path)
	if err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(u.Path, "/") {
		return errors.New("HTTP probe requires an absolute request path")
	}
	u.Scheme, u.Host = port.Protocol, address
	transport := &http.Transport{
		// Bind dialing to this attempt; net/http may detach its dial context
		// from a request so a connection can be reused by another request.
		DialContext: func(_ context.Context, network, address string) (net.Conn, error) {
			return dial(ctx, network, address)
		},
		TLSClientConfig:     &tls.Config{RootCAs: n.RootCAs, ServerName: n.ServerName},
		TLSHandshakeTimeout: time.Duration(effective.Timeout),
		DisableKeepAlives:   true,
		DisableCompression:  true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("HTTP probe request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return attemptError(ctx, "HTTP probe", err)
	}
	defer response.Body.Close()
	if err := contextError(ctx); err != nil {
		return err
	}
	if !slices.Contains(effective.HTTP.ExpectStatus, response.StatusCode) {
		return &StatusError{StatusCode: response.StatusCode}
	}
	return nil
}

func attemptError(ctx context.Context, operation string, err error) error {
	if cause := contextError(ctx); cause != nil {
		err = cause
	}
	return fmt.Errorf("%s: %w", operation, err)
}
