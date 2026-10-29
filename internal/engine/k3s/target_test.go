package k3s_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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
	"k8s.io/client-go/rest"
)

func instance() k3s.Instance {
	return k3s.Instance{Namespace: "team-a", PodName: "app-a", PodUID: "pod-generation-a", ContainerName: "app", ContainerID: "containerd://generation-a"}
}

func runningPod() *corev1.Pod {
	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "app-a", UID: "pod-generation-a",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "sidecar"}, {Name: "app"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.12", ContainerStatuses: []corev1.ContainerStatus{
			{Name: "sidecar", ContainerID: "containerd://other"},
			{Name: "app", ContainerID: "containerd://generation-a", Ready: false,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now().Add(-time.Minute).Truncate(time.Second))}}},
		}},
	}
}

func apiClient(t *testing.T, handler http.HandlerFunc) *coreclient.CoreV1Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := coreclient.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func podClient(t *testing.T, pod *corev1.Pod, calls *atomic.Int32) *coreclient.CoreV1Client {
	t.Helper()
	return apiClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/namespaces/team-a/pods/app-a" {
			t.Errorf("unexpected engine request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(pod); err != nil {
			t.Error(err)
		}
	})
}

func TestResolveTargetUsesObservedGenerationAndDeclaredPorts(t *testing.T) {
	for _, ip := range []string{"10.0.0.12", "fd00::12"} {
		t.Run(ip, func(t *testing.T) {
			pod := runningPod()
			pod.Status.PodIP = ip
			var calls atomic.Int32
			resolver := k3s.Resolver{Client: podClient(t, pod, &calls)}
			ports := map[string]manifest.Port{"web": {Number: 8080, Protocol: "http", Visibility: "project"}}
			target, err := resolver.Resolve(context.Background(), instance(), ports)
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 || target.Network.Host != ip || target.Network.Ports["web"] != ports["web"] {
				t.Fatalf("resolved target = %#v, requests=%d", target, calls.Load())
			}
			// Pod creation/image pull age must not consume the process startup budget.
			if !target.ProcessStartedAt.Equal(pod.Status.ContainerStatuses[1].State.Running.StartedAt.Time) {
				t.Fatalf("process start = %v", target.ProcessStartedAt)
			}
			target.Network.Ports["web"] = manifest.Port{Number: 9999, Protocol: "tcp"}
			if ports["web"].Number != 8080 {
				t.Fatal("resolved ports alias the declared manifest")
			}
		})
	}
}

func TestResolveTargetRejectsReplacedOrUnavailableInstances(t *testing.T) {
	for _, mode := range []string{"pod replaced", "container restarted", "wrong name", "wrong namespace", "pending", "failed", "succeeded", "terminating", "missing container", "missing status", "waiting", "terminated", "missing container ID", "missing start", "missing IP", "invalid IP", "unspecified IP", "multicast IP", "zoned IP"} {
		t.Run(mode, func(t *testing.T) {
			pod := runningPod()
			want := k3s.ErrInstanceUnavailable
			switch mode {
			case "pod replaced":
				pod.UID = "pod-generation-b"
				want = k3s.ErrStaleInstance
			case "container restarted":
				pod.Status.ContainerStatuses[1].ContainerID = "containerd://generation-b"
				want = k3s.ErrStaleInstance
			case "wrong name":
				pod.Name = "other"
				want = k3s.ErrStaleInstance
			case "wrong namespace":
				pod.Namespace = "other"
				want = k3s.ErrStaleInstance
			case "pending":
				pod.Status.Phase = corev1.PodPending
			case "failed":
				pod.Status.Phase = corev1.PodFailed
			case "succeeded":
				pod.Status.Phase = corev1.PodSucceeded
			case "terminating":
				now := metav1.Now()
				pod.DeletionTimestamp = &now
			case "missing container":
				pod.Spec.Containers = pod.Spec.Containers[:1]
			case "missing status":
				pod.Status.ContainerStatuses = pod.Status.ContainerStatuses[:1]
			case "waiting":
				pod.Status.ContainerStatuses[1].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}}
			case "terminated":
				pod.Status.ContainerStatuses[1].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}
			case "missing container ID":
				pod.Status.ContainerStatuses[1].ContainerID = ""
			case "missing start":
				pod.Status.ContainerStatuses[1].State.Running.StartedAt = metav1.Time{}
			case "missing IP":
				pod.Status.PodIP = ""
			case "invalid IP":
				pod.Status.PodIP = "http://other.example/path"
			case "unspecified IP":
				pod.Status.PodIP = "0.0.0.0"
			case "multicast IP":
				pod.Status.PodIP = "ff02::1"
			case "zoned IP":
				pod.Status.PodIP = "fe80::1%eth0"
			}
			var calls atomic.Int32
			target, err := (k3s.Resolver{Client: podClient(t, pod, &calls)}).Resolve(context.Background(), instance(), nil)
			if !errors.Is(err, want) || target.Network.Host != "" || !target.ProcessStartedAt.IsZero() {
				t.Fatalf("invalid instance returned target: %#v error=%v, want %v", target, err, want)
			}
		})
	}
}

