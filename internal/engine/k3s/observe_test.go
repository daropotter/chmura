package k3s_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/engine/k3s"
	"github.com/daropotter/chmura/internal/manifest"
	"github.com/daropotter/chmura/internal/probe"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
)

func observedPod() *corev1.Pod {
	pod := runningPod()
	pod.ResourceVersion = "42"
	return pod
}

func observerClient(t *testing.T, pod *corev1.Pod, watchHandler http.HandlerFunc) *coreclient.CoreV1Client {
	t.Helper()
	return apiClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("observation changed the cluster: %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces/team-a/pods/app-a":
			_ = json.NewEncoder(w).Encode(pod)
		case "/api/v1/namespaces/team-a/pods":
			q := r.URL.Query()
			if q.Get("watch") != "true" || q.Get("resourceVersion") != pod.ResourceVersion || q.Get("fieldSelector") != "metadata.name=app-a" {
				t.Errorf("watch is not bound to its snapshot: %s", r.URL.RawQuery)
			}
			watchHandler(w, r)
		default:
			t.Errorf("unexpected observation path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func openWatch(w http.ResponseWriter) {
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()
}

func watchEvent(t *testing.T, w http.ResponseWriter, kind string, object any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(map[string]any{"type": kind, "object": object}); err != nil {
		t.Error(err)
	}
	w.(http.Flusher).Flush()
}

func TestObserveInstanceStopsOnRuntimeChanges(t *testing.T) {
	for _, mode := range []string{"deleted", "terminated", "waiting", "terminating pod", "pod replaced", "container restarted", "address changed", "start time changed"} {
		t.Run(mode, func(t *testing.T) {
			pod := observedPod()
			changed := pod.DeepCopy()
			changed.ResourceVersion = "43"
			kind, want := "MODIFIED", k3s.ErrInstanceUnavailable
			switch mode {
			case "deleted":
				kind = "DELETED"
			case "terminated":
				changed.Status.ContainerStatuses[1].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}
			case "waiting":
				changed.Status.ContainerStatuses[1].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}}
			case "terminating pod":
				now := metav1.Now()
				changed.DeletionTimestamp = &now
			case "pod replaced":
				changed.UID = "pod-generation-b"
				want = k3s.ErrStaleInstance
			case "container restarted":
				changed.Status.ContainerStatuses[1].ContainerID = "containerd://generation-b"
				want = k3s.ErrStaleInstance
			case "address changed":
				changed.Status.PodIP = "10.0.0.13"
				want = k3s.ErrObservationLost
			case "start time changed":
				changed.Status.ContainerStatuses[1].State.Running.StartedAt = metav1.NewTime(time.Now().Truncate(time.Second))
				want = k3s.ErrObservationLost
			}
			client := observerClient(t, pod, func(w http.ResponseWriter, r *http.Request) {
				openWatch(w)
				// A change between GET and opening the watch is delivered after
				// snapshot version 42 rather than silently missed.
				watchEvent(t, w, kind, changed)
				<-r.Context().Done()
			})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			calls := 0
			ports := map[string]manifest.Port{"web": {Number: 8080, Protocol: "http"}}
			err := (k3s.Resolver{Client: client}).Observe(ctx, instance(), ports, func(target k3s.Target) {
				calls++
				if target.Network.Host != pod.Status.PodIP || target.Network.Ports["web"].Number != 8080 || !target.ProcessStartedAt.Equal(pod.Status.ContainerStatuses[1].State.Running.StartedAt.Time) {
					t.Errorf("initial observation target = %#v", target)
				}
			})
			if !errors.Is(err, want) || calls != 1 {
				t.Fatalf("runtime observation: error=%v starts=%d, want %v", err, calls, want)
			}
		})
	}
}

