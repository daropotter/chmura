package k3s_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/deployment"
	"github.com/daropotter/chmura/internal/engine/k3s"
	"github.com/daropotter/chmura/internal/manifest"
	"github.com/daropotter/chmura/internal/probe"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func supervisionPlan(port int) deployment.InstancePlan {
	return deployment.InstancePlan{
		Readiness: deployment.ReadinessChecked,
		Ports:     map[string]manifest.Port{"web": {Number: port, Protocol: "http"}},
		Health: manifest.Health{
			Check: &manifest.Probe{HTTP: &manifest.HTTPCheck{Path: "/ready", Port: "web"}, Interval: manifest.Duration(time.Millisecond)},
			Ready: manifest.ReadyRule{Successes: 1, UnreadyFailures: 1}, StartupTimeout: manifest.Duration(5 * time.Second),
		},
	}
}

func supervisedPod() *corev1.Pod {
	pod := observedPod()
	pod.Status.PodIP = "127.0.0.1"
	pod.Status.ContainerStatuses[1].State.Running.StartedAt = metav1.NewTime(time.Now().Truncate(time.Second))
	return pod
}

func checkSupervisionState(t *testing.T, updates <-chan probe.Readiness, done <-chan error, want probe.Readiness) {
	t.Helper()
	select {
	case got := <-updates:
		if got != want {
			t.Fatalf("supervision state=%#v, want %#v", got, want)
		}
	case err := <-done:
		t.Fatalf("supervision stopped before state %#v: %v", want, err)
	case <-time.After(3 * time.Second):
		t.Fatal("supervision did not publish state")
	}
}

func TestSuperviseReadinessLossRecoversWithoutRestart(t *testing.T) {
	for _, processOnlyHealthy := range []bool{false, true} {
		t.Run(map[bool]string{false: "no healthy", true: "process-only healthy"}[processOnlyHealthy], func(t *testing.T) {
			var status atomic.Int32
			status.Store(http.StatusOK)
			app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/ready" {
					t.Errorf("readiness inherited another probe: %s", r.URL.Path)
				}
				w.WriteHeader(int(status.Load()))
			}))
			defer app.Close()
			plan := supervisionPlan(app.Listener.Addr().(*net.TCPAddr).Port)
			if processOnlyHealthy {
				plan.Health.Healthy = &manifest.HealthyRule{UnhealthyFailures: 1}
			}
			var watches atomic.Int32
			client := observerClient(t, supervisedPod(), func(w http.ResponseWriter, r *http.Request) {
				watches.Add(1)
				openWatch(w)
				<-r.Context().Done()
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			updates, done := make(chan probe.Readiness, 8), make(chan error, 1)
			go func() {
				done <- (k3s.Supervisor{Resolver: k3s.Resolver{Client: client}}).Monitor(ctx, k3s.SupervisionRequest{Instance: instance(), Plan: plan}, func(state probe.Readiness) { updates <- state })
			}()
			checkSupervisionState(t, updates, done, probe.Readiness{})
			checkSupervisionState(t, updates, done, probe.Readiness{Ready: true, EverReady: true})
			status.Store(http.StatusServiceUnavailable)
			checkSupervisionState(t, updates, done, probe.Readiness{EverReady: true})
			status.Store(http.StatusOK)
			checkSupervisionState(t, updates, done, probe.Readiness{Ready: true, EverReady: true})
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("supervision stop=%v", err)
			}
			if state := <-updates; state != (probe.Readiness{EverReady: true}) {
				t.Fatalf("stop retained current readiness: %#v", state)
			}
			if watches.Load() != 1 {
				t.Fatalf("readiness changes restarted observation: %d", watches.Load())
			}
		})
	}
}

