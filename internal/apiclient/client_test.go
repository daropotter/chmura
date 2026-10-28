package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/daropotter/chmura/internal/api"
	"github.com/daropotter/chmura/internal/server"
	"github.com/daropotter/chmura/internal/version"
)

func TestInfoThroughHTTPS(t *testing.T) {
	for _, prefix := range []string{"", "/chmura"} {
		t.Run(prefix, func(t *testing.T) {
			handler, err := server.NewHandler("dev")
			if err != nil {
				t.Fatal(err)
			}
			fixture := httptest.NewTLSServer(http.StripPrefix(prefix, handler))
			defer fixture.Close()
			client, err := New(fixture.URL+prefix+"/", fixture.Client())
			if err != nil {
				t.Fatal(err)
			}
			info, err := client.Info(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if info.ServerVersion != version.Version || info.Profile != "dev" || len(info.APIVersions) != 1 || info.APIVersions[0] != api.Version {
				t.Errorf("unexpected handshake: %#v", info)
			}
		})
	}
}

func TestRejectsUnsafeEndpoints(t *testing.T) {
	for _, endpoint := range []string{
		"http://example.com", "https://", "not a URL", "https://user:private@example.com", "https://example.com?token=private", "https://example.com#private",
	} {
		if _, err := New(endpoint, nil); err == nil {
			t.Error("invalid or unsafe endpoint was accepted")
		} else if strings.Contains(err.Error(), "private") {
			t.Error("validation error exposed endpoint credentials")
		}
	}
}

func TestRejectsInvalidHandshake(t *testing.T) {
	for _, tc := range []struct {
		name, body, contentType, signal string
		status                          int
	}{
		{"HTTP failure", `{}`, "application/json", "503", http.StatusServiceUnavailable},
		{"not JSON", `<!doctype html>`, "text/html", "JSON", http.StatusOK},
		{"malformed JSON", `{`, "application/json", "JSON", http.StatusOK},
		{"unsupported API", `{"server-version":"0.1","api-versions":["v2"],"profile":"dev","features":[]}`, "application/json", "v1", http.StatusOK},
		{"missing identity", `{"api-versions":["v1"],"profile":"dev","features":[]}`, "application/json", "version", http.StatusOK},
		{"unknown profile", `{"server-version":"0.1","api-versions":["v1"],"profile":"unknown","features":[]}`, "application/json", "profile", http.StatusOK},
		{"missing features", `{"server-version":"0.1","api-versions":["v1"],"profile":"dev"}`, "application/json", "features", http.StatusOK},
		{"oversized response", strings.Repeat(" ", 2<<20), "application/json", "large", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer fixture.Close()
			client, err := New(fixture.URL, fixture.Client())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Info(context.Background()); err == nil || !strings.Contains(err.Error(), tc.signal) {
				t.Errorf("expected an error explaining %q, got %v", tc.signal, err)
			}
		})
	}
}

func TestDoesNotFollowRedirects(t *testing.T) {
	var requests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(api.Info{ServerVersion: "0.1", APIVersions: []string{"v1"}, Profile: "dev", Features: []string{}})
	}))
	defer target.Close()
	fixture := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer fixture.Close()
	client, err := New(fixture.URL, fixture.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Info(context.Background()); err == nil {
		t.Error("redirect should be rejected")
	}
	if requests.Load() != 0 {
		t.Error("client followed a redirect to a different endpoint")
	}
}

func TestInfoHonorsCancellation(t *testing.T) {
	fixture := httptest.NewTLSServer(http.NotFoundHandler())
	defer fixture.Close()
	client, err := New(fixture.URL, fixture.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Info(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled request error = %v", err)
	}
}

func TestInfoRequiresTrustedCertificate(t *testing.T) {
	handler, err := server.NewHandler("production")
	if err != nil {
		t.Fatal(err)
	}
	fixture := httptest.NewTLSServer(handler)
	defer fixture.Close()
	client, err := New(fixture.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Info(context.Background()); err == nil {
		t.Error("untrusted certificate should be rejected")
	}
}
