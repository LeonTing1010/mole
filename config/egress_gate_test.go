package config

import (
	"testing"

	"github.com/LeonTing1010/mole/core"
)

// Quality/what-x-what: compare real external TLS through distinct transports.
func TestExternalGateForwardersCannotFollowTheTrafficSelector(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// Why: a fast loopback keeper hid foreign-site timeouts on October 7.
	cfg, err := BuildWithStandby(testHy2URI, testRealityURI)
	if err != nil {
		t.Fatal(err)
	}
	for tag, port := range map[string]int{"egress-reality-in": 8100, "egress-hy2-in": 8101} {
		found := false
		for _, in := range cfg.Inbounds {
			if in.Tag != tag {
				continue
			}
			found = true
			if in.Type != "direct" || in.Listen != "127.0.0.1" || in.ListenPort != port || in.OverrideAddress != "cloudflare.com" || in.OverridePort != 443 {
				t.Fatalf("%s must expose only the fixed external TLS target on loopback: %+v", tag, in)
			}
		}
		if !found {
			t.Errorf("missing real egress probe for %s", tag)
		}
		found = false
		for _, r := range cfg.Route.Rules {
			for _, in := range r.Inbound {
				if in == tag {
					found = true
					if r.Outbound == core.ProxySelectorTag || r.Outbound == "direct" {
						t.Fatalf("%s can measure the wrong transport: %s", tag, r.Outbound)
					}
				}
			}
		}
		if !found {
			t.Errorf("no transport-bound rule for %s", tag)
		}
	}
}

// Quality/what-x-what: every independent origin must reach the intended transport.
func TestPerformanceSecondaryOriginsAreFixedAndTransportBound(t *testing.T) {
	// Why: a declared secondary target does nothing if its listener/routing is missing.
	t.Setenv("HOME", t.TempDir())
	cfg, err := BuildWithStandby(testHy2URI, testRealityURI)
	if err != nil {
		t.Fatal(err)
	}
	targets := core.LegEgressTargets()
	if len(targets) < 2 || targets[0].Host == targets[1].Host {
		t.Fatal("external probes have no independent origin")
	}
	for _, target := range targets {
		for _, ep := range []struct {
			tag  string
			port int
			out  string
		}{
			{target.RealityTag, target.RealityPort, core.RealityLegTag}, {target.Hy2Tag, target.Hy2Port, core.Hy2ProbeSelectorTag},
		} {
			listener, route := false, false
			for _, in := range cfg.Inbounds {
				if in.Tag == ep.tag {
					listener = in.Listen == "127.0.0.1" && in.ListenPort == ep.port && in.Type == "direct" && in.OverrideAddress == target.Host && in.OverridePort == 443
				}
			}
			for _, r := range cfg.Route.Rules {
				for _, tag := range r.Inbound {
					if tag == ep.tag {
						route = r.Outbound == ep.out
					}
				}
			}
			if !listener || !route {
				t.Fatalf("origin %s leg %s: listener=%t route=%t", target.Host, ep.tag, listener, route)
			}
		}
	}
}

// Quality/what: the UDP gate must measure the ceiling it proposes to select.
func TestHy2GateCanMeasureEveryConfiguredCeiling(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// Why: a top-rung session cannot qualify a different, throttled rung.
	cfg, err := BuildWithStandby(testHy2URI, testRealityURI)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range cfg.Outbounds {
		if o.Tag == "probe-hy2" {
			if o.Type != "selector" || len(o.Outbounds) < 2 {
				t.Fatal("UDP probe must be able to select the scheduled rung")
			}
			for _, member := range o.Outbounds {
				if member == core.RealityLegTag || member == core.ProxySelectorTag {
					t.Fatal("UDP probe selector can leave the UDP leg")
				}
			}
			return
		}
	}
	t.Fatal("no independently selectable UDP probe leg")
}
