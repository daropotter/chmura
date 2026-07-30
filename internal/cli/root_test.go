package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/daropotter/chmura/internal/version"
)

// run executes a fresh root command with args, capturing stdout and stderr
// into one buffer.
func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestHelpShowsModelAndCommands(t *testing.T) {
	out, err := run(t, "--help")
	if err != nil {
		t.Fatalf("--help returned error: %v", err)
	}
	for _, want := range []string{"private cloud", "version"} {
		if !strings.Contains(out, want) {
			t.Errorf("--help output missing %q\n%s", want, out)
		}
	}
}

func TestNoArgsShowsHelp(t *testing.T) {
	out, err := run(t)
	if err != nil {
		t.Fatalf("no-args returned error: %v", err)
	}
	if !strings.Contains(out, "Usage") {
		t.Errorf("no-args output should show usage help\n%s", out)
	}
}

func TestVersionFlagIsBare(t *testing.T) {
	out, err := run(t, "--version")
	if err != nil {
		t.Fatalf("--version returned error: %v", err)
	}
	if want := version.Version + "\n"; out != want {
		t.Errorf("--version = %q, want %q", out, want)
	}
}

func TestVersionHasNoShortFlag(t *testing.T) {
	// -v is reserved for --verbose (CLI contract); it must not mean --version.
	if _, err := run(t, "-v"); err == nil {
		t.Fatal("-v should not be recognized yet (reserved for --verbose)")
	}
}

func TestVersionSubcommandIsBare(t *testing.T) {
	out, err := run(t, "version")
	if err != nil {
		t.Fatalf("version returned error: %v", err)
	}
	if got := strings.TrimSpace(out); got != version.Version {
		t.Errorf("version = %q, want %q", got, version.Version)
	}
}
