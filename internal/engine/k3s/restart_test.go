package k3s_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/engine/k3s"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func restartRequest(t *testing.T, previous k3s.InstanceRequest, ref k3s.Instance) k3s.RestartRequest {
	t.Helper()
	next := previous
	next.IdempotencyKey = "restart-generation-2"
	return k3s.RestartRequest{Previous: previous, Instance: ref, Next: next}
}

func TestRestartWaitsForRetirementThenCreatesAndReplaysOneGeneration(t *testing.T) {
	s := &lifetimeStore{}
	client, previous, ref := lifetimeClient(t, s)
	request := restartRequest(t, previous, ref)
	request.Next.NodeName = "node-b"
	request.Next.CPUMilli = 500
	restarter := k3s.Restarter{Client: client}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for range 2 {
		result, err := restarter.Ensure(ctx, request)
		if err != nil || !result.Retiring || result.Instance.Identity.PodUID != "" {
			t.Fatalf("restart skipped retirement: %#v %v", result, err)
		}
		s.mu.Lock()
		creates, count := s.creates, len(s.pods)
		s.mu.Unlock()
		if creates != 0 || count != 1 {
			t.Fatal("successor was created before old resource disappeared")
		}
	}
	s.mu.Lock()
	delete(s.pods, ref.PodName)
	s.mu.Unlock()
	first, err := restarter.Ensure(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Retiring || !first.Instance.Created || first.Instance.Identity.PodUID == "" || first.Instance.Identity.PodName == ref.PodName {
		t.Fatalf("restart did not acknowledge a fresh generation: %#v", first)
	}
	second, err := restarter.Ensure(ctx, request)
	if err != nil || second.Retiring || second.Instance.Created || second.Instance.Identity != first.Instance.Identity {
		t.Fatalf("restart replay changed identity: %#v %v", second, err)
	}
	pod, _, _ := s.snapshot(first.Instance.Identity.PodName)
	s.mu.Lock()
	creates, count := s.creates, len(s.pods)
	s.mu.Unlock()
	if creates != 1 || count != 1 || pod.Spec.NodeName != "node-b" || pod.Spec.Containers[0].Resources.Requests.Cpu().MilliValue() != 500 {
		t.Fatal("restart duplicated a generation or ignored explicit admission")
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever || len(pod.Status.Conditions) != 0 {
		t.Fatal("restart inherited native restart policy or prior readiness")
	}
}

func TestRestartConcurrentReplayCreatesOnlyOneSuccessor(t *testing.T) {
	s := &lifetimeStore{}
	client, previous, ref := lifetimeClient(t, s)
	request := restartRequest(t, previous, ref)
	s.mu.Lock()
	delete(s.pods, ref.PodName)
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type outcome struct {
		result k3s.RestartResult
		err    error
	}
	done := make(chan outcome, 12)
	for range 12 {
		go func() {
			result, err := (k3s.Restarter{Client: client}).Ensure(ctx, request)
			done <- outcome{result, err}
		}()
	}
	created := 0
	for range 12 {
		got := <-done
		if got.err != nil || got.result.Retiring || got.result.Instance.Identity.PodUID != "new-uid-1" {
			t.Fatalf("concurrent restart: %#v %v", got.result, got.err)
		}
		if got.result.Instance.Created {
			created++
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.creates != 1 || len(s.pods) != 1 || created != 1 {
		t.Fatalf("concurrent duplicate: creates=%d pods=%d results=%d", s.creates, len(s.pods), created)
	}
}

func TestRestartValidatesSuccessorBeforeRetiringOldGeneration(t *testing.T) {
	for _, mode := range []string{"same key", "different namespace", "different application", "different digest", "different health", "invalid allocation"} {
		t.Run(mode, func(t *testing.T) {
			s := &lifetimeStore{}
			client, previous, ref := lifetimeClient(t, s)
			request := restartRequest(t, previous, ref)
			switch mode {
			case "same key":
				request.Next.IdempotencyKey = previous.IdempotencyKey
			case "different namespace":
				request.Next.Namespace = "other"
			case "different application":
				request.Next.Plan.ApplicationName = "other"
			case "different digest":
				request.Next.ResolvedImage = "registry.example.com/team/api@sha256:" + strings.Repeat("b", 64)
			case "different health":
				request.Next.Plan.Health.Ready.Successes++
			case "invalid allocation":
				request.Next.MemoryBytes = 0
			}
			if _, err := (k3s.Restarter{Client: client}).Ensure(context.Background(), request); err == nil {
				t.Fatal("invalid restart was accepted")
			}
			pod, deletes, requests := s.snapshot(ref.PodName)
			if deletes != 0 || requests != 0 || pod.DeletionTimestamp != nil {
				t.Fatal("invalid successor affected the old generation")
			}
		})
	}
}

func TestRestartRejectsConflictingSuccessorBeforeDeletion(t *testing.T) {
	s := &lifetimeStore{}
	client, previous, ref := lifetimeClient(t, s)
	request := restartRequest(t, previous, ref)
	foreign, err := k3s.PrepareInstance(request.Next)
	if err != nil {
		t.Fatal(err)
	}
	foreign.UID = "foreign"
	foreign.ResourceVersion = "2"
	foreign.Labels["app.kubernetes.io/managed-by"] = "other"
	s.mu.Lock()
	s.pods[foreign.Name] = foreign
	s.mu.Unlock()
	if _, err := (k3s.Restarter{Client: client}).Ensure(context.Background(), request); !errors.Is(err, k3s.ErrInstanceConflict) {
		t.Fatalf("conflicting successor was accepted: %v", err)
	}
	pod, deletes, _ := s.snapshot(ref.PodName)
	if deletes != 0 || pod.DeletionTimestamp != nil {
		t.Fatal("old generation retired before successor conflict was reported")
	}
}

func TestRestartRecoversLostCreationResponse(t *testing.T) {
	s := &lifetimeStore{dropCreateReply: true}
	client, previous, ref := lifetimeClient(t, s)
	request := restartRequest(t, previous, ref)
	s.mu.Lock()
	delete(s.pods, ref.PodName)
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	restarter := k3s.Restarter{Client: client}
	_, _ = restarter.Ensure(ctx, request)
	result, err := restarter.Ensure(ctx, request)
	s.mu.Lock()
	creates := s.creates
	s.mu.Unlock()
	if err != nil || result.Retiring || result.Instance.Identity.PodUID != "new-uid-1" || result.Instance.Created || creates != 1 {
		t.Fatalf("lost response duplicated restart: %#v creates=%d error=%v", result, creates, err)
	}
}

func TestRestartPreservesErrorsAndRejectsInvalidOwnerInput(t *testing.T) {
	for _, mode := range []string{"no client", "missing old identity", "pre-canceled", "forbidden preflight", "forbidden create", "replacement"} {
		t.Run(mode, func(t *testing.T) {
			s := &lifetimeStore{}
			client, previous, ref := lifetimeClient(t, s)
			request := restartRequest(t, previous, ref)
			restarter := k3s.Restarter{Client: client}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			s.mu.Lock()
			switch mode {
			case "no client":
				restarter.Client = nil
			case "missing old identity":
				request.Instance.PodUID = ""
			case "pre-canceled":
				cancel()
			case "forbidden preflight":
				s.getStatus = 403
			case "forbidden create":
				delete(s.pods, ref.PodName)
				s.createStatus = 403
			case "replacement":
				s.pods[ref.PodName].UID = "replacement"
			}
			s.mu.Unlock()
			result, err := restarter.Ensure(ctx, request)
			if err == nil || result.Instance.Identity.PodUID != "" {
				t.Fatalf("failed restart returned success: %#v %v", result, err)
			}
			if strings.HasPrefix(mode, "forbidden") && !apierrors.IsForbidden(err) || mode == "replacement" && !errors.Is(err, k3s.ErrStaleInstance) || mode == "pre-canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("restart cause lost: %v", err)
			}
			_, deletes, requests := s.snapshot(ref.PodName)
			if deletes != 0 {
				t.Fatal("failed restart deleted an object")
			}
			if (mode == "no client" || mode == "missing old identity" || mode == "pre-canceled") && requests != 0 {
				t.Fatal("invalid input contacted API")
			}
		})
	}
}
