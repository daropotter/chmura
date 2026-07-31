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

// The next two pin a subtle coupling: the project name, the chmura.yaml write
// location, and the Dockerfile lookup must all follow the SAME directory — the
// arg, or cwd by default — never diverge (e.g. name from cwd, file from the arg).

func TestInitNameAndManifestFollowCwdByDefault(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shop")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "Dockerfile"), "FROM scratch\n")
	t.Chdir(dir)

	if code, out := execInit(t); code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitOK, out)
	}
	b, err := os.ReadFile("chmura.yaml") // relative to cwd
	if err != nil {
		t.Fatalf("chmura.yaml not written into cwd: %v", err)
	}
	if !strings.Contains(string(b), "name: shop") {
		t.Errorf("name should come from cwd (shop)\n%s", string(b))
	}
}

func TestInitNameFollowsDirArgNotCwd(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "outer")
	target := filepath.Join(t.TempDir(), "api")
	for _, d := range []string{cwd, target} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(target, "Dockerfile"), "FROM scratch\n")
	t.Chdir(cwd)

	if code, out := execInit(t, target); code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitOK, out)
	}

	b, err := os.ReadFile(filepath.Join(target, "chmura.yaml"))
	if err != nil {
		t.Fatalf("chmura.yaml not written into the target dir: %v", err)
	}
	content := string(b)
	if !strings.Contains(content, "name: api") {
		t.Errorf("name should follow the dir arg (api), not cwd (outer)\n%s", content)
	}
	if strings.Contains(content, "name: outer") {
		t.Errorf("name must not come from cwd\n%s", content)
	}
	if _, err := os.Stat(filepath.Join(cwd, "chmura.yaml")); !os.IsNotExist(err) {
		t.Error("chmura.yaml must not be written into cwd when a dir arg is given")
	}
}
