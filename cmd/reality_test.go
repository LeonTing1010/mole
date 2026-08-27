package cmd

import (
	"encoding/base64"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/crypto/curve25519"
)

// A REALITY node has no Brutal ceiling: congestion control is the kernel's on
// both ends. If ladder() invented one, config.Build would materialise a
// selector over rungs the outbound cannot have and the supervisor would spend
// every tick flipping between them.
func TestRealityServerHasNoLadder(t *testing.T) {
	srv := Server{Proto: protoReality, UUID: "u", IP: "1.2.3.4", Port: 443}
	if got := srv.Schedule().Rungs(); len(got) != 0 {
		t.Fatalf("reality server produced a bandwidth ladder: %v", got)
	}
	if !strings.HasPrefix(srv.URI(), "vless://") {
		t.Fatalf("reality server did not emit a vless URI: %s", srv.URI())
	}
	if strings.Contains(srv.URI(), "upmbps") {
		t.Errorf("reality URI carries a Brutal ceiling: %s", srv.URI())
	}
}

// The hysteria2 shape must be untouched by the protocol split: every server
// recorded before REALITY existed has an empty Proto.
func TestEmptyProtoStaysHysteria2(t *testing.T) {
	srv := Server{Password: "pw", IP: "1.2.3.4", Port: 443, SNI: "bing.com"}
	uri := srv.URI()
	if !strings.HasPrefix(uri, "hy2://") {
		t.Fatalf("empty Proto no longer means hysteria2: %s", uri)
	}
	if !strings.Contains(uri, "upmbps=") {
		t.Errorf("hysteria2 URI lost its explicit ceiling: %s", uri)
	}
}

// Server.URI() and the import path are inverses, or a link mole emits is a link
// mole cannot read back.
func TestRealityURIRoundTrip(t *testing.T) {
	srv := Server{
		Name: "icn-test", Proto: protoReality, IP: "158.247.230.81", Port: 443,
		UUID:           "96738e78-6353-4d57-863f-f3bd26d2a856",
		RealityPubKey:  "Sl-2p_V0AFMK09BlyvW-CyvV-7jZpiXBl8x6dvmPxRg",
		RealityShortID: "ac9a7dcddc45b2b7", RealityDest: "www.apple.com",
	}
	back, err := serverFromURI(srv.URI())
	if err != nil {
		t.Fatalf("re-import own URI: %v", err)
	}
	for _, c := range []struct{ field, want, got string }{
		{"proto", srv.Proto, back.Proto},
		{"uuid", srv.UUID, back.UUID},
		{"ip", srv.IP, back.IP},
		{"pbk", srv.RealityPubKey, back.RealityPubKey},
		{"sid", srv.RealityShortID, back.RealityShortID},
		{"dest", srv.RealityDest, back.RealityDest},
		{"name", srv.Name, back.Name},
	} {
		if c.want != c.got {
			t.Errorf("%s: round-tripped to %q, want %q", c.field, c.got, c.want)
		}
	}
}

// A plain (non-REALITY) vless link to a bare IP has nothing that authenticates
// the peer. Importing it would be importing an outbound mole cannot verify.
func TestImportRejectsNonRealityVLESS(t *testing.T) {
	_, err := serverFromURI("vless://uuid@1.2.3.4:443?security=tls&sni=x.com#n")
	if err == nil {
		t.Fatal("plain vless link was accepted")
	}
}

// The keypair is the whole of REALITY's authentication: a public key that isn't
// the private key's own leaves a node nothing can connect to, on a box that has
// no other way in.
func TestRealityIdentityKeypairMatches(t *testing.T) {
	id, err := newRealityIdentity("")
	if err != nil {
		t.Fatal(err)
	}
	priv, err := base64.RawURLEncoding.DecodeString(id.PrivateKey)
	if err != nil {
		t.Fatalf("private key is not base64url: %v", err)
	}
	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		t.Fatal(err)
	}
	if got := base64.RawURLEncoding.EncodeToString(pub); got != id.PublicKey {
		t.Fatalf("public key %s is not derived from the private key (want %s)", id.PublicKey, got)
	}
	if id.Dest != defaultRealityDest {
		t.Errorf("empty dest did not default: %q", id.Dest)
	}
	if len(id.ShortID) != 16 {
		t.Errorf("short id %q is not 8 bytes of hex", id.ShortID)
	}
	if _, err := url.Parse("vless://" + id.UUID + "@1.2.3.4:443"); err != nil {
		t.Errorf("uuid %q does not survive a URI: %v", id.UUID, err)
	}
}

// The boot script is the only thing the VPS ever sees. If the private key, the
// uuid or the borrowed target went missing from it, the node would boot serving
// a REALITY endpoint no client of ours can authenticate against.
func TestRealityBootScriptCarriesIdentity(t *testing.T) {
	id, err := newRealityIdentity("www.samsung.com")
	if err != nil {
		t.Fatal(err)
	}
	script := realityBootScript(8443, id)
	for _, want := range []string{
		id.PrivateKey, id.UUID, id.ShortID,
		`"listen_port": 8443`,
		`"handshake": {"server": "www.samsung.com"`,
		"xtls-rprx-vision",
		"systemctl enable --now sing-box",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("boot script is missing %q", want)
		}
	}
	// The public key belongs to the client. Shipping it to the server is
	// harmless but signals the halves were mixed up.
	if strings.Contains(script, id.PublicKey) {
		t.Error("boot script ships the client's public key")
	}
}

