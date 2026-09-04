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
	o, err := parseServerURL("vless://96738e78-6353-4d57-863f-f3bd26d2a856@158.247.230.81:443" +
		"?encryption=none&security=reality&type=tcp&flow=xtls-rprx-vision&fp=chrome" +
		"&sni=www.apple.com&pbk=Sl-2p_V0AFMK09BlyvW-CyvV-7jZpiXBl8x6dvmPxRg&sid=ac9a7dcddc45b2b7#icn-test")
	if err != nil {
		t.Fatal(err)
	}
	if o.Type != "vless" || o.UUID != "96738e78-6353-4d57-863f-f3bd26d2a856" || o.ServerPort != 443 {
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
	if o.TLS.Reality.PublicKey != "Sl-2p_V0AFMK09BlyvW-CyvV-7jZpiXBl8x6dvmPxRg" {
		t.Errorf("public_key = %q", o.TLS.Reality.PublicKey)
	}
	if o.TLS.Reality.ShortID != "ac9a7dcddc45b2b7" {
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
	outs := buildProxyOutbounds(uri, "", o)
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
	outs := buildProxyOutbounds(hy2, reality, o)
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
	outs := buildProxyOutbounds(hy2, "://nonsense", o)
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
	outs := buildProxyOutbounds(hy2, reality, o)
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
