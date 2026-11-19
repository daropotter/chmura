package k3s_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/engine/k3s"
	"github.com/daropotter/chmura/internal/manifest"
	"github.com/daropotter/chmura/internal/probe"
	"github.com/daropotter/chmura/internal/state"
	"github.com/daropotter/chmura/internal/state/sqlite"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func copyProcessRestart(t *testing.T, request k3s.ProcessRestartRequest) k3s.ProcessRestartRequest {
	t.Helper()
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var copied k3s.ProcessRestartRequest
	if err := json.Unmarshal(data, &copied); err != nil {
		t.Fatal(err)
	}
	return copied
}

func processRestartInput(t *testing.T, mode string) k3s.ProcessRestartRequest {
	t.Helper()
	previous := creationRequest(t)
	if mode == "unhealthy" {
		app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
		t.Cleanup(app.Close)
		previous.Plan.Ports["web"] = manifest.Port{Number: app.Listener.Addr().(*net.TCPAddr).Port, Protocol: "http"}
		previous.Plan.Health.Check = &manifest.Probe{HTTP: &manifest.HTTPCheck{Path: "/ready", Port: "web"}}
		previous.Plan.Health.Healthy = &manifest.HealthyRule{UnhealthyFailures: 1, Check: &manifest.Probe{HTTP: &manifest.HTTPCheck{Path: "/independent-health", Port: "web"}}}
	}
	pod, err := k3s.PrepareInstance(previous)
	if err != nil {
		t.Fatal(err)
	}
	ref := k3s.Instance{Namespace: previous.Namespace, PodName: pod.Name, PodUID: "original-uid", ContainerName: "app", ContainerID: "containerd://original"}
	identity, err := k3s.EncodeProcessIdentity(ref)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().Add(-time.Hour).Truncate(time.Second)
	p := state.Process{Scope: "project-a", SlotID: "api/0", GenerationID: "generation-a", Identity: identity, StartedAt: started, EverReady: mode == "unhealthy"}
	var cause error = &k3s.InstanceExitError{Instance: ref, ResourceVersion: "exit-snapshot", ExitCode: 0, Reason: "Completed", StartedAt: started, FinishedAt: started.Add(time.Second)}
	if mode == "exit nonzero" {
		cause.(*k3s.InstanceExitError).ExitCode = 17
	}
	if mode == "startup" {
		cause = probe.ErrStartupTimeout
	}
	if mode == "unhealthy" {
		pod.UID, pod.ResourceVersion = "original-uid", "runtime-snapshot"
		pod.Status.Phase, pod.Status.PodIP = corev1.PodRunning, "127.0.0.1"
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", ContainerID: ref.ContainerID, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(started)}}}}
		client := apiClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("supervision mutated runtime: %s", r.Method)
			}
			if strings.HasSuffix(r.URL.Path, "/pods") {
				openWatch(w)
				<-r.Context().Done()
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(pod)
		})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cause = (k3s.Supervisor{Resolver: k3s.Resolver{Client: client}}).Monitor(ctx, k3s.SupervisionRequest{Instance: ref, Plan: previous.Plan, EverReady: true}, func(probe.Readiness) {})
		if !errors.Is(cause, probe.ErrUnhealthy) {
			t.Fatalf("missing actual healthy outcome: %v", cause)
		}
	}
	terminal, err := k3s.PrepareProcessTerminal(k3s.ProcessTerminalRequest{Process: p, Instance: ref, Plan: previous.Plan, Cause: cause, IdempotencyKey: "terminal", ObservedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	p.Terminal = &terminal.Terminal
	next := previous
	next.IdempotencyKey, next.NodeName, next.CPUMilli = "next-generation", "node-b", previous.CPUMilli+100
	return copyProcessRestart(t, k3s.ProcessRestartRequest{Process: p, Instance: ref, Previous: previous, Next: next})
}

func TestPrepareProcessRestartRecoversAfterReopenAndFeedsExecutor(t *testing.T) {
	for _, mode := range []string{"exit zero", "exit nonzero", "unhealthy"} {
		t.Run(mode, func(t *testing.T) {
			request := processRestartInput(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			path := filepath.Join(t.TempDir(), "state.db")
			store, err := sqlite.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if store != nil {
					_ = store.Close()
				}
			})
			p := request.Process
			admitResumeGeneration(t, store, k3s.ResumeRequest{Scope: p.Scope, SlotID: p.SlotID, GenerationID: p.GenerationID}, 0)
			if _, _, err := store.RecordProcess(ctx, state.ProcessObservation{Scope: p.Scope, SlotID: p.SlotID, GenerationID: p.GenerationID, IdempotencyKey: "binding", Identity: p.Identity, StartedAt: p.StartedAt, EverReady: p.EverReady}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.RecordProcessTerminal(ctx, state.ProcessTerminalObservation{Scope: p.Scope, SlotID: p.SlotID, GenerationID: p.GenerationID, IdempotencyKey: "terminal", Identity: p.Identity, StartedAt: p.StartedAt, Terminal: *p.Terminal}); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = sqlite.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			request.Process, err = store.GetProcess(ctx, p.Scope, p.SlotID, p.GenerationID)
			if err != nil {
				t.Fatal(err)
			}
			s := &lifetimeStore{}
			client, _, _ := lifetimeClient(t, s)
			old, err := k3s.PrepareInstance(request.Previous)
			if err != nil {
				t.Fatal(err)
			}
			old.UID, old.ResourceVersion = "original-uid", "runtime-snapshot"
			old.Status.Phase = corev1.PodRunning
			old.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", ContainerID: request.Instance.ContainerID, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(p.StartedAt)}}}}
			s.mu.Lock()
			s.pods = map[string]*corev1.Pod{old.Name: old}
			s.mu.Unlock()
			prepared, err := k3s.PrepareProcessRestart(request)
			if err != nil || prepared.Instance != request.Instance {
				t.Fatalf("retained terminal did not prepare executor request: %#v %v", prepared, err)
			}
			s.mu.Lock()
			requests := s.requests
			s.mu.Unlock()
			if requests != 0 {
				t.Fatal("preparation contacted runtime")
			}
			restarter := k3s.Restarter{Client: client}
			pending, err := restarter.Ensure(ctx, prepared) // The test owner explicitly authorizes execution.
			s.mu.Lock()
			creates := s.creates
			s.mu.Unlock()
			if err != nil || !pending.Retiring || creates != 0 {
				t.Fatalf("restart skipped confirmed absence: %#v %v", pending, err)
			}
			s.mu.Lock()
			delete(s.pods, old.Name) // Simulate runtime confirmation of graceful retirement.
			s.dropCreateReply = true
			s.mu.Unlock()
			_, _ = restarter.Ensure(ctx, prepared)
			created, err := restarter.Ensure(ctx, prepared)
			if err != nil || created.Retiring || created.Instance.Created || created.Instance.Identity.PodUID == "" || created.Instance.Identity.PodName == old.Name {
				t.Fatalf("restart replay failed: %#v %v", created, err)
			}
			pod, _, _ := s.snapshot(created.Instance.Identity.PodName)
			s.mu.Lock()
			creates = s.creates
			s.mu.Unlock()
			if creates != 1 || pod.Spec.NodeName != request.Next.NodeName || pod.Spec.Containers[0].Resources.Requests.Cpu().MilliValue() != request.Next.CPUMilli || len(pod.Status.Conditions) != 0 {
				t.Fatal("prepared restart duplicated a successor or changed admission/readiness")
			}
		})
	}
}

func TestPrepareProcessRestartRejectsMissingUnknownAndMalformedEvidence(t *testing.T) {
	base := processRestartInput(t, "exit zero")
	for _, mode := range []string{"no terminal", "startup", "unknown cause", "no evidence", "broken JSON", "null", "future version", "foreign engine", "unknown field", "duplicate key", "whitespace", "trailing data", "missing scope", "missing slot", "missing generation", "identity mismatch", "start mismatch", "occurrence mismatch", "missing exit version", "unknown exit reason"} {
		t.Run(mode, func(t *testing.T) {
			request := copyProcessRestart(t, base)
			terminal := request.Process.Terminal
			evidence := string(terminal.Evidence)
			switch mode {
			case "no terminal":
				request.Process.Terminal = nil
			case "startup":
				request = processRestartInput(t, "startup")
			case "unknown cause":
				terminal.Cause = "watch-lost"
			case "no evidence":
				terminal.Evidence = nil
			case "broken JSON":
				terminal.Evidence = []byte("{")
			case "null":
				terminal.Evidence = []byte("null")
			case "future version":
				terminal.Evidence = []byte(strings.Replace(evidence, `"version":1`, `"version":2`, 1))
			case "foreign engine":
				terminal.Evidence = []byte(strings.Replace(evidence, `"engine":"k3s"`, `"engine":"other"`, 1))
			case "unknown field":
				terminal.Evidence = []byte(strings.Replace(evidence, `"version":1`, `"version":1,"extra":true`, 1))
			case "duplicate key":
				terminal.Evidence = []byte(strings.Replace(evidence, `"version":1`, `"version":2,"version":1`, 1))
			case "whitespace":
				terminal.Evidence = append([]byte(" "), terminal.Evidence...)
			case "trailing data":
				terminal.Evidence = append(terminal.Evidence, []byte("{}")...)
			case "missing scope":
				request.Process.Scope = ""
			case "missing slot":
				request.Process.SlotID = ""
			case "missing generation":
				request.Process.GenerationID = ""
			case "identity mismatch":
				request.Instance.ContainerID = "other"
			case "start mismatch":
				request.Process.StartedAt = request.Process.StartedAt.Add(time.Second)
			case "occurrence mismatch":
				terminal.OccurredAt = terminal.OccurredAt.Add(time.Second)
			case "missing exit version":
				terminal.Evidence = []byte(strings.Replace(evidence, `"resource-version":"exit-snapshot"`, `"resource-version":""`, 1))
			case "unknown exit reason":
				terminal.Evidence = []byte(strings.Replace(evidence, `"reason":"Completed"`, `"reason":"ContainerStatusUnknown"`, 1))
			}
			result, err := k3s.PrepareProcessRestart(request)
			if err == nil || !reflect.DeepEqual(result, k3s.RestartRequest{}) {
				t.Fatalf("unsafe evidence supplied restart: mode=%s result=%#v error=%v", mode, result, err)
			}
			if (mode == "no terminal" || mode == "startup" || mode == "future version") && !errors.Is(err, k3s.ErrNoRestartEvidence) {
				t.Fatalf("missing restart classification: %v", err)
			}
		})
	}
}

func TestPrepareProcessRestartBindsIndependentHealthyRule(t *testing.T) {
	base := processRestartInput(t, "unhealthy")
	for _, mode := range []string{"not ever ready", "healthy removed", "process-only healthy", "changed threshold", "changed path", "changed interval", "changed port", "readiness substituted", "changed digest"} {
		t.Run(mode, func(t *testing.T) {
			request := copyProcessRestart(t, base)
			if mode == "not ever ready" {
				request.Process.EverReady = false
			}
			for _, current := range []*k3s.InstanceRequest{&request.Previous, &request.Next} {
				switch mode {
				case "healthy removed":
					current.Plan.Health.Healthy = nil
				case "process-only healthy":
					current.Plan.Health.Healthy.Check = nil
				case "changed threshold":
					current.Plan.Health.Healthy.UnhealthyFailures++
				case "changed path":
					current.Plan.Health.Healthy.Check.HTTP.Path += "/other"
				case "changed interval":
					current.Plan.Health.Healthy.Check.Interval = manifest.Duration(7 * time.Second)
				case "changed port":
					port := current.Plan.Ports["web"]
					port.Number++
					current.Plan.Ports["web"] = port
				case "readiness substituted":
					current.Plan.Health.Healthy.Check = current.Plan.Health.Check
				}
			}
			if mode == "changed digest" {
				var fields struct {
					Unhealthy struct {
						Digest string `json:"check-sha256"`
					} `json:"unhealthy"`
				}
				if err := json.Unmarshal(request.Process.Terminal.Evidence, &fields); err != nil {
					t.Fatal(err)
				}
				request.Process.Terminal.Evidence = []byte(strings.Replace(string(request.Process.Terminal.Evidence), fields.Unhealthy.Digest, strings.Repeat("0", 64), 1))
			}
			if result, err := k3s.PrepareProcessRestart(request); err == nil || !reflect.DeepEqual(result, k3s.RestartRequest{}) {
				t.Fatalf("healthy evidence transferred to changed rule: %#v %v", result, err)
			}
		})
	}
}

func TestPrepareProcessRestartValidatesExplicitSuccessor(t *testing.T) {
	base := processRestartInput(t, "exit zero")
	for _, mode := range []string{"same key", "foreign namespace", "changed image", "changed declaration", "no allocation", "wrong original key"} {
		t.Run(mode, func(t *testing.T) {
			request := copyProcessRestart(t, base)
			switch mode {
			case "same key":
				request.Next.IdempotencyKey = request.Previous.IdempotencyKey
			case "foreign namespace":
				request.Next.Namespace = "other"
			case "changed image":
				request.Next.ResolvedImage = "docker.io/library/nginx@sha256:" + strings.Repeat("b", 64)
			case "changed declaration":
				request.Next.Plan.Health.Ready.Successes++
			case "no allocation":
				request.Next.CPUMilli = 0
			case "wrong original key":
				request.Previous.IdempotencyKey = "other"
			}
			if result, err := k3s.PrepareProcessRestart(request); err == nil || !reflect.DeepEqual(result, k3s.RestartRequest{}) {
				t.Fatalf("invalid successor supplied restart: %#v %v", result, err)
			}
		})
	}
}

func TestPrepareProcessRestartKeepsStableIndependentRequests(t *testing.T) {
	for _, mode := range []string{"exit zero", "unhealthy"} {
		t.Run(mode, func(t *testing.T) {
			request := processRestartInput(t, mode)
			request.Next.Plan = request.Previous.Plan // Exercise aliased caller configuration.
			original := copyProcessRestart(t, request)
			prepared, err := k3s.PrepareProcessRestart(request)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(request)
			request.Process.StartedAt = request.Process.StartedAt.In(time.FixedZone("equivalent", 7200))
			request.Process.Terminal.OccurredAt = request.Process.Terminal.OccurredAt.In(time.FixedZone("equivalent", 7200))
			for _, current := range []*k3s.InstanceRequest{&request.Previous, &request.Next} {
				current.Plan.Health = *current.Plan.Health.Effective()
				current.Plan.Health.StartupTimeout = 0
				current.Plan.Health.Ready = manifest.ReadyRule{}
				current.Plan.Health.Check.Interval, current.Plan.Health.Check.Timeout = 0, 0
				current.Plan.Health.Check.HTTP.ExpectStatus = nil
				if healthy := current.Plan.Health.Healthy; healthy != nil && healthy.Check != nil {
					healthy.Check.Interval, healthy.Check.Timeout = 0, 0
					if healthy.Check.HTTP != nil {
						healthy.Check.HTTP.ExpectStatus = nil
					}
				}
			}
			equivalent, err := k3s.PrepareProcessRestart(request)
			if err != nil || !reflect.DeepEqual(prepared, equivalent) {
				t.Fatalf("time zones/defaults changed prepared request: %v", err)
			}
			unchanged, _ := json.Marshal(original)
			if !bytes.Equal(before, unchanged) {
				t.Fatal("preparation mutated caller configuration")
			}
			request.Previous.Plan.Health.Check.HTTP.Path = "/changed-after-preparation"
			request.Previous.Plan.Ports["web"] = manifest.Port{Number: 1, Protocol: "tcp"}
			old, _ := k3s.PrepareInstance(prepared.Previous)
			want, _ := k3s.PrepareInstance(original.Previous)
			if !reflect.DeepEqual(old, want) {
				t.Fatal("prepared request aliases caller configuration")
			}
			prepared.Next.Plan.Health.Check.HTTP.ExpectStatus[0] = 418
			if prepared.Previous.Plan.Health.Check.HTTP.ExpectStatus[0] == 418 {
				t.Fatal("successor mutation changed the prepared predecessor")
			}
		})
	}
}
