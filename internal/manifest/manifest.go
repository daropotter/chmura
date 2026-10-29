// Package manifest parses and validates portable project definitions.
// The schema covers project identity, application sources, ports and health.
package manifest

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/parser"
)

// Project is desired state, independent of any installation or deployment target.
type Project struct {
	Version      int                    `yaml:"version"`
	Name         string                 `yaml:"name"`
	Applications map[string]Application `yaml:"applications"`
}

// Application describes an application in a project.
type Application struct {
	Source Source          `yaml:"source"`
	Ports  map[string]Port `yaml:"ports,omitempty"`
	Health *Health         `yaml:"health,omitempty"`
}

// Source selects a prebuilt image or a Dockerfile within a build context.
type Source struct {
	Image      string `yaml:"image,omitempty"`
	Context    string `yaml:"context,omitempty"`
	Dockerfile string `yaml:"dockerfile,omitempty"`
}

// Parse accepts exactly one YAML project. Unknown and not-yet-implemented fields
// are rejected rather than silently discarded. Parsing never reads source paths,
// contacts a registry, or performs a build.
func Parse(data []byte) (*Project, error) {
	file, err := parser.ParseBytes(data, 0)
	if err != nil {
		return nil, fmt.Errorf("manifest YAML: %w", err)
	}
	if len(file.Docs) != 1 || file.Docs[0].Body == nil {
		return nil, errors.New("manifest must contain exactly one project document")
	}
	var project Project
	if err := yaml.NodeToValue(file.Docs[0].Body, &project,
		yaml.DisallowUnknownField(),
		yaml.CustomUnmarshaler[string](decodeString),
		yaml.CustomUnmarshaler[int](decodeInteger),
	); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if err := project.validate(); err != nil {
		return nil, err
	}
	return &project, nil
}

func (p Project) validate() error {
	if p.Version != 1 {
		return errors.New("version: expected manifest schema version 1")
	}
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("name: project name is required")
	}
	if len(p.Applications) == 0 {
		return errors.New("applications: at least one application is required")
	}
	for _, name := range slices.Sorted(maps.Keys(p.Applications)) {
		path := "applications." + name + ".source"
		source := p.Applications[name].Source
		if source.Image != "" {
			if source.Context != "" || source.Dockerfile != "" {
				return fmt.Errorf("%s: image and Dockerfile build fields are mutually exclusive", path)
			}
		} else if source.Context == "" {
			return fmt.Errorf("%s: an image or a build context is required", path)
		} else if source.Dockerfile == "" {
			return fmt.Errorf("%s.dockerfile: Dockerfile path is required with context", path)
		}
		if err := p.Applications[name].normalizePorts("applications." + name); err != nil {
			return err
		}
		if err := p.Applications[name].normalizeHealth("applications." + name); err != nil {
			return err
		}
	}
	return nil
}

// go-yaml normally coerces scalars (for example false into "false", and 1.5
// into 1). Manifest names, paths and schema versions must keep their YAML types.
func decodeString(target *string, data []byte) error {
	var value any
	if err := yaml.Unmarshal(data, &value); err != nil {
		return err
	}
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return errors.New("manifest string fields and names must be non-empty strings")
	}
	*target = text
	return nil
}

func decodeInteger(target *int, data []byte) error {
	var value any
	if err := yaml.Unmarshal(data, &value); err != nil {
		return err
	}
	switch value.(type) {
	case int64, uint64:
		return yaml.Unmarshal(data, target)
	default:
		return errors.New("expected an integer")
	}
}
