package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/daropotter/chmura/internal/manifest"
)

func TestInitOutputPassesManifestValidation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "false")
	if err := os.MkdirAll(filepath.Join(dir, "api: v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "Dockerfile"), "FROM scratch\n")
	writeFile(t, filepath.Join(dir, "api: v1", "Dockerfile"), "FROM scratch\n")
	if code, out := execInit(t, "--depth", "1", dir); code != ExitOK {
		t.Fatalf("init exit = %d: %s", code, out)
	}
	project, err := manifest.Parse([]byte(readManifest(t, dir)))
	if err != nil {
		t.Fatalf("init generated an invalid manifest: %v", err)
	}
	if project.Name != "false" || len(project.Applications) != 2 {
		t.Fatalf("unexpected project: %#v", project)
	}
	for name, context := range map[string]string{"false": ".", "api-v1": "api: v1"} {
		source := project.Applications[name].Source
		if source.Context != context || source.Dockerfile != "Dockerfile" {
			t.Errorf("application %q source = %#v", name, source)
		}
	}
}
