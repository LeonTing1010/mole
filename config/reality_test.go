package config

import (
	"testing"

	"github.com/LeonTing1010/mole/core"
)

// The client half of REALITY is three things sing-box will not infer: the
// borrowed target as SNI, a forged browser hello, and the keypair. Miss any one
// and the server reports "processed invalid connection" while the client sees a
// bare EOF — a failure that reads like a broken implementation rather than a
// missing field.
func TestParseVLESSReality(t *testing.T) {
	o, err := parseServerURL("vless://00000000-0000-4000-8000-000000000000@198.51.100.10:443" +
		"?encryption=none&security=reality&type=tcp&flow=xtls-rprx-vision&fp=chrome" +
		"&sni=www.apple.com&pbk=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA&sid=0123456789abcdef#icn-test")
	if err != nil {
		t.Fatal(err)
	}
	if o.Type != "vless" || o.UUID != "00000000-0000-4000-8000-000000000000" || o.ServerPort != 443 {
		t.Fatalf("unexpected outbound: %+v", o)
	}
	if o.Flow != "xtls-rprx-vision" {
		t.Errorf("flow = %q", o.Flow)
	}
	if o.Network != "" {
		t.Errorf("network = %q, want empty (sing-box defaults to tcp; the field is rejected here)", o.Network)
	}
	if o.TLS == nil || !o.TLS.Enabled {
		t.Fatal("TLS not enabled")
	}
	if o.TLS.ServerName != "www.apple.com" {
		t.Errorf("server_name = %q", o.TLS.ServerName)
	}
	if o.TLS.UTLS == nil || !o.TLS.UTLS.Enabled || o.TLS.UTLS.Fingerprint != "chrome" {
		t.Errorf("utls = %+v", o.TLS.UTLS)
	}
	if o.TLS.Reality == nil || !o.TLS.Reality.Enabled {
		t.Fatal("reality not enabled")
	}
	if o.TLS.Reality.PublicKey != "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" {
		t.Errorf("public_key = %q", o.TLS.Reality.PublicKey)
	}
	if o.TLS.Reality.ShortID != "0123456789abcdef" {
		t.Errorf("short_id = %q", o.TLS.Reality.ShortID)
	}
}

// insecure is meaningless under REALITY — the certificate is the borrowed
// site's real one — and leaving it on would be a standing invitation to accept
// anything at all if the reality block were ever dropped.
func TestRealityIgnoresAllowInsecure(t *testing.T) {
	o, err := parseServerURL("vless://u@1.2.3.4:443?security=reality&pbk=k&allowInsecure=1")
	if err != nil {
		t.Fatal(err)
	}
	if o.TLS.Insecure {
		t.Error("reality outbound kept insecure=true")
	}
	if o.TLS.ServerName != defaultRealityDest {
		t.Errorf("missing ?sni= did not fall back to the default dest: %q", o.TLS.ServerName)
	}
}

// A REALITY outbound must not be laddered: sing-box bakes hysteria2's declared
// bandwidth into each rung, and a vless outbound has no such field to vary.
func TestRealityOutboundIsNotLaddered(t *testing.T) {
	uri := "vless://u@1.2.3.4:443?security=reality&pbk=k&sni=www.apple.com"
	o, err := parseServerURL(uri)
	if err != nil {
		t.Fatal(err)
	}
	outs, _ := buildProxyOutbounds(uri, "", o)
	if len(outs) != 1 || outs[0].Type != "vless" {
		t.Fatalf("expected a single vless outbound, got %+v", outs)
	}
}

