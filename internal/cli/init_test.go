package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func execInit(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Execute(append([]string{"init"}, args...), &out, &errb)
	return code, out.String() + errb.String()
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInitWritesManifest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shop")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "Dockerfile"), "FROM scratch\n")

	if code, out := execInit(t, dir); code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitOK, out)
	}

	b, err := os.ReadFile(filepath.Join(dir, "chmura.yaml"))
	if err != nil {
		t.Fatalf("chmura.yaml not written: %v", err)
	}
	content := string(b)
	for _, want := range []string{"version: 1", "name: shop", "dockerfile: Dockerfile"} {
		if !strings.Contains(content, want) {
			t.Errorf("manifest missing %q\n%s", want, content)
		}
	}
}

func TestInitNoSource(t *testing.T) {
	dir := t.TempDir()
	code, _ := execInit(t, dir)
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
	if _, err := os.Stat(filepath.Join(dir, "chmura.yaml")); !os.IsNotExist(err) {
		t.Error("chmura.yaml should not be written when no source is found")
	}
}

func TestInitDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "chmura.yaml")
	const original = "name: keep-me\n"
	writeFile(t, manifest, original)
	writeFile(t, filepath.Join(dir, "Dockerfile"), "FROM scratch\n")

	code, _ := execInit(t, dir)
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
	if b, _ := os.ReadFile(manifest); string(b) != original {
		t.Errorf("existing manifest was modified: %q", string(b))
	}
}

func TestInitRejectsExtraArgs(t *testing.T) {
	if code, _ := execInit(t, "a", "b"); code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
}
