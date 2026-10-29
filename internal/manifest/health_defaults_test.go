package manifest_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/manifest"
)

func TestEffectiveHealthDefaults(t *testing.T) {
	project, err := manifest.Parse(healthInput(`{check: {http: {path: /ready, port: http}}}`))
	if err != nil {
		t.Fatal(err)
	}
	declared := project.Applications["api"].Health
	effective := declared.Effective()
	if time.Duration(effective.StartupTimeout) != 2*time.Minute || time.Duration(effective.Check.Interval) != 5*time.Second || time.Duration(effective.Check.Timeout) != 2*time.Second {
		t.Fatalf("effective timings = %#v", effective)
	}
	if !reflect.DeepEqual(effective.Check.HTTP.ExpectStatus, []int{200}) {
		t.Errorf("expected HTTP statuses = %v, want [200]", effective.Check.HTTP.ExpectStatus)
	}
	if effective.Ready.Successes != 1 || effective.Ready.UnreadyFailures != 3 || effective.Healthy != nil {
		t.Errorf("effective readiness/restart rules = %#v", effective)
	}
	if declared.StartupTimeout != 0 || declared.Check.Interval != 0 || declared.Check.Timeout != 0 || declared.Check.HTTP.ExpectStatus != nil {
		t.Fatal("resolving defaults overwrote omitted declaration fields")
	}
}

func TestEffectiveHealthPreservesOverrides(t *testing.T) {
	project, err := manifest.Parse(healthInput(`{
      check: {http: {path: /ready, port: http, expect-status: [204, 206]}, interval: 9s, timeout: 4s},
      ready: {successes: 2, unready-failures: 5},
      healthy: {unhealthy-failures: 7, check: {exec: {command: [/bin/self-check]}, interval: 11s, timeout: 3s}},
      startup-timeout: 3m
    }`))
	if err != nil {
		t.Fatal(err)
	}
	declared := project.Applications["api"].Health
	if effective := declared.Effective(); !reflect.DeepEqual(effective, declared) {
		t.Errorf("explicit overrides changed: got %#v, want %#v", effective, declared)
	}
}

func TestEffectiveHealthResolvesTimingsIndependently(t *testing.T) {
	for _, tc := range []struct {
		config            string
		interval, timeout time.Duration
	}{
		{`{check: {tcp: {port: stream}, interval: 9s}}`, 9 * time.Second, 2 * time.Second},
		{`{check: {tcp: {port: stream}, timeout: 4s}}`, 5 * time.Second, 4 * time.Second},
	} {
		project, err := manifest.Parse(healthInput(tc.config))
		if err != nil {
			t.Fatal(err)
		}
		probe := project.Applications["api"].Health.Effective().Check
		if time.Duration(probe.Interval) != tc.interval || time.Duration(probe.Timeout) != tc.timeout {
			t.Errorf("effective probe = %#v", probe)
		}
	}
}

func TestEffectiveHealthPreservesProcessOnlyModes(t *testing.T) {
	var absent *manifest.Health
	if absent.Effective() != nil {
		t.Fatal("absent health configuration was silently created")
	}
	for _, config := range []string{`{}`, `{healthy: {unhealthy-failures: 6}}`, `{check: {process: {name: app}}, healthy: {unhealthy-failures: 6}}`} {
		project, err := manifest.Parse(healthInput(config))
		if err != nil {
			t.Fatal(err)
		}
		declared := project.Applications["api"].Health
		effective := declared.Effective()
		if (effective.Check == nil) != (declared.Check == nil) || (effective.Healthy == nil) != (declared.Healthy == nil) {
			t.Fatal("defaults enabled a readiness probe or restart rule")
		}
		if effective.Healthy != nil && (effective.Healthy.Check != nil || effective.Healthy.UnhealthyFailures != 6) {
			t.Fatal("process-only restart rule changed or inherited readiness")
		}
	}
}

func TestEffectiveHealthCopiesAreIndependent(t *testing.T) {
	for _, tc := range []struct{ kind, probe string }{
		{"http", `{http: {path: /ready, port: http}}`},
		{"http-explicit-status", `{http: {path: /ready, port: http, expect-status: [204, 206]}}`},
		{"tcp", `{tcp: {port: stream}}`},
		{"process", `{process: {name: app}}`},
		{"exec", `{exec: {command: [/bin/check, ""]}}`},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			input := healthInput(`{check: ` + tc.probe + `, healthy: {unhealthy-failures: 6, check: {exec: {command: [/bin/self-check]}}}}`)
			project, err := manifest.Parse(input)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := manifest.Parse(input)
			if err != nil {
				t.Fatal(err)
			}
			declared := project.Applications["api"].Health
			first, second := declared.Effective(), declared.Effective()
			if first.Check.Interval != manifest.Duration(5*time.Second) || first.Check.Timeout != manifest.Duration(2*time.Second) || first.Healthy.Check.Interval != manifest.Duration(5*time.Second) || first.Healthy.Check.Timeout != manifest.Duration(2*time.Second) {
				t.Fatal("readiness or restart probe timings were not resolved")
			}
			first.StartupTimeout = manifest.Duration(time.Minute)
			first.Ready.Successes = 9
			first.Check.Interval = manifest.Duration(time.Second)
			first.Healthy.UnhealthyFailures = 10
			first.Healthy.Check.Exec.Command[0] = "/changed"
			switch tc.kind {
			case "http", "http-explicit-status":
				first.Check.HTTP.Path = "/changed"
				first.Check.HTTP.ExpectStatus[0] = 503
			case "tcp":
				first.Check.TCP.Port = "changed"
			case "process":
				first.Check.Process.Name = "changed"
			case "exec":
				first.Check.Exec.Command[0] = "/changed"
			}
			if !reflect.DeepEqual(project, snapshot) {
				t.Fatal("editing effective configuration changed the original declaration")
			}
			if !reflect.DeepEqual(second, snapshot.Applications["api"].Health.Effective()) {
				t.Fatal("editing one effective configuration changed another")
			}
		})
	}
}
