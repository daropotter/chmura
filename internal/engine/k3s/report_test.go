package k3s_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/engine/k3s"
	"github.com/daropotter/chmura/internal/probe"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
)

func reportingOptions() k3s.ReporterOptions {
	return k3s.ReporterOptions{AttemptTimeout: time.Second, RetryInterval: 10 * time.Millisecond}
}

func reporterFor(t *testing.T, s *gateStore, options k3s.ReporterOptions) *k3s.ReadinessReporter {
	t.Helper()
	r, err := k3s.NewReadinessReporter(gateClient(t, s), instance(), "supervisor-session-a", options)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func startReporter(t *testing.T, reporter *k3s.ReadinessReporter) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	done := make(chan error, 1)
	go func() { done <- reporter.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("reporter stopped unexpectedly: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("reporter failed to stop")
		}
	})
	return ctx
}

func TestReporterCoalescesBeforeRunAndFlushesAcceptedState(t *testing.T) {
	s := &gateStore{}
	r := reporterFor(t, s, reportingOptions())
	for i := 0; i < 1000; i++ {
		r.Observe(probe.Readiness{Ready: i%2 != 0, EverReady: true})
	}
	_, _, requests := s.snapshot()
	if requests != 0 {
		t.Fatal("local observer performed API I/O")
	}
	ctx := startReporter(t, r)
	// Every waiter observes completion; acknowledgements are not single-consumer events.
	done := make(chan error, 8)
	for range 8 {
		go func() { done <- r.Flush(ctx) }()
	}
	for range 8 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	pod, writes, _ := s.snapshot()
	if writes != 1 || gateCondition(t, pod).Status != corev1.ConditionTrue {
		t.Fatalf("coalesced state: writes=%d condition=%#v", writes, gateCondition(t, pod))
	}
	r.Observe(probe.Readiness{EverReady: true})
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	pod, writes, _ = s.snapshot()
	if writes != 2 || gateCondition(t, pod).Status != corev1.ConditionFalse {
		t.Fatal("flush did not drain the final unready state")
	}
}

func TestReporterObserverDoesNotWaitForBlockedAPI(t *testing.T) {
	entered, canceled := make(chan struct{}), make(chan struct{})
	var puts atomic.Int32
	s := &gateStore{beforeRequest: func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut && puts.Add(1) == 1 {
			_, _ = io.Copy(io.Discard, r.Body)
			close(entered)
			<-r.Context().Done()
			close(canceled)
			return false
		}
		return true
	}}
	options := reportingOptions()
	options.AttemptTimeout = time.Hour
	r := reporterFor(t, s, options)
	r.Observe(probe.Readiness{})
	ctx := startReporter(t, r)
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("reporter never reached blocked API")
	}
	observed := make(chan struct{})
	go func() {
		r.Observe(probe.Readiness{Ready: true, EverReady: true})
		r.Observe(probe.Readiness{EverReady: true})
		r.Observe(probe.Readiness{Ready: true, EverReady: true})
		close(observed)
	}()
	select {
	case <-observed:
	case <-ctx.Done():
		t.Fatal("observer waited for outbound I/O")
	}
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("superseded write was not canceled")
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	pod, writes, _ := s.snapshot()
	if writes != 1 || gateCondition(t, pod).Status != corev1.ConditionTrue {
		t.Fatalf("superseded result persisted: writes=%d condition=%#v", writes, gateCondition(t, pod))
	}
}

func TestReporterRetriesLostResponsesTransientErrorsAndVersionConflicts(t *testing.T) {
	for _, mode := range []string{"lost response", "server error", "kubelet conflict"} {
		t.Run(mode, func(t *testing.T) {
			s := &gateStore{}
			var puts atomic.Int32
			switch mode {
			case "lost response":
				s.dropReply = true
			case "server error":
				s.beforeRequest = func(w http.ResponseWriter, r *http.Request) bool {
					if r.Method == http.MethodPut && puts.Add(1) == 1 {
						apiStatus(w, 500)
						return false
					}
					return true
				}
			case "kubelet conflict":
				s.interfere = func(*corev1.Pod) {}
			}
			r := reporterFor(t, s, reportingOptions())
			r.Observe(probe.Readiness{Ready: true, EverReady: true})
			ctx := startReporter(t, r)
			if err := r.Flush(ctx); err != nil {
				t.Fatal(err)
			}
			pod, writes, _ := s.snapshot()
			if writes != 1 || gateCondition(t, pod).Status != corev1.ConditionTrue {
				t.Fatalf("retry did not recover one accepted update: writes=%d state=%#v", writes, gateCondition(t, pod))
			}
		})
	}
}

