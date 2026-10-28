package manifest_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/manifest"
	"github.com/goccy/go-yaml"
)

const projectWithPorts = imageProject + `    ports:
      http: {number: 8080, protocol: http}
      tls: {number: 8443, protocol: https}
      stream: {number: 9000, protocol: tcp}
      datagrams: {number: 9000, protocol: udp}
`

func healthInput(config string) []byte {
	return []byte(projectWithPorts + "    health: " + config + "\n")
}

func TestParseReadinessProbes(t *testing.T) {
	for _, tc := range []struct{ kind, config string }{
		{"http", `{http: {path: /ready, port: http, expect-status: [200, 204]}}`},
		{"tcp", `{tcp: {port: stream}}`},
		{"process", `{process: {name: worker}}`},
		{"exec", `{exec: {command: [/bin/check, ""]}}`},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			project, err := manifest.Parse(healthInput("{check: " + tc.config + "}"))
			if err != nil {
				t.Fatal(err)
			}
			health := project.Applications["api"].Health
			if health == nil || health.Check == nil {
				t.Fatal("missing readiness check")
			}
			probe := health.Check
			switch tc.kind {
			case "http":
				if probe.HTTP == nil || probe.HTTP.Path != "/ready" || probe.HTTP.Port != "http" || len(probe.HTTP.ExpectStatus) != 2 || probe.HTTP.ExpectStatus[1] != 204 {
					t.Fatalf("HTTP probe = %#v", probe.HTTP)
				}
			case "tcp":
				if probe.TCP == nil || probe.TCP.Port != "stream" {
					t.Fatalf("TCP probe = %#v", probe.TCP)
				}
			case "process":
				if probe.Process == nil || probe.Process.Name != "worker" {
					t.Fatalf("process probe = %#v", probe.Process)
				}
			case "exec":
				if probe.Exec == nil || len(probe.Exec.Command) != 2 || probe.Exec.Command[0] != "/bin/check" || probe.Exec.Command[1] != "" {
					t.Fatalf("exec probe = %#v", probe.Exec)
				}
			}
			if health.Ready.Successes != 1 || health.Ready.UnreadyFailures != 3 || health.Healthy != nil {
				t.Fatalf("unexpected readiness/restart defaults: %#v", health)
			}
			if probe.Interval != 0 || probe.Timeout != 0 || health.StartupTimeout != 0 {
				t.Fatal("unspecified probe timings were invented")
			}
		})
	}
}

func TestParseHealthTimingsAndRules(t *testing.T) {
	project, err := manifest.Parse(healthInput(`{
      check: {http: {path: /ready, port: http}, interval: 5s, timeout: 2s},
      ready: {successes: 2, unready-failures: 4},
      healthy: {unhealthy-failures: 6},
      startup-timeout: 2m
    }`))
	if err != nil {
		t.Fatal(err)
	}
	health := project.Applications["api"].Health
	if time.Duration(health.Check.Interval) != 5*time.Second || time.Duration(health.Check.Timeout) != 2*time.Second || time.Duration(health.StartupTimeout) != 2*time.Minute {
		t.Fatalf("probe timings were not preserved: %#v", health)
	}
	if health.Ready.Successes != 2 || health.Ready.UnreadyFailures != 4 || health.Healthy.UnhealthyFailures != 6 {
		t.Fatalf("check thresholds were not preserved: %#v", health)
	}
	if health.Healthy.Check != nil {
		t.Fatal("restart rule inherited the readiness probe")
	}
}

func TestParseSeparateRestartProbe(t *testing.T) {
	project, err := manifest.Parse(healthInput(`{check: {http: {path: /ready, port: http}}, healthy: {unhealthy-failures: 6, check: {exec: {command: [/bin/self-check]}, interval: 10s}}}`))
	if err != nil {
		t.Fatal(err)
	}
	health := project.Applications["api"].Health
	if health.Check.HTTP == nil || health.Healthy.Check.Exec == nil || health.Healthy.Check.Exec.Command[0] != "/bin/self-check" || time.Duration(health.Healthy.Check.Interval) != 10*time.Second {
		t.Fatalf("readiness and restart probes were not kept separate: %#v", health)
	}
}

