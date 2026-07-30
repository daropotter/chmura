package cli

import (
	"bytes"
	"strings"
	"testing"
)

// These tests assert the outcome — that a mistyped flag or command points the
// user at the right one, with a usage exit code — not the exact wording or
// layout of the message. The typos are chosen so the echoed token does not
// itself contain the target, so a match proves a suggestion was offered.

func stderrOf(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Execute(args, &out, &errb)
	return errb.String(), code
}

func TestSuggestsCloseFlag(t *testing.T) {
	s, code := stderrOf(t, "--helo") // typo of --help
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(s, "--help") {
		t.Errorf("expected a suggestion of --help\n%s", s)
	}
}

func TestNoSuggestionForFarFlag(t *testing.T) {
	s, code := stderrOf(t, "--zzzzzzzz")
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
	if strings.Contains(s, "--help") || strings.Contains(s, "--version") {
		t.Errorf("did not expect a suggestion for a far-off flag\n%s", s)
	}
}

func TestSuggestsCloseCommand(t *testing.T) {
	s, code := stderrOf(t, "verzion") // typo of version
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(s, "version") {
		t.Errorf("expected a suggestion of version\n%s", s)
	}
}

func TestNoSuggestionForFarCommand(t *testing.T) {
	s, code := stderrOf(t, "zzzzzzzz")
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
	if strings.Contains(s, "version") {
		t.Errorf("did not expect a suggestion for a far-off command\n%s", s)
	}
}
