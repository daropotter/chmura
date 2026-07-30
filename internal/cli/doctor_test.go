package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestDoctorRunsAndSucceeds(t *testing.T) {
	var out, errb bytes.Buffer
	// The only check never fails (a missing runtime is a warning), so doctor
	// exits 0 regardless of what is installed on the machine running the test.
	if code := Execute([]string{"doctor"}, &out, &errb); code != ExitOK {
		t.Errorf("exit = %d, want %d\n%s", code, ExitOK, errb.String())
	}
	if !strings.Contains(out.String(), "container runtime") {
		t.Errorf("doctor output should list the container-runtime check\n%s", out.String())
	}
}

func TestDoctorRejectsArgs(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Execute([]string{"doctor", "extra"}, &out, &errb); code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
}
