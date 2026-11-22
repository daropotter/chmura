package servercmd

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeBootstrapFixture(t *testing.T, path string) []byte {
	t.Helper()
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	token := hex.AppendEncode(nil, random[:])
	if err := os.WriteFile(path, append(bytes.Clone(token), '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	return token
}

func initArgs(database, tokenFile string) []string {
	return []string{"init", "--state", database, "--admin", "alice", "--admin-token-file", tokenFile, "--idempotency-key", "bootstrap"}
}

func TestInitCreatesFirstAdministratorAndReplays(t *testing.T) {
	dir := t.TempDir()
	database, tokenFile := filepath.Join(dir, "state.db"), filepath.Join(dir, "credential")
	token := writeBootstrapFixture(t, tokenFile)
	ctx := context.Background()
	for i := range 2 {
		var out, stderr bytes.Buffer
		if code := Execute(ctx, initArgs(database, tokenFile), &out, &stderr); code != 0 {
			t.Fatalf("bootstrap attempt %d exited %d", i, code)
		}
		if !strings.Contains(out.String(), "alice") || bytes.Contains(out.Bytes(), token) || bytes.Contains(stderr.Bytes(), token) {
			t.Fatal("bootstrap output omitted the identity or exposed the credential")
		}
	}

	for _, field := range []string{"admin", "token", "key"} {
		t.Run(field, func(t *testing.T) {
			args := initArgs(database, tokenFile)
			switch field {
			case "admin":
				args[4] = "bob"
			case "token":
				args[6] = filepath.Join(dir, "other-credential")
				writeBootstrapFixture(t, args[6])
			case "key":
				args[8] = "different-request"
			}
			var out, stderr bytes.Buffer
			if code := Execute(ctx, args, &out, &stderr); code != 3 {
				t.Fatalf("changed bootstrap field exited %d, want 3", code)
			}
		})
	}

	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	var name string
	var digest []byte
	if err := db.QueryRow("SELECT count(*), name, token_hash FROM bootstrap_admin").Scan(&count, &name, &digest); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(token)
	if count != 1 || name != "alice" || !bytes.Equal(digest, want[:]) {
		t.Fatal("bootstrap did not retain exactly one administrator and its expected verifier")
	}
	saved, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(saved, token) {
		t.Fatal("state database contains the raw credential")
	}
}

func TestInitRejectsUnsafeInputBeforeCreatingDatabase(t *testing.T) {
	for _, mode := range []string{"relative state", "missing admin", "missing key", "missing token", "directory token", "shared token", "invalid token", "oversized token", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			database, tokenFile := filepath.Join(dir, "state.db"), filepath.Join(dir, "credential")
			writeBootstrapFixture(t, tokenFile)
			args := initArgs(database, tokenFile)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := 2
			switch mode {
			case "relative state":
				args[2] = "state.db"
			case "missing admin":
				args[4] = ""
			case "missing key":
				args[8] = ""
			case "missing token":
				args[6] = filepath.Join(dir, "absent")
			case "directory token":
				args[6] = dir
			case "shared token":
				if err := os.Chmod(tokenFile, 0644); err != nil {
					t.Fatal(err)
				}
			case "invalid token":
				if err := os.WriteFile(tokenFile, []byte(strings.Repeat("g", 64)), 0600); err != nil {
					t.Fatal(err)
				}
			case "oversized token":
				if err := os.WriteFile(tokenFile, []byte(strings.Repeat("a", 100)), 0600); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				cancel()
				want = 130
			}
			var out, stderr bytes.Buffer
			if code := Execute(ctx, args, &out, &stderr); code != want || out.Len() != 0 {
				t.Fatalf("bootstrap exited %d, want %d and no success output", code, want)
			}
			if _, err := os.Stat(database); !os.IsNotExist(err) {
				t.Fatalf("rejected input created state: %v", err)
			}
		})
	}
}