func TestSuperviseHealthyWaitsForFullReadinessAndSurvivesReadinessLoss(t *testing.T) {
	secondReady, grantReady, healthyStarted, failHealthy := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var readyCalls, healthyCalls, readyStatus atomic.Int32
	readyStatus.Store(http.StatusOK)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ready":
			if readyCalls.Add(1) == 2 {
				close(secondReady)
				select {
				case <-grantReady:
				case <-r.Context().Done():
					return
				}
			}
			w.WriteHeader(int(readyStatus.Load()))
		case "/healthy":
			if healthyCalls.Add(1) == 1 {
				close(healthyStarted)
			}
			select {
			case <-failHealthy:
			case <-r.Context().Done():
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			t.Errorf("unexpected application probe: %s", r.URL.Path)
		}
	}))
	defer app.Close()
	plan := supervisionPlan(app.Listener.Addr().(*net.TCPAddr).Port)
	plan.Health.Ready.Successes = 2
	plan.Health.Healthy = &manifest.HealthyRule{UnhealthyFailures: 1, Check: &manifest.Probe{HTTP: &manifest.HTTPCheck{Path: "/healthy", Port: "web"}}}
	client := observerClient(t, supervisedPod(), func(w http.ResponseWriter, r *http.Request) { openWatch(w); <-r.Context().Done() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	updates, done := make(chan probe.Readiness, 8), make(chan error, 1)
	go func() {
		done <- (k3s.Supervisor{Resolver: k3s.Resolver{Client: client}}).Monitor(ctx, k3s.SupervisionRequest{Instance: instance(), Plan: plan}, func(state probe.Readiness) { updates <- state })
	}()
	checkSupervisionState(t, updates, done, probe.Readiness{})
	select {
	case <-secondReady:
	case err := <-done:
		t.Fatalf("stopped before full readiness: %v", err)
	}
	if healthyCalls.Load() != 0 {
		t.Fatal("healthy started after partial readiness")
	}
	close(grantReady)
	checkSupervisionState(t, updates, done, probe.Readiness{Ready: true, EverReady: true})
	select {
	case <-healthyStarted:
	case err := <-done:
		t.Fatalf("healthy never started: %v", err)
	}
	readyStatus.Store(http.StatusServiceUnavailable)
	checkSupervisionState(t, updates, done, probe.Readiness{EverReady: true})
	close(failHealthy)
	if err := <-done; !errors.Is(err, probe.ErrUnhealthy) || errors.Is(err, context.Canceled) {
		t.Fatalf("healthy decision lost after readiness loss: %v", err)
	}
}

func TestSuperviseRuntimeStopCancelsAndWaitsForActiveProbe(t *testing.T) {
	for _, mode := range []string{"terminated", "pod replaced", "container replaced", "watch closed", "watch expired", "parent canceled"} {
		t.Run(mode, func(t *testing.T) {
			pod := supervisedPod()
			trigger, watchStopped := make(chan struct{}), make(chan struct{})
			client := observerClient(t, pod, func(w http.ResponseWriter, r *http.Request) {
				defer close(watchStopped)
				openWatch(w)
				select {
				case <-trigger:
				case <-r.Context().Done():
					return
				}
				changed := pod.DeepCopy()
				changed.ResourceVersion = "43"
				switch mode {
				case "terminated":
					changed.Status.ContainerStatuses[1].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}
				case "pod replaced":
					changed.UID = "replacement"
				case "container replaced":
					changed.Status.ContainerStatuses[1].ContainerID = "replacement"
				case "watch closed":
					return
				case "watch expired":
					watchEvent(t, w, "ERROR", &metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: metav1.StatusReasonExpired, Code: 410})
					<-r.Context().Done()
					return
				}
				watchEvent(t, w, "MODIFIED", changed)
				<-r.Context().Done()
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			supervisor := k3s.Supervisor{Resolver: k3s.Resolver{Client: client}, Network: probe.Network{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				close(started)
				<-ctx.Done()
				close(canceled)
				<-release
				return nil, ctx.Err()
			}}}
			done := make(chan error, 1)
			go func() {
				done <- supervisor.Monitor(ctx, k3s.SupervisionRequest{Instance: instance(), Plan: supervisionPlan(8080)}, func(probe.Readiness) {})
			}()
			select {
			case <-started:
			case err := <-done:
				t.Fatalf("stopped before probe start: %v", err)
			}
			if mode == "parent canceled" {
				cancel()
			} else {
				close(trigger)
			}
			select {
			case <-canceled:
			case err := <-done:
				t.Fatalf("did not cancel active probe: %v", err)
			}
			select {
			case err := <-done:
				t.Fatalf("returned before probe cleanup: %v", err)
			default:
			}
			releaseOnce.Do(func() { close(release) })
			err := <-done
			want := k3s.ErrInstanceUnavailable
			switch mode {
			case "pod replaced", "container replaced":
				want = k3s.ErrStaleInstance
			case "watch closed", "watch expired":
				want = k3s.ErrObservationLost
			case "parent canceled":
				want = context.Canceled
			}
			if !errors.Is(err, want) || errors.Is(err, probe.ErrUnhealthy) {
				t.Fatalf("runtime stop=%v, want %v", err, want)
			}
			if mode == "watch expired" && !apierrors.IsResourceExpired(err) {
				t.Fatalf("watch error cause lost: %v", err)
			}
			select {
			case <-watchStopped:
			case <-time.After(time.Second):
				t.Fatal("watch left open")
			}
		})
	}
}

