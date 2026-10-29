// Package deployment builds portable execution plans from validated manifests.
package deployment

import (
	// Register the digest algorithms accepted by the image-reference parser.
	_ "crypto/sha256"
	_ "crypto/sha512"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/distribution/reference"

	"github.com/daropotter/chmura/internal/manifest"
)

// ReadinessMode distinguishes explicit probe readiness from process-only readiness.
type ReadinessMode string

const (
	ReadinessChecked     ReadinessMode = "checked"
	ReadinessProcessOnly ReadinessMode = "process-only"
)

// InstancePlan snapshots one application's configuration before placement and
// resource creation. Health contains effective lifecycle defaults, including
// when the declaration omitted health; absent probes/rules remain absent.
// The image reference retains its declared form and may still contain a tag.
type InstancePlan struct {
	ProjectName     string
	ApplicationName string
	Image           string
	Ports           map[string]manifest.Port
	Health          manifest.Health
	Readiness       ReadinessMode
}

// ErrBuildRequired means the selected application needs the deferred image-build stage.
var ErrBuildRequired = errors.New("application requires an image build")

// PlanInstance plans one explicitly selected application in a project validated
// by manifest.Parse. It validates image-reference syntax and resolves shared
// health defaults into an independent snapshot. It does not normalize names or
// references, contact registries, assign runtime identity, or allocate resources.
// Callers planning a whole project must plan all applications before executing.
func PlanInstance(project *manifest.Project, application string) (*InstancePlan, error) {
	if project == nil || project.Version != 1 || strings.TrimSpace(project.Name) == "" || strings.TrimSpace(application) == "" {
		return nil, errors.New("instance planning requires a schema-v1 project and explicit project/application names")
	}
	app, exists := project.Applications[application]
	if !exists {
		return nil, fmt.Errorf("applications: application %q is not declared", application)
	}
	if app.Source.Context != "" || app.Source.Dockerfile != "" {
		if app.Source.Image != "" {
			return nil, fmt.Errorf("applications.%s.source: image and build fields are mutually exclusive", application)
		}
		return nil, fmt.Errorf("applications.%s.source: %w", application, ErrBuildRequired)
	}
	if _, err := reference.ParseNormalizedNamed(app.Source.Image); err != nil {
		return nil, fmt.Errorf("applications.%s.source.image: %w", application, err)
	}
	health := app.Health
	if health == nil {
		health = &manifest.Health{}
	}
	effective := health.Effective()
	mode := ReadinessProcessOnly
	if effective.Check != nil {
		mode = ReadinessChecked
	}
	return &InstancePlan{
		ProjectName:     project.Name,
		ApplicationName: application,
		Image:           app.Source.Image,
		Ports:           maps.Clone(app.Ports),
		Health:          *effective,
		Readiness:       mode,
	}, nil
}
