package k3s_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/engine/k3s"
	"github.com/daropotter/chmura/internal/manifest"
	"github.com/daropotter/chmura/internal/probe"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func exitRestartDecision(t *testing.T, store *lifetimeStore, request k3s.InstanceRequest, ref k3s.Instance, supervisor k3s.Supervisor) *k3s.RestartNeededError {
	t.Helper()
	store.mu.Lock()
	pod := store.pods[ref.PodName]
	started := pod.Status.ContainerStatuses[0].State.Running.StartedAt
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ContainerID: ref.ContainerID, ExitCode: 0, Reason: "Completed", StartedAt: started,
		FinishedAt: metav1.NewTime(started.Add(time.Second)),
	}}
	store.mu.Unlock()
	err := supervisor.Monitor(context.Background(), k3s.SupervisionRequest{Instance: ref, Plan: request.Plan}, func(probe.Readiness) {
		t.Error("already exited process published readiness")
	})
	var decision *k3s.RestartNeededError
	var exit *k3s.InstanceExitError
	if !errors.As(err, &decision) || !errors.As(err, &exit) || exit.ExitCode != 0 || !errors.Is(err, k3s.ErrInstanceExited) || !errors.Is(err, k3s.ErrInstanceUnavailable) {
		t.Fatalf("process exit did not retain a restart decision and its facts: %v", err)
	}
	return decision
}

func TestRestartDecisionHandsOffExplicitRequestToExecutor(t *testing.T) {
	store := &lifetimeStore{}
	client, previous, ref := lifetimeClient(t, store)
	decision := exitRestartDecision(t, store, previous, ref, k3s.Supervisor{Resolver: k3s.Resolver{Client: client}})
	next := previous
	next.IdempotencyKey = "explicit-successor"
	next.NodeName, next.CPUMilli = "node-b", 500
	request, err := decision.Request(previous, next)
	if err != nil || request.Instance != ref || request.Next.IdempotencyKey != next.IdempotencyKey {
		t.Fatalf("decision lost generation or explicit successor: %#v %v", request, err)
	}
	_, deletes, calls := store.snapshot(ref.PodName)
	if deletes != 0 || calls != 1 {
		t.Fatal("decision or request construction performed runtime mutation or extra I/O")
	}
	restarter := k3s.Restarter{Client: client}
	result, err := restarter.Ensure(context.Background(), request)
	if err != nil || !result.Retiring {
		t.Fatalf("authorized decision did not enter graceful retirement: %#v %v", result, err)
	}
	store.mu.Lock()
	delete(store.pods, ref.PodName)
	store.mu.Unlock()
	result, err = restarter.Ensure(context.Background(), request)
	if err != nil || result.Retiring || !result.Instance.Created || result.Instance.Identity.PodUID == "" {
		t.Fatalf("decision did not reconcile a successor: %#v %v", result, err)
	}
	replay, err := restarter.Ensure(context.Background(), request)
	if err != nil || replay.Instance.Created || replay.Instance.Identity != result.Instance.Identity {
		t.Fatalf("decision replay changed successor: %#v %v", replay, err)
	}
}

func TestRestartDecisionBindsTheObservedPlanAndGeneration(t *testing.T) {
	for _, mode := range []string{"previous key", "previous namespace", "previous application", "previous health", "previous check", "previous port", "same successor key", "changed successor digest", "zero decision", "nil decision"} {
		t.Run(mode, func(t *testing.T) {
			store := &lifetimeStore{}
			client, previous, ref := lifetimeClient(t, store)
			decision := exitRestartDecision(t, store, previous, ref, k3s.Supervisor{Resolver: k3s.Resolver{Client: client}})
			next := previous
			next.IdempotencyKey = "explicit-successor"
			switch mode {
			case "previous key":
				previous.IdempotencyKey = "other-generation"
			case "previous namespace":
				previous.Namespace = "other"
			case "previous application":
				previous.Plan.ApplicationName = "other"
				next.Plan.ApplicationName = "other"
			case "previous health":
				// Changing both sides must not turn an unrelated plan into the
				// supervised plan, even though they match each other.
				previous.Plan.Health.Ready.Successes++
				next.Plan.Health.Ready.Successes++
			case "previous port":
				previous.Plan.Ports["web listener"] = manifest.Port{Number: 9090, Protocol: "http"}
			case "previous check":
				previous.Plan.Health.Check.HTTP.Path = "/different"
			case "same successor key":
				next.IdempotencyKey = previous.IdempotencyKey
			case "changed successor digest":
				next.ResolvedImage = "registry.example.com/team/api@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			case "zero decision":
				decision = &k3s.RestartNeededError{}
			case "nil decision":
				decision = nil
			}
			request, err := decision.Request(previous, next)
			if err == nil || request.Instance.PodUID != "" {
				t.Fatalf("unrelated request accepted: %#v %v", request, err)
			}
			_, deletes, calls := store.snapshot(ref.PodName)
			if deletes != 0 || calls != 1 {
				t.Fatal("request validation contacted runtime")
			}
		})
	}
}

func TestRestartDecisionAllowsEquivalentEffectivePlan(t *testing.T) {
	store := &lifetimeStore{}
	client, previous, ref := lifetimeClient(t, store)
	decision := exitRestartDecision(t, store, previous, ref, k3s.Supervisor{Resolver: k3s.Resolver{Client: client}})
	previous.Plan.Health.StartupTimeout = 0
	previous.Plan.Health.Check.Interval = 0
	previous.Plan.Health.Check.Timeout = 0
	next := previous
	next.IdempotencyKey = "explicit-successor"
	if _, err := decision.Request(previous, next); err != nil {
		t.Fatalf("omitted defaults changed effective supervision intent: %v", err)
	}
}