func TestReporterNewStateInterruptsRetryDelay(t *testing.T) {
	failed := make(chan struct{})
	var puts atomic.Int32
	s := &gateStore{beforeRequest: func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut && puts.Add(1) == 1 {
			apiStatus(w, 500)
			close(failed)
			return false
		}
		return true
	}}
	options := reportingOptions()
	options.RetryInterval = time.Hour
	r := reporterFor(t, s, options)
	r.Observe(probe.Readiness{Ready: true, EverReady: true})
	ctx := startReporter(t, r)
	select {
	case <-failed:
	case <-ctx.Done():
		t.Fatal("initial publish never failed")
	}
	r.Observe(probe.Readiness{EverReady: true})
	if err := r.Flush(ctx); err != nil {
		t.Fatalf("new state remained behind retry delay: %v", err)
	}
	pod, _, _ := s.snapshot()
	if gateCondition(t, pod).Status != corev1.ConditionFalse {
		t.Fatal("retry published obsolete readiness")
	}
}

func TestReporterAttemptTimeoutRetriesTheLatestState(t *testing.T) {
	var gets atomic.Int32
	timedOut := make(chan struct{})
	s := &gateStore{beforeRequest: func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && gets.Add(1) == 1 {
			<-r.Context().Done()
			close(timedOut)
			return false
		}
		return true
	}}
	options := reportingOptions()
	options.AttemptTimeout = 50 * time.Millisecond
	r := reporterFor(t, s, options)
	r.Observe(probe.Readiness{Ready: true, EverReady: true})
	ctx := startReporter(t, r)
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-timedOut:
	default:
		t.Fatal("blocked API attempt did not time out")
	}
	pod, writes, _ := s.snapshot()
	if writes != 1 || gateCondition(t, pod).Status != corev1.ConditionTrue {
		t.Fatal("attempt timeout lost the pending state")
	}
}

func TestReporterBackoffDoesNotSpinDuringAPIOutage(t *testing.T) {
	var puts atomic.Int32
	failed := make(chan struct{})
	s := &gateStore{beforeRequest: func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut {
			if puts.Add(1) == 1 {
				close(failed)
			}
			apiStatus(w, 500)
			return false
		}
		return true
	}}
	options := reportingOptions()
	options.RetryInterval = time.Hour
	r := reporterFor(t, s, options)
	r.Observe(probe.Readiness{})
	ctx := startReporter(t, r)
	select {
	case <-failed:
	case <-ctx.Done():
		t.Fatal("initial reporting attempt never reached the API")
	}
	flush, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := r.Flush(flush); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("outage flush=%v", err)
	}
	if puts.Load() != 1 {
		t.Fatalf("outage ignored retry delay: %d attempts", puts.Load())
	}
}

