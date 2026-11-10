package k3s_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/engine/k3s"
	"github.com/daropotter/chmura/internal/probe"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func markExited(pod *corev1.Pod) {
	status := &pod.Status.ContainerStatuses[1]
	started := status.State.Running.StartedAt
	status.State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ContainerID: status.ContainerID, ExitCode: 17, Reason: "Error", StartedAt: started, FinishedAt: metav1.NewTime(started.Add(30 * time.Second))}}
	pod.Status.Phase = corev1.PodFailed
	pod.Status.PodIP = "" // Exit evidence must not require an old network target.
}

func TestResolveReportsExitForTheExactGeneration(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodRunning, corev1.PodFailed, corev1.PodSucceeded} {
		t.Run(string(phase), func(t *testing.T) {
			pod := observedPod()
			markExited(pod)
			pod.Status.Phase = phase
			if phase == corev1.PodSucceeded {
				pod.Status.ContainerStatuses[1].State.Terminated.ExitCode = 0
				pod.Status.ContainerStatuses[1].State.Terminated.Reason = "Completed"
			}
			var calls atomic.Int32
			target, err := (k3s.Resolver{Client: podClient(t, pod, &calls)}).Resolve(context.Background(), instance(), nil)
			var exit *k3s.InstanceExitError
			if !errors.Is(err, k3s.ErrInstanceExited) || !errors.Is(err, k3s.ErrInstanceUnavailable) || !errors.As(err, &exit) || target.Network.Host != "" {
				t.Fatalf("reported exit became a target or lost its type: target=%#v error=%v", target, err)
			}
			state := pod.Status.ContainerStatuses[1].State.Terminated
			if exit.Instance != instance() || exit.ResourceVersion != pod.ResourceVersion || exit.ExitCode != state.ExitCode || exit.Reason != state.Reason || !exit.StartedAt.Equal(state.StartedAt.Time) || !exit.FinishedAt.Equal(state.FinishedAt.Time) {
				t.Fatalf("exit facts=%#v", exit)
			}
		})
	}
}

func TestResolveDoesNotInferExitFromIncompleteUnknownOrRetiringState(t *testing.T) {
	for _, mode := range []string{"missing finish", "missing start", "reversed times", "missing version", "missing terminated ID", "different terminated ID", "unknown termination", "deleting pod", "unknown phase", "pending phase", "also running", "also waiting", "missing current ID", "different current ID", "different pod UID"} {
		t.Run(mode, func(t *testing.T) {
			pod := observedPod()
			markExited(pod)
			status := &pod.Status.ContainerStatuses[1]
			want := k3s.ErrInstanceUnavailable
			switch mode {
			case "missing finish":
				status.State.Terminated.FinishedAt = metav1.Time{}
			case "missing start":
				status.State.Terminated.StartedAt = metav1.Time{}
			case "reversed times":
				status.State.Terminated.FinishedAt = metav1.NewTime(status.State.Terminated.StartedAt.Add(-time.Second))
			case "missing version":
				pod.ResourceVersion = ""
			case "missing terminated ID":
				status.State.Terminated.ContainerID = ""
			case "different terminated ID":
				status.State.Terminated.ContainerID = "containerd://other"
			case "unknown termination":
				status.State.Terminated.Reason = "ContainerStatusUnknown"
			case "deleting pod":
				now := metav1.Now()
				pod.DeletionTimestamp = &now
			case "unknown phase":
				pod.Status.Phase = corev1.PodUnknown
			case "pending phase":
				pod.Status.Phase = corev1.PodPending
			case "also running":
				status.State.Running = &corev1.ContainerStateRunning{StartedAt: status.State.Terminated.StartedAt}
			case "also waiting":
				status.State.Waiting = &corev1.ContainerStateWaiting{}
			case "missing current ID":
				status.ContainerID = ""
			case "different current ID":
				status.ContainerID = "containerd://other"
				want = k3s.ErrStaleInstance
			case "different pod UID":
				pod.UID = "other"
				want = k3s.ErrStaleInstance
			}
			var calls atomic.Int32
			_, err := (k3s.Resolver{Client: podClient(t, pod, &calls)}).Resolve(context.Background(), instance(), nil)
			var exit *k3s.InstanceExitError
			if !errors.Is(err, want) || errors.Is(err, k3s.ErrInstanceExited) || errors.As(err, &exit) {
				t.Fatalf("uncertain state became exit evidence: %v", err)
			}
		})
	}
}

