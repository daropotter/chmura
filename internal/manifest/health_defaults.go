package manifest

import (
	"slices"
	"time"
)

// Effective returns an independent copy of validated health configuration with
// defaults resolved for planning or execution. A nil configuration stays nil.
// It does not modify the declaration, enable probes, or enable restart rules.
func (h *Health) Effective() *Health {
	if h == nil {
		return nil
	}
	effective := *h
	effective.Ready = h.Ready.withDefaults()
	if effective.StartupTimeout == 0 {
		effective.StartupTimeout = Duration(2 * time.Minute)
	}
	effective.Check = effectiveProbe(h.Check)
	if h.Healthy != nil {
		healthy := *h.Healthy
		healthy.Check = effectiveProbe(h.Healthy.Check)
		effective.Healthy = &healthy
	}
	return &effective
}

func (r ReadyRule) withDefaults() ReadyRule {
	if r.Successes == 0 {
		r.Successes = 1
	}
	if r.UnreadyFailures == 0 {
		r.UnreadyFailures = 3
	}
	return r
}

func effectiveProbe(p *Probe) *Probe {
	if p == nil {
		return nil
	}
	effective := *p
	if effective.Interval == 0 {
		effective.Interval = Duration(5 * time.Second)
	}
	if effective.Timeout == 0 {
		effective.Timeout = Duration(2 * time.Second)
	}
	if p.HTTP != nil {
		http := *p.HTTP
		http.ExpectStatus = slices.Clone(p.HTTP.ExpectStatus)
		if http.ExpectStatus == nil {
			http.ExpectStatus = []int{200}
		}
		effective.HTTP = &http
	}
	if p.TCP != nil {
		tcp := *p.TCP
		effective.TCP = &tcp
	}
	if p.Process != nil {
		process := *p.Process
		effective.Process = &process
	}
	if p.Exec != nil {
		exec := *p.Exec
		exec.Command = slices.Clone(p.Exec.Command)
		effective.Exec = &exec
	}
	return &effective
}
