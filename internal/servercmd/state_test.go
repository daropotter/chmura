package servercmd

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/api"
	"github.com/daropotter/chmura/internal/state"
	"github.com/daropotter/chmura/internal/state/sqlite"
)

func stateServeArgs(cert, key, path string) []string {
	return []string{"serve", "--listen", "127.0.0.1:0", "--profile", "dev", "--tls-cert", cert, "--tls-key", key, "--state", path}
}

func TestCommandStatePersistsAcrossServerRestarts(t *testing.T) {
	cert, key, client := commandTLSFixture(t)
	path := filepath.Join(t.TempDir(), "state?#.db")
	ctx := context.Background()
	var accepted string
	for attempt := range 2 {
		serve, cancel := context.WithCancel(ctx)
		t.Cleanup(cancel)
		reader, writer := io.Pipe()
		t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
		var stderr bytes.Buffer
		done := make(chan int, 1)
		go func() {
			code := Execute(serve, stateServeArgs(cert, key, path), writer, &stderr)
			_ = writer.Close()
			done <- code
		}()
		lines := make(chan string, 1)
		go func() { line, _ := bufio.NewReader(reader).ReadString('\n'); lines <- line }()
		var line string
		select {
		case line = <-lines:
		case code := <-done:
			t.Fatalf("state server exited before readiness: %d %s", code, stderr.String())
		case <-time.After(5 * time.Second):
			t.Fatal("state server did not announce readiness")
		}
		var endpoint string
		for _, word := range strings.Fields(line) {
			if strings.HasPrefix(word, "https://") {
				endpoint = word
			}
		}
		if endpoint == "" {
			cancel()
			select {
			case code := <-done:
				t.Fatalf("state server did not start: %d %s", code, stderr.String())
			case <-time.After(5 * time.Second):
				t.Fatal("state server did not stop after missing readiness")
			}
		}
		// Observing readiness must mean the selected literal file is initialized.
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("server announced readiness before its state file existed: %v", err)
		}
		u := url.URL{Scheme: "file", Path: path, RawQuery: url.Values{"mode": {"ro"}}.Encode()}
		db, err := sql.Open("sqlite", u.String())
		if err != nil {
			t.Fatal(err)
		}
		var version int
		err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version)
		_ = db.Close()
		if err != nil || version <= 0 {
			t.Fatalf("state schema was not ready: version=%d error=%v", version, err)
		}
		response, err := client.Get(endpoint + "/v1/info")
		if err != nil {
			t.Fatal(err)
		}
		var info api.Info
		err = json.NewDecoder(response.Body).Decode(&info)
		_ = response.Body.Close()
		if err != nil || info.Profile != "dev" || len(info.Features) != 0 {
			t.Fatalf("state wiring changed discovery capabilities: %#v %v", info, err)
		}
		store, err := sqlite.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		op, created, err := store.Accept(ctx, state.Submission{Scope: "explicit-scope", IdempotencyKey: "state-restart-receipt", Kind: "fixture", Intent: []byte(`{"fixture":true}`)})
		_ = store.Close()
		if err != nil || (attempt == 0) != created {
			t.Fatalf("state receipt was lost or duplicated across serve: %#v created=%v error=%v", op, created, err)
		}
		if attempt == 0 {
			accepted = op.ID
		} else if op.ID != accepted {
			t.Fatal("server restart replaced retained operation identity")
		}
		cancel()
		select {
		case code := <-done:
			if code != 130 {
				t.Fatalf("state server stop exit=%d error=%s", code, stderr.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatal("state server did not stop")
		}
		_ = reader.Close()
	}
}

func TestCommandStateRejectsInvalidPathsAndUnsupportedSchema(t *testing.T) {
	cert, key, _ := commandTLSFixture(t)
	for _, mode := range []string{"relative", "empty", "blank", "missing parent", "directory", "future schema"} {
		t.Run(mode, func(t *testing.T) {
			path, want := filepath.Join(t.TempDir(), "state.db"), 1
			switch mode {
			case "relative":
				path, want = "relative-state.db", 2
			case "empty":
				path, want = "", 2
			case "blank":
				path, want = " ", 2
			case "missing parent":
				path = filepath.Join(t.TempDir(), "missing", "state.db")
			case "directory":
				path = t.TempDir()
			case "future schema":
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec("PRAGMA user_version=99"); err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var out, stderr bytes.Buffer
			if code := Execute(ctx, stateServeArgs(cert, key, path), &out, &stderr); code != want {
				t.Fatalf("state startup exit=%d want=%d error=%s", code, want, stderr.String())
			}
			if strings.Contains(out.String(), "Listening") {
				t.Fatal("invalid state startup announced readiness")
			}
			if mode == "missing parent" {
				if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
					t.Fatalf("server silently created parent directories: %v", err)
				}
			}
		})
	}
}

func TestCommandStatePreflightDoesNotCreateDatabase(t *testing.T) {
	cert, key, _ := commandTLSFixture(t)
	for _, mode := range []string{"TLS failure", "profile failure", "pre-canceled"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			args := stateServeArgs(cert, key, path)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := 1
			switch mode {
			case "TLS failure":
				args[6] = "/missing/certificate.pem"
			case "profile failure":
				args[4], want = "invalid", 2
			case "pre-canceled":
				cancel()
				want = 130
			}
			var out, stderr bytes.Buffer
			if code := Execute(ctx, args, &out, &stderr); code != want {
				t.Fatalf("preflight exit=%d want=%d error=%s", code, want, stderr.String())
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("failed preflight created state: %v", err)
			}
			if strings.Contains(out.String(), "Listening") {
				t.Fatal("failed preflight announced readiness")
			}
		})
	}
}
