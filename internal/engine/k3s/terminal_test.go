package k3s_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/engine/k3s"
	"github.com/daropotter/chmura/internal/manifest"
	"github.com/daropotter/chmura/internal/probe"
	"github.com/daropotter/chmura/internal/state"
	"github.com/daropotter/chmura/internal/state/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func terminalInput(t *testing.T, ready bool) (*sqlite.Store, string, k3s.ProcessTerminalRequest) {
	t.Helper()
	started := time.Now().Add(-time.Hour).Truncate(time.Second)
	store, path, resume := retainedProcess(t, started, ready)
	process, err := store.GetProcess(context.Background(), resume.Scope, resume.SlotID, resume.GenerationID)
	if err != nil {
		t.Fatal(err)
	}
	return store, path, k3s.ProcessTerminalRequest{Process: process, Instance: resume.Instance, Plan: resume.Plan, IdempotencyKey: "terminal-decision",
		ObservedAt: started.Add(time.Minute), Cause: &k3s.InstanceExitError{Instance: resume.Instance, ResourceVersion: "43", ExitCode: 0, Reason: "Completed", StartedAt: started, FinishedAt: started.Add(time.Second)}}
}

func unhealthyInput(t *testing.T) (*sqlite.Store, string, k3s.ProcessTerminalRequest) {
	t.Helper()
	store, path, request := terminalInput(t, true)
	pod := supervisedPod()
	pod.Status.ContainerStatuses[1].State.Running.StartedAt = metav1.NewTime(request.Process.StartedAt)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer app.Close()
	request.Plan = supervisionPlan(app.Listener.Addr().(*net.TCPAddr).Port)
	request.Plan.Health.Healthy = &manifest.HealthyRule{UnhealthyFailures: 1, Check: &manifest.Probe{HTTP: &manifest.HTTPCheck{Path: "/healthy?private-config=do-not-retain", Port: "web"}}}
	client := observerClient(t, pod, func(w http.ResponseWriter, r *http.Request) { openWatch(w); <-r.Context().Done() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request.Cause = (k3s.Supervisor{Resolver: k3s.Resolver{Client: client}}).Resume(ctx, store, k3s.ResumeRequest{Scope: request.Process.Scope, SlotID: request.Process.SlotID,
		GenerationID: request.Process.GenerationID, Instance: request.Instance, Plan: request.Plan}, func(probe.Readiness) {})
	if !errors.Is(request.Cause, probe.ErrUnhealthy) {
		t.Fatalf("expected real supervisor unhealthy decision: %v", request.Cause)
	}
	request.ObservedAt = time.Now()
	return store, path, request
}

func TestPrepareProcessTerminalExitPersistsAndReplaysAfterReopen(t *testing.T) {
	for _, code := range []int32{0, 17} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			store, path, request := terminalInput(t, false)
			exit := request.Cause.(*k3s.InstanceExitError)
			exit.ExitCode = code
			request.Cause = fmt.Errorf("supervisor outcome: %w", exit)
			observation, err := k3s.PrepareProcessTerminal(request)
			if err != nil || observation.Terminal.Cause != state.ProcessExited || !observation.Terminal.OccurredAt.Equal(exit.FinishedAt) || !observation.StartedAt.Equal(request.Process.StartedAt) {
				t.Fatalf("prepare exit: %#v %v", observation, err)
			}
			var facts struct {
				Version int    `json:"version"`
				Engine  string `json:"engine"`
				Exit    struct {
					Code            int32     `json:"exit-code"`
					Reason          string    `json:"reason"`
					ResourceVersion string    `json:"resource-version"`
					StartedAt       time.Time `json:"started-at"`
					FinishedAt      time.Time `json:"finished-at"`
				} `json:"runtime-exit"`
			}
			if err := json.Unmarshal(observation.Terminal.Evidence, &facts); err != nil || facts.Version != 1 || facts.Engine != "k3s" || facts.Exit.Code != code || facts.Exit.Reason != exit.Reason || facts.Exit.ResourceVersion != exit.ResourceVersion || !facts.Exit.StartedAt.Equal(exit.StartedAt) || !facts.Exit.FinishedAt.Equal(exit.FinishedAt) {
				t.Fatalf("exit evidence lost facts: %s %v", observation.Terminal.Evidence, err)
			}
			originalEvidence := bytes.Clone(observation.Terminal.Evidence)
			request.Process.Identity[0] = '!'
			exit.Reason = "mutated caller data"
			if !bytes.Equal(observation.Terminal.Evidence, originalEvidence) || bytes.Equal(observation.Identity, request.Process.Identity) {
				t.Fatal("prepared observation aliases caller data")
			}
			if got, recorded, err := store.RecordProcessTerminal(context.Background(), observation); err != nil || !recorded || got.Terminal == nil {
				t.Fatalf("persist prepared exit: %#v %v %v", got, recorded, err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = sqlite.Open(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			retained, err := store.GetProcess(context.Background(), observation.Scope, observation.SlotID, observation.GenerationID)
			if err != nil || retained.Terminal == nil || !bytes.Equal(retained.Terminal.Evidence, originalEvidence) {
				t.Fatalf("reopen lost terminal facts: %#v %v", retained, err)
			}
			request.Process = retained
			exit.Reason = "Completed"
			request.ObservedAt = request.ObservedAt.Add(time.Hour)
			exit.StartedAt, exit.FinishedAt = exit.StartedAt.In(time.FixedZone("offset", 3600)), exit.FinishedAt.In(time.FixedZone("offset", 3600))
			retry, err := k3s.PrepareProcessTerminal(request)
			if err != nil || !bytes.Equal(retry.Terminal.Evidence, originalEvidence) || !retry.Terminal.OccurredAt.Equal(observation.Terminal.OccurredAt) {
				t.Fatalf("re-preparation changed runtime evidence: %#v %v", retry, err)
			}
			if _, recorded, err := store.RecordProcessTerminal(context.Background(), retry); err != nil || recorded {
				t.Fatalf("reopened retry changed receipt: %v %v", recorded, err)
			}
		})
	}
}

func TestPrepareProcessTerminalUsesStableStartupDeadlineAndEffectiveDefaults(t *testing.T) {
	store, _, request := terminalInput(t, false)
	request.Cause = probe.ErrStartupTimeout
	request.Plan.Health.StartupTimeout = 0
	request.ObservedAt = request.Process.StartedAt.Add(3 * time.Minute)
	prepared, err := k3s.PrepareProcessTerminal(request)
	if err != nil || prepared.Terminal.Cause != state.ProcessStartupTimeout || !prepared.Terminal.OccurredAt.Equal(request.Process.StartedAt.Add(2*time.Minute)) {
		t.Fatalf("startup deadline renewed: %#v %v", prepared, err)
	}
	// Frozen v1 fingerprint input protects receipt compatibility without retaining
	// a raw probe path in the stored evidence.
	const frozenCheck = `{"expect-status":[200],"interval-ns":1000000,"kind":"http","path":"/ready","port-name":"web","port-number":8080,"protocol":"http","timeout-ns":2000000000}`
	var facts struct {
		Startup struct {
			TimeoutNS int64              `json:"timeout-ns"`
			Successes manifest.Threshold `json:"ready-successes"`
			Digest    string             `json:"check-sha256"`
		} `json:"startup-timeout"`
	}
	if err := json.Unmarshal(prepared.Terminal.Evidence, &facts); err != nil || facts.Startup.TimeoutNS != int64(2*time.Minute) || facts.Startup.Successes != 1 || facts.Startup.Digest != fmt.Sprintf("%x", sha256.Sum256([]byte(frozenCheck))) {
		t.Fatalf("startup v1 evidence changed: %s %v", prepared.Terminal.Evidence, err)
	}
	if _, _, err := store.RecordProcessTerminal(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	request.Plan.Health = *request.Plan.Health.Effective()
	request.ObservedAt = request.ObservedAt.Add(time.Hour)
	retry, err := k3s.PrepareProcessTerminal(request)
	if err != nil || !bytes.Equal(retry.Terminal.Evidence, prepared.Terminal.Evidence) || !retry.Terminal.OccurredAt.Equal(prepared.Terminal.OccurredAt) {
		t.Fatalf("equivalent startup retry changed facts: %#v %v", retry, err)
	}
	if _, recorded, err := store.RecordProcessTerminal(context.Background(), retry); err != nil || recorded {
		t.Fatalf("startup retry: %v %v", recorded, err)
	}
}

func TestPrepareProcessTerminalSupportsTCPStartupEvidence(t *testing.T) {
	_, _, request := terminalInput(t, false)
	request.Cause = probe.ErrStartupTimeout
	request.Plan.Health.Check = &manifest.Probe{TCP: &manifest.TCPCheck{Port: "web"}}
	got, err := k3s.PrepareProcessTerminal(request)
	if err != nil || got.Terminal.Cause != state.ProcessStartupTimeout || !json.Valid(got.Terminal.Evidence) {
		t.Fatalf("TCP startup evidence: %#v %v", got, err)
	}
}

func TestPrepareProcessTerminalRetainsIndependentHealthyDecision(t *testing.T) {
	store, path, request := unhealthyInput(t)
	prepared, err := k3s.PrepareProcessTerminal(request)
	if err != nil || prepared.Terminal.Cause != state.ProcessUnhealthy || !prepared.Terminal.OccurredAt.Equal(request.ObservedAt) {
		t.Fatalf("healthy decision: %#v %v", prepared, err)
	}
	var facts struct {
		Healthy struct {
			Failures manifest.Threshold `json:"unhealthy-failures"`
			Digest   string             `json:"check-sha256"`
		} `json:"unhealthy"`
	}
	if err := json.Unmarshal(prepared.Terminal.Evidence, &facts); err != nil || facts.Healthy.Failures != 1 || len(facts.Healthy.Digest) != 64 || strings.Contains(string(prepared.Terminal.Evidence), "private-config") {
		t.Fatalf("healthy evidence lost threshold or retained raw check: %s %v", prepared.Terminal.Evidence, err)
	}
	if _, _, err := store.RecordProcessTerminal(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, recorded, err := store.RecordProcessTerminal(context.Background(), prepared); err != nil || recorded {
		t.Fatalf("healthy decision replay: %v %v", recorded, err)
	}
	changed := request
	changed.ObservedAt = changed.ObservedAt.Add(time.Nanosecond)
	another, err := k3s.PrepareProcessTerminal(changed)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.RecordProcessTerminal(context.Background(), another); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("same key changed healthy observation time: %v", err)
	}
	changed = request
	changed.Process.StartedAt = changed.Process.StartedAt.Add(time.Nanosecond)
	if _, err := k3s.PrepareProcessTerminal(changed); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("healthy decision transferred to another process start: %v", err)
	}
	changed = request
	changed.Process.EverReady = false
	if _, err := k3s.PrepareProcessTerminal(changed); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("healthy decision accepted before acknowledged first readiness: %v", err)
	}
	request.Plan.Health.Healthy.Check.HTTP.Path = "/another"
	if _, err := k3s.PrepareProcessTerminal(request); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("mutable plan replaced decision snapshot: %v", err)
	}
}

