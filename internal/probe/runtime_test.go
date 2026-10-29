package probe_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/daropotter/chmura/internal/manifest"
	"github.com/daropotter/chmura/internal/probe"
)

func processCheck() manifest.Probe {
	return manifest.Probe{Process: &manifest.ProcessCheck{Name: "worker"}}
}

func execCheck() manifest.Probe {
	return manifest.Probe{Exec: &manifest.ExecCheck{Command: manifest.Command{"/bin/self-check"}}}
}

func TestProcessProbeOutcomes(t *testing.T) {
	adapterError := errors.New("instance unavailable")
	for _, tc := range []struct {
		name    string
		running bool
		cause   error
		want    error
	}{
		{"running", true, nil, nil},
		{"not running", false, nil, probe.ErrProcessNotRunning},
		{"adapter error", false, adapterError, adapterError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			runner := probe.Runtime{ProcessRunning: func(_ context.Context, name string) (bool, error) {
				calls++
				if name != "worker" {
					t.Errorf("process name = %q", name)
				}
				return tc.running, tc.cause
			}}
			err := runner.Run(context.Background(), processCheck())
			if calls != 1 || !errors.Is(err, tc.want) {
				t.Fatalf("process result: calls=%d error=%v, want %v", calls, err, tc.want)
			}
		})
	}
}

func TestExecProbeExitStatusAndLiteralArguments(t *testing.T) {
	adapterError := errors.New("cannot exec in instance")
	for _, tc := range []struct {
		name string
		code int
		err  error
	}{
		{"success", 0, nil},
		{"nonzero exit", 17, nil},
		{"adapter error", 0, adapterError},
		{"adapter error with nonzero exit", 17, adapterError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := []string{"/bin/self-check", "two words", "", "$HOME", "$(touch ignored)", "*", "a; b"}
			check := execCheck()
			check.Exec.Command = slices.Clone(want)
			calls := 0
			runner := probe.Runtime{Exec: func(_ context.Context, command []string) (int, error) {
				calls++
				if !slices.Equal(command, want) {
					t.Errorf("exec arguments = %#v, want %#v", command, want)
				}
				command[0] = "adapter-owned-copy"
				return tc.code, tc.err
			}}
			for range 2 {
				err := runner.Run(context.Background(), check)
				var exit *probe.ExitError
				switch {
				case tc.err != nil:
					if !errors.Is(err, adapterError) || errors.As(err, &exit) {
						t.Errorf("adapter error lost or classified as exit status: %v", err)
					}
				case tc.code != 0:
					if !errors.As(err, &exit) || exit.ExitCode != tc.code {
						t.Errorf("exec error = %v, want exit %d", err, tc.code)
					}
				default:
					if err != nil {
						t.Error(err)
					}
				}
			}
			if calls != 2 || !slices.Equal(check.Exec.Command, want) || check.Timeout != 0 {
				t.Fatalf("runner modified declaration: calls=%d check=%#v", calls, check)
			}
		})
	}
}

func TestRuntimeProbeTimeouts(t *testing.T) {
	for _, kind := range []string{"process", "exec"} {
		for _, tc := range []struct {
			name                  string
			timeout, parent, want time.Duration
			nilAtDeadline         bool
		}{
			{"default", 0, 3 * time.Second, 2 * time.Second, false},
			{"override", 20 * time.Millisecond, time.Second, 20 * time.Millisecond, false},
			{"earlier parent", time.Second, 5 * time.Millisecond, 5 * time.Millisecond, false},
			{"nil at deadline", 20 * time.Millisecond, time.Second, 20 * time.Millisecond, true},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), tc.parent)
					defer cancel()
					calls := 0
					block := func(ctx context.Context) {
						calls++
						if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) != tc.want {
							t.Errorf("adapter deadline = %v, present=%v", time.Until(deadline), ok)
						}
						if tc.nilAtDeadline {
							time.Sleep(tc.want)
						} else {
							<-ctx.Done()
						}
					}
					runner := probe.Runtime{
						ProcessRunning: func(ctx context.Context, _ string) (bool, error) { block(ctx); return true, nil },
						Exec:           func(ctx context.Context, _ []string) (int, error) { block(ctx); return 0, nil },
					}
					check := processCheck()
					if kind == "exec" {
						check = execCheck()
					}
					check.Timeout = manifest.Duration(tc.timeout)
					started := time.Now()
					err := runner.Run(ctx, check)
					if !errors.Is(err, context.DeadlineExceeded) || calls != 1 || time.Since(started) != tc.want {
						t.Fatalf("deadline result: error=%v calls=%d elapsed=%v", err, calls, time.Since(started))
					}
				})
			})
		}
	}
}

