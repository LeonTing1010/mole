package utils

import "testing"

// The health verdict must keep the two probes' jurisdictions separate: the
// direct-UDP probe (outside the tunnel) distinguishes a dead hy2 process
// (refused → vps-down, immediate) from a dark path (silent streak →
// path-blackhole); the keepalive streak covers "VPS answers small stateless
// probes but the established tunnel passes nothing". The dangerous case is
// the path blackhole — which the old tunneled junk-byte probe reported as
// 🟢 healthy for its entire duration.
func TestStateHealth(t *testing.T) {
	cases := []struct {
		name           string
		probeFails     int
		probeVerdict   ProbeVerdict
		keepaliveFails int
		want           Health
	}{
		{"all green", 0, ProbeAlive, 0, HealthOK},
		{"keepalive hiccup below threshold", 0, ProbeAlive, KeepaliveFailThreshold - 1, HealthOK},
		{"keepalive streak at threshold", 0, ProbeAlive, KeepaliveFailThreshold, HealthPathDead},
		{"probe silent below threshold", ProbeSilentThreshold - 1, ProbeSilent, 0, HealthOK},
		{"probe silent at threshold", ProbeSilentThreshold, ProbeSilent, 0, HealthPathDead},
		{"probe error streak also means dark", ProbeSilentThreshold, ProbeError, 0, HealthPathDead},
		{"refused flips immediately", 1, ProbeRefused, 0, HealthVPSDown},
		{"refused wins over keepalive blackhole", 2, ProbeRefused, KeepaliveFailThreshold, HealthVPSDown},
		{"stale refused verdict without fails stays green", 0, ProbeRefused, 0, HealthOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &State{
				ConsecutiveFails: c.probeFails,
				LastProbeVerdict: c.probeVerdict,
				KeepaliveFails:   c.keepaliveFails,
			}
			if got := s.Health(); got != c.want {
				t.Errorf("Health() = %s, want %s (probe %s ×%d, keepalive ×%d)",
					got, c.want, c.probeVerdict, c.probeFails, c.keepaliveFails)
			}
		})
	}
}
