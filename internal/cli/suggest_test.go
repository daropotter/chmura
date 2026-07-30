package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"help", "help", 0},
		{"helpp", "help", 1},
		{"replica", "replicas", 1},
		{"kitten", "sitting", 3},
	}
	for _, c := range cases {
		if got := levenshtein(c.a, c.b); got != c.want {
			t.Errorf("levenshtein(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestSuggestFlag(t *testing.T) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.Int("replicas", 1, "")
	fs.Bool("verbose", false, "")

	if got := suggestFlag(fs, "replica"); got != "replicas" {
		t.Errorf("suggestFlag(replica) = %q, want replicas", got)
	}
	if got := suggestFlag(fs, "zzzzzz"); got != "" {
		t.Errorf("suggestFlag(zzzzzz) = %q, want empty", got)
	}
	if got := suggestFlag(fs, "replicas"); got != "" {
		t.Errorf("exact match should not suggest, got %q", got)
	}
}

func TestUnknownLongFlag(t *testing.T) {
	if tok, ok := unknownLongFlag(errors.New("unknown flag: --replica")); !ok || tok != "--replica" {
		t.Errorf("unknownLongFlag = (%q, %v), want (--replica, true)", tok, ok)
	}
	if _, ok := unknownLongFlag(errors.New("unknown shorthand flag: 'x' in -x")); ok {
		t.Error("shorthand error should not match unknownLongFlag")
	}
}

func TestExecuteSuggestsCloseFlag(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Execute([]string{"--helpp"}, &out, &errb); code != ExitUsage {
		t.Errorf("code = %d, want %d", code, ExitUsage)
	}
	s := errb.String()
	for _, want := range []string{`unknown option "--helpp"`, "Did you mean?", "--help"} {
		if !strings.Contains(s, want) {
			t.Errorf("stderr missing %q\n%s", want, s)
		}
	}
}

func TestExecuteNoSuggestionForFarFlag(t *testing.T) {
	var out, errb bytes.Buffer
	Execute([]string{"--zzzzzzzz"}, &out, &errb)
	s := errb.String()
	if !strings.Contains(s, `unknown option "--zzzzzzzz"`) {
		t.Errorf("stderr missing the unknown-option line\n%s", s)
	}
	if strings.Contains(s, "Did you mean?") {
		t.Errorf("should not suggest for a far-off flag\n%s", s)
	}
}

func TestExecuteSuggestsCloseCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Execute([]string{"versionn"}, &out, &errb); code != ExitUsage {
		t.Errorf("code = %d, want %d", code, ExitUsage)
	}
	s := errb.String()
	for _, want := range []string{`unknown command "versionn"`, "Did you mean?", "version"} {
		if !strings.Contains(s, want) {
			t.Errorf("stderr missing %q\n%s", want, s)
		}
	}
}

func TestExecuteNoSuggestionForFarCommand(t *testing.T) {
	var out, errb bytes.Buffer
	Execute([]string{"zzzzzzzz"}, &out, &errb)
	s := errb.String()
	if !strings.Contains(s, `unknown command "zzzzzzzz"`) {
		t.Errorf("stderr missing the unknown-command line\n%s", s)
	}
	if strings.Contains(s, "Did you mean?") {
		t.Errorf("should not suggest for a far-off command\n%s", s)
	}
}
