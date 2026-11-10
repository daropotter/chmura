package k3s_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

type lifetimeStore struct {
	mu                      sync.Mutex
	pods                    map[string]*corev1.Pod
	requests, deletes       int
	getStatus, deleteStatus int
	interfere               func(*corev1.Pod)
	dropReply, holdReply    bool
	accepted                chan struct{}
}

func (s *lifetimeStore) snapshot(name string) (*corev1.Pod, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pods[name].DeepCopy(), s.deletes, s.requests
}

func lifetimeClient(t *testing.T, s *lifetimeStore) (*coreclient.CoreV1Client, k3s.InstanceRequest, k3s.Instance) {
	t.Helper()
	request := creationRequest(t)
	pod, err := k3s.PrepareInstance(request)
	if err != nil {
		t.Fatal(err)
	}
	pod.UID = "original-uid"
	pod.ResourceVersion = "1"
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", ContainerID: "containerd://original", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}}}
	s.pods = map[string]*corev1.Pod{pod.Name: pod}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		prefix := "/api/v1/namespaces/project-a/pods/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			t.Errorf("unexpected lifecycle resource: %s %s", r.Method, r.URL.Path)
			apiStatus(w, 404)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, prefix)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.requests++
		stored := s.pods[name]
		switch r.Method {
		case http.MethodGet:
			if s.getStatus != 0 {
				apiStatus(w, s.getStatus)
				return
			}
			if stored == nil {
				apiStatus(w, 404)
				return
			}
			_ = json.NewEncoder(w).Encode(stored)
		case http.MethodDelete:
			var options metav1.DeleteOptions
			if err := json.NewDecoder(r.Body).Decode(&options); err != nil {
				t.Error(err)
				apiStatus(w, 400)
				return
			}
			if options.Preconditions == nil || options.Preconditions.UID == nil || options.Preconditions.ResourceVersion == nil {
				t.Error("deletion omitted identity/version preconditions")
				apiStatus(w, 400)
				return
			}
			if options.GracePeriodSeconds != nil {
				t.Error("retirement overrode graceful termination")
				apiStatus(w, 400)
				return
			}
			if s.deleteStatus != 0 {
				apiStatus(w, s.deleteStatus)
				return
			}
			if stored == nil {
				apiStatus(w, 404)
				return
			}
			if s.interfere != nil {
				s.interfere(stored)
				stored.ResourceVersion = "changed"
				s.interfere = nil
			}
			if *options.Preconditions.UID != stored.UID || *options.Preconditions.ResourceVersion != stored.ResourceVersion {
				w.WriteHeader(409)
				_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: metav1.StatusReasonConflict, Code: 409})
				return
			}
			s.deletes++
			now := metav1.Now()
			stored.DeletionTimestamp = &now
			stored.ResourceVersion = "deleting"
			if s.accepted != nil {
				close(s.accepted)
				s.accepted = nil
			}
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
			if s.holdReply {
				<-r.Context().Done()
				return
			}
			_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Success"})
		default:
			t.Errorf("unexpected lifecycle mutation: %s", r.Method)
			apiStatus(w, 405)
		}
	}))
	t.Cleanup(server.Close)
	client, err := coreclient.NewForConfig(&rest.Config{Host: server.URL, QPS: -1, ContentConfig: rest.ContentConfig{ContentType: "application/json"}})
	if err != nil {
		t.Fatal(err)
	}
	return client, request, k3s.Instance{Namespace: request.Namespace, PodName: pod.Name, PodUID: string(pod.UID), ContainerName: "app", ContainerID: "containerd://original"}
}

func TestRetireGenerationWaitsForActualAbsence(t *testing.T) {
	s := &lifetimeStore{}
	client, request, ref := lifetimeClient(t, s)
	retirer := k3s.Retirer{Client: client}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := retirer.Ensure(ctx, request, ref)
	if err != nil || result.Absent {
		t.Fatalf("delete acknowledgement claimed absence: %#v %v", result, err)
	}
	result, err = retirer.Ensure(ctx, request, ref)
	pod, deletes, _ := s.snapshot(ref.PodName)
	if err != nil || result.Absent || deletes != 1 || pod.DeletionTimestamp == nil {
		t.Fatalf("retirement replay: result=%#v deletes=%d error=%v", result, deletes, err)
	}
	s.mu.Lock()
	delete(s.pods, ref.PodName)
	s.mu.Unlock()
	result, err = retirer.Ensure(ctx, request, ref)
	if err != nil || !result.Absent {
		t.Fatalf("absence was not confirmed: %#v %v", result, err)
	}
}

func TestRetireGenerationCanCleanUpTerminatedProcess(t *testing.T) {
	s := &lifetimeStore{}
	client, request, ref := lifetimeClient(t, s)
	s.mu.Lock()
	s.pods[ref.PodName].Status.Phase = corev1.PodFailed
	s.pods[ref.PodName].Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}
	s.mu.Unlock()
	if _, err := (k3s.Retirer{Client: client}).Ensure(context.Background(), request, ref); err != nil {
		t.Fatal(err)
	}
	_, deletes, _ := s.snapshot(ref.PodName)
	if deletes != 1 {
		t.Fatal("terminated generation was not retired")
	}
}

