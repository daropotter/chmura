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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

type podStore struct {
	mu                      sync.Mutex
	pods                    map[string]*corev1.Pod
	posts, gets, creates    int
	createStatus, getStatus int
	dropReply               bool
	holdReply               bool
	created                 chan struct{}
}

func (s *podStore) counts() (posts, creates, pods int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.posts, s.creates, len(s.pods)
}

func apiStatus(w http.ResponseWriter, code int) {
	reason := metav1.StatusReasonUnknown
	switch code {
	case 403:
		reason = metav1.StatusReasonForbidden
	case 404:
		reason = metav1.StatusReasonNotFound
	case 409:
		reason = metav1.StatusReasonAlreadyExists
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: reason, Code: int32(code)})
}

func creationClient(t *testing.T, store *podStore) *coreclient.CoreV1Client {
	t.Helper()
	store.pods = make(map[string]*corev1.Pod)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix := "/api/v1/namespaces/project-a/pods"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			t.Errorf("unexpected API resource: %s %s", r.Method, r.URL.Path)
			apiStatus(w, 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			var pod corev1.Pod
			if err := json.NewDecoder(r.Body).Decode(&pod); err != nil {
				t.Errorf("decode Pod creation: %v", err)
				apiStatus(w, 400)
				return
			}
			store.mu.Lock()
			store.posts++
			code := store.createStatus
			if code == 0 && store.pods[pod.Name] != nil {
				code = 409
			}
			if code != 0 {
				store.mu.Unlock()
				apiStatus(w, code)
				return
			}
			store.creates++
			pod.UID = types.UID(fmt.Sprintf("uid-%d", store.creates))
			pod.ResourceVersion = "1"
			pod.Status.Phase = corev1.PodPending
			// Normal admission/defaulting must not make retries look foreign.
			pod.Spec.ServiceAccountName, pod.Spec.DeprecatedServiceAccount = "default", "default"
			pod.Spec.SecurityContext = &corev1.PodSecurityContext{}
			zero, hostUsers, preemption := int32(0), true, corev1.PreemptLowerPriority
			pod.Spec.Priority, pod.Spec.HostUsers, pod.Spec.PreemptionPolicy = &zero, &hostUsers, &preemption
			pod.Spec.Tolerations = []corev1.Toleration{{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute}}
			store.pods[pod.Name] = pod.DeepCopy()
			drop, hold, created := store.dropReply, store.holdReply, store.created
			store.dropReply = false
			store.mu.Unlock()
			if created != nil {
				close(created)
			}
			if drop {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Errorf("drop response: %v", err)
					return
				}
				_ = conn.Close()
				return
			}
			if hold {
				<-r.Context().Done()
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(&pod)
		case http.MethodGet:
			name := strings.TrimPrefix(r.URL.Path, prefix+"/")
			store.mu.Lock()
			store.gets++
			code := store.getStatus
			pod := store.pods[name].DeepCopy()
			store.mu.Unlock()
			if code != 0 {
				apiStatus(w, code)
				return
			}
			if pod == nil {
				apiStatus(w, 404)
				return
			}
			_ = json.NewEncoder(w).Encode(pod)
		default:
			t.Errorf("writer mutated an existing resource: %s", r.Method)
			apiStatus(w, http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	client, err := coreclient.NewForConfig(&rest.Config{Host: server.URL, QPS: -1, ContentConfig: rest.ContentConfig{ContentType: "application/json"}})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestEnsureInstanceCreatesAndReusesOneIdentity(t *testing.T) {
	store := &podStore{}
	client := creationClient(t, store)
	request := creationRequest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	first, err := (k3s.Creator{Client: client}).Ensure(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := (k3s.Creator{Client: client}).Ensure(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	_, creates, _ := store.counts()
	if !first.Created || second.Created || first.Identity != second.Identity || first.Identity.PodUID != "uid-1" || first.Identity.ContainerName != "app" || creates != 1 {
		t.Fatalf("request replay: first=%#v second=%#v creates=%d", first, second, creates)
	}
	// Equivalent quantity representations and runtime status are not drift.
	store.mu.Lock()
	pod := store.pods[first.Identity.PodName]
	pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("0.25")
	pod.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("134217728")
	pod.Status.Phase = corev1.PodFailed
	store.mu.Unlock()
	third, err := (k3s.Creator{Client: client}).Ensure(ctx, request)
	_, creates, _ = store.counts()
	if err != nil || third.Created || third.Identity != first.Identity || creates != 1 {
		t.Fatalf("replay replaced an existing generation: result=%#v error=%v", third, err)
	}
}

func TestEnsureInstanceHandlesConcurrentReplay(t *testing.T) {
	store := &podStore{}
	client, request := creationClient(t, store), creationRequest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type outcome struct {
		result k3s.CreateResult
		err    error
	}
	results := make(chan outcome, 12)
	for range 12 {
		go func() {
			result, err := (k3s.Creator{Client: client}).Ensure(ctx, request)
			results <- outcome{result, err}
		}()
	}
	created := 0
	for range 12 {
		got := <-results
		if got.err != nil || got.result.Identity.PodUID != "uid-1" {
			t.Fatalf("concurrent replay = %#v, %v", got.result, got.err)
		}
		if got.result.Created {
			created++
		}
	}
	_, creates, count := store.counts()
	if created != 1 || creates != 1 || count != 1 {
		t.Fatalf("concurrent request created duplicates: results=%d resources=%d", created, creates)
	}
}

func TestEnsureInstanceRejectsChangedIntentAndForeignResources(t *testing.T) {
	for _, mode := range []string{"changed health", "changed allocation", "changed digest", "foreign label", "foreign key", "foreign owner", "changed image", "changed command", "changed node", "changed resources", "extra container", "native probe", "host network", "missing gate"} {
		t.Run(mode, func(t *testing.T) {
			store := &podStore{}
			client, request := creationClient(t, store), creationRequest(t)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			first, err := (k3s.Creator{Client: client}).Ensure(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			store.mu.Lock()
			pod := store.pods[first.Identity.PodName]
			switch mode {
			case "changed health":
				request.Plan.Health.Ready.Successes++
			case "changed allocation":
				request.CPUMilli++
			case "changed digest":
				request.ResolvedImage = "registry.example.com/team/api@sha256:" + strings.Repeat("b", 64)
			case "foreign label":
				pod.Labels["app.kubernetes.io/managed-by"] = "other"
			case "foreign key":
				pod.Annotations["chmura/request-key-sha256"] = "other"
			case "foreign owner":
				pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "Other", Name: "other", UID: "other"}}
			case "changed image":
				pod.Spec.Containers[0].Image = "other:v1"
			case "changed command":
				pod.Spec.Containers[0].Command = []string{"/other"}
			case "changed node":
				pod.Spec.NodeName = "other"
			case "changed resources":
				pod.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("1Gi")
			case "extra container":
				pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "other", Image: "other:v1"})
			case "native probe":
				pod.Spec.Containers[0].LivenessProbe = &corev1.Probe{}
			case "host network":
				pod.Spec.HostNetwork = true
			case "missing gate":
				pod.Spec.ReadinessGates = nil
			}
			before := pod.DeepCopy()
			store.mu.Unlock()
			result, err := (k3s.Creator{Client: client}).Ensure(ctx, request)
			_, creates, _ := store.counts()
			if !errors.Is(err, k3s.ErrInstanceConflict) || result.Identity.PodUID != "" || creates != 1 {
				t.Fatalf("conflicting resource was reused: result=%#v error=%v", result, err)
			}
			store.mu.Lock()
			if !reflect.DeepEqual(before, store.pods[first.Identity.PodName]) {
				t.Error("conflict changed the existing resource")
			}
			store.mu.Unlock()
		})
	}
}

func TestEnsureInstanceRetryAfterLostResponse(t *testing.T) {
	store := &podStore{dropReply: true}
	client, request := creationClient(t, store), creationRequest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = (k3s.Creator{Client: client}).Ensure(ctx, request)
	result, err := (k3s.Creator{Client: client}).Ensure(ctx, request)
	_, creates, _ := store.counts()
	if err != nil || result.Created || result.Identity.PodUID != "uid-1" || creates != 1 {
		t.Fatalf("lost response created another instance: result=%#v error=%v creates=%d", result, err, creates)
	}
}

func TestEnsureInstanceCancellationKeepsReplaySafe(t *testing.T) {
	store := &podStore{holdReply: true, created: make(chan struct{})}
	client, request := creationClient(t, store), creationRequest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := (k3s.Creator{Client: client}).Ensure(ctx, request); done <- err }()
	select {
	case <-store.created:
	case err := <-done:
		t.Fatalf("creation stopped early: %v", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("create cancellation = %v", err)
	}
	retryCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	result, err := (k3s.Creator{Client: client}).Ensure(retryCtx, request)
	_, creates, _ := store.counts()
	if err != nil || result.Created || result.Identity.PodUID != "uid-1" || creates != 1 {
		t.Fatalf("canceled request was duplicated: result=%#v error=%v", result, err)
	}
}

func TestEnsureInstancePreservesAPIErrorsAndRejectsInvalidRequests(t *testing.T) {
	for _, mode := range []string{"missing namespace", "forbidden create", "forbidden read", "no client", "no key", "already canceled"} {
		t.Run(mode, func(t *testing.T) {
			store := &podStore{}
			client, request := creationClient(t, store), creationRequest(t)
			creator := k3s.Creator{Client: client}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			switch mode {
			case "missing namespace":
				store.mu.Lock()
				store.createStatus = 404
				store.mu.Unlock()
			case "forbidden create":
				store.mu.Lock()
				store.createStatus = 403
				store.mu.Unlock()
			case "forbidden read":
				if _, err := creator.Ensure(ctx, request); err != nil {
					t.Fatal(err)
				}
				store.mu.Lock()
				store.getStatus = 403
				store.mu.Unlock()
			case "no client":
				creator.Client = nil
			case "no key":
				request.IdempotencyKey = ""
			case "already canceled":
				cancel()
			}
			result, err := creator.Ensure(ctx, request)
			if err == nil || result.Identity.PodUID != "" {
				t.Fatalf("failed request returned an identity: %#v, %v", result, err)
			}
			if mode == "missing namespace" && !apierrors.IsNotFound(err) || strings.HasPrefix(mode, "forbidden") && !apierrors.IsForbidden(err) || mode == "already canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("API/context cause was lost: %v", err)
			}
			posts, _, _ := store.counts()
			if (mode == "no key" || mode == "no client" || mode == "already canceled") && posts != 0 {
				t.Fatal("invalid request reached the API")
			}
		})
	}
}