func TestResolveIgnoresHistoricalTerminationOfAnotherProcess(t *testing.T) {
	pod := observedPod()
	running := pod.Status.ContainerStatuses[1].State
	markExited(pod)
	pod.Status.ContainerStatuses[1].LastTerminationState = pod.Status.ContainerStatuses[1].State
	pod.Status.ContainerStatuses[1].LastTerminationState.Terminated.ContainerID = "containerd://previous"
	pod.Status.ContainerStatuses[1].State = running
	pod.Status.Phase = corev1.PodRunning
	pod.Status.PodIP = "10.0.0.12"
	var calls atomic.Int32
	target, err := (k3s.Resolver{Client: podClient(t, pod, &calls)}).Resolve(context.Background(), instance(), nil)
	if err != nil || target.Network.Host != pod.Status.PodIP {
		t.Fatalf("historical exit replaced current target: %#v %v", target, err)
	}
}

func TestObserveDistinguishesReportedExitFromDeletionAndChangedStart(t *testing.T) {
	for _, mode := range []string{"exit", "deleted event", "changed start"} {
		t.Run(mode, func(t *testing.T) {
			pod := observedPod()
			changed := pod.DeepCopy()
			markExited(changed)
			changed.ResourceVersion = "43"
			kind, want := "MODIFIED", k3s.ErrInstanceExited
			switch mode {
			case "deleted event":
				kind = "DELETED"
				want = k3s.ErrInstanceUnavailable
			case "changed start":
				changed.Status.ContainerStatuses[1].State.Terminated.StartedAt = metav1.NewTime(changed.Status.ContainerStatuses[1].State.Terminated.StartedAt.Add(time.Second))
				want = k3s.ErrObservationLost
			}
			client := observerClient(t, pod, func(w http.ResponseWriter, r *http.Request) {
				openWatch(w)
				watchEvent(t, w, kind, changed)
				<-r.Context().Done()
			})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			started := 0
			err := (k3s.Resolver{Client: client}).Observe(ctx, instance(), nil, func(k3s.Target) { started++ })
			if !errors.Is(err, want) || started != 1 {
				t.Fatalf("exit observation: starts=%d error=%v, want %v", started, err, want)
			}
			if mode != "exit" && errors.Is(err, k3s.ErrInstanceExited) {
				t.Fatalf("observation discontinuity became process exit: %v", err)
			}
			if mode == "exit" {
				var exit *k3s.InstanceExitError
				if !errors.As(err, &exit) || exit.ResourceVersion != "43" {
					t.Fatalf("watch lost exit snapshot facts: %v", err)
				}
			}
		})
	}
}

func TestSupervisorReportedExitCancelsAndJoinsActiveProbe(t *testing.T) {
	pod := observedPod()
	changed := pod.DeepCopy()
	markExited(changed)
	changed.ResourceVersion = "43"
	trigger := make(chan struct{})
	client := observerClient(t, pod, func(w http.ResponseWriter, r *http.Request) {
		openWatch(w)
		select {
		case <-trigger:
		case <-r.Context().Done():
			return
		}
		watchEvent(t, w, "MODIFIED", changed)
		<-r.Context().Done()
	})
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	releaseCleanup := func() { once.Do(func() { close(release) }) }
	defer releaseCleanup()
	supervisor := k3s.Supervisor{Resolver: k3s.Resolver{Client: client}, Network: probe.Network{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		return nil, ctx.Err()
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	plan := supervisionPlan(8080)
	plan.Health.StartupTimeout = 0 // Use the normal budget for the minute-old snapshot.
	done := make(chan error, 1)
	go func() {
		done <- supervisor.Monitor(ctx, k3s.SupervisionRequest{Instance: instance(), Plan: plan}, func(probe.Readiness) {})
	}()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("monitor stopped before probe: %v", err)
	}
	close(trigger)
	select {
	case <-canceled:
	case err := <-done:
		t.Fatalf("exit did not cancel probe: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("exit returned before cleanup: %v", err)
	default:
	}
	releaseCleanup()
	err := <-done
	var exit *k3s.InstanceExitError
	if !errors.Is(err, k3s.ErrInstanceExited) || !errors.As(err, &exit) || exit.ExitCode != 17 || errors.Is(err, probe.ErrUnhealthy) || errors.Is(err, context.Canceled) {
		t.Fatalf("supervisor lost reported-exit cause: %v", err)
	}
}