func TestRuntimeProbeCancellationWaitsForAdapterCleanup(t *testing.T) {
	for _, kind := range []string{"process", "exec"} {
		for _, mode := range []string{"parent cancel", "attempt timeout"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					canceled, release := make(chan struct{}), make(chan struct{})
					block := func(ctx context.Context) {
						<-ctx.Done()
						close(canceled)
						<-release
					}
					adapterError := errors.New("adapter stopped")
					runner := probe.Runtime{
						ProcessRunning: func(ctx context.Context, _ string) (bool, error) { block(ctx); return false, adapterError },
						Exec:           func(ctx context.Context, _ []string) (int, error) { block(ctx); return 7, adapterError },
					}
					check := processCheck()
					if kind == "exec" {
						check = execCheck()
					}
					want := context.Canceled
					if mode == "attempt timeout" {
						check.Timeout = manifest.Duration(20 * time.Millisecond)
						want = context.DeadlineExceeded
					}
					done := make(chan error, 1)
					go func() { done <- runner.Run(ctx, check) }()
					synctest.Wait()
					if mode == "parent cancel" {
						cancel()
					}
					<-canceled
					synctest.Wait()
					select {
					case err := <-done:
						t.Errorf("runner abandoned adapter cleanup: %v", err)
						close(release)
						return
					default:
					}
					close(release)
					if err := <-done; !errors.Is(err, want) || errors.Is(err, adapterError) {
						t.Fatalf("stop cause lost: %v, want %v", err, want)
					}
				})
			})
		}
	}
}

func TestRuntimeProbeAlreadyCanceledDoesNotCallAdapter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := probe.Runtime{
		ProcessRunning: func(context.Context, string) (bool, error) { t.Error("canceled process check ran"); return true, nil },
		Exec:           func(context.Context, []string) (int, error) { t.Error("canceled exec check ran"); return 0, nil },
	}
	for _, check := range []manifest.Probe{processCheck(), execCheck()} {
		if err := runner.Run(ctx, check); !errors.Is(err, context.Canceled) {
			t.Errorf("canceled result = %v", err)
		}
	}
}

func TestRuntimeProbeRejectsInvalidOrUnsupportedChecks(t *testing.T) {
	for _, mode := range []string{"empty", "http", "tcp", "mixed process exec", "mixed network", "empty process", "empty command", "empty executable", "negative timeout", "missing process adapter", "missing exec adapter"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			runner := probe.Runtime{
				ProcessRunning: func(context.Context, string) (bool, error) { calls++; return true, nil },
				Exec:           func(context.Context, []string) (int, error) { calls++; return 0, nil },
			}
			check := processCheck()
			switch mode {
			case "empty":
				check = manifest.Probe{}
			case "http":
				check = httpCheck()
			case "tcp":
				check = manifest.Probe{TCP: &manifest.TCPCheck{Port: "service"}}
			case "mixed process exec":
				check.Exec = execCheck().Exec
			case "mixed network":
				check.HTTP = httpCheck().HTTP
			case "empty process":
				check.Process.Name = " \t"
			case "empty command":
				check = manifest.Probe{Exec: &manifest.ExecCheck{}}
			case "empty executable":
				check = execCheck()
				check.Exec.Command = manifest.Command{" ", "argument"}
			case "negative timeout":
				check.Timeout = -1
			case "missing process adapter":
				runner.ProcessRunning = nil
			case "missing exec adapter":
				check = execCheck()
				runner.Exec = nil
			}
			if err := runner.Run(context.Background(), check); err == nil || calls != 0 {
				t.Fatalf("invalid probe called adapter: calls=%d error=%v", calls, err)
			}
		})
	}
}

func TestRuntimeProbesDriveReadinessAndHealthyRules(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		config := healthyConfig()
		config.Check.Interval = manifest.Duration(10 * time.Millisecond)
		config.Healthy.UnhealthyFailures = 2
		var processCalls, execCalls atomic.Int32
		runner := probe.Runtime{
			ProcessRunning: func(_ context.Context, name string) (bool, error) {
				if name != config.Check.Process.Name {
					t.Errorf("unexpected readiness process %q", name)
				}
				return processCalls.Add(1) > 1, nil
			},
			Exec: func(_ context.Context, command []string) (int, error) {
				if !slices.Equal(command, config.Healthy.Check.Exec.Command) {
					t.Errorf("unexpected healthy command %#v", command)
				}
				if execCalls.Add(1) == 1 {
					return 0, nil
				}
				return 9, nil
			},
		}
		var once sync.Once
		firstReady := make(chan struct{})
		readyDone, healthyDone := make(chan error, 1), make(chan error, 1)
		started := time.Now()
		go func() {
			readyDone <- probe.MonitorReadiness(ctx, started, config, runner.Run, func(state probe.Readiness) {
				if state.EverReady {
					once.Do(func() { close(firstReady) })
				}
			})
		}()
		go func() { healthyDone <- probe.MonitorHealthy(ctx, firstReady, config, runner.Run) }()
		if err := <-healthyDone; !errors.Is(err, probe.ErrUnhealthy) || execCalls.Load() != 3 || time.Since(started) != 30*time.Millisecond {
			t.Fatalf("runtime monitor decision: error=%v exec-calls=%d elapsed=%v", err, execCalls.Load(), time.Since(started))
		}
		cancel()
		if err := <-readyDone; !errors.Is(err, context.Canceled) {
			t.Fatalf("readiness stop = %v", err)
		}
	})
}

func TestExecProbeErrorDescribesExitStatus(t *testing.T) {
	err := (&probe.ExitError{ExitCode: 17}).Error()
	if !strings.Contains(err, "17") {
		t.Fatalf("exit status missing from error: %q", err)
	}
}
