package manifest

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// Health keeps readiness and opt-in restart checks independent.
// A nil Check means process-only readiness; a nil Healthy disables probe-driven restarts.
type Health struct {
	Check          *Probe       `yaml:"check,omitempty"`
	Ready          ReadyRule    `yaml:"ready,omitempty"`
	Healthy        *HealthyRule `yaml:"healthy,omitempty"`
	StartupTimeout Duration     `yaml:"startup-timeout,omitempty"`
}

// ReadyRule defines consecutive outcomes that grant or revoke readiness.
type ReadyRule struct {
	Successes       Threshold `yaml:"successes,omitempty"`
	UnreadyFailures Threshold `yaml:"unready-failures,omitempty"`
}

// HealthyRule opts into restarts, watching the process unless it has its own Check.
type HealthyRule struct {
	UnhealthyFailures Threshold `yaml:"unhealthy-failures"`
	Check             *Probe    `yaml:"check,omitempty"`
}

// Probe selects exactly one check type. Zero timings remain unspecified.
type Probe struct {
	HTTP     *HTTPCheck    `yaml:"http,omitempty"`
	TCP      *TCPCheck     `yaml:"tcp,omitempty"`
	Process  *ProcessCheck `yaml:"process,omitempty"`
	Exec     *ExecCheck    `yaml:"exec,omitempty"`
	Interval Duration      `yaml:"interval,omitempty"`
	Timeout  Duration      `yaml:"timeout,omitempty"`
}

// HTTPCheck probes a path on an application's named HTTP or HTTPS port.
type HTTPCheck struct {
	Path         string `yaml:"path"`
	Port         string `yaml:"port"`
	ExpectStatus []int  `yaml:"expect-status,omitempty"`
}

// TCPCheck opens a TCP connection to an application's named port.
type TCPCheck struct {
	Port string `yaml:"port"`
}

// ProcessCheck checks that a named process or executable is running.
type ProcessCheck struct {
	Name string `yaml:"name"`
}

// ExecCheck runs a command whose zero exit status indicates success.
type ExecCheck struct {
	Command Command `yaml:"command"`
}

func (a Application) normalizeHealth(path string) error {
	h := a.Health
	if h == nil {
		return nil
	}
	path += ".health"
	if h.Check != nil {
		if err := h.Check.validate(a.Ports, path+".check"); err != nil {
			return err
		}
	}
	if h.Ready.Successes == 0 {
		h.Ready.Successes = 1
	}
	if h.Ready.UnreadyFailures == 0 {
		h.Ready.UnreadyFailures = 3
	}
	if h.Healthy != nil {
		if h.Healthy.UnhealthyFailures == 0 {
			return fmt.Errorf("%s.healthy.unhealthy-failures: a positive failure threshold is required", path)
		}
		// A restart rule never inherits the readiness probe.
		if h.Healthy.Check != nil {
			return h.Healthy.Check.validate(a.Ports, path+".healthy.check")
		}
	}
	return nil
}

func (p Probe) validate(ports map[string]Port, path string) error {
	count := 0
	for _, present := range []bool{p.HTTP != nil, p.TCP != nil, p.Process != nil, p.Exec != nil} {
		if present {
			count++
		}
	}
	if count != 1 {
		return fmt.Errorf("%s: select exactly one of http, tcp, process or exec", path)
	}
	if p.HTTP != nil {
		if _, err := url.ParseRequestURI(p.HTTP.Path); err != nil || !strings.HasPrefix(p.HTTP.Path, "/") {
			return fmt.Errorf("%s.http.path: an absolute HTTP request path is required", path)
		}
		if err := validateProbePort(ports, p.HTTP.Port, path+".http.port", []string{"http", "https"}); err != nil {
			return err
		}
		if p.HTTP.ExpectStatus != nil && len(p.HTTP.ExpectStatus) == 0 {
			return fmt.Errorf("%s.http.expect-status: at least one status is required when specified", path)
		}
		for _, status := range p.HTTP.ExpectStatus {
			if status < 100 || status > 599 {
				return fmt.Errorf("%s.http.expect-status: expected HTTP status codes between 100 and 599", path)
			}
		}
	}
	if p.TCP != nil {
		return validateProbePort(ports, p.TCP.Port, path+".tcp.port", []string{"http", "https", "tcp"})
	}
	if p.Process != nil && strings.TrimSpace(p.Process.Name) == "" {
		return fmt.Errorf("%s.process.name: a process name is required", path)
	}
	if p.Exec != nil && len(p.Exec.Command) == 0 {
		return fmt.Errorf("%s.exec.command: a command is required", path)
	}
	return nil
}

func validateProbePort(ports map[string]Port, name, path string, protocols []string) error {
	port, exists := ports[name]
	if !exists {
		return fmt.Errorf("%s: port %q is not declared on this application", path, name)
	}
	if !slices.Contains(protocols, port.Protocol) {
		return fmt.Errorf("%s: port %q has incompatible protocol %q", path, name, port.Protocol)
	}
	return nil
}
