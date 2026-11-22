package k3s

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/daropotter/chmura/internal/deployment"
	"github.com/daropotter/chmura/internal/manifest"
	"github.com/goccy/go-yaml"
)

// restartOperationIntent is a private, canonical v1 recovery format, not a
// manifest or public wire API. Keep its encoding stable for accepted operations.
type restartOperationIntent struct {
	Version              int                   `yaml:"version"`
	Engine               string                `yaml:"engine"`
	Scope                string                `yaml:"scope"`
	SlotID               string                `yaml:"slot-id"`
	PreviousGenerationID string                `yaml:"previous-generation-id"`
	GenerationID         string                `yaml:"generation-id"`
	PreviousVersion      int64                 `yaml:"previous-version"`
	Previous             restartCreationIntent `yaml:"previous"`
	Next                 restartCreationIntent `yaml:"next"`
}

type restartCreationIntent struct {
	Namespace      string            `yaml:"namespace"`
	IdempotencyKey string            `yaml:"idempotency-key"`
	ResolvedImage  string            `yaml:"resolved-image"`
	NodeName       string            `yaml:"node-name"`
	CPUMilli       int64             `yaml:"cpu-milli"`
	MemoryBytes    int64             `yaml:"memory-bytes"`
	Plan           restartPlanIntent `yaml:"plan"`
}

type restartPlanIntent struct {
	ProjectName     string                   `yaml:"project-name"`
	ApplicationName string                   `yaml:"application-name"`
	Image           string                   `yaml:"image"`
	Ports           map[string]manifest.Port `yaml:"ports"`
	Health          manifest.Health          `yaml:"health"`
	Readiness       deployment.ReadinessMode `yaml:"readiness"`
}

// PrepareRestartOperationIntent freezes validated retained restart evidence and
// effective creation requests into a private v1 intent. The owner authorizes
// scope/intent, accepts/starts the operation and admits the explicit successor.
// Preparation alone grants no execution permission. Creation keys are private
// replay data; requests must contain references rather than secret values.
func PrepareRestartOperationIntent(input ProcessRestartRequest, successorGenerationID string, previousVersion int64) ([]byte, error) {
	request, err := PrepareProcessRestart(input)
	if err != nil {
		return nil, err
	}
	intent := restartOperationIntent{Version: 1, Engine: "k3s", Scope: input.Process.Scope, SlotID: input.Process.SlotID,
		PreviousGenerationID: input.Process.GenerationID, GenerationID: successorGenerationID, PreviousVersion: previousVersion,
		Previous: creationIntent(request.Previous), Next: creationIntent(request.Next)}
	if err := intent.validate(); err != nil {
		return nil, err
	}
	return yaml.Marshal(intent)
}

func creationIntent(request InstanceRequest) restartCreationIntent {
	plan := request.Plan
	return restartCreationIntent{Namespace: request.Namespace, IdempotencyKey: request.IdempotencyKey,
		ResolvedImage: request.ResolvedImage, NodeName: request.NodeName, CPUMilli: request.CPUMilli, MemoryBytes: request.MemoryBytes,
		Plan: restartPlanIntent{plan.ProjectName, plan.ApplicationName, plan.Image, plan.Ports, *plan.Health.Effective(), plan.Readiness}}
}

func (intent restartCreationIntent) request() InstanceRequest {
	plan := intent.Plan
	return InstanceRequest{Namespace: intent.Namespace, IdempotencyKey: intent.IdempotencyKey,
		ResolvedImage: intent.ResolvedImage, NodeName: intent.NodeName, CPUMilli: intent.CPUMilli, MemoryBytes: intent.MemoryBytes,
		Plan: deployment.InstancePlan{ProjectName: plan.ProjectName, ApplicationName: plan.ApplicationName, Image: plan.Image,
			Ports: plan.Ports, Health: *plan.Health.Effective(), Readiness: plan.Readiness}}
}

func (intent restartOperationIntent) validate() error {
	for _, value := range []string{intent.Scope, intent.SlotID, intent.PreviousGenerationID, intent.GenerationID} {
		if strings.TrimSpace(value) == "" || !utf8.ValidString(value) {
			return errors.New("restart intent requires complete UTF-8 scope, slot and generation identities")
		}
	}
	if intent.Version != 1 || intent.Engine != "k3s" || intent.GenerationID == intent.PreviousGenerationID || intent.PreviousVersion <= 0 || intent.PreviousVersion == math.MaxInt64 {
		return errors.New("restart intent requires v1 k3s, a different successor and a positive predecessor version")
	}
	previous, next := intent.Previous.request(), intent.Next.request()
	pod, err := PrepareInstance(previous)
	if err != nil {
		return err
	}
	// Validate the creation constraints without adopting a runtime identity.
	_, err = restartPayload(RestartRequest{Previous: previous, Next: next, Instance: Instance{
		Namespace: pod.Namespace, PodName: pod.Name, PodUID: "validate-intent", ContainerName: "app", ContainerID: "validate-intent"}})
	return err
}

func decodeRestartOperationIntent(data []byte) (restartOperationIntent, error) {
	var intent restartOperationIntent
	if err := yaml.UnmarshalWithOptions(data, &intent, yaml.DisallowUnknownField()); err != nil {
		return restartOperationIntent{}, fmt.Errorf("decode restart intent: %w", err)
	}
	if err := intent.validate(); err != nil {
		return restartOperationIntent{}, err
	}
	intent.Previous, intent.Next = creationIntent(intent.Previous.request()), creationIntent(intent.Next.request())
	canonical, err := yaml.Marshal(intent)
	if err != nil || !bytes.Equal(data, canonical) {
		return restartOperationIntent{}, errors.New("restart intent is not canonical private v1 data")
	}
	return intent, nil
}