func TestObserveInstanceIgnoresReadinessChangesAndBookmarks(t *testing.T) {
	pod := observedPod()
	client := observerClient(t, pod, func(w http.ResponseWriter, r *http.Request) {
		openWatch(w)
		bookmark := &corev1.Pod{TypeMeta: pod.TypeMeta, ObjectMeta: metav1.ObjectMeta{ResourceVersion: "43"}}
		watchEvent(t, w, "BOOKMARK", bookmark)
		changed := pod.DeepCopy()
		changed.ResourceVersion = "44"
		changed.Status.ContainerStatuses[1].Ready = true
		watchEvent(t, w, "MODIFIED", changed)
		changed.Status.ContainerStatuses[1].Ready = false
		watchEvent(t, w, "MODIFIED", changed)
		watchEvent(t, w, "ADDED", changed)
		watchEvent(t, w, "DELETED", changed)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	calls := 0
	err := (k3s.Resolver{Client: client}).Observe(ctx, instance(), nil, func(k3s.Target) { calls++ })
	if !errors.Is(err, k3s.ErrInstanceUnavailable) || calls != 1 {
		t.Fatalf("readiness changes interrupted observation: error=%v starts=%d", err, calls)
	}
}

func TestObserveInstanceDistinguishesLossOfObservation(t *testing.T) {
	for _, mode := range []string{"closed", "expired", "wrong object", "missing event UID", "missing event version", "unknown event", "forbidden", "missing resource version"} {
		t.Run(mode, func(t *testing.T) {
			pod := observedPod()
			if mode == "missing resource version" {
				pod.ResourceVersion = ""
			}
			var watches atomic.Int32
			client := observerClient(t, pod, func(w http.ResponseWriter, r *http.Request) {
				watches.Add(1)
				if mode == "forbidden" {
					w.WriteHeader(http.StatusForbidden)
					_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: metav1.StatusReasonForbidden, Code: 403})
					return
				}
				openWatch(w)
				switch mode {
				case "closed":
					return
				case "expired":
					watchEvent(t, w, "ERROR", &metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: metav1.StatusReasonExpired, Code: 410})
				case "wrong object":
					watchEvent(t, w, "MODIFIED", &corev1.Node{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Node"}})
				case "missing event UID":
					invalid := pod.DeepCopy()
					invalid.UID = ""
					watchEvent(t, w, "MODIFIED", invalid)
				case "missing event version":
					invalid := pod.DeepCopy()
					invalid.ResourceVersion = ""
					watchEvent(t, w, "MODIFIED", invalid)
				case "unknown event":
					watchEvent(t, w, "UNEXPECTED", pod)
				}
				<-r.Context().Done()
			})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			starts := 0
			err := (k3s.Resolver{Client: client}).Observe(ctx, instance(), nil, func(k3s.Target) { starts++ })
			if !errors.Is(err, k3s.ErrObservationLost) || errors.Is(err, probe.ErrUnhealthy) || errors.Is(err, k3s.ErrInstanceUnavailable) {
				t.Fatalf("observation failure was confused with process/health failure: %v", err)
			}
			if mode == "expired" && !apierrors.IsResourceExpired(err) || mode == "forbidden" && !apierrors.IsForbidden(err) {
				t.Fatalf("watch API cause lost: %v", err)
			}
			wantStarts, wantWatches := 1, int32(1)
			if mode == "forbidden" || mode == "missing resource version" {
				wantStarts = 0
			}
			if mode == "missing resource version" {
				wantWatches = 0
			}
			if starts != wantStarts || watches.Load() != wantWatches {
				t.Fatalf("unestablished observation started work: starts=%d watches=%d", starts, watches.Load())
			}
		})
	}
}

func TestObserveInstanceCancellationClosesWatch(t *testing.T) {
	for _, mode := range []string{"already canceled", "cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			stopped := make(chan struct{})
			client := observerClient(t, observedPod(), func(w http.ResponseWriter, r *http.Request) {
				openWatch(w)
				<-r.Context().Done()
				close(stopped)
			})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			want := context.Canceled
			if mode == "deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				want = context.DeadlineExceeded
			} else if mode == "already canceled" {
				cancel()
			}
			started, done := make(chan struct{}), make(chan error, 1)
			go func() {
				done <- (k3s.Resolver{Client: client}).Observe(ctx, instance(), nil, func(k3s.Target) { close(started) })
			}()
			if mode != "already canceled" {
				select {
				case <-started:
				case err := <-done:
					t.Fatalf("observation stopped before start: %v", err)
				}
				if mode == "cancel" {
					cancel()
				}
			}
			if err := <-done; !errors.Is(err, want) || errors.Is(err, k3s.ErrObservationLost) {
				t.Fatalf("observation stop = %v, want %v", err, want)
			}
			if mode != "already canceled" {
				select {
				case <-stopped:
				case <-time.After(time.Second):
					t.Fatal("observer left the watch request open")
				}
			}
		})
	}
}

func TestObserveInstanceRejectsInvalidStart(t *testing.T) {
	for _, mode := range []string{"no observer", "incomplete reference", "stale pod", "unavailable pod"} {
		t.Run(mode, func(t *testing.T) {
			pod, ref := observedPod(), instance()
			callback := func(k3s.Target) { t.Error("invalid observation started work") }
			switch mode {
			case "no observer":
				callback = nil
			case "incomplete reference":
				ref.ContainerID = ""
			case "stale pod":
				pod.UID = "other"
			case "unavailable pod":
				pod.Status.Phase = corev1.PodFailed
			}
			client := observerClient(t, pod, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid observation opened watch") })
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := (k3s.Resolver{Client: client}).Observe(ctx, ref, nil, callback); err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("invalid observation start = %v", err)
			}
		})
	}
}

func TestObservedExitAllowsOwnerToCancelReadiness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	attemptStarted := make(chan struct{})
	client := observerClient(t, observedPod(), func(w http.ResponseWriter, r *http.Request) {
		openWatch(w)
		select {
		case <-attemptStarted:
			watchEvent(t, w, "DELETED", observedPod())
		case <-r.Context().Done():
			return
		}
		<-r.Context().Done()
	})
	readyDone := make(chan error, 1)
	var last probe.Readiness
	err := (k3s.Resolver{Client: client}).Observe(ctx, instance(), nil, func(target k3s.Target) {
		go func() {
			health := &manifest.Health{Check: &manifest.Probe{Process: &manifest.ProcessCheck{Name: "app"}}}
			readyDone <- probe.MonitorReadiness(ctx, target.ProcessStartedAt, health, func(ctx context.Context, _ manifest.Probe) error {
				close(attemptStarted)
				<-ctx.Done()
				return ctx.Err()
			}, func(state probe.Readiness) { last = state })
		}()
	})
	cancel()
	if !errors.Is(err, k3s.ErrInstanceUnavailable) {
		t.Fatalf("observed exit = %v", err)
	}
	select {
	case err := <-readyDone:
		if !errors.Is(err, context.Canceled) || last.StartupFailed || last.Ready {
			t.Fatalf("lifecycle stop became readiness failure: state=%#v error=%v", last, err)
		}
	case <-time.After(time.Second):
		t.Fatal("readiness did not clean up after the observed exit")
	}
}