func TestPrepareProcessTerminalRejectsUncertainAndMalformedOutcomes(t *testing.T) {
	for _, mode := range []string{"nil", "nil decision", "zero decision", "nil exit", "canceled", "deadline", "watch loss", "unavailable", "replaced", "unhealthy sentinel", "bare exit sentinel", "other error", "ambiguous causes", "exit plus cancellation", "exit plus watch loss", "missing version", "missing finish", "reversed exit times", "unknown termination", "invalid exit reason", "changed exit instance", "changed exit start", "startup before deadline", "startup after readiness", "startup without check", "negative startup timeout", "invalid probe port", "blank scope", "blank slot", "blank generation", "blank key", "wrong identity", "zero start", "invalid start", "zero observed time", "invalid observed time"} {
		t.Run(mode, func(t *testing.T) {
			_, _, request := terminalInput(t, false)
			exit := request.Cause.(*k3s.InstanceExitError)
			want := k3s.ErrNoTerminalEvidence
			switch mode {
			case "nil":
				request.Cause = nil
			case "nil decision":
				var decision *k3s.RestartNeededError
				request.Cause = decision
			case "zero decision":
				request.Cause = &k3s.RestartNeededError{}
			case "nil exit":
				var terminated *k3s.InstanceExitError
				request.Cause = terminated
			case "canceled":
				request.Cause = context.Canceled
			case "deadline":
				request.Cause = context.DeadlineExceeded
			case "watch loss":
				request.Cause = k3s.ErrObservationLost
			case "unavailable":
				request.Cause = k3s.ErrInstanceUnavailable
			case "replaced":
				request.Cause = k3s.ErrStaleInstance
			case "unhealthy sentinel":
				request.Cause = probe.ErrUnhealthy
			case "bare exit sentinel":
				request.Cause = k3s.ErrInstanceExited
			case "other error":
				request.Cause = errors.New("private diagnostic: do-not-retain")
			case "ambiguous causes":
				request.Cause = errors.Join(probe.ErrUnhealthy, probe.ErrStartupTimeout)
			case "exit plus cancellation":
				request.Cause = errors.Join(exit, context.Canceled)
			case "exit plus watch loss":
				request.Cause = errors.Join(exit, k3s.ErrObservationLost)
			case "missing version":
				exit.ResourceVersion = ""
			case "missing finish":
				exit.FinishedAt = time.Time{}
			case "reversed exit times":
				exit.FinishedAt = exit.StartedAt.Add(-time.Second)
			case "unknown termination":
				exit.Reason = "ContainerStatusUnknown"
			case "invalid exit reason":
				exit.Reason = string([]byte{0xff})
			case "changed exit instance":
				exit.Instance.ContainerID = "another"
				want = state.ErrConflict
			case "changed exit start":
				exit.StartedAt = exit.StartedAt.Add(time.Nanosecond)
				want = state.ErrConflict
			case "startup before deadline":
				request.Cause = probe.ErrStartupTimeout
				request.ObservedAt = request.Process.StartedAt
			case "startup after readiness":
				request.Cause = probe.ErrStartupTimeout
				request.Process.EverReady = true
				want = state.ErrConflict
			case "startup without check":
				request.Cause = probe.ErrStartupTimeout
				request.Plan.Health.Check = nil
				want = k3s.ErrUnsupportedPlan
			case "negative startup timeout":
				request.Cause = probe.ErrStartupTimeout
				request.Plan.Health.StartupTimeout = -1
				want = k3s.ErrUnsupportedPlan
			case "invalid probe port":
				request.Cause = probe.ErrStartupTimeout
				request.Plan.Ports["web"] = manifest.Port{Number: 0, Protocol: "http"}
				want = k3s.ErrUnsupportedPlan
			case "blank scope":
				request.Process.Scope = " "
				want = nil
			case "blank slot":
				request.Process.SlotID = ""
				want = nil
			case "blank generation":
				request.Process.GenerationID = ""
				want = nil
			case "blank key":
				request.IdempotencyKey = " "
				want = nil
			case "wrong identity":
				request.Process.Identity = []byte("wrong")
				want = state.ErrConflict
			case "zero start":
				request.Process.StartedAt = time.Time{}
				want = nil
			case "invalid start":
				request.Process.StartedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
				want = nil
			case "zero observed time":
				request.Cause = probe.ErrStartupTimeout
				request.ObservedAt = time.Time{}
				want = nil
			case "invalid observed time":
				request.Cause = probe.ErrStartupTimeout
				request.ObservedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
				want = nil
			}
			got, err := k3s.PrepareProcessTerminal(request)
			if err == nil || (want != nil && !errors.Is(err, want)) || got.GenerationID != "" || len(got.Terminal.Evidence) != 0 {
				t.Fatalf("uncertain/invalid outcome became terminal: %#v %v, want %v", got, err, want)
			}
		})
	}
}

func TestPrepareProcessTerminalCanonicalExitEncoding(t *testing.T) {
	_, _, request := terminalInput(t, false)
	request.Process.StartedAt = time.Date(2026, 11, 17, 11, 0, 0, 123, time.FixedZone("offset", 3600))
	exit := request.Cause.(*k3s.InstanceExitError)
	exit.StartedAt, exit.FinishedAt = request.Process.StartedAt, request.Process.StartedAt.Add(time.Second)
	got, err := k3s.PrepareProcessTerminal(request)
	const frozen = `{"version":1,"engine":"k3s","runtime-exit":{"resource-version":"43","exit-code":0,"reason":"Completed","started-at":"2026-11-17T10:00:00.000000123Z","finished-at":"2026-11-17T10:00:01.000000123Z"}}`
	if err != nil || string(got.Terminal.Evidence) != frozen {
		t.Fatalf("canonical v1 encoding changed: %s %v", got.Terminal.Evidence, err)
	}
}
