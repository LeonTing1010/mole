package utils

import "testing"

// The health verdict must keep the two probes' jurisdictions separate, and the
// separation is finer than "up or down":
//
//   - the direct-UDP probe runs OUTSIDE the tunnel and speaks only about NEW
//     flows: refused → vps-down immediately; a silent streak → degraded, because
//     ~20% of its fresh-five-tuple datagrams are lost on this path even while
//     the tunnel carries 50 Mbps, and calling that an outage produced 21 false
//     blackouts in a single day;
//   - the keepalive rides an ESTABLISHED session and is therefore the authority
//     on whether the tunnel passes traffic. When it is failing the path is dead
//     however good a fresh datagram looks — that asymmetry is exactly the
//     per-five-tuple throttling signature;
//   - no local IPv4 is neither: it is this machine's uplink being gone, and
//     nothing about the far end is knowable until it returns.
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
		{"probe silent at threshold is degraded, not an outage", ProbeSilentThreshold, ProbeSilent, 0, HealthDegraded},
		{"probe error streak is degraded too", ProbeSilentThreshold, ProbeError, 0, HealthDegraded},
		{"keepalive outranks a silent probe", ProbeSilentThreshold, ProbeSilent, KeepaliveFailThreshold, HealthPathDead},
		{"refused flips immediately", 1, ProbeRefused, 0, HealthVPSDown},
		{"refused wins over keepalive blackhole", 2, ProbeRefused, KeepaliveFailThreshold, HealthVPSDown},
		{"stale refused verdict without fails stays green", 0, ProbeRefused, 0, HealthOK},
		{"no local uplink is not the path's fault", 1, ProbeNoLink, 0, HealthNoLink},
		{"no uplink outranks the keepalive it also broke", 1, ProbeNoLink, KeepaliveFailThreshold, HealthNoLink},
		{"stale no-link verdict without fails stays green", 0, ProbeNoLink, 0, HealthOK},
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

// Usable is what callers ask when deciding whether to act on the verdict.
// Degraded must answer yes: the whole reason it was split out of
// HealthPathDead is that the tunnel is still carrying traffic, and treating it
// as an outage is what threw away self-healing's clean streak and kept the
// ceiling from ever climbing back.
func TestHealthUsable(t *testing.T) {
	for h, want := range map[Health]bool{
		HealthOK:       true,
		HealthDegraded: true,
		HealthPathDead: false,
		HealthVPSDown:  false,
		HealthNoLink:   false,
	} {
		if got := h.Usable(); got != want {
			t.Errorf("%s.Usable() = %v, want %v", h, got, want)
		}
	}
}
