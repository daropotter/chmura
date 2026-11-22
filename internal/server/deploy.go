package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/goccy/go-yaml"

	"github.com/daropotter/chmura/internal/deployment"
	"github.com/daropotter/chmura/internal/manifest"
	"github.com/daropotter/chmura/internal/state"
)

// ErrInvalidDeployManifest identifies a manifest that cannot be submitted.
var ErrInvalidDeployManifest = errors.New("invalid deploy manifest")

// DeploySubmission carries user-supplied YAML and the caller's authorized target.
// The expected base is read by the client before submitting this command.
type DeploySubmission struct {
	SpaceID            string
	ProjectID          string
	IdempotencyKey     string
	ExpectedVersion    int64
	ExpectedRevisionID string
	Manifest           []byte
}

// DeployStore resolves the project and atomically accepts a validated deploy.
type DeployStore interface {
	GetProject(ctx context.Context, spaceID, projectID string) (state.Project, error)
	AcceptDeploy(ctx context.Context, request state.DeployRequest) (state.DeployAcceptance, bool, error)
}

// SubmitDeploy validates the target declaration and plans its single prebuilt
// application before handing the normalized manifest to the store. The caller
// must authorize access to the space and project before calling this function.
func SubmitDeploy(ctx context.Context, store DeployStore, request DeploySubmission) (state.DeployAcceptance, bool, error) {
	if err := ctx.Err(); err != nil {
		return state.DeployAcceptance{}, false, err
	}
	project, err := manifest.Parse(request.Manifest)
	if err != nil {
		return state.DeployAcceptance{}, false, fmt.Errorf("%w: %w", ErrInvalidDeployManifest, err)
	}
	target, err := store.GetProject(ctx, request.SpaceID, request.ProjectID)
	if err != nil {
		return state.DeployAcceptance{}, false, err
	}
	if project.Name != target.Name {
		return state.DeployAcceptance{}, false, fmt.Errorf("manifest project %q does not match target %q: %w", project.Name, target.Name, state.ErrConflict)
	}
	if len(project.Applications) != 1 {
		return state.DeployAcceptance{}, false, fmt.Errorf("%w: deploy requires exactly one application", ErrInvalidDeployManifest)
	}
	for name := range project.Applications {
		if _, err := deployment.PlanInstance(project, name); err != nil {
			return state.DeployAcceptance{}, false, fmt.Errorf("%w: %w", ErrInvalidDeployManifest, err)
		}
	}
	// JSON-style quoting preserves string values that resemble YAML scalars.
	canonical, err := yaml.MarshalWithOptions(project, yaml.JSON())
	if err != nil {
		return state.DeployAcceptance{}, false, fmt.Errorf("encode deploy manifest: %w", err)
	}
	return store.AcceptDeploy(ctx, state.DeployRequest{
		SpaceID: request.SpaceID, ProjectID: request.ProjectID, IdempotencyKey: request.IdempotencyKey,
		ExpectedVersion: request.ExpectedVersion, ExpectedRevisionID: request.ExpectedRevisionID, Manifest: canonical,
	})
}
