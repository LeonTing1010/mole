package config

import "testing"

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
	outs := buildProxyOutbounds(uri, o)
	if len(outs) != 1 || outs[0].Type != "vless" {
		t.Fatalf("expected a single vless outbound, got %+v", outs)
	}
}