func TestReporterPublishesSupervisorReadinessAndDrainsItsFinalState(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer app.Close()
	s := &gateStore{beforeRequest: func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("watch") == "true" {
			openWatch(w)
			<-r.Context().Done()
			return false
		}
		return true
	}}
	client := gateClient(t, s)
	s.mu.Lock()
	s.pod.Status.PodIP = "127.0.0.1"
	s.pod.Status.ContainerStatuses[1].State.Running.StartedAt = metav1.NewTime(time.Now().Truncate(time.Second))
	s.mu.Unlock()
	r, err := k3s.NewReadinessReporter(client, instance(), "integrated-session", reportingOptions())
	if err != nil {
		t.Fatal(err)
	}
	ctx := startReporter(t, r)
	monitor, cancel := context.WithCancel(ctx)
	defer cancel()
	ready, done := make(chan struct{}), make(chan error, 1)
	var once sync.Once
	go func() {
		done <- (k3s.Supervisor{Resolver: k3s.Resolver{Client: client}}).Monitor(monitor, k3s.SupervisionRequest{Instance: instance(), Plan: supervisionPlan(app.Listener.Addr().(*net.TCPAddr).Port)}, func(state probe.Readiness) {
			r.Observe(state)
			if state.Ready {
				once.Do(func() { close(ready) })
			}
		})
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("monitor stopped before ready: %v", err)
	case <-ctx.Done():
		t.Fatal("monitor never became ready")
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	pod, _, _ := s.snapshot()
	if gateCondition(t, pod).Status != corev1.ConditionTrue {
		t.Fatal("supervisor readiness did not reach the gate")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	pod, _, _ = s.snapshot()
	if gateCondition(t, pod).Status != corev1.ConditionFalse {
		t.Fatal("monitor stop left the gate ready")
	}
}

func TestReporterStopsOnPermanentErrorsAndDoesNotAdopt(t *testing.T) {
	for _, mode := range []string{"forbidden read", "forbidden update", "missing pod", "replacement", "terminated", "foreign owner"} {
		t.Run(mode, func(t *testing.T) {
			s := &gateStore{}
			r := reporterFor(t, s, reportingOptions())
			s.mu.Lock()
			switch mode {
			case "forbidden read":
				s.getStatus = 403
			case "forbidden update":
				s.putStatus = 403
			case "missing pod":
				s.getStatus = 404
			case "replacement":
				s.pod.UID = "replacement"
			case "terminated":
				s.pod.Status.ContainerStatuses[1].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}
			case "foreign owner":
				s.pod.Labels["app.kubernetes.io/managed-by"] = "other"
			}
			s.mu.Unlock()
			r.Observe(probe.Readiness{Ready: true, EverReady: true})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			err := r.Run(ctx)
			if err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("permanent error was retried: %v", err)
			}
			if strings.HasPrefix(mode, "forbidden") && !apierrors.IsForbidden(err) || mode == "missing pod" && !apierrors.IsNotFound(err) || mode == "replacement" && !errors.Is(err, k3s.ErrStaleInstance) || mode == "terminated" && !errors.Is(err, k3s.ErrInstanceUnavailable) || mode == "foreign owner" && !errors.Is(err, k3s.ErrInstanceConflict) {
				t.Fatalf("reporter lost terminal cause: %v", err)
			}
			if flushErr := r.Flush(ctx); flushErr == nil || errors.Is(flushErr, context.DeadlineExceeded) {
				t.Fatalf("flush hid reporter failure: %v", flushErr)
			}
			_, writes, _ := s.snapshot()
			if writes != 0 {
				t.Fatal("reporter modified a rejected generation")
			}
		})
	}
}

func TestReporterCancellationStopsIOAndUnblocksFlush(t *testing.T) {
	entered, canceled := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s := &gateStore{beforeRequest: func(w http.ResponseWriter, r *http.Request) bool {
		once.Do(func() { close(entered) })
		<-r.Context().Done()
		close(canceled)
		return false
	}}
	r := reporterFor(t, s, reportingOptions())
	r.Observe(probe.Readiness{Ready: true, EverReady: true})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("reporter never began I/O")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("reporter cancellation=%v", err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("outbound request was left active")
	}
	if err := r.Flush(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("flush lost cancellation: %v", err)
	}
	r.Observe(probe.Readiness{}) // A late local callback remains safe after worker stop.
	if err := r.Run(context.Background()); err == nil {
		t.Fatal("reporter permitted reuse of its session sequence")
	}
}

func TestReporterRejectsInvalidConfiguration(t *testing.T) {
	for _, mode := range []string{"no client", "no session key", "incomplete generation", "no attempt timeout", "no retry interval"} {
		t.Run(mode, func(t *testing.T) {
			s := &gateStore{}
			var client coreclient.CoreV1Interface = gateClient(t, s)
			ref := instance()
			key := "session"
			options := reportingOptions()
			switch mode {
			case "no client":
				client = nil
			case "no session key":
				key = " "
			case "incomplete generation":
				ref.ContainerID = ""
			case "no attempt timeout":
				options.AttemptTimeout = 0
			case "no retry interval":
				options.RetryInterval = 0
			}
			if r, err := k3s.NewReadinessReporter(client, ref, key, options); err == nil || r != nil {
				t.Fatal("invalid reporter was constructed")
			}
			_, _, requests := s.snapshot()
			if requests != 0 {
				t.Fatal("reporter construction contacted API")
			}
		})
	}
}
