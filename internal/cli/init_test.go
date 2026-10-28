package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
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

	manifest := parseInitManifest(t, dir)
	if manifest.Version != 1 || manifest.Name != "shop" {
		t.Errorf("manifest version/name = %d/%#v, want 1/shop", manifest.Version, manifest.Name)
	}
	assertInitApplications(t, manifest, map[string]string{"shop": "."})
}

func TestInitPreservesYAMLStrings(t *testing.T) {
	cases := []struct {
		name, project, directory, application string
	}{
		{"plain strings", "shop", "api", "api"},
		{"null", "null", "null", "null"},
		{"booleans", "false", "true", "true"},
		{"integers", "123", "456", "456"},
		{"numeric notation", "1e3", "0x10", "0x10"},
		{"colon", "shop", "api: v1", "api-v1"},
		{"comment", "shop", "api # v1", "api-v1"},
		{"sequence", "shop", "[api]", "api"},
		{"mapping", "shop", "{api}", "api"},
		{"alias", "shop", "*api", "api"},
		{"anchor", "shop", "&api", "api"},
		{"newline", "shop", "api\nv1", "api-v1"},
		{"tab", "shop", "api\tv1", "api-v1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), tc.project)
			if err := os.MkdirAll(filepath.Join(root, tc.directory), 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(root, tc.directory, "Dockerfile"), "FROM scratch\n")
			if code, out := execInit(t, "--depth", "1", root); code != ExitOK {
				t.Fatalf("exit = %d, want %d\n%s", code, ExitOK, out)
			}

			// Decode without string-typed fields so implicit YAML scalar types
			// cannot be silently converted back into the strings we expected.
			var manifest map[string]any
			if err := yaml.Unmarshal([]byte(readManifest(t, root)), &manifest); err != nil {
				t.Fatalf("generated manifest is not valid YAML: %v", err)
			}
			if got := manifest["name"]; got != tc.project {
				t.Errorf("project name = %#v (%T), want string %q", got, got, tc.project)
			}
			applications, ok := manifest["applications"].(map[string]any)
			if !ok || len(applications) != 1 {
				t.Fatalf("applications = %#v, want one application", manifest["applications"])
			}
			application, ok := applications[tc.application].(map[string]any)
			if !ok {
				t.Fatalf("application %q missing from %#v", tc.application, applications)
			}
			source, ok := application["source"].(map[string]any)
			if !ok {
				t.Fatalf("source = %#v, want a mapping", application["source"])
			}
			if got := source["context"]; got != tc.directory {
				t.Errorf("build context = %#v (%T), want string %q", got, got, tc.directory)
			}
		})
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

func TestInitDoesNotFollowManifestSymlinks(t *testing.T) {
	for _, targetExists := range []bool{false, true} {
		name := "missing target"
		if targetExists {
			name = "existing target"
		}
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "project")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(dir, "Dockerfile"), "FROM scratch\n")
			target := filepath.Join(parent, "outside.yaml")
			const original = "name: keep-me\n"
			if targetExists {
				writeFile(t, target, original)
			}
			manifest := filepath.Join(dir, "chmura.yaml")
			if err := os.Symlink(target, manifest); err != nil {
				t.Fatal(err)
			}

			if code, out := execInit(t, dir); code != ExitUsage {
				t.Errorf("exit = %d, want %d when a manifest symlink exists\n%s", code, ExitUsage, out)
			}
			if got, err := os.Readlink(manifest); err != nil || got != target {
				t.Errorf("manifest symlink = %q, %v; want unchanged target %q", got, err, target)
			}
			content, err := os.ReadFile(target)
			if targetExists {
				if err != nil || string(content) != original {
					t.Errorf("symlink target was modified: %q, %v", content, err)
				}
			} else if !os.IsNotExist(err) {
				t.Errorf("init must not create the symlink target: %q, %v", content, err)
			}
		})
	}
}

