package cli

import (
	"bytes"
	"errors"
	"testing"
)

func code(t *testing.T, args ...string) int {
	t.Helper()
	var out, errb bytes.Buffer
	return Execute(args, &out, &errb)
}

func TestExecuteExitCodes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"version subcommand", []string{"version"}, ExitOK},
		{"version flag", []string{"--version"}, ExitOK},
		{"help", []string{"--help"}, ExitOK},
		{"no args", nil, ExitOK},
		{"unknown flag", []string{"--nope"}, ExitUsage},
		{"unknown shorthand", []string{"-v"}, ExitUsage},
		{"unknown command", []string{"bogus"}, ExitUsage},
		{"extra args to version", []string{"version", "extra"}, ExitUsage},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := code(t, c.args...); got != c.want {
				t.Errorf("Execute(%v) = %d, want %d", c.args, got, c.want)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, ExitOK},
		{"execution", errors.New("boom"), ExitError},
		{"usage-wrapped", &usageError{errors.New("bad flag")}, ExitUsage},
		{"unknown command", errors.New(`unknown command "x" for "chmura"`), ExitUsage},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classify(c.err); got != c.want {
				t.Errorf("classify(%v) = %d, want %d", c.err, got, c.want)
			}
		})
	}
}

func TestExecutePrintsUsageErrorToStderr(t *testing.T) {
	var out, errb bytes.Buffer
	Execute([]string{"bogus"}, &out, &errb)
	if errb.Len() == 0 {
		t.Error("expected an error message on stderr for a usage error")
	}
	if out.Len() != 0 {
		t.Errorf("expected empty stdout for a usage error, got %q", out.String())
	}
}