// The dual-leg shape: one box, hysteria2 on UDP/443 and REALITY on TCP/443. The
// standby must land in the SAME selector as the ladder rungs, because that is
// what makes the failover a single Clash call instead of a sing-box restart.
func TestStandbyLegJoinsTheLadderSelector(t *testing.T) {
	hy2 := "hy2://pw@1.2.3.4:443?upmbps=5&downmbps=65&peakdownmbps=50&peakupmbps=5&peakstart=12&peakend=2"
	reality := "vless://u@1.2.3.4:443?security=reality&pbk=k&sni=www.apple.com"
	o, err := parseServerURL(hy2)
	if err != nil {
		t.Fatal(err)
	}
	outs, _ := buildProxyOutbounds(hy2, reality, o)
	if outs[0].Type != "selector" || outs[0].Tag != core.ProxySelectorTag {
		t.Fatalf("first outbound should be the proxy selector, got %+v", outs[0])
	}
	var found bool
	for _, m := range outs[0].Outbounds {
		if m == core.RealityLegTag {
			found = true
		}
	}
	if !found {
		t.Fatalf("standby leg missing from selector members: %v", outs[0].Outbounds)
	}
	// And it must exist as a real outbound, not just a name in the member list —
	// selecting a member sing-box has no definition for fails the API call.
	var def *OutboundConfig
	for i := range outs {
		if outs[i].Tag == core.RealityLegTag {
			def = &outs[i]
		}
	}
	if def == nil {
		t.Fatal("standby named in the selector but never defined as an outbound")
	}
	if def.Type != "vless" {
		t.Errorf("standby outbound type = %q, want vless", def.Type)
	}
	// REALITY is the primary, so a fresh config must come up on it rather than
	// spend its first minute on the ladder waiting for the supervisor's tick.
	if outs[0].Default != core.RealityLegTag {
		t.Errorf("a fresh config must default to the REALITY leg, got %q", outs[0].Default)
	}
}

// A malformed standby must not take the tunnel down with it — the primary leg is
// what the user is trying to bring up.
func TestBadStandbyURIDegradesToSingleLeg(t *testing.T) {
	hy2 := "hy2://pw@1.2.3.4:443?upmbps=5&downmbps=65&peakdownmbps=50&peakupmbps=5&peakstart=12&peakend=2"
	o, err := parseServerURL(hy2)
	if err != nil {
		t.Fatal(err)
	}
	outs, _ := buildProxyOutbounds(hy2, "://nonsense", o)
	for _, out := range outs {
		if out.Tag == core.RealityLegTag {
			t.Fatal("a malformed standby URI produced a standby member")
		}
	}
	if outs[0].Type != "selector" {
		t.Fatalf("ladder selector should survive a bad standby, got %+v", outs[0])
	}
}

// A ceiling so low the ladder collapses to one rung would, without a standby,
// skip the selector entirely and hand the route a bare outbound. Once there is a
// second leg the selector has to exist anyway, or there is nothing to fail over
// to.
func TestStandbyBuildsASelectorEvenWhenTheLadderCollapses(t *testing.T) {
	hy2 := "hy2://pw@1.2.3.4:443?upmbps=1&downmbps=1" // no standard rung sits below 1
	reality := "vless://u@1.2.3.4:443?security=reality&pbk=k&sni=www.apple.com"
	o, err := parseServerURL(hy2)
	if err != nil {
		t.Fatal(err)
	}
	outs, _ := buildProxyOutbounds(hy2, reality, o)
	if outs[0].Type != "selector" {
		t.Fatalf("expected a selector for the two legs, got %+v", outs[0])
	}
	want := []string{core.BandwidthRungTag(1), core.RealityLegTag}
	if len(outs[0].Outbounds) != 2 || outs[0].Outbounds[0] != want[0] || outs[0].Outbounds[1] != want[1] {
		t.Fatalf("members = %v, want %v", outs[0].Outbounds, want)
	}
	if outs[0].Default != want[1] {
		t.Errorf("default = %q, want the REALITY primary %q", outs[0].Default, want[1])
	}
}

