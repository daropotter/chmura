package k3s_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/engine/k3s"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

type gateStore struct {
	mu                   sync.Mutex
	pod                  *corev1.Pod
	writes, requests     int
	getStatus, putStatus int
	interfere            func(*corev1.Pod)
	dropReply            bool
}

func (s *gateStore) snapshot() (*corev1.Pod, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pod.DeepCopy(), s.writes, s.requests
}

func gateClient(t *testing.T, s *gateStore) *coreclient.CoreV1Client {
	t.Helper()
	s.pod = observedPod()
	s.pod.Labels = map[string]string{"app.kubernetes.io/managed-by": "chmura", "chmura/instance": s.pod.Name}
	s.pod.Spec.ReadinessGates = []corev1.PodReadinessGate{{ConditionType: k3s.ReadinessGate}}
	s.pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "ReadinessGatesNotReady"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		s.mu.Lock()
		defer s.mu.Unlock()
		s.requests++
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/team-a/pods/app-a":
			if s.getStatus != 0 {
				apiStatus(w, s.getStatus)
				return
			}
			_ = json.NewEncoder(w).Encode(s.pod)
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/namespaces/team-a/pods/app-a/status":
			if s.putStatus != 0 {
				apiStatus(w, s.putStatus)
				return
			}
			var submitted corev1.Pod
			if err := json.NewDecoder(r.Body).Decode(&submitted); err != nil {
				t.Error(err)
				apiStatus(w, 400)
				return
			}
			if s.interfere != nil {
				s.interfere(s.pod)
				s.pod.ResourceVersion = "interfered"
				s.interfere = nil
			}
			if submitted.ResourceVersion != s.pod.ResourceVersion || submitted.UID != s.pod.UID {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: metav1.StatusReasonConflict, Code: 409})
				return
			}
			s.writes++
			s.pod.Status = submitted.Status
			s.pod.ResourceVersion = fmt.Sprintf("written-%d", s.writes)
			if s.dropReply {
				s.dropReply = false
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
				return
			}
			_ = json.NewEncoder(w).Encode(s.pod)
		default:
			t.Errorf("readiness changed the wrong resource: %s %s", r.Method, r.URL.Path)
			apiStatus(w, 404)
		}
	}))
	t.Cleanup(server.Close)
	client, err := coreclient.NewForConfig(&rest.Config{Host: server.URL, QPS: -1, ContentConfig: rest.ContentConfig{ContentType: "application/json"}})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func readinessRequest() k3s.ReadinessUpdate {
	return k3s.ReadinessUpdate{Instance: instance(), ResourceVersion: "42", IdempotencyKey: "readiness-result-a", Ready: false}
}

func gateCondition(t *testing.T, pod *corev1.Pod) corev1.PodCondition {
	t.Helper()
	for _, c := range pod.Status.Conditions {
		if c.Type == k3s.ReadinessGate {
			return c
		}
	}
	t.Fatal("Chmura readiness condition is absent")
	return corev1.PodCondition{}
}

