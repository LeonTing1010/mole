package utils

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// The paired A/B experiment declaration, read at `mole up` time to materialize
// probe-only outbounds and read again by the supervisor to know what to sample.
//
// It exists because every previous attempt to fix the UDP leg — obfs, a pinned
// certificate, port hopping — was judged by running it and seeing whether the
// evening felt better. That cannot work here. The disease is intermittent at a
// scale that swamps any single trial: 73% of one afternoon (2026-09-01) and 0%
// of others, on the same node over the same route. A change measured on a good
// evening looks like a cure and a change measured on a bad one looks like a
// regression, and both readings have happened.
//
// The only design that survives that is a PAIRED one: two outbounds differing
// in exactly one variable, alive at the same time, sampled in the same seconds.
// Then the episode is common-mode and cancels, and what is left is the variable.
// This is the same reason the fresh-dial arm dials REALITY before failing over
// rather than trusting a threshold.

// ExperimentArm is one side of a comparison: the base hy2 outbound with at most
// a couple of fields overridden. Everything not named here is copied from the
// live outbound verbatim — including the Brutal ceiling — so the two arms differ
// only in what is under test and any ceiling effect is common-mode.
type ExperimentArm struct {
	// Name goes into the outbound tag (ab-<name>) and every report. Keep it
	// short and descriptive of the VARIABLE, not the verdict: "p20443", not
	// "the good one".
	Name string `json:"name"`
	// Note is free text shown in `mole ab`.
	Note string `json:"note,omitempty"`
	// Port overrides the UDP port. Zero keeps the live one — which is how the
	// control arm is declared.
	Port int `json:"port,omitempty"`
	// Server overrides the host, for comparing two nodes (a different provider,
	// a different region, a different server implementation on the same port).
	// Empty keeps the live one.
	Server string `json:"server,omitempty"`
	// Samples is the rolling result window, newest last. Written by the daemon,
	// not by whoever declares the experiment.
	Samples []DialSample `json:"samples,omitempty"`
}

// Experiment is one comparison in flight. Only one at a time, deliberately: two
// running together would each add probe traffic to the other's measurement, and
// the whole point is to hold everything but one variable still.
type Experiment struct {
	Name    string          `json:"name"`
	Note    string          `json:"note,omitempty"`
	Arms    []ExperimentArm `json:"arms"`
	Started string          `json:"started,omitempty"`
}

// ExperimentWindow is how many paired beats each arm's verdict looks at. At the
// 20s sampling cadence this is ~30 minutes, long enough that an episode's edges
// do not dominate and short enough to still be about right now.
const ExperimentWindow = 90

// ArmBad counts slow-or-failed samples in an arm's window, using the same
// threshold as the fresh-dial latch (DialSlowMs) so the two instruments agree
// about what "bad" means.
func (a *ExperimentArm) ArmBad() int {
	bad := 0
	for _, s := range a.Samples {
		if !s.OK || s.Ms > DialSlowMs {
			bad++
		}
	}
	return bad
}

// Record appends one result, trimming to the window.
func (a *ExperimentArm) Record(ms int, err error) {
	s := DialSample{Ms: ms, OK: err == nil}
	if err != nil {
		s.Ms = 0
	}
	a.Samples = append(a.Samples, s)
	if n := len(a.Samples); n > ExperimentWindow {
		a.Samples = append([]DialSample(nil), a.Samples[n-ExperimentWindow:]...)
	}
}

// Validate rejects a declaration that cannot produce a usable comparison.
// Called before writing and again after reading, because a hand-edited file is
// an expected input — the whole file is meant to be readable and editable.
func (e *Experiment) Validate() error {
	if e == nil || len(e.Arms) < 2 {
		return fmt.Errorf("an experiment needs at least 2 arms (a control and the thing under test)")
	}
	seen := map[string]bool{}
	for i := range e.Arms {
		n := strings.TrimSpace(e.Arms[i].Name)
		if n == "" {
			return fmt.Errorf("arm %d has no name", i)
		}
		if strings.ContainsAny(n, " \t/") {
			return fmt.Errorf("arm name %q must be a single word — it becomes an outbound tag", n)
		}
		if seen[n] {
			return fmt.Errorf("two arms are both named %q", n)
		}
		seen[n] = true
		e.Arms[i].Name = n
	}
	return nil
}

// ReadExperiment returns the declared experiment, or nil when none is running.
// A malformed file reads as "no experiment" rather than an error, for the same
// reason a malformed ceiling pin does: a broken declaration must cost some
// probes, never the tunnel.
func ReadExperiment() *Experiment {
	b, err := os.ReadFile(ExperimentPath())
	if err != nil {
		return nil
	}
	var e Experiment
	if err := json.Unmarshal(b, &e); err != nil {
		return nil
	}
	if err := e.Validate(); err != nil {
		return nil
	}
	return &e
}

// WriteExperiment persists the declaration, results included — the daemon uses
// it to checkpoint the rolling windows so a restart does not throw away an
// evening of paired samples.
func WriteExperiment(e *Experiment) error {
	if err := e.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	path := ExperimentPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ClearExperiment ends the experiment. A missing file is success.
func ClearExperiment() error {
	err := os.Remove(ExperimentPath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
