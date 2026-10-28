package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/daropotter/chmura/internal/version"
)

func TestHandshakeReportsInstallation(t *testing.T) {
	for _, profile := range []string{"production", "dev"} {
		t.Run(profile, func(t *testing.T) {
			handler, err := NewHandler(profile)
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/info", nil))
			if response.Code != http.StatusOK {
				t.Fatalf("handshake status = %d, want 200", response.Code)
			}
			if got := response.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("handshake content type = %q, want application/json", got)
			}
			var info struct {
				ServerVersion string   `json:"server-version"`
				APIVersions   []string `json:"api-versions"`
				Profile       string   `json:"profile"`
				Features      []string `json:"features"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &info); err != nil {
				t.Fatal(err)
			}
			if info.ServerVersion != version.Version || info.Profile != profile {
				t.Errorf("installation identity = %#v", info)
			}
			if len(info.APIVersions) != 1 || info.APIVersions[0] != "v1" {
				t.Errorf("API versions = %v, want v1", info.APIVersions)
			}
			if info.Features == nil || len(info.Features) != 0 {
				t.Errorf("features = %#v; no deployment features are implemented yet", info.Features)
			}
		})
	}
}

func TestHandshakeRejectsInvalidProfile(t *testing.T) {
	for _, profile := range []string{"", "prod", "Production"} {
		if _, err := NewHandler(profile); err == nil {
			t.Errorf("profile %q should be rejected", profile)
		}
	}
}

func TestHandshakeRouting(t *testing.T) {
	handler, err := NewHandler("production")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodPost, "/v1/info", http.StatusMethodNotAllowed},
		{http.MethodGet, "/v1/unknown", http.StatusNotFound},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, nil))
		if response.Code != tc.status {
			t.Errorf("%s %s: status = %d, want %d", tc.method, tc.path, response.Code, tc.status)
		}
	}
}
