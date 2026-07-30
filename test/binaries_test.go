package test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/daropotter/chmura/internal/version"
)

// All four artifacts must report their version on stdout and exit 0.
var binaries = []string{"chmura", "chmura-server", "chmura-agent", "chmura-dev"}

func TestBinariesReportVersion(t *testing.T) {
	for _, name := range binaries {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			out, err := exec.Command("go", "run", "../cmd/"+name, "--version").CombinedOutput()
			if err != nil {
				t.Fatalf("go run ../cmd/%s --version failed: %v\n%s", name, err, out)
			}

			got := strings.TrimSpace(string(out))
			if got != version.Version {
				t.Fatalf("%s --version = %q, want %q", name, got, version.Version)
			}
		})
	}
}
