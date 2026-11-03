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

// The fresh-dial arm exists because the two older signals are both structurally
// blind to new connections failing while an established one is perfect — the
// state measured on 2026-09-01, when the probe was alive 291 times running and
// the warm keepalive held a flat 120ms through hours of it.
//
// Its verdict is latched rather than recomputed, and that is the part worth
// pinning down: the real failure rate that day was ~15%, which straddles any
// fixed threshold, so an unlatched verdict flaps for the whole episode.
func TestNewFlowLatch(t *testing.T) {
	// fill records n dials, marking the first bad of them slow-or-failed.
	fill := func(s *State, n, bad int) {
		for i := 0; i < n; i++ {
			if i < bad {
				s.RecordDial(0, errFake{})
			} else {
				s.RecordDial(200, nil)
			}
		}
	}

	t.Run("a partial window below the failure threshold does not latch", func(t *testing.T) {
		// Quality/what. Why: early detection still tolerates a brief hiccup.
		s := &State{}
		fill(s, DialBadEnter-1, DialBadEnter-1)
		if s.SlowNewFlows || s.Health() != HealthOK {
			t.Fatal("latched before enough bad samples were observed")
		}
	})

	t.Run("a healthy leg never latches", func(t *testing.T) {
		s := &State{}
		// REALITY measured 0 bad in 20; hy2 healthy samples are 0.222–0.290s,
		// all well under DialSlowMs.
		fill(s, DialWindow*3, 0)
		if s.SlowNewFlows {
			t.Fatal("latched with a clean window")
		}
	})

	t.Run("slow-but-successful dials count as bad", func(t *testing.T) {
		s := &State{}
		for i := 0; i < DialWindow; i++ {
			// Completed, so err is nil — but the user already waited 2s.
			s.RecordDial(2000, nil)
		}
		if s.DialBad() != DialWindow {
			t.Fatalf("DialBad() = %d, want %d — a slow dial is a bad dial", s.DialBad(), DialWindow)
		}
		if !s.SlowNewFlows {
			t.Fatal("did not latch on an all-slow window")
		}
	})

	t.Run("enters at the threshold and reports degraded, not dead", func(t *testing.T) {
		s := &State{LastProbeVerdict: ProbeAlive}
		fill(s, DialWindow, DialBadEnter)
		if !s.SlowNewFlows {
			t.Fatalf("did not latch at %d/%d bad", s.DialBad(), DialWindow)
		}
		// Degraded, and therefore Usable: the tunnel is passing traffic, and
		// self-healing must not drop the ceiling for a failure no ceiling fixes.
		if got := s.Health(); got != HealthDegraded {
			t.Fatalf("Health() = %q, want %q", got, HealthDegraded)
		}
		if !s.Health().Usable() {
			t.Fatal("degraded must stay usable")
		}
	})

	t.Run("below the enter threshold stays green", func(t *testing.T) {
		s := &State{}
		fill(s, DialWindow, DialBadEnter-1)
		if s.SlowNewFlows {
			t.Fatalf("latched at %d/%d bad, below the %d threshold", s.DialBad(), DialWindow, DialBadEnter)
		}
	})

	t.Run("holds through the rate that would make it flap", func(t *testing.T) {
		// Bad beats have to be SPREAD, not clustered at the front: a burst of
		// four ages out of the window together and takes the count to zero on
		// the way past, which is a property of the test data and not of the
		// path. A real episode arrives at a rate.
		every := func(s *State, n, nth int) {
			for i := 0; i < n; i++ {
				if i%nth == 0 {
					s.RecordDial(0, errFake{})
				} else {
					s.RecordDial(200, nil)
				}
			}
		}
		s := &State{}
		every(s, DialWindow, 4) // 5 bad in 20 — over the enter threshold
		if !s.SlowNewFlows {
			t.Fatalf("setup: %d/%d bad did not latch", s.DialBad(), DialWindow)
		}
		// Drift down to the ~15% that was actually measured on 2026-09-01 —
		// above the leave threshold, below the enter one. An unlatched verdict
		// would drop out and re-enter on every beat, all episode long. Checked
		// on every step, because it is the transition that flaps, not the end
		// state.
		for i := 0; i < DialWindow*2; i++ {
			if i%7 == 0 {
				s.RecordDial(0, errFake{})
			} else {
				s.RecordDial(200, nil)
			}
			if bad := s.DialBad(); bad > DialBadLeave && !s.SlowNewFlows {
				t.Fatalf("beat %d: dropped the verdict at %d/%d bad, inside the hysteresis band", i, bad, DialWindow)
			}
		}
		if bad := s.DialBad(); bad <= DialBadLeave || bad >= DialBadEnter {
			t.Fatalf("setup: %d bad is not between the leave (%d) and enter (%d) thresholds", bad, DialBadLeave, DialBadEnter)
		}
		if !s.SlowNewFlows {
			t.Fatal("dropped the verdict in the hysteresis band")
		}
	})

	t.Run("clears only when the window is genuinely clean", func(t *testing.T) {
		s := &State{}
		fill(s, DialWindow, DialWindow)
		if !s.SlowNewFlows {
			t.Fatal("setup: expected to be latched")
		}
		fill(s, DialWindow, DialBadLeave)
		if s.SlowNewFlows {
			t.Fatalf("still latched at %d/%d bad, at or below the leave threshold %d", s.DialBad(), DialWindow, DialBadLeave)
		}
		if got := s.Health(); got != HealthOK {
			t.Fatalf("Health() = %q, want %q", got, HealthOK)
		}
	})

	t.Run("the window stays bounded", func(t *testing.T) {
		s := &State{}
		fill(s, DialWindow*10, 0)
		if len(s.DialSamples) != DialWindow {
			t.Fatalf("len(DialSamples) = %d, want %d", len(s.DialSamples), DialWindow)
		}
		if cap(s.DialSamples) > DialWindow*2 {
			t.Fatalf("cap(DialSamples) = %d — the backing array is growing behind a fixed window", cap(s.DialSamples))
		}
	})

	t.Run("a real outage still outranks it", func(t *testing.T) {
		s := &State{LastProbeVerdict: ProbeAlive, KeepaliveFails: KeepaliveFailThreshold}
		fill(s, DialWindow, DialWindow)
		if got := s.Health(); got != HealthPathDead {
			t.Fatalf("Health() = %q, want %q — the warm keepalive is still the authority", got, HealthPathDead)
		}
	})
}

type errFake struct{}

func (errFake) Error() string { return "dial failed" }
