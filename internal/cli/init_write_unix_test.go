//go:build linux || darwin

package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
)

func TestInitCleansUpFailedManifestWrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shop")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "Dockerfile"), "FROM scratch\n")

	// Apply a real file-size limit only to a child process. The manifest write
	// must fail partway through without changing this test process's limits.
	cmd := exec.Command(os.Args[0], "-test.run=^TestInitWriteFailureProcess$")
	cmd.Env = append(os.Environ(), "CHMURA_TEST_WRITE_FAILURE_DIR="+dir)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != ExitError {
		t.Fatalf("failed write should exit %d: %v\n%s", ExitError, err, out)
	}
	if _, err := os.Lstat(filepath.Join(dir, "chmura.yaml")); !os.IsNotExist(err) {
		t.Errorf("failed write left a manifest behind: %v", err)
	}
	if code, out := execInit(t, dir); code != ExitOK {
		t.Fatalf("retry after write failure: exit = %d, want %d\n%s", code, ExitOK, out)
	}
	assertInitApplications(t, parseInitManifest(t, dir), map[string]string{"shop": "."})
}

func TestInitWriteFailureProcess(t *testing.T) {
	dir := os.Getenv("CHMURA_TEST_WRITE_FAILURE_DIR")
	if dir == "" {
		return
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 128, Max: 128}); err != nil {
		fmt.Fprintln(os.Stderr, "cannot set test file-size limit:", err)
		os.Exit(99)
	}
	signal.Ignore(syscall.SIGXFSZ)
	os.Exit(Execute([]string{"init", dir}, os.Stdout, os.Stderr))
}