// The failover gate's two forwarders must be a matched pair: same destination
// on the node, different legs carrying them. If they are not — if one is
// missing, or both end up on the same leg — the gate goes on comparing two
// things that are not comparable, which is the 2026-09-12 failure (fourteen
// switches onto a leg measured 3-9x worse) wearing a different disguise.
//
// This is the check that distinguishes "wired" from "present in the JSON",
// because nothing at runtime would report the difference: a gate probe routed
// over REALITY answers perfectly well, it just answers about the wrong leg.
func TestLegGateForwardersRideDifferentLegs(t *testing.T) {
	hy2 := "hy2://pw@1.2.3.4:443?upmbps=5&downmbps=100&obfs=salamander&obfs-password=x"
	reality := "vless://u@1.2.3.4:443?security=reality&pbk=k&sni=www.apple.com"
	cfg, err := BuildWithStandby(hy2, reality)
	if err != nil {
		t.Fatal(err)
	}

	ports := map[string]int{}
	for _, in := range cfg.Inbounds {
		switch in.Tag {
		case core.BrutalInboundTag, core.LegGateInboundTag:
			ports[in.Tag] = in.ListenPort
			// Both must land on the keeper's port at the far end: the gate
			// compares legs, so the destination has to be the one thing they
			// do not differ in.
			if in.OverridePort != core.BrutalKeeperPort {
				t.Errorf("%s forwards to port %d, want the keeper's %d", in.Tag, in.OverridePort, core.BrutalKeeperPort)
			}
		}
	}
	if ports[core.BrutalInboundTag] != core.BrutalKeeperPort {
		t.Errorf("REALITY forwarder listens on %d, want %d", ports[core.BrutalInboundTag], core.BrutalKeeperPort)
	}
	if ports[core.LegGateInboundTag] != core.LegGatePort {
		t.Fatalf("hy2 gate forwarder listens on %d, want %d", ports[core.LegGateInboundTag], core.LegGatePort)
	}

	legOf := map[string]string{}
	for _, r := range cfg.Route.Rules {
		for _, in := range r.Inbound {
			if in == core.BrutalInboundTag || in == core.LegGateInboundTag {
				legOf[in] = r.Outbound
			}
		}
	}
	if legOf[core.BrutalInboundTag] != core.RealityLegTag {
		t.Errorf("REALITY forwarder routed to %q, want %q", legOf[core.BrutalInboundTag], core.RealityLegTag)
	}
	gate := legOf[core.LegGateInboundTag]
	if gate == "" {
		t.Fatal("hy2 gate forwarder has no route rule — its probes would fall through to the catch-all and measure whatever the selector is on")
	}
	if gate == core.RealityLegTag || gate == core.ProxySelectorTag {
		t.Fatalf("hy2 gate forwarder routed to %q — it must name a ladder member, or the gate times the leg it is deciding about", gate)
	}
	if gate != core.Hy2ProbeSelectorTag {
		t.Errorf("hy2 gate forwarder routed to %q, want its own selectable UDP leg", gate)
	}
	// And the member it names has to be one sing-box will have defined.
	var defined bool
	for _, o := range cfg.Outbounds {
		if o.Tag == gate {
			defined = true
			if o.Type != "selector" {
				t.Errorf("gate target %q is type %q, want selector", gate, o.Type)
			}
			for _, member := range o.Outbounds {
				found := false
				for _, target := range cfg.Outbounds {
					if target.Tag == member && target.Type == "hysteria2" {
						found = true
					}
				}
				if !found {
					t.Errorf("gate can leave the UDP transport through %q", member)
				}
			}
		}
	}
	if !defined {
		t.Errorf("gate routed to %q, which no outbound defines — sing-box refuses to start on that", gate)
	}
}

// A node with no REALITY leg has nothing to compare, and a route rule naming an
// outbound that does not exist is a config sing-box refuses to start on.
func TestLegGateAbsentWithoutASecondLeg(t *testing.T) {
	hy2 := "hy2://pw@1.2.3.4:443?upmbps=5&downmbps=100"
	cfg, err := Build(hy2)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range cfg.Inbounds {
		if in.Tag == core.LegGateInboundTag {
			t.Error("single-leg config carries a gate forwarder with no second leg to gate")
		}
	}
	for _, r := range cfg.Route.Rules {
		for _, in := range r.Inbound {
			if in == core.LegGateInboundTag {
				t.Error("single-leg config carries a gate route rule")
			}
		}
	}
}