func TestResolveTargetRejectsIncompleteReferenceBeforeAPICall(t *testing.T) {
	for _, field := range []string{"namespace", "pod", "uid", "container", "container ID", "client"} {
		t.Run(field, func(t *testing.T) {
			var calls atomic.Int32
			resolver := k3s.Resolver{Client: podClient(t, runningPod(), &calls)}
			ref := instance()
			switch field {
			case "namespace":
				ref.Namespace = ""
			case "pod":
				ref.PodName = ""
			case "uid":
				ref.PodUID = ""
			case "container":
				ref.ContainerName = ""
			case "container ID":
				ref.ContainerID = ""
			case "client":
				resolver.Client = nil
			}
			if _, err := resolver.Resolve(context.Background(), ref, nil); err == nil || calls.Load() != 0 {
				t.Fatalf("incomplete reference: error=%v requests=%d", err, calls.Load())
			}
		})
	}
}

func TestResolveTargetPreservesAPIErrors(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusForbidden} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			client := apiClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(code)
				reason := metav1.StatusReasonNotFound
				if code == http.StatusForbidden {
					reason = metav1.StatusReasonForbidden
				}
				_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: reason, Code: int32(code)})
			})
			_, err := (k3s.Resolver{Client: client}).Resolve(context.Background(), instance(), nil)
			if code == http.StatusNotFound && !apierrors.IsNotFound(err) || code == http.StatusForbidden && !apierrors.IsForbidden(err) {
				t.Fatalf("API error cause lost: %v", err)
			}
		})
	}
}

func TestResolveTargetHonorsCancellation(t *testing.T) {
	for _, mode := range []string{"already canceled", "cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{})
			client := apiClient(t, func(w http.ResponseWriter, r *http.Request) {
				close(started)
				<-r.Context().Done()
			})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			want := context.Canceled
			if mode == "deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
				defer cancel()
				want = context.DeadlineExceeded
			} else if mode == "already canceled" {
				cancel()
			}
			done := make(chan error, 1)
			go func() {
				_, err := (k3s.Resolver{Client: client}).Resolve(ctx, instance(), nil)
				done <- err
			}()
			if mode == "cancel" {
				<-started
				cancel()
			}
			if err := <-done; !errors.Is(err, want) {
				t.Fatalf("lookup stop = %v, want %v", err, want)
			}
			if mode == "already canceled" {
				select {
				case <-started:
					t.Fatal("pre-canceled lookup contacted the API")
				default:
				}
			}
		})
	}
}

func TestResolvedTargetFeedsNetworkProbe(t *testing.T) {
	var requests atomic.Int32
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/ready" {
			t.Errorf("probe path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer app.Close()
	u, _ := url.Parse(app.URL)
	port, _ := strconv.Atoi(u.Port())
	pod := runningPod()
	pod.Status.PodIP = u.Hostname()
	var calls atomic.Int32
	resolved, err := (k3s.Resolver{Client: podClient(t, pod, &calls)}).Resolve(context.Background(), instance(), map[string]manifest.Port{"web": {Number: port, Protocol: "http"}})
	if err != nil {
		t.Fatal(err)
	}
	check := manifest.Probe{HTTP: &manifest.HTTPCheck{Path: "/ready", Port: "web"}}
	if err := (probe.Network{}).Run(context.Background(), resolved.Network, check); err != nil || requests.Load() != 1 || calls.Load() != 1 {
		t.Fatalf("resolved network check: error=%v app-requests=%d API-requests=%d", err, requests.Load(), calls.Load())
	}
}
