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

func readManifest(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "chmura.yaml"))
	if err != nil {
		t.Fatalf("chmura.yaml not written: %v", err)
	}
	return string(b)
}

func TestInitDepthChildrenFindsDirectSubdirs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mono")
	for _, d := range []string{root, filepath.Join(root, "api"), filepath.Join(root, "web"), filepath.Join(root, "web/deep")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(root, "api/Dockerfile"), "FROM scratch\n")
	writeFile(t, filepath.Join(root, "web/Dockerfile"), "FROM scratch\n")
	writeFile(t, filepath.Join(root, "web/deep/Dockerfile"), "FROM scratch\n")

	if code, out := execInit(t, "--depth", "1", root); code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitOK, out)
	}

	content := readManifest(t, root)
	for _, want := range []string{
		"name: mono",
		"  api:",
		"    context: api",
		"  web:",
		"    context: web",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("manifest missing %q\n%s", want, content)
		}
	}
	if strings.Contains(content, "deep") {
		t.Errorf("depth 1 must not descend below direct subdirectories\n%s", content)
	}
}

func TestInitDepthAllFindsWholeTree(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mono")
	for _, d := range []string{root, filepath.Join(root, "services/api"), filepath.Join(root, "tools/scripts/deploy")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(root, "Dockerfile"), "FROM scratch\n")
	writeFile(t, filepath.Join(root, "services/api/Dockerfile"), "FROM scratch\n")
	writeFile(t, filepath.Join(root, "tools/scripts/deploy/Dockerfile"), "FROM scratch\n")

	if code, out := execInit(t, "--depth", "all", root); code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitOK, out)
	}

	content := readManifest(t, root)
	for _, want := range []string{
		"name: mono",
		"  mono:",
		"    context: .",
		"  api:",
		"    context: services/api",
		"  deploy:",
		"    context: tools/scripts/deploy",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("manifest missing %q\n%s", want, content)
		}
	}
}

func TestInitDepthAllSkipsHiddenAndNestedProjects(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mono")
	separate := filepath.Join(root, "separate")
	for _, d := range []string{root, filepath.Join(root, "web"), filepath.Join(root, ".github"), separate, filepath.Join(separate, "nested")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(root, "web/Dockerfile"), "FROM scratch\n")
	writeFile(t, filepath.Join(root, ".github/Dockerfile"), "FROM scratch\n")
	writeFile(t, filepath.Join(separate, "Dockerfile"), "FROM scratch\n")
	writeFile(t, filepath.Join(separate, "chmura.yaml"), "name: separate\n")
	writeFile(t, filepath.Join(separate, "nested/Dockerfile"), "FROM scratch\n")

	if code, out := execInit(t, "--depth", "all", root); code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitOK, out)
	}

	content := readManifest(t, root)
	if !strings.Contains(content, "  web:") {
		t.Errorf("a real application should be found\n%s", content)
	}
	if strings.Contains(content, ".github") {
		t.Errorf("hidden directories must be skipped\n%s", content)
	}
	if strings.Contains(content, "separate") {
		t.Errorf("a nested chmura project must be skipped, not descended into\n%s", content)
	}
}

func TestInitDepthAllNoSource(t *testing.T) {
	dir := t.TempDir()
	if code, out := execInit(t, "--depth", "all", dir); code != ExitUsage {
		t.Errorf("exit = %d, want %d\n%s", code, ExitUsage, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "chmura.yaml")); !os.IsNotExist(err) {
		t.Error("chmura.yaml should not be written when no source is found")
	}
}

func TestInitDepthDefaultsToDirOnly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mono")
	if err := os.MkdirAll(filepath.Join(root, "services/api"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "Dockerfile"), "FROM scratch\n")
	writeFile(t, filepath.Join(root, "services/api/Dockerfile"), "FROM scratch\n")

	if code, out := execInit(t, root); code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitOK, out)
	}

	content := readManifest(t, root)
	if !strings.Contains(content, "  mono:") {
		t.Errorf("default depth should find the directory's own Dockerfile\n%s", content)
	}
	if strings.Contains(content, "services/api") {
		t.Errorf("default depth must not scan subdirectories\n%s", content)
	}
}

func TestInitDepthInvalidValue(t *testing.T) {
	for _, depth := range []string{"2", "bogus", "-1"} {
		if code, out := execInit(t, "--depth", depth, t.TempDir()); code != ExitUsage {
			t.Errorf("--depth %s: exit = %d, want %d\n%s", depth, code, ExitUsage, out)
		}
	}
}

func TestInitNameCollisionIsPreciseError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mono")
	for _, d := range []string{filepath.Join(root, "user-api"), filepath.Join(root, "user_api")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(root, "user-api/Dockerfile"), "FROM scratch\n")
	writeFile(t, filepath.Join(root, "user_api/Dockerfile"), "FROM scratch\n")

	code, out := execInit(t, "--depth", "1", root)
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitUsage, out)
	}
	for _, want := range []string{"user-api", "user_api", "normalize"} {
		if !strings.Contains(out, want) {
			t.Errorf("collision error should name both sources and the colliding name; missing %q\n%s", want, out)
		}
	}
}

func TestInitNormalizesNames(t *testing.T) {
	root := filepath.Join(t.TempDir(), "My Service")
	if err := os.MkdirAll(filepath.Join(root, "user_API"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "user_API/Dockerfile"), "FROM scratch\n")

	if code, out := execInit(t, "--depth", "1", root); code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitOK, out)
	}

	content := readManifest(t, root)
	for _, want := range []string{"name: my-service", "  user-api:", "    context: user_API"} {
		if !strings.Contains(content, want) {
			t.Errorf("manifest missing %q\n%s", want, content)
		}
	}
}

func TestInitNonexistentDir(t *testing.T) {
	code, out := execInit(t, filepath.Join(t.TempDir(), "missing"))
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d\n%s", code, ExitUsage, out)
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