func TestReadinessWriterSetsOnlyItsGateAndReplays(t *testing.T) {
	s := &gateStore{}
	client := gateClient(t, s)
	before, _, _ := s.snapshot()
	request := readinessRequest()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	writer := k3s.ReadinessWriter{Client: client}
	version, err := writer.Apply(ctx, request)
	if err != nil || version == "" || version == request.ResourceVersion {
		t.Fatalf("apply readiness: version=%q error=%v", version, err)
	}
	after, writes, _ := s.snapshot()
	condition := gateCondition(t, after)
	if condition.Status != corev1.ConditionFalse || condition.LastTransitionTime.IsZero() || writes != 1 {
		t.Fatalf("initial readiness condition=%#v writes=%d", condition, writes)
	}
	if !reflect.DeepEqual(before.Spec, after.Spec) || !reflect.DeepEqual(before.Status.ContainerStatuses, after.Status.ContainerStatuses) || before.Status.Conditions[0] != after.Status.Conditions[0] {
		t.Fatal("writer changed native readiness or workload configuration")
	}
	payload, _ := json.Marshal(after)
	if strings.Contains(string(payload), request.IdempotencyKey) {
		t.Fatal("readiness leaked the raw request key")
	}
	// A kubelet update after acceptance must not break lost-response recovery.
	s.mu.Lock()
	s.pod.ResourceVersion = "kubelet-update"
	s.mu.Unlock()
	version, err = writer.Apply(ctx, request)
	_, writes, _ = s.snapshot()
	if err != nil || version != "kubelet-update" || writes != 1 {
		t.Fatalf("replay: version=%q writes=%d error=%v", version, writes, err)
	}
	request.IdempotencyKey, request.ResourceVersion, request.Ready = "readiness-result-b", version, true
	version, err = writer.Apply(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	after, writes, _ = s.snapshot()
	readyCondition := gateCondition(t, after)
	if readyCondition.Status != corev1.ConditionTrue || writes != 2 {
		t.Fatalf("readiness not acquired: %#v writes=%d", readyCondition, writes)
	}
	// Another successful result changes its receipt but not the transition time.
	request.IdempotencyKey, request.ResourceVersion = "readiness-result-c", version
	if _, err := writer.Apply(ctx, request); err != nil {
		t.Fatal(err)
	}
	after, _, _ = s.snapshot()
	if !gateCondition(t, after).LastTransitionTime.Time.Equal(readyCondition.LastTransitionTime.Time) {
		t.Fatal("unchanged readiness reset transition time")
	}
	// A delayed earlier result cannot revoke a newer readiness decision.
	if _, err := writer.Apply(ctx, readinessRequest()); !errors.Is(err, k3s.ErrReadinessConflict) {
		t.Fatalf("stale replay changed readiness: %v", err)
	}
	after, writes, _ = s.snapshot()
	if gateCondition(t, after).Status != corev1.ConditionTrue || writes != 3 {
		t.Fatal("stale result modified the gate")
	}
	request.IdempotencyKey, request.ResourceVersion, request.Ready = "readiness-result-d", after.ResourceVersion, false
	if _, err := writer.Apply(ctx, request); err != nil {
		t.Fatal(err)
	}
	after, _, _ = s.snapshot()
	if gateCondition(t, after).Status != corev1.ConditionFalse {
		t.Fatal("readiness loss was not applied")
	}
}

func TestReadinessWriterConcurrentIdenticalUpdates(t *testing.T) {
	s := &gateStore{}
	writer := k3s.ReadinessWriter{Client: gateClient(t, s)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 12)
	for range 12 {
		go func() { _, err := writer.Apply(ctx, readinessRequest()); done <- err }()
	}
	for range 12 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	_, writes, _ := s.snapshot()
	if writes != 1 {
		t.Fatalf("identical concurrent update applied %d times", writes)
	}
}

func TestReadinessWriterRecoversLostResponse(t *testing.T) {
	s := &gateStore{dropReply: true}
	writer := k3s.ReadinessWriter{Client: gateClient(t, s)}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = writer.Apply(ctx, readinessRequest())
	version, err := writer.Apply(ctx, readinessRequest())
	_, writes, _ := s.snapshot()
	if err != nil || version == "" || writes != 1 {
		t.Fatalf("lost-response retry: version=%q writes=%d error=%v", version, writes, err)
	}
}

func TestReadinessWriterRejectsKeyReuse(t *testing.T) {
	for _, mode := range []string{"changed state", "changed base version"} {
		t.Run(mode, func(t *testing.T) {
			s := &gateStore{}
			writer := k3s.ReadinessWriter{Client: gateClient(t, s)}
			request := readinessRequest()
			version, err := writer.Apply(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "changed state" {
				request.Ready = true
			} else {
				request.ResourceVersion = version
			}
			if _, err := writer.Apply(context.Background(), request); !errors.Is(err, k3s.ErrReadinessConflict) {
				t.Fatalf("changed request reused key: %v", err)
			}
			_, writes, _ := s.snapshot()
			if writes != 1 {
				t.Fatal("key reuse modified readiness")
			}
		})
	}
}

func TestReadinessWriterRejectsForeignStaleAndUnavailableInstances(t *testing.T) {
	for _, mode := range []string{"pod replaced", "container replaced", "container terminated", "terminating", "foreign label", "foreign instance", "owned elsewhere", "missing gate", "old version"} {
		t.Run(mode, func(t *testing.T) {
			s := &gateStore{}
			writer := k3s.ReadinessWriter{Client: gateClient(t, s)}
			want := k3s.ErrInstanceConflict
			s.mu.Lock()
			switch mode {
			case "pod replaced":
				s.pod.UID = "replacement"
				want = k3s.ErrStaleInstance
			case "container replaced":
				s.pod.Status.ContainerStatuses[1].ContainerID = "replacement"
				want = k3s.ErrStaleInstance
			case "container terminated":
				s.pod.Status.ContainerStatuses[1].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}
				want = k3s.ErrInstanceUnavailable
			case "terminating":
				now := metav1.Now()
				s.pod.DeletionTimestamp = &now
				want = k3s.ErrInstanceUnavailable
			case "foreign label":
				s.pod.Labels["app.kubernetes.io/managed-by"] = "other"
			case "foreign instance":
				s.pod.Labels["chmura/instance"] = "other"
			case "owned elsewhere":
				s.pod.OwnerReferences = []metav1.OwnerReference{{UID: "other"}}
			case "missing gate":
				s.pod.Spec.ReadinessGates = nil
			case "old version":
				s.pod.ResourceVersion = "newer"
				want = k3s.ErrReadinessConflict
			}
			s.mu.Unlock()
			before, _, _ := s.snapshot()
			if _, err := writer.Apply(context.Background(), readinessRequest()); !errors.Is(err, want) {
				t.Fatalf("unsafe gate update: %v, want %v", err, want)
			}
			after, writes, _ := s.snapshot()
			if writes != 0 || !reflect.DeepEqual(before, after) {
				t.Fatal("invalid generation was modified")
			}
		})
	}
}

func TestReadinessWriterDoesNotRetryAcrossConcurrentChanges(t *testing.T) {
	for _, mode := range []string{"kubelet status", "replacement"} {
		t.Run(mode, func(t *testing.T) {
			s := &gateStore{interfere: func(pod *corev1.Pod) {
				if mode == "replacement" {
					pod.UID = "replacement"
				}
			}}
			writer := k3s.ReadinessWriter{Client: gateClient(t, s)}
			_, err := writer.Apply(context.Background(), readinessRequest())
			want := k3s.ErrReadinessConflict
			if mode == "replacement" {
				want = k3s.ErrStaleInstance
			}
			if !errors.Is(err, want) {
				t.Fatalf("concurrent mutation was overwritten: %v", err)
			}
			_, writes, _ := s.snapshot()
			if writes != 0 {
				t.Fatal("writer retried stale result against a new version")
			}
		})
	}
}

func TestReadinessWriterRejectsInvalidRequestsAndPreservesAPIErrors(t *testing.T) {
	for _, mode := range []string{"no client", "no key", "no version", "incomplete reference", "already canceled", "missing pod", "forbidden read", "forbidden update"} {
		t.Run(mode, func(t *testing.T) {
			s := &gateStore{}
			writer := k3s.ReadinessWriter{Client: gateClient(t, s)}
			request := readinessRequest()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			s.mu.Lock()
			switch mode {
			case "no client":
				writer.Client = nil
			case "no key":
				request.IdempotencyKey = ""
			case "no version":
				request.ResourceVersion = ""
			case "incomplete reference":
				request.Instance.ContainerID = ""
			case "already canceled":
				cancel()
			case "missing pod":
				s.getStatus = 404
			case "forbidden read":
				s.getStatus = 403
			case "forbidden update":
				s.putStatus = 403
			}
			s.mu.Unlock()
			version, err := writer.Apply(ctx, request)
			if err == nil || version != "" {
				t.Fatalf("invalid update succeeded: %q %v", version, err)
			}
			if mode == "missing pod" && !apierrors.IsNotFound(err) || strings.HasPrefix(mode, "forbidden") && !apierrors.IsForbidden(err) || mode == "already canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("error lost its cause: %v", err)
			}
			_, writes, requests := s.snapshot()
			if writes != 0 {
				t.Fatal("failed request modified the gate")
			}
			if (strings.HasPrefix(mode, "no ") || mode == "incomplete reference" || mode == "already canceled") && requests != 0 {
				t.Fatal("invalid request reached API")
			}
		})
	}
}

func TestResolveTargetIncludesReadinessBaseVersion(t *testing.T) {
	s := &gateStore{}
	resolver := k3s.Resolver{Client: gateClient(t, s)}
	target, err := resolver.Resolve(context.Background(), instance(), nil)
	if err != nil || target.ResourceVersion != "42" {
		t.Fatalf("readiness base version=%q error=%v", target.ResourceVersion, err)
	}
}
