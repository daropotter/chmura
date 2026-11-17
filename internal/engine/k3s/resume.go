package k3s

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/daropotter/chmura/internal/deployment"
	"github.com/daropotter/chmura/internal/probe"
	"github.com/daropotter/chmura/internal/state"
)

// SupervisionHistory reads desired and observed history in an authorized scope.
// These snapshots are not worker leases; the owner must serialize admissions,
// terminal decisions and monitor startup/shutdown. Reads alone do not fence an
// obsolete or duplicate owner. sqlite.Store implements this interface.
type SupervisionHistory interface {
	CurrentGeneration(context.Context, string, string) (state.Generation, error)
	GetProcess(context.Context, string, string, string) (state.Process, error)
}

// ResumeRequest explicitly selects a previously bound current generation.
// The owner authorizes the scope and supplies the validated plan belonging to
// its desired intent. Resume neither decodes desired intent nor adopts runtime
// identities from the database. EverReady comes only from retained history.
type ResumeRequest struct {
	Scope        string
	SlotID       string
	GenerationID string
	Instance     Instance
	Plan         deployment.InstancePlan
}

// EncodeProcessIdentity encodes the complete private k3s reference for durable
// state.Process identity comparisons. Version 1 is canonical and must remain
// stable for stored bindings; mutable IP, resource version and readiness are
// excluded. This format is internal, never manifest or public API data.
func EncodeProcessIdentity(ref Instance) ([]byte, error) {
	for _, value := range []string{ref.Namespace, ref.PodName, ref.PodUID, ref.ContainerName, ref.ContainerID} {
		if strings.TrimSpace(value) == "" || !utf8.ValidString(value) {
			return nil, errors.New("process identity requires a complete UTF-8 runtime reference")
		}
	}
	return json.Marshal(struct {
		Version       int    `json:"version"`
		Engine        string `json:"engine"`
		Namespace     string `json:"namespace"`
		PodName       string `json:"pod-name"`
		PodUID        string `json:"pod-uid"`
		ContainerName string `json:"container-name"`
		ContainerID   string `json:"container-id"`
	}{1, "k3s", ref.Namespace, ref.PodName, ref.PodUID, ref.ContainerName, ref.ContainerID})
}

// Resume checks retained history before any runtime I/O, then monitors the exact
// identity/start using retained first readiness. Current readiness still starts
// false and requires the full threshold. Missing, superseded, terminal or
// mismatched history never starts observation. Errors preserve storage causes.
// As with Monitor, the callback must return promptly and do no outbound I/O.
// This method writes no history/status and executes no restart; the owner must
// persist new first-readiness/terminal facts and authorize any subsequent action.
func (s Supervisor) Resume(ctx context.Context, history SupervisionHistory, request ResumeRequest, onChange func(probe.Readiness)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if history == nil || onChange == nil || strings.TrimSpace(request.Scope) == "" || strings.TrimSpace(request.SlotID) == "" || strings.TrimSpace(request.GenerationID) == "" {
		return errors.New("supervision resume requires history, scope, slot, generation and an observer")
	}
	retained, err := retainedSupervisionProcess(ctx, history, request)
	if err != nil {
		return err
	}
	return s.monitor(ctx, SupervisionRequest{Instance: request.Instance, Plan: request.Plan, EverReady: retained.EverReady}, onChange, retained.StartedAt)
}

func retainedSupervisionProcess(ctx context.Context, history SupervisionHistory, request ResumeRequest) (state.Process, error) {
	if err := ctx.Err(); err != nil {
		return state.Process{}, err
	}
	if history == nil || strings.TrimSpace(request.Scope) == "" || strings.TrimSpace(request.SlotID) == "" || strings.TrimSpace(request.GenerationID) == "" {
		return state.Process{}, errors.New("supervision history requires scope, slot and generation")
	}
	identity, err := EncodeProcessIdentity(request.Instance)
	if err != nil {
		return state.Process{}, err
	}
	head, err := history.CurrentGeneration(ctx, request.Scope, request.SlotID)
	if cause := ctx.Err(); cause != nil {
		return state.Process{}, cause
	}
	if err != nil {
		return state.Process{}, fmt.Errorf("read supervision slot head: %w", err)
	}
	if head.Scope != request.Scope || head.SlotID != request.SlotID || head.ID != request.GenerationID || head.Version <= 0 {
		return state.Process{}, fmt.Errorf("%w: selected supervision generation is not current", state.ErrConflict)
	}
	retained, err := history.GetProcess(ctx, request.Scope, request.SlotID, request.GenerationID)
	if cause := ctx.Err(); cause != nil {
		return state.Process{}, cause
	}
	if err != nil {
		return state.Process{}, fmt.Errorf("read supervision process history: %w", err)
	}
	if retained.Scope != request.Scope || retained.SlotID != request.SlotID || retained.GenerationID != request.GenerationID || retained.StartedAt.IsZero() || !bytes.Equal(retained.Identity, identity) {
		return state.Process{}, fmt.Errorf("%w: retained process does not match supervision selection", state.ErrConflict)
	}
	if retained.Terminal != nil {
		return state.Process{}, fmt.Errorf("%w: process supervision is terminal", state.ErrConflict)
	}
	return retained, nil
}
