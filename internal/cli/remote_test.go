package cli

import (
	"bytes"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daropotter/chmura/internal/server"
	"github.com/daropotter/chmura/internal/version"
)

func TestRemoteInspectReportsInstallation(t *testing.T) {
	for _, profile := range []string{"production", "dev"} {
		t.Run(profile, func(t *testing.T) {
			handler, err := server.NewHandler(profile)
			if err != nil {
				t.Fatal(err)
			}
			fixture := httptest.NewTLSServer(handler)
			defer fixture.Close()
			ca := filepath.Join(t.TempDir(), "ca.pem")
			if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: fixture.Certificate().Raw}), 0o644); err != nil {
				t.Fatal(err)
			}
			config := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", config)
			var out, errb bytes.Buffer
			code := Execute([]string{"remote", "inspect", "--endpoint", fixture.URL, "--ca-cert", ca}, &out, &errb)
			if code != ExitOK {
				t.Fatalf("inspect exit = %d\n%s", code, errb.String())
			}
			for _, signal := range []string{version.Version, profile, "v1"} {
				if !strings.Contains(out.String(), signal) {
					t.Errorf("inspection is missing %q: %s", signal, out.String())
				}
			}
			entries, err := os.ReadDir(config)
			if err != nil || len(entries) != 0 {
				t.Errorf("direct inspection should not write local settings: %v, %v", entries, err)
			}
		})
	}
}

func TestRemoteInspectConfigurationErrors(t *testing.T) {
	badCA := filepath.Join(t.TempDir(), "invalid.pem")
	writeFile(t, badCA, "not a certificate")
	for _, args := range [][]string{
		{"remote", "inspect"},
		{"remote", "inspect", "extra", "--endpoint", "https://example.com"},
		{"remote", "inspect", "--endpoint", "http://example.com"},
		{"remote", "inspect", "--endpoint", "invalid"},
		{"remote", "inspect", "--endpoint", "https://example.com", "--ca-cert", badCA},
		{"remote", "inspect", "--endpoint", "https://example.com", "--ca-cert", filepath.Join(t.TempDir(), "missing.pem")},
	} {
		var out, errb bytes.Buffer
		if code := Execute(args, &out, &errb); code != ExitUsage {
			t.Errorf("%v: exit = %d, want %d\n%s", args, code, ExitUsage, errb.String())
		}
		if out.Len() != 0 {
			t.Errorf("configuration error printed successful output: %s", out.String())
		}
	}
}

func TestRemoteInspectRejectsUntrustedServer(t *testing.T) {
	handler, err := server.NewHandler("dev")
	if err != nil {
		t.Fatal(err)
	}
	fixture := httptest.NewTLSServer(handler)
	defer fixture.Close()
	var out, errb bytes.Buffer
	if code := Execute([]string{"remote", "inspect", "--endpoint", fixture.URL}, &out, &errb); code != ExitError {
		t.Errorf("untrusted certificate exit = %d, want %d\n%s", code, ExitError, errb.String())
	}
	if out.Len() != 0 {
		t.Errorf("untrusted server printed successful output: %s", out.String())
	}
}

func TestRemoteInspectReportsServerFailure(t *testing.T) {
	fixture := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer fixture.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: fixture.Certificate().Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := Execute([]string{"remote", "inspect", "--endpoint", fixture.URL, "--ca-cert", ca}, &out, &errb); code != ExitError {
		t.Errorf("failed server exit = %d, want %d", code, ExitError)
	}
	if !strings.Contains(errb.String(), "503") || out.Len() != 0 {
		t.Errorf("server error: stdout=%s stderr=%s", out.String(), errb.String())
	}
}
