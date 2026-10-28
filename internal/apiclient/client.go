// Package apiclient communicates with a Chmura installation over HTTPS.
package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/daropotter/chmura/internal/api"
)

const maxInfoBytes = 1 << 20

// Client is an installation client. The supplied HTTP client may provide
// explicit CA trust; it is copied so redirect policy and timeouts stay local.
type Client struct {
	infoURL string
	http    *http.Client
}

// New accepts an HTTPS endpoint, optionally under a reverse-proxy path prefix.
// Credentials, queries and fragments are rejected without echoing their values.
func New(endpoint string, transport *http.Client) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("endpoint must be an HTTPS URL without credentials, query or fragment")
	}
	infoURL, err := url.JoinPath(u.String(), "v1", "info")
	if err != nil {
		return nil, errors.New("invalid HTTPS endpoint path")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	if transport != nil {
		*client = *transport
	}
	if client.Timeout <= 0 {
		client.Timeout = 10 * time.Second
	}
	// A handshake must identify the chosen installation, not a redirected host
	// or an HTTP downgrade. Preserve the response for a precise status error.
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{infoURL: infoURL, http: client}, nil
}

// Info discovers the installation and checks compatibility before further use.
func (c *Client) Info(ctx context.Context) (api.Info, error) {
	var info api.Info
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.infoURL, nil)
	if err != nil {
		return info, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return info, fmt.Errorf("installation handshake: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return info, fmt.Errorf("installation handshake: HTTP %d %s", response.StatusCode, http.StatusText(response.StatusCode))
	}
	contentType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		return info, errors.New("installation handshake must return JSON")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxInfoBytes+1))
	if err != nil {
		return info, fmt.Errorf("read installation handshake: %w", err)
	}
	if len(body) > maxInfoBytes {
		return info, errors.New("installation handshake response is too large")
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return info, fmt.Errorf("invalid handshake JSON: %w", err)
	}
	if strings.TrimSpace(info.ServerVersion) == "" {
		return info, errors.New("installation handshake is missing its server version")
	}
	if !slices.Contains(info.APIVersions, api.Version) {
		return info, fmt.Errorf("installation does not support API %s", api.Version)
	}
	if info.Profile != "production" && info.Profile != "dev" {
		return info, errors.New("installation handshake has an unknown profile")
	}
	if info.Features == nil {
		return info, errors.New("installation handshake is missing its features list")
	}
	return info, nil
}
