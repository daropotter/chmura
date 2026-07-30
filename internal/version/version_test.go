package version

import (
	"bytes"
	"testing"
)

func TestRun(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantHandled bool
		wantCode    int
		wantPrinted bool
	}{
		{"long flag", []string{"--version"}, true, 0, true},
		{"subcommand", []string{"version"}, true, 0, true},
		{"flag among others", []string{"--version", "--quiet"}, true, 0, true},
		{"no args", nil, false, 0, false},
		{"other command", []string{"deploy", "--dry-run"}, false, 0, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			handled, code := Run(c.args, &out)

			if handled != c.wantHandled {
				t.Errorf("handled = %v, want %v", handled, c.wantHandled)
			}
			if code != c.wantCode {
				t.Errorf("code = %d, want %d", code, c.wantCode)
			}

			got := out.String()
			if c.wantPrinted {
				want := Version + "\n"
				if got != want {
					t.Errorf("output = %q, want %q", got, want)
				}
			} else if got != "" {
				t.Errorf("output = %q, want empty", got)
			}
		})
	}
}

func TestVersionHasDefault(t *testing.T) {
	if Version == "" {
		t.Fatal("Version must have a non-empty default (fallback when not set via -ldflags)")
	}
}