// Vultr spells a single-port rule several ways across responses; matching on
// the raw string would re-add a rule that already exists on every deploy.
func TestFirewallRuleKeyNormalisesPorts(t *testing.T) {
	a := firewallRuleKey("v4", "tcp", "443")
	for _, spelling := range []string{"443 - 443", "443-443", "443:443", " 443 "} {
		if got := firewallRuleKey("v4", "tcp", spelling); got != a {
			t.Errorf("port %q keyed as %q, want %q", spelling, got, a)
		}
	}
	if firewallRuleKey("v4", "udp", "443") == a {
		t.Error("udp and tcp on the same port collide")
	}
}

// The private key mole ships is the one the VPS installs verbatim, and
// REALITY's server side uses it as given rather than clamping it first. An
// unclamped scalar therefore fails every handshake while looking entirely
// self-consistent from this side — TestRealityIdentityKeypairMatches passes
// either way, because Go's curve25519 clamps internally when deriving the
// public half. Assert the bits directly; nothing else catches this.
func TestRealityPrivateKeyIsClamped(t *testing.T) {
	for i := 0; i < 32; i++ {
		id, err := newRealityIdentity("")
		if err != nil {
			t.Fatal(err)
		}
		priv, err := base64.RawURLEncoding.DecodeString(id.PrivateKey)
		if err != nil {
			t.Fatal(err)
		}
		if len(priv) != 32 {
			t.Fatalf("private key is %d bytes, want 32", len(priv))
		}
		if priv[0]&7 != 0 {
			t.Fatalf("low 3 bits of priv[0] set (%#x): scalar is not clamped", priv[0])
		}
		if priv[31]&128 != 0 {
			t.Fatalf("high bit of priv[31] set (%#x): scalar is not clamped", priv[31])
		}
		if priv[31]&64 == 0 {
			t.Fatalf("bit 254 of priv[31] clear (%#x): scalar is not clamped", priv[31])
		}
	}
}

// Every hysteria2 node carries the REALITY leg too, so the URI the client needs
// for it has to render from the same Server entry.
func TestStandbyURIRendersForADualLegNode(t *testing.T) {
	srv := Server{
		Name: "icn-1", IP: "1.2.3.4", Port: 443, Proto: protoHysteria2,
		Password: "pw", UUID: "u-1", RealityPubKey: "pbk-1",
		RealityShortID: "abcd1234", RealityDest: "www.apple.com",
	}
	if !srv.hasStandbyLeg() {
		t.Fatal("a hysteria2 node with REALITY material should report a standby leg")
	}
	got := srv.StandbyURI()
	if !strings.HasPrefix(got, "vless://u-1@1.2.3.4:443?") {
		t.Fatalf("standby URI = %q", got)
	}
	for _, want := range []string{"security=reality", "pbk=pbk-1", "sid=abcd1234", "sni=www.apple.com", "flow=xtls-rprx-vision"} {
		if !strings.Contains(got, want) {
			t.Errorf("standby URI missing %q: %s", want, got)
		}
	}
	// The primary URI must be untouched by any of this — an existing node's hy2
	// link is what every deployed client already holds.
	if !strings.HasPrefix(srv.URI(), "hy2://pw@1.2.3.4:443?") {
		t.Errorf("primary URI changed shape: %s", srv.URI())
	}
}

// A node with no REALITY material — anything deployed before this existed — must
// report no standby rather than a half-formed URI that cannot connect.
func TestStandbyURIEmptyWithoutRealityMaterial(t *testing.T) {
	srv := Server{Name: "old", IP: "1.2.3.4", Port: 443, Password: "pw"}
	if srv.hasStandbyLeg() || srv.StandbyURI() != "" {
		t.Fatalf("legacy node reported a standby leg: %q", srv.StandbyURI())
	}
}

// A REALITY-only node is already the leg that survives a UDP block; it has
// nothing to fall back to and must not claim otherwise.
func TestRealityNodeHasNoStandbyOfItsOwn(t *testing.T) {
	srv := Server{
		Name: "r", IP: "1.2.3.4", Port: 443, Proto: protoReality,
		UUID: "u", RealityPubKey: "k",
	}
	if srv.hasStandbyLeg() {
		t.Fatal("a REALITY node must not report a standby leg")
	}
}

// The dual-leg boot script has to carry BOTH servers: hysteria2's config and the
// REALITY inbound. Appending the snippet is what makes that true, and a shebang
// or a second `set -e` in the middle would break the composed script.
func TestRealityServerSnippetComposesOntoAHy2Script(t *testing.T) {
	rid := &realityIdentity{
		UUID: "u", PrivateKey: "priv", PublicKey: "pub",
		ShortID: "aabbccdd", Dest: "www.apple.com",
	}
	snip := realityServerSnippet(443, rid)
	if strings.Contains(snip, "#!/bin/bash") || strings.Contains(snip, "set -e") {
		t.Fatal("snippet carries a shebang or set -e; it cannot be appended")
	}
	for _, want := range []string{"sing-box", `"type": "vless"`, `"private_key": "priv"`, "systemctl enable --now sing-box"} {
		if !strings.Contains(snip, want) {
			t.Errorf("snippet missing %q", want)
		}
	}
	// And the standalone script must still be a valid script on its own.
	full := realityBootScript(443, rid)
	if !strings.HasPrefix(full, "#!/bin/bash\nset -e\n") {
		t.Errorf("standalone reality boot script lost its header: %.40q", full)
	}
}