func TestRetireGenerationRejectsStaleForeignAndChangedRequests(t *testing.T) {
	for _, mode := range []string{"pod replaced", "container replaced", "missing container ID", "foreign label", "foreign key", "changed image", "changed intent", "missing resource version"} {
		t.Run(mode, func(t *testing.T) {
			s := &lifetimeStore{}
			client, request, ref := lifetimeClient(t, s)
			want := k3s.ErrInstanceConflict
			s.mu.Lock()
			pod := s.pods[ref.PodName]
			switch mode {
			case "pod replaced":
				pod.UID = "replacement"
				want = k3s.ErrStaleInstance
			case "container replaced":
				pod.Status.ContainerStatuses[0].ContainerID = "replacement"
				want = k3s.ErrStaleInstance
			case "missing container ID":
				pod.Status.ContainerStatuses = nil
				want = k3s.ErrInstanceUnavailable
			case "foreign label":
				pod.Labels["app.kubernetes.io/managed-by"] = "other"
			case "foreign key":
				pod.Annotations["chmura/request-key-sha256"] = "other"
			case "changed image":
				pod.Spec.Containers[0].Image = "other:v1"
			case "changed intent":
				request.CPUMilli++
			case "missing resource version":
				pod.ResourceVersion = ""
			}
			s.mu.Unlock()
			if _, err := (k3s.Retirer{Client: client}).Ensure(context.Background(), request, ref); !errors.Is(err, want) {
				t.Fatalf("unsafe retirement: %v, want %v", err, want)
			}
			pod, deletes, _ := s.snapshot(ref.PodName)
			if deletes != 0 || pod.DeletionTimestamp != nil {
				t.Fatal("rejected generation was deleted")
			}
		})
	}
}

func TestRetireGenerationPreservesConcurrentReplacementAndVersionConflict(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "status update", true: "replacement"}[replace], func(t *testing.T) {
			s := &lifetimeStore{interfere: func(pod *corev1.Pod) {
				if replace {
					pod.UID = "replacement"
				}
			}}
			client, request, ref := lifetimeClient(t, s)
			if _, err := (k3s.Retirer{Client: client}).Ensure(context.Background(), request, ref); !apierrors.IsConflict(err) {
				t.Fatalf("concurrent update was overwritten: %v", err)
			}
			pod, deletes, _ := s.snapshot(ref.PodName)
			if deletes != 0 || pod.DeletionTimestamp != nil {
				t.Fatal("conflict deleted the current object")
			}
		})
	}
}

func TestRetireGenerationLostResponseAndCancellationAreReplayable(t *testing.T) {
	for _, mode := range []string{"lost response", "cancellation"} {
		t.Run(mode, func(t *testing.T) {
			accepted := make(chan struct{})
			s := &lifetimeStore{dropReply: mode == "lost response", holdReply: mode == "cancellation", accepted: accepted}
			client, request, ref := lifetimeClient(t, s)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := (k3s.Retirer{Client: client}).Ensure(ctx, request, ref); done <- err }()
			select {
			case <-accepted:
			case err := <-done:
				t.Fatalf("retirement stopped before acceptance: %v", err)
			}
			if mode == "cancellation" {
				cancel()
			}
			err := <-done
			if mode == "cancellation" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation cause: %v", err)
			}
			retry, stop := context.WithTimeout(context.Background(), 3*time.Second)
			defer stop()
			result, err := (k3s.Retirer{Client: client}).Ensure(retry, request, ref)
			_, deletes, _ := s.snapshot(ref.PodName)
			if err != nil || result.Absent || deletes != 1 {
				t.Fatalf("retirement replay duplicated mutation: %#v deletes=%d error=%v", result, deletes, err)
			}
		})
	}
}

func TestRetireGenerationValidatesBeforeMutationAndPreservesAPIError(t *testing.T) {
	for _, mode := range []string{"no client", "no key", "wrong name", "incomplete generation", "pre-canceled", "forbidden read", "forbidden delete"} {
		t.Run(mode, func(t *testing.T) {
			s := &lifetimeStore{}
			client, request, ref := lifetimeClient(t, s)
			retirer := k3s.Retirer{Client: client}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			s.mu.Lock()
			switch mode {
			case "no client":
				retirer.Client = nil
			case "no key":
				request.IdempotencyKey = ""
			case "wrong name":
				ref.PodName = "other"
			case "incomplete generation":
				ref.ContainerID = ""
			case "pre-canceled":
				cancel()
			case "forbidden read":
				s.getStatus = 403
			case "forbidden delete":
				s.deleteStatus = 403
			}
			s.mu.Unlock()
			_, err := retirer.Ensure(ctx, request, ref)
			if err == nil {
				t.Fatal("invalid retirement succeeded")
			}
			if strings.HasPrefix(mode, "forbidden") && !apierrors.IsForbidden(err) || mode == "pre-canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cause was lost: %v", err)
			}
			_, deletes, requests := s.snapshot(ref.PodName)
			if deletes != 0 {
				t.Fatal("failed retirement deleted a generation")
			}
			if !strings.HasPrefix(mode, "forbidden") && requests != 0 {
				t.Fatal("invalid request contacted API")
			}
		})
	}
}
