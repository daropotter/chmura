package k3s_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/engine/k3s"
	"github.com/daropotter/chmura/internal/manifest"
	"github.com/daropotter/chmura/internal/state"
	"github.com/daropotter/chmura/internal/state/sqlite"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
)

func restartDriverFixture(t *testing.T, mode string, alter func([]byte) []byte) (string, *sqlite.Store, *lifetimeStore, *coreclient.CoreV1Client, k3s.RestartSelection) {
	t.Helper()
	ctx := context.Background()
	input := processRestartInput(t, mode)
	intent, err := k3s.PrepareRestartOperationIntent(input, "generation-b", 1)
	if err != nil {
		t.Fatal(err)
	}
	if alter != nil {
		intent = alter(intent)
	}
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	p := input.Process
	admitResumeGeneration(t, store, k3s.ResumeRequest{Scope: p.Scope, SlotID: p.SlotID, GenerationID: p.GenerationID}, 0)
	if _, _, err := store.RecordProcess(ctx, state.ProcessObservation{Scope: p.Scope, SlotID: p.SlotID, GenerationID: p.GenerationID, Identity: p.Identity, StartedAt: p.StartedAt, EverReady: p.EverReady, IdempotencyKey: "binding"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.RecordProcessTerminal(ctx, state.ProcessTerminalObservation{Scope: p.Scope, SlotID: p.SlotID, GenerationID: p.GenerationID, Identity: p.Identity, StartedAt: p.StartedAt, Terminal: *p.Terminal, IdempotencyKey: "terminal"}); err != nil {
		t.Fatal(err)
	}
	op, _, err := store.Accept(ctx, state.Submission{Scope: p.Scope, Kind: state.RestartOperationKind, IdempotencyKey: "restart", Intent: intent})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Update(ctx, state.OperationUpdate{Scope: p.Scope, OperationID: op.ID, ExpectedVersion: op.Version, Status: state.OperationRunning, IdempotencyKey: "start"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AdmitRestart(ctx, state.RestartAdmissionRequest{GenerationRequest: state.GenerationRequest{Scope: p.Scope, SlotID: p.SlotID, ID: "generation-b", OperationID: op.ID, ExpectedVersion: 1, Intent: intent, IdempotencyKey: "admit"}, PreviousGenerationID: p.GenerationID}); err != nil {
		t.Fatal(err)
	}
	runtime := &lifetimeStore{}
	client, _, _ := lifetimeClient(t, runtime)
	old, err := k3s.PrepareInstance(input.Previous)
	if err != nil {
		t.Fatal(err)
	}
	old.UID, old.ResourceVersion = "original-uid", "runtime-snapshot"
	old.Status.Phase = corev1.PodRunning
	old.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", ContainerID: input.Instance.ContainerID, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(p.StartedAt)}}}}
	runtime.mu.Lock()
	runtime.pods = map[string]*corev1.Pod{old.Name: old}
	runtime.mu.Unlock()
	return path, store, runtime, client, k3s.RestartSelection{Scope: p.Scope, SlotID: p.SlotID, GenerationID: "generation-b", OperationID: op.ID}
}

func TestRestartDriverRecoversAndPersistsCompletion(t *testing.T) {
	for _, mode := range []string{"exit zero", "unhealthy"} {
		t.Run(mode, func(t *testing.T) {
			path, store, runtime, client, selection := restartDriverFixture(t, mode, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err := sqlite.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			driver := k3s.Restarter{Client: client}
			first, err := driver.EnsureAdmitted(ctx, store, selection)
			if err != nil || !first.Retiring || first.Identity.PodUID != "" {
				t.Fatalf("durable driver skipped retirement: %#v %v", first, err)
			}
			runtime.mu.Lock()
			for name := range runtime.pods {
				delete(runtime.pods, name)
			}
			runtime.dropCreateReply = true
			runtime.mu.Unlock()
			_, _ = driver.EnsureAdmitted(ctx, store, selection)
			created, err := driver.EnsureAdmitted(ctx, store, selection)
			if err != nil || created.Retiring || created.Identity.PodUID == "" {
				t.Fatalf("durable completion failed: %#v %v", created, err)
			}
			op, err := store.Get(ctx, selection.Scope, selection.OperationID)
			if err != nil || op.Status != state.OperationSucceeded || len(op.Result) == 0 {
				t.Fatalf("completion returned before durable result: %#v %v", op, err)
			}
			if mode == "exit zero" {
				// Durable v1 result bytes are a compatibility contract. The
				// fixture's intent and first acknowledged UID are deterministic.
				const want = `{"version":1,"engine":"k3s","intent-sha256":"5ac5bca1adb369a44a5fed5d23e8a91164ea825a7fbc70d4f78cda791f8e0459","namespace":"project-a","pod-name":"chmura-48c8b629a79316c97d2a1b13c50a651e","pod-uid":"new-uid-1","container-name":"app"}`
				if string(op.Result) != want {
					t.Fatalf("v1 completion encoding changed: %s", op.Result)
				}
			}
			if _, _, err := store.RecordProcess(ctx, state.ProcessObservation{Scope: selection.Scope, SlotID: selection.SlotID,
				GenerationID: selection.GenerationID, Identity: []byte("bound-successor"), StartedAt: time.Now(), IdempotencyKey: "bind-next"}); err != nil {
				t.Fatal(err)
			}
			runtime.mu.Lock()
			delete(runtime.pods, created.Identity.PodName)
			requests, creates := runtime.requests, runtime.creates
			runtime.mu.Unlock()
			admitResumeGeneration(t, store, k3s.ResumeRequest{Scope: selection.Scope, SlotID: selection.SlotID, GenerationID: "generation-c"}, 2)
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = sqlite.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			replayed, err := (k3s.Restarter{}).EnsureAdmitted(ctx, store, selection)
			if err != nil || replayed != created {
				t.Fatalf("historical completion did not replay: %#v %v", replayed, err)
			}
			runtime.mu.Lock()
			defer runtime.mu.Unlock()
			if creates != 1 || runtime.creates != creates || runtime.requests != requests || len(runtime.pods) != 0 {
				t.Fatal("completion replay contacted runtime or recreated a successor")
			}
		})
	}
}

type restartResultStore struct {
	*sqlite.Store
	update func(context.Context, state.OperationUpdate) (state.Operation, bool, error)
}

func (s restartResultStore) Update(ctx context.Context, request state.OperationUpdate) (state.Operation, bool, error) {
	return s.update(ctx, request)
}

func TestRestartDriverDoesNotExposeUnacknowledgedCompletion(t *testing.T) {
	for _, mode := range []string{"before commit", "lost reply", "cancellation"} {
		t.Run(mode, func(t *testing.T) {
			_, store, runtime, client, selection := restartDriverFixture(t, "exit zero", nil)
			runtime.mu.Lock()
			for name := range runtime.pods {
				delete(runtime.pods, name)
			}
			runtime.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			lost := errors.New("result acknowledgement unavailable")
			journal := restartResultStore{Store: store, update: func(ctx context.Context, request state.OperationUpdate) (state.Operation, bool, error) {
				if mode == "before commit" {
					return state.Operation{}, false, lost
				}
				if mode == "cancellation" {
					cancel()
					return state.Operation{}, false, ctx.Err()
				}
				if _, _, err := store.Update(ctx, request); err != nil {
					return state.Operation{}, false, err
				}
				return state.Operation{}, false, lost
			}}
			result, err := (k3s.Restarter{Client: client}).EnsureAdmitted(ctx, journal, selection)
			if err == nil || result != (k3s.RestartProgress{}) {
				t.Fatalf("unacknowledged result exposed: %#v %v", result, err)
			}
			if mode != "cancellation" && !errors.Is(err, lost) {
				t.Fatalf("storage cause lost: %v", err)
			}
			retry, err := (k3s.Restarter{Client: client}).EnsureAdmitted(context.Background(), store, selection)
			if err != nil || retry.Retiring || retry.Identity.PodUID == "" {
				t.Fatalf("result recovery failed: %#v %v", retry, err)
			}
			runtime.mu.Lock()
			defer runtime.mu.Unlock()
			if runtime.creates != 1 {
				t.Fatalf("result recovery duplicated creation: %d", runtime.creates)
			}
		})
	}
}

func TestRestartDriverRejectsIneligibleSelectionWithoutRuntime(t *testing.T) {
	for _, mode := range []string{"canceled", "failed", "superseded", "bound successor", "wrong slot", "wrong generation", "wrong operation", "pre-canceled"} {
		t.Run(mode, func(t *testing.T) {
			_, store, runtime, client, selection := restartDriverFixture(t, "exit zero", nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "canceled", "failed":
				op, err := store.Get(ctx, selection.Scope, selection.OperationID)
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := store.Update(ctx, state.OperationUpdate{Scope: op.Scope, OperationID: op.ID, ExpectedVersion: op.Version, Status: state.OperationStatus(mode), Result: []byte(`{}`), IdempotencyKey: "finish"}); err != nil {
					t.Fatal(err)
				}
			case "superseded":
				admitResumeGeneration(t, store, k3s.ResumeRequest{Scope: selection.Scope, SlotID: selection.SlotID, GenerationID: "generation-c"}, 2)
			case "bound successor":
				if _, _, err := store.RecordProcess(ctx, state.ProcessObservation{Scope: selection.Scope, SlotID: selection.SlotID, GenerationID: selection.GenerationID, Identity: []byte("already-known-process"), StartedAt: time.Now(), IdempotencyKey: "bind-next"}); err != nil {
					t.Fatal(err)
				}
			case "wrong slot":
				selection.SlotID = "other"
			case "wrong generation":
				selection.GenerationID = "other"
			case "wrong operation":
				selection.OperationID = "other"
			case "pre-canceled":
				cancel()
			}
			result, err := (k3s.Restarter{Client: client}).EnsureAdmitted(ctx, store, selection)
			if err == nil || result != (k3s.RestartProgress{}) {
				t.Fatalf("ineligible selection executed: %#v %v", result, err)
			}
			runtime.mu.Lock()
			defer runtime.mu.Unlock()
			if runtime.requests != 0 {
				t.Fatalf("rejected work contacted runtime: %d", runtime.requests)
			}
		})
	}
}

func TestRestartDriverRejectsBadIntentWithoutRuntime(t *testing.T) {
	for _, mode := range []string{"version", "engine", "scope", "slot", "previous", "generation", "base", "extra field", "duplicate field", "trailing document", "noncanonical", "broken"} {
		t.Run(mode, func(t *testing.T) {
			alter := func(intent []byte) []byte {
				s := string(intent)
				switch mode {
				case "version":
					s = strings.Replace(s, "version: 1", "version: 2", 1)
				case "engine":
					s = strings.Replace(s, "engine: k3s", "engine: other", 1)
				case "scope":
					s = strings.Replace(s, "scope: project-a", "scope: other", 1)
				case "slot":
					s = strings.Replace(s, "slot-id: api/0", "slot-id: other", 1)
				case "previous":
					s = strings.Replace(s, "previous-generation-id: generation-a", "previous-generation-id: other", 1)
				case "generation":
					s = strings.Replace(s, "generation-id: generation-b", "generation-id: other", 1)
				case "base":
					s = strings.Replace(s, "previous-version: 1", "previous-version: 2", 1)
				case "extra field":
					s += "unknown: true\n"
				case "duplicate field":
					s += "version: 1\n"
				case "trailing document":
					s += "---\nversion: 1\n"
				case "noncanonical":
					s = "# extra representation\n" + s
				case "broken":
					s = "["
				}
				return []byte(s)
			}
			_, store, runtime, client, selection := restartDriverFixture(t, "exit zero", alter)
			if result, err := (k3s.Restarter{Client: client}).EnsureAdmitted(context.Background(), store, selection); err == nil || result != (k3s.RestartProgress{}) {
				t.Fatalf("bad intent executed: %#v %v", result, err)
			}
			runtime.mu.Lock()
			defer runtime.mu.Unlock()
			if runtime.requests != 0 {
				t.Fatal("bad intent contacted runtime")
			}
		})
	}
}

func TestPrepareRestartOperationIntentUsesEffectiveIndependentSnapshot(t *testing.T) {
	input := processRestartInput(t, "exit zero")
	first, err := k3s.PrepareRestartOperationIntent(input, "generation-b", 1)
	if err != nil {
		t.Fatal(err)
	}
	input.Previous.Plan.Health.Ready = manifest.ReadyRule{}
	input.Next.Plan.Health.Ready = manifest.ReadyRule{}
	input.Previous.Plan.Health.StartupTimeout, input.Next.Plan.Health.StartupTimeout = 0, 0
	second, err := k3s.PrepareRestartOperationIntent(input, "generation-b", 1)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("omitted defaults changed canonical intent: %v", err)
	}
	input.Next.Plan.Ports["new-port"] = manifest.Port{Number: 9000, Protocol: "tcp"}
	if _, err := k3s.PrepareRestartOperationIntent(input, "generation-b", 1); err == nil {
		t.Fatal("restart became a rollout")
	}
	for _, selection := range []struct {
		id      string
		version int64
	}{{"", 1}, {"generation-a", 1}, {"generation-b", 0}, {"generation-b", -1}} {
		if result, err := k3s.PrepareRestartOperationIntent(processRestartInput(t, "exit zero"), selection.id, selection.version); err == nil || len(result) != 0 {
			t.Fatal("invalid restart selection encoded")
		}
	}
}
