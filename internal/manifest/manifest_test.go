package manifest_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/daropotter/chmura/internal/manifest"
)

const imageProject = `version: 1
name: shop
applications:
  api:
    source:
      image: ghcr.io/acme/api:1.0
`

func TestParseSources(t *testing.T) {
	input := imageProject + `  worker:
    source:
      context: ./worker
      dockerfile: build/Dockerfile
`
	got, err := manifest.Parse([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	want := &manifest.Project{
		Version: 1, Name: "shop",
		Applications: map[string]manifest.Application{
			"api":    {Source: manifest.Source{Image: "ghcr.io/acme/api:1.0"}},
			"worker": {Source: manifest.Source{Context: "./worker", Dockerfile: "build/Dockerfile"}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("project = %#v, want %#v", got, want)
	}
}

func TestParsePreservesNamesAndPaths(t *testing.T) {
	for _, name := range []string{"false", "null", "123", "API: v1"} {
		t.Run(name, func(t *testing.T) {
			input := fmt.Sprintf("version: 1\nname: %q\napplications:\n  %q:\n    source: {context: %q, dockerfile: Dockerfile}\n", name, name, "./API: v1")
			got, err := manifest.Parse([]byte(input))
			if err != nil {
				t.Fatal(err)
			}
			if got.Name != name || got.Applications[name].Source.Context != "./API: v1" {
				t.Errorf("names or paths changed: %#v", got)
			}
		})
	}
}

func TestParseRejectsInvalidProjects(t *testing.T) {
	for _, tc := range []struct {
		name, input, signal string
	}{
		{"empty", "", "document"},
		{"comments only", "# no project\n", "document"},
		{"missing version", strings.Replace(imageProject, "version: 1\n", "", 1), "version"},
		{"unknown version", strings.Replace(imageProject, "version: 1", "version: 2", 1), "version"},
		{"missing project name", strings.Replace(imageProject, "name: shop\n", "", 1), "name"},
		{"no applications", "version: 1\nname: shop\n", "applications"},
		{"empty applications", "version: 1\nname: shop\napplications: {}\n", "applications"},
		{"null applications", "version: 1\nname: shop\napplications: null\n", "applications"},
		{"missing application source", "version: 1\nname: shop\napplications: {api: {}}\n", "applications.api.source"},
		{"null application", "version: 1\nname: shop\napplications: {api: null}\n", "applications.api.source"},
		{"empty source", "version: 1\nname: shop\napplications: {api: {source: {}}}\n", "applications.api.source"},
		{"both sources", strings.Replace(imageProject, "image: ghcr.io/acme/api:1.0", "image: ghcr.io/acme/api:1.0\n      context: .\n      dockerfile: Dockerfile", 1), "applications.api.source"},
		{"dockerfile with image", strings.Replace(imageProject, "image: ghcr.io/acme/api:1.0", "image: ghcr.io/acme/api:1.0\n      dockerfile: Dockerfile", 1), "applications.api.source"},
		{"dockerfile without context", strings.Replace(imageProject, "image: ghcr.io/acme/api:1.0", "dockerfile: Dockerfile", 1), "applications.api.source"},
		{"context without dockerfile", strings.Replace(imageProject, "image: ghcr.io/acme/api:1.0", "context: .", 1), "applications.api.source.dockerfile"},
		{"root typo", imageProject + "aplications: {}\n", "aplications"},
		{"application typo", strings.Replace(imageProject, "source:", "soruce:", 1), "soruce"},
		{"source typo", strings.Replace(imageProject, "image:", "imgae:", 1), "imgae"},
		{"target in manifest", imageProject + "space: production\n", "space"},
		{"engine escape hatch", strings.Replace(imageProject, "source:", "kubernetes: {}\n    source:", 1), "kubernetes"},
		{"unimplemented field", strings.Replace(imageProject, "source:", "resources: {memory: 512Mi}\n    source:", 1), "resources"},
		{"duplicate root key", imageProject + "name: other\n", "name"},
		{"duplicate application", imageProject + "  api: {source: {image: other:1}}\n", "api"},
		{"duplicate source key", strings.Replace(imageProject, "image: ghcr.io/acme/api:1.0", "image: ghcr.io/acme/api:1.0\n      image: other:1", 1), "image"},
		{"second document", imageProject + "---\n" + imageProject, "document"},
		{"empty second document", imageProject + "---\n", "document"},
		{"malformed YAML", "version: [\n", ""},
		{"scalar document", "hello\n", ""},
		{"sequence document", "- hello\n", ""},
		{"applications sequence", "version: 1\nname: shop\napplications: []\n", ""},
		{"source scalar", "version: 1\nname: shop\napplications: {api: {source: nginx}}\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project, err := manifest.Parse([]byte(tc.input))
			if err == nil {
				t.Fatal("invalid project accepted")
			}
			if project != nil {
				t.Errorf("returned a partial project after validation failed: %#v", project)
			}
			if !strings.Contains(err.Error(), tc.signal) {
				t.Errorf("error = %q, want signal %q", err, tc.signal)
			}
		})
	}
}

func TestParseRejectsScalarCoercion(t *testing.T) {
	for _, tc := range []struct{ name, from, to string }{
		{"quoted version", "version: 1", `version: "1"`},
		{"fractional version", "version: 1", "version: 1.5"},
		{"boolean name", "name: shop", "name: false"},
		{"numeric name", "name: shop", "name: 123"},
		{"null name", "name: shop", "name: null"},
		{"blank name", "name: shop", `name: "  "`},
		{"boolean application name", "  api:", "  false:"},
		{"numeric application name", "  api:", "  123:"},
		{"empty application name", "  api:", `  "":`},
		{"numeric image", "image: ghcr.io/acme/api:1.0", "image: 123"},
		{"null image", "image: ghcr.io/acme/api:1.0", "image: null"},
		{"blank image", "image: ghcr.io/acme/api:1.0", `image: "  "`},
		{"boolean context", "image: ghcr.io/acme/api:1.0", "context: false\n      dockerfile: Dockerfile"},
		{"numeric dockerfile", "image: ghcr.io/acme/api:1.0", "context: .\n      dockerfile: 123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := manifest.Parse([]byte(strings.Replace(imageProject, tc.from, tc.to, 1))); err == nil {
				t.Fatal("invalid scalar accepted")
			}
		})
	}
}