func TestInitConcurrentCreationDoesNotOverwrite(t *testing.T) {
	root := t.TempDir()
	for i := range 32 {
		dir := filepath.Join(root, fmt.Sprintf("app-%d", i))
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "Dockerfile"), "FROM scratch\n")
	}
	const workers = 16
	start := make(chan struct{})
	codes := make(chan int, workers)
	for range workers {
		go func() {
			<-start
			code, _ := execInit(t, "--depth", "1", root)
			codes <- code
		}()
	}
	close(start)
	successes := 0
	for range workers {
		switch code := <-codes; code {
		case ExitOK:
			successes++
		case ExitUsage:
			// A different invocation created the manifest first.
		default:
			t.Errorf("exit = %d, want success or an existing-manifest error", code)
		}
	}
	if successes != 1 {
		t.Errorf("%d concurrent invocations succeeded, want exactly one", successes)
	}
	if got := len(parseInitManifest(t, root).Applications); got != 32 {
		t.Errorf("manifest has %d applications, want 32", got)
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
	if got := parseInitManifest(t, ".").Name; got != "shop" {
		t.Errorf("name should come from cwd: got %#v, want shop", got)
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

type parsedInitManifest struct {
	Version      int `yaml:"version"`
	Name         any `yaml:"name"`
	Applications map[string]struct {
		Source struct {
			Context    any    `yaml:"context"`
			Dockerfile string `yaml:"dockerfile"`
		} `yaml:"source"`
	} `yaml:"applications"`
}

func parseInitManifest(t *testing.T, dir string) parsedInitManifest {
	t.Helper()
	var manifest parsedInitManifest
	if err := yaml.Unmarshal([]byte(readManifest(t, dir)), &manifest); err != nil {
		t.Fatalf("generated manifest is not valid YAML: %v", err)
	}
	return manifest
}

func assertInitApplications(t *testing.T, manifest parsedInitManifest, want map[string]string) {
	t.Helper()
	if len(manifest.Applications) != len(want) {
		t.Errorf("applications = %#v, want %v", manifest.Applications, want)
	}
	for name, context := range want {
		application, ok := manifest.Applications[name]
		if !ok {
			t.Errorf("application %q missing", name)
			continue
		}
		if application.Source.Context != context || application.Source.Dockerfile != "Dockerfile" {
			t.Errorf("application %q source = %#v, want context %q and Dockerfile", name, application.Source, context)
		}
	}
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

	manifest := parseInitManifest(t, root)
	if manifest.Name != "mono" {
		t.Errorf("project name = %#v, want mono", manifest.Name)
	}
	assertInitApplications(t, manifest, map[string]string{"api": "api", "web": "web"})
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

	assertInitApplications(t, parseInitManifest(t, root), map[string]string{
		"mono": ".", "api": "services/api", "deploy": "tools/scripts/deploy",
	})
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

	assertInitApplications(t, parseInitManifest(t, root), map[string]string{"web": "web"})
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

	assertInitApplications(t, parseInitManifest(t, root), map[string]string{"mono": "."})
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

	manifest := parseInitManifest(t, root)
	if manifest.Name != "my-service" {
		t.Errorf("project name = %#v, want my-service", manifest.Name)
	}
	assertInitApplications(t, manifest, map[string]string{"user-api": "user_API"})
}

func TestInitNonexistentDir(t *testing.T) {
	code, out := execInit(t, filepath.Join(t.TempDir(), "missing"))
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d\n%s", code, ExitUsage, out)
	}
}

// A mid-scan failure must fail the command and write nothing — a partial
// manifest would silently omit every application below the unreadable subtree.
func TestInitScanErrorDoesNotWritePartialManifest(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; an unreadable directory is still readable")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no directory read permissions to revoke")
	}
	root := filepath.Join(t.TempDir(), "mono")
	// "aa" sorts before "zz", so the readable application is collected before
	// the walk hits the unreadable directory — the partial-manifest case.
	for _, d := range []string{filepath.Join(root, "aa"), filepath.Join(root, "zz")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(root, "aa/Dockerfile"), "FROM scratch\n")
	if err := os.Chmod(filepath.Join(root, "zz"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(root, "zz"), 0o755) })

	code, out := execInit(t, "--depth", "all", root)
	if code != ExitError {
		t.Fatalf("exit = %d, want %d (a scan error must fail, not write a partial manifest)\n%s", code, ExitError, out)
	}
	if err := os.Chmod(filepath.Join(root, "zz"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "chmura.yaml")); !os.IsNotExist(err) {
		t.Error("chmura.yaml must not be written when the scan cannot complete")
	}
}

// The depth-1 path reads the root's entries directly (os.ReadDir); a failure
// there must also abort before writing — the sibling of the WalkDir case.
func TestInitDepthOneScanErrorFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; an unreadable directory is still readable")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no directory read permissions to revoke")
	}
	root := filepath.Join(t.TempDir(), "mono")
	if err := os.MkdirAll(filepath.Join(root, "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "api/Dockerfile"), "FROM scratch\n")
	if err := os.Chmod(root, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(root, 0o755) })

	code, out := execInit(t, "--depth", "1", root)
	if code != ExitError {
		t.Fatalf("exit = %d, want %d (a read failure at depth 1 must fail, not write a partial manifest)\n%s", code, ExitError, out)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "chmura.yaml")); !os.IsNotExist(err) {
		t.Error("chmura.yaml must not be written when the scan cannot complete")
	}
}

// A directory literally named "Dockerfile" can never be built as a Dockerfile,
// so it is not a source — detection must require a regular file.
func TestInitIgnoresDirectoryNamedDockerfile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mono")
	if err := os.MkdirAll(filepath.Join(root, "Dockerfile"), 0o755); err != nil {
		t.Fatal(err)
	}

	if code, out := execInit(t, root); code != ExitUsage {
		t.Fatalf("exit = %d, want %d (a directory named Dockerfile is not a source)\n%s", code, ExitUsage, out)
	}
	if _, err := os.Stat(filepath.Join(root, "chmura.yaml")); !os.IsNotExist(err) {
		t.Error("chmura.yaml must not be written when no real source is found")
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

	if got := parseInitManifest(t, target).Name; got != "api" {
		t.Errorf("name should follow the dir arg: got %#v, want api", got)
	}
	if _, err := os.Stat(filepath.Join(cwd, "chmura.yaml")); !os.IsNotExist(err) {
		t.Error("chmura.yaml must not be written into cwd when a dir arg is given")
	}
}