func TestSuperviseUsesPriorFirstReadinessForHealthyAndStartup(t *testing.T) {
	for _, prior := range []bool{false, true} {
		t.Run(map[bool]string{false: "never ready", true: "previously ready"}[prior], func(t *testing.T) {
			var healthyCalls atomic.Int32
			app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/healthy" {
					healthyCalls.Add(1)
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer app.Close()
			plan := supervisionPlan(app.Listener.Addr().(*net.TCPAddr).Port)
			plan.Health.Healthy = &manifest.HealthyRule{UnhealthyFailures: 1, Check: &manifest.Probe{HTTP: &manifest.HTTPCheck{Path: "/healthy", Port: "web"}}}
			pod := supervisedPod()
			pod.Status.ContainerStatuses[1].State.Running.StartedAt = metav1.NewTime(time.Now().Add(-3 * time.Minute))
			client := observerClient(t, pod, func(w http.ResponseWriter, r *http.Request) { openWatch(w); <-r.Context().Done() })
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var last probe.Readiness
			err := (k3s.Supervisor{Resolver: k3s.Resolver{Client: client}}).Monitor(ctx, k3s.SupervisionRequest{Instance: instance(), Plan: plan, EverReady: prior}, func(state probe.Readiness) { last = state })
			want := probe.ErrStartupTimeout
			if prior {
				want = probe.ErrUnhealthy
			}
			if !errors.Is(err, want) || last.Ready || last.EverReady != prior || last.StartupFailed == prior {
				t.Fatalf("prior=%v state=%#v error=%v", prior, last, err)
			}
			if prior && healthyCalls.Load() != 1 || !prior && healthyCalls.Load() != 0 {
				t.Fatalf("prior=%v healthy calls=%d", prior, healthyCalls.Load())
			}
		})
	}
}

func TestSuperviseRejectsUnsupportedPlansBeforeObservation(t *testing.T) {
	for _, mode := range []string{"process-only readiness", "exec readiness", "exec healthy", "invalid threshold", "no observer", "pre-canceled"} {
		t.Run(mode, func(t *testing.T) {
			client := apiClient(t, func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("invalid supervision contacted API: %s", r.URL.Path)
				w.WriteHeader(500)
			})
			request := k3s.SupervisionRequest{Instance: instance(), Plan: supervisionPlan(8080)}
			observer := func(probe.Readiness) { t.Error("invalid supervision published readiness") }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "process-only readiness":
				request.Plan.Readiness = deployment.ReadinessProcessOnly
				request.Plan.Health.Check = nil
			case "exec readiness":
				request.Plan.Health.Check = &manifest.Probe{Exec: &manifest.ExecCheck{Command: manifest.Command{"check"}}}
			case "exec healthy":
				request.Plan.Health.Healthy = &manifest.HealthyRule{UnhealthyFailures: 1, Check: &manifest.Probe{Exec: &manifest.ExecCheck{Command: manifest.Command{"check"}}}}
			case "invalid threshold":
				request.Plan.Health.Ready.Successes = -1
			case "no observer":
				observer = nil
			case "pre-canceled":
				cancel()
			}
			err := (k3s.Supervisor{Resolver: k3s.Resolver{Client: client}}).Monitor(ctx, request, observer)
			if err == nil {
				t.Fatal("invalid supervision succeeded")
			}
			if mode == "pre-canceled" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if (mode == "process-only readiness" || mode == "exec readiness" || mode == "exec healthy") && !errors.Is(err, k3s.ErrUnsupportedPlan) {
				t.Fatalf("unsupported plan error=%v", err)
			}
		})
	}
}

func TestSuperviseDoesNotStartChecksBeforeWatchOpens(t *testing.T) {
	client := observerClient(t, supervisedPod(), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: metav1.StatusReasonForbidden, Code: 403})
	})
	supervisor := k3s.Supervisor{Resolver: k3s.Resolver{Client: client}, Network: probe.Network{DialContext: func(context.Context, string, string) (net.Conn, error) {
		t.Error("check ran without observation")
		return nil, errors.New("unexpected dial")
	}}}
	err := supervisor.Monitor(context.Background(), k3s.SupervisionRequest{Instance: instance(), Plan: supervisionPlan(8080)}, func(probe.Readiness) { t.Error("readiness published without observation") })
	if !errors.Is(err, k3s.ErrObservationLost) || !apierrors.IsForbidden(err) {
		t.Fatalf("unestablished watch error=%v", err)
	}
}
