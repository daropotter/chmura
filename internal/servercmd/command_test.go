package servercmd

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/api"
	"github.com/daropotter/chmura/internal/version"
)

func TestCommandExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"help", []string{"--help"}, 0},
		{"unknown command", []string{"bogus"}, 2},
		{"unknown flag", []string{"serve", "--bogus"}, 2},
		{"extra argument", []string{"serve", "extra"}, 2},
		{"missing TLS", []string{"serve"}, 2},
		{"missing key", []string{"serve", "--tls-cert", "cert.pem"}, 2},
		{"invalid profile", []string{"serve", "--profile", "invalid", "--tls-cert", "cert.pem", "--tls-key", "key.pem"}, 2},
		{"unreadable TLS files", []string{"serve", "--tls-cert", "/missing/cert.pem", "--tls-key", "/missing/key.pem"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if got := Execute(context.Background(), tc.args, &out, &errb); got != tc.code {
				t.Errorf("exit = %d, want %d\n%s", got, tc.code, errb.String())
			}
		})
	}
}

func TestCommandVersion(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}} {
		var out, errb bytes.Buffer
		if code := Execute(context.Background(), args, &out, &errb); code != 0 || strings.TrimSpace(out.String()) != version.Version {
			t.Errorf("version output=%q exit=%d error=%s", out.String(), code, errb.String())
		}
	}
}

func TestCommandServesHTTPS(t *testing.T) {
	fixture := httptest.NewTLSServer(http.NotFoundHandler())
	certificate := fixture.TLS.Certificates[0]
	client := fixture.Client()
	client.Timeout = 5 * time.Second
	fixture.Close()
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	key, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reader, writer := io.Pipe()
	t.Cleanup(func() { reader.Close(); writer.Close() })
	var errb bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- Execute(ctx, []string{"serve", "--listen", "127.0.0.1:0", "--profile", "dev", "--tls-cert", certPath, "--tls-key", keyPath}, writer, &errb)
		writer.Close()
	}()
	lines := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(reader).ReadString('\n'); lines <- line }()
	var endpoint string
	select {
	case line := <-lines:
		for _, word := range strings.Fields(line) {
			if strings.HasPrefix(word, "https://") {
				endpoint = word
			}
		}
	case code := <-done:
		t.Fatalf("server exited before listening: %d, %s", code, errb.String())
	case <-time.After(5 * time.Second):
		t.Fatal("server did not report its listening address")
	}
	if endpoint == "" {
		t.Fatal("server did not report an HTTPS endpoint")
	}
	response, err := client.Get(endpoint + "/v1/info")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var info api.Info
	if err := json.NewDecoder(response.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.Profile != "dev" || info.ServerVersion != version.Version {
		t.Errorf("unexpected installation identity: %#v", info)
	}
	cancel()
	select {
	case code := <-done:
		if code != 130 {
			t.Errorf("cancel exit = %d, want 130; %s", code, errb.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server command did not stop")
	}
}