func TestHealthManifestRoundTrip(t *testing.T) {
	project, err := manifest.Parse(healthInput(`{check: {http: {path: /ready, port: http}, interval: 5s, timeout: 2s}, healthy: {unhealthy-failures: 6, check: {exec: {command: [/bin/check, ""]}}}, startup-timeout: 2m}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := yaml.Marshal(project)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := manifest.Parse(encoded)
	if err != nil {
		t.Fatalf("serialized manifest is invalid: %v", err)
	}
	if !reflect.DeepEqual(decoded, project) {
		t.Errorf("serialization changed the manifest: got %#v, want %#v", decoded, project)
	}
}

func TestParseProcessOnlyReadiness(t *testing.T) {
	project, err := manifest.Parse([]byte(imageProject))
	if err != nil {
		t.Fatal(err)
	}
	if project.Applications["api"].Health != nil {
		t.Fatal("health configuration was silently created")
	}
	for _, config := range []string{`{}`, `{healthy: {unhealthy-failures: 6}}`, `{check: {exec: {command: [/bin/check]}}}`} {
		project, err := manifest.Parse([]byte(imageProject + "    health: " + config + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		health := project.Applications["api"].Health
		if !strings.Contains(config, "exec:") && health.Check != nil {
			t.Fatal("readiness probe was silently created")
		}
	}
}

func TestParseProbeTransportCompatibility(t *testing.T) {
	for _, check := range []string{`{http: {path: /ready, port: tls}}`, `{tcp: {port: http}}`, `{tcp: {port: tls}}`} {
		if _, err := manifest.Parse(healthInput("{check: " + check + "}")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestParseReadinessDefaultsIndependently(t *testing.T) {
	for _, tc := range []struct {
		config              string
		successes, failures manifest.Threshold
	}{
		{`{ready: {successes: 2}}`, 2, 3},
		{`{ready: {unready-failures: 5}}`, 1, 5},
	} {
		project, err := manifest.Parse(healthInput(tc.config))
		if err != nil {
			t.Fatal(err)
		}
		ready := project.Applications["api"].Health.Ready
		if ready.Successes != tc.successes || ready.UnreadyFailures != tc.failures {
			t.Errorf("readiness rules = %#v", ready)
		}
	}
}

func TestParseRejectsProbePortOnAnotherApplication(t *testing.T) {
	input := imageProject + `    health: {check: {tcp: {port: backend}}}
  worker:
    source: {image: worker:1}
    ports: {backend: {number: 9000, protocol: tcp}}
`
	if _, err := manifest.Parse([]byte(input)); err == nil || !strings.Contains(err.Error(), ".health.check.tcp.port") {
		t.Fatalf("probe referenced another application's port: %v", err)
	}
}

func TestParseRejectsInvalidHealth(t *testing.T) {
	for _, tc := range []struct{ name, config, signal string }{
		{"missing probe type", `{check: {interval: 5s}}`, ".health.check"},
		{"multiple probe types", `{check: {http: {path: /ready, port: http}, tcp: {port: stream}}}`, ".health.check"},
		{"unknown named port", `{check: {http: {path: /ready, port: missing}}}`, ".port"},
		{"numeric port", `{check: {http: {path: /ready, port: 8080}}}`, "string"},
		{"missing port", `{check: {tcp: {}}}`, ".port"},
		{"http over tcp", `{check: {http: {path: /ready, port: stream}}}`, ".port"},
		{"http over udp", `{check: {http: {path: /ready, port: datagrams}}}`, ".port"},
		{"tcp over udp", `{check: {tcp: {port: datagrams}}}`, ".port"},
		{"missing HTTP path", `{check: {http: {port: http}}}`, ".path"},
		{"relative HTTP path", `{check: {http: {path: ready, port: http}}}`, ".path"},
		{"URL instead of path", `{check: {http: {path: 'https://other/ready', port: http}}}`, ".path"},
		{"malformed path", `{check: {http: {path: '/bad%zz', port: http}}}`, ".path"},
		{"invalid status", `{check: {http: {path: /ready, port: http, expect-status: [600]}}}`, "expect-status"},
		{"low status", `{check: {http: {path: /ready, port: http, expect-status: [99]}}}`, "expect-status"},
		{"empty statuses", `{check: {http: {path: /ready, port: http, expect-status: []}}}`, "expect-status"},
		{"quoted status", `{check: {http: {path: /ready, port: http, expect-status: ["200"]}}}`, "integer"},
		{"missing process", `{check: {process: {}}}`, ".name"},
		{"empty command", `{check: {exec: {command: []}}}`, "command"},
		{"missing command", `{check: {exec: {}}}`, "command"},
		{"blank executable", `{check: {exec: {command: [" "]}}}`, "command"},
		{"numeric argument", `{check: {exec: {command: [/bin/check, 1]}}}`, "command"},
		{"scalar command", `{check: {exec: {command: /bin/check}}}`, ""},
		{"zero interval", `{check: {process: {name: app}, interval: 0s}}`, "duration"},
		{"negative timeout", `{check: {process: {name: app}, timeout: -1s}}`, "duration"},
		{"numeric duration", `{startup-timeout: 10}`, "duration"},
		{"invalid duration", `{startup-timeout: tomorrow}`, "duration"},
		{"zero successes", `{ready: {successes: 0}}`, "positive"},
		{"negative failures", `{ready: {unready-failures: -1}}`, "positive"},
		{"fractional threshold", `{ready: {successes: 1.5}}`, "integer"},
		{"quoted threshold", `{ready: {successes: "2"}}`, "integer"},
		{"missing restart threshold", `{healthy: {}}`, "unhealthy-failures"},
		{"zero restart threshold", `{healthy: {unhealthy-failures: 0}}`, "positive"},
		{"restart probe missing port", `{healthy: {unhealthy-failures: 6, check: {tcp: {port: missing}}}}`, ".healthy.check.tcp.port"},
		{"unknown probe field", `{check: {http: {path: /ready, port: http, host: other}}}`, "host"},
		{"misspelled threshold", `{ready: {unready_failures: 3}}`, "unready_failures"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project, err := manifest.Parse(healthInput(tc.config))
			if err == nil || project != nil {
				t.Fatalf("invalid health configuration accepted: project=%#v error=%v", project, err)
			}
			if !strings.Contains(err.Error(), tc.signal) {
				t.Errorf("error = %q, want signal %q", err, tc.signal)
			}
		})
	}
}
