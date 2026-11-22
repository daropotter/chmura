package server_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/deployment"
	"github.com/daropotter/chmura/internal/manifest"
	"github.com/daropotter/chmura/internal/server"
	"github.com/daropotter/chmura/internal/state"
	"github.com/daropotter/chmura/internal/state/sqlite"
)

const deployYAML = `version: 1
name: "true"
applications:
  "123":
    source: {image: ghcr.io/acme/web:v1}
    ports:
      web: {number: 8080, protocol: http}
      metrics: {number: 9090, protocol: tcp}
    health:
      check:
        http: {port: web, path: /ready}
        timeout: 2s
`

const reformattedDeployYAML = `# Equivalent manifest with reordered keys and explicit port visibility.
name: 'true'
applications:
  '123':
    health: {check: {timeout: 2000ms, http: {path: /ready, port: web}}}
    ports:
      metrics: {protocol: tcp, number: 9090, visibility: project}
      web: {visibility: project, protocol: http, number: 8080}
    source:
      image: ghcr.io/acme/web:v1
version: 1
`

func TestSubmitDeployStoresReplayableManifest(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cluster, _, err := store.CreateCluster(ctx, state.CreateClusterRequest{Name: "home", IdempotencyKey: "cluster"})
	if err != nil {
		t.Fatal(err)
	}
	space, _, err := store.CreateSpace(ctx, state.CreateSpaceRequest{Name: "production", ClusterID: cluster.ID, IdempotencyKey: "space"})
	if err != nil {
		t.Fatal(err)
	}
	project, _, err := store.CreateProject(ctx, state.CreateProjectRequest{Name: "true", SpaceID: space.ID, IdempotencyKey: "project"})
	if err != nil {
		t.Fatal(err)
	}

	request := server.DeploySubmission{SpaceID: space.ID, ProjectID: project.ID, IdempotencyKey: "deploy", Manifest: []byte(deployYAML)}
	first, created, err := server.SubmitDeploy(ctx, store, request)
	if err != nil || !created || first.Operation.Status != state.OperationPending || first.Selection.Version != 1 {
		t.Fatalf("submit deploy: %+v, created=%v, error=%v", first, created, err)
	}
	revision, err := store.GetRevision(ctx, space.ID, project.ID, first.Selection.RevisionID)
	if err != nil || bytes.Equal(revision.Manifest, request.Manifest) {
		t.Fatalf("expected normalized snapshot: %q, error=%v", revision.Manifest, err)
	}
	parsed, err := manifest.Parse(revision.Manifest)
	if err != nil {
		t.Fatalf("saved manifest cannot be parsed again: %v", err)
	}
	plan, err := deployment.PlanInstance(parsed, "123")
	if err != nil || plan.ProjectName != project.Name || plan.Image != "ghcr.io/acme/web:v1" ||
		plan.Readiness != deployment.ReadinessChecked || plan.Ports["web"].Visibility != "project" ||
		plan.Health.Check == nil || plan.Health.Check.HTTP == nil || plan.Health.Check.HTTP.Path != "/ready" ||
		time.Duration(plan.Health.Check.Timeout) != 2*time.Second {
		t.Fatalf("saved manifest changed plan: %+v, error=%v", plan, err)
	}

	// Even after reopening, YAML formatting and equivalent scalar values should
	// not turn a retry into a different operation.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	request.Manifest = []byte(reformattedDeployYAML)
	replay, created, err := server.SubmitDeploy(ctx, store, request)
	if err != nil || created || replay.Operation.ID != first.Operation.ID || replay.Selection != first.Selection {
		t.Fatalf("retry after reopen: %+v, created=%v, error=%v", replay, created, err)
	}
	request.Manifest = []byte(strings.Replace(reformattedDeployYAML, "web:v1", "web:v2", 1))
	if _, created, err := server.SubmitDeploy(ctx, store, request); !errors.Is(err, state.ErrConflict) || created {
		t.Fatalf("changed deploy did not conflict: created=%v, error=%v", created, err)
	}
	request.IdempotencyKey = "next"
	if _, created, err := server.SubmitDeploy(ctx, store, request); !errors.Is(err, state.ErrConflict) || created {
		t.Fatalf("stale base did not conflict: created=%v, error=%v", created, err)
	}
	if got, err := store.CurrentRevision(ctx, space.ID, project.ID); err != nil || got != first.Selection {
		t.Fatalf("rejected deploy changed selection: %+v, error=%v", got, err)
	}
	request.ExpectedVersion, request.ExpectedRevisionID = first.Selection.Version, first.Selection.RevisionID
	next, created, err := server.SubmitDeploy(ctx, store, request)
	if err != nil || !created || next.Selection.Version != 2 || next.Selection.BaseRevisionID != first.Selection.RevisionID {
		t.Fatalf("deploy with refreshed base: %+v, created=%v, error=%v", next, created, err)
	}
}

func TestSubmitDeployRejectsInvalidRequestsBeforeAcceptance(t *testing.T) {
	for _, tc := range []struct {
		name  string
		yaml  string
		space string
		want  error
	}{
		{"invalid manifest", "version: [1", "space", server.ErrInvalidDeployManifest},
		{"unsupported field", deployYAML + "unknown: true\n", "space", server.ErrInvalidDeployManifest},
		{"wrong project name", strings.Replace(deployYAML, `name: "true"`, "name: other", 1), "space", state.ErrConflict},
		{"another space", deployYAML, "other", state.ErrNotFound},
		{"multiple applications", deployYAML + "  worker: {source: {image: ghcr.io/acme/worker:v1}}\n", "space", server.ErrInvalidDeployManifest},
		{"build source", strings.Replace(deployYAML, "{image: ghcr.io/acme/web:v1}", "{context: ., dockerfile: Dockerfile}", 1), "space", deployment.ErrBuildRequired},
		{"invalid image", strings.Replace(deployYAML, "ghcr.io/acme/web:v1", "INVALID IMAGE", 1), "space", server.ErrInvalidDeployManifest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A rejected submission must never reach the write transaction.
			store := rejectingDeployStore{t: t}
			_, created, err := server.SubmitDeploy(context.Background(), store, server.DeploySubmission{
				SpaceID: tc.space, ProjectID: "project", IdempotencyKey: "deploy", Manifest: []byte(tc.yaml),
			})
			if created || !errors.Is(err, tc.want) {
				t.Fatalf("created=%v, error=%v, want %v", created, err, tc.want)
			}
		})
	}
}

type rejectingDeployStore struct {
	t *testing.T
}

func (s rejectingDeployStore) GetProject(_ context.Context, spaceID, projectID string) (state.Project, error) {
	if spaceID != "space" || projectID != "project" {
		return state.Project{}, state.ErrNotFound
	}
	return state.Project{ID: projectID, SpaceID: spaceID, Name: "true"}, nil
}

func (s rejectingDeployStore) AcceptDeploy(context.Context, state.DeployRequest) (state.DeployAcceptance, bool, error) {
	s.t.Fatal("rejected request reached persistence")
	return state.DeployAcceptance{}, false, nil
}
