package core

import (
	"strings"
	"testing"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

// Quality/what: only failures dialing this node's REALITY endpoint accelerate sampling.
func TestApplicationNodeDialSensorFramesAndFiltersEvidence(t *testing.T) {
	// Why: an origin failure or stale-node error must not change current leg sampling.
	var sensor nodeDialSensor
	sensor.configure("192.0.2.10:443")
	line := "\x1b[31mERROR\x1b[0m connection: open connection to chatgpt.com:443 using outbound/vless[proxy-reality]: dial tcp 192.0.2.10:443: i/o timeout\n"
	for _, b := range []byte(line) {
		_, _ = sensor.Write([]byte{b})
	}
	if got := sensor.snapshot(); got.sequence != 1 || got.last.IsZero() {
		t.Fatalf("split failure lost: %+v", got)
	}
	for _, bad := range []string{
		strings.ReplaceAll(line, "192.0.2.10:443", "192.0.2.11:443"),
		strings.ReplaceAll(line, "[proxy-reality]", "[direct]"),
		"open connection to chatgpt.com:443 using outbound/vless[proxy-reality]: read tcp 192.0.2.10:443: connection reset by peer\n",
		"dial tcp 192.0.2.10:443: i/o timeout\n",
	} {
		_, _ = sensor.Write([]byte(bad))
	}
	if got := sensor.snapshot().sequence; got != 1 {
		t.Fatalf("unrelated errors counted: %d", got)
	}
	_, _ = sensor.Write([]byte(strings.Repeat("x", 100000) + "\n" + line))
	if sensor.snapshot().sequence != 2 {
		t.Fatal("oversized log record broke the following valid failure")
	}
	sensor.configure("192.0.2.11:443")
	if sensor.snapshot().sequence != 0 {
		t.Fatal("new node inherited previous evidence")
	}
	sensor.configure("[2001:db8::1]:443")
	_, _ = sensor.Write([]byte("open connection to chatgpt.com:443 using outbound/vless[proxy-reality]: dial tcp [2001:db8::1]:443: connect: connection refused\n"))
	if sensor.snapshot().sequence != 1 {
		t.Fatal("native IPv6 node refusal lost")
	}
}

// Quality/what-x-what: passive evidence requests a real measurement, never invents failures.
func TestApplicationDialFailureShortensSamplingWithoutBypassingGuards(t *testing.T) {
	// Why: a sparse clean probe window can miss real application dial failures.
	t.Setenv("HOME", t.TempDir())
	now := time.Now()
	s := &Supervisor{keepaliveEvery: 15 * time.Second, state: &utils.State{}, applicationDialSnapshotFn: func() nodeDialSnapshot { return nodeDialSnapshot{sequence: 3, last: now} }}
	for i := 0; i < utils.DialWindow; i++ {
		s.state.RecordDial(100, nil)
	}
	if !s.shouldSampleFreshDial(now.Add(-20 * time.Second)) {
		t.Fatal("application failure did not bring forward clean-window sampling")
	}
	if s.shouldSampleFreshDial(now.Add(-5 * time.Second)) {
		t.Fatal("error burst bypassed normal sampling rate limit")
	}
	if s.state.DialFails() != 0 || s.onFallback() {
		t.Fatal("passive log evidence became a failed probe or handoff")
	}
	if s.state.ApplicationNodeDialFailures != 3 || !s.state.LastApplicationNodeDialFailureAt.Equal(now) {
		t.Fatal("application evidence was hidden from persisted diagnostic state")
	}
	s.applicationDialSnapshotFn = func() nodeDialSnapshot { return nodeDialSnapshot{last: now.Add(-time.Minute)} }
	if s.shouldSampleFreshDial(now.Add(-20 * time.Second)) {
		t.Fatal("stale application evidence accelerated sampling")
	}
	s.applicationDialSnapshotFn = func() nodeDialSnapshot { return nodeDialSnapshot{last: now} }
	s.state.LinkBad = true
	if s.shouldSampleFreshDial(now.Add(-time.Minute)) {
		t.Fatal("local-link gate bypassed")
	}
	s.state.LinkBad = false
	s.hasReality = true
	s.fallback.on = true
	if s.shouldSampleFreshDial(now.Add(-20 * time.Second)) {
		t.Fatal("inactive primary errors accelerated the active fallback")
	}
	s.fallback.on = false
	if err := utils.WriteLeg(utils.LegHy2); err != nil {
		t.Fatal(err)
	}
	if s.shouldSampleFreshDial(now.Add(-20 * time.Second)) {
		t.Fatal("inactive REALITY errors changed pinned HY2 sampling")
	}
}
