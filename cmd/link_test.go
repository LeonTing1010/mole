package cmd

import (
	"net/url"
	"strings"
	"testing"
)

// dualLegNode is the shape every node deploys into now: hysteria2 on UDP/443
// with VLESS+REALITY beside it on TCP/443.
func dualLegNode() *Server {
	return &Server{
		Name: "icn-1", IP: "1.2.3.4", Port: 443, Proto: protoHysteria2,
		Password: "pw", UUID: "u-1", RealityPubKey: "pbk-1",
		RealityShortID: "abcd1234", RealityDest: "www.apple.com",
	}
}

// A dual-leg node hands out its REALITY leg and never the hy2 one. The hy2 link
// is mole's interchange format: an importer drops the pinned certificate along
// with the ceiling and the peak window, and dropping the pin is not a
// degradation but a broken handshake, because mole omits insecure=1 precisely
// because it pins.
func TestPortableLinkOffersTheRealityLegOfADualLegNode(t *testing.T) {
	srv := dualLegNode()
	link, leg, err := portableLink(srv)
	if err != nil {
		t.Fatalf("portableLink: %v", err)
	}
	if link != srv.StandbyURI() {
		t.Errorf("portable link is not the standby leg:\n got %q\nwant %q", link, srv.StandbyURI())
	}
	if strings.HasPrefix(link, "hy2://") {
		t.Errorf("offered the hysteria2 link to another client: %s", link)
	}
	if leg == "" {
		t.Error("no description of which leg the link is")
	}
}

// A node deployed as REALITY-only has no standby; its own URI is already the
// portable one.
func TestPortableLinkOffersARealityOnlyNode(t *testing.T) {
	srv := &Server{
		Name: "r-1", IP: "1.2.3.4", Port: 443, Proto: protoReality,
		UUID: "u-1", RealityPubKey: "pbk-1", RealityShortID: "abcd1234",
	}
	link, _, err := portableLink(srv)
	if err != nil {
		t.Fatalf("portableLink: %v", err)
	}
	if link != srv.URI() {
		t.Errorf("portable link = %q, want the node's own URI %q", link, srv.URI())
	}
	if !strings.HasPrefix(link, "vless://") {
		t.Errorf("reality-only node offered %q", link)
	}
}

// Anything deployed before both legs became standard has nothing to hand out.
// Saying so beats printing a hy2 link that cannot work in the client it was
// printed for.
func TestPortableLinkRefusesANodeWithNoRealityLeg(t *testing.T) {
	srv := &Server{Name: "old", IP: "1.2.3.4", Port: 443, Password: "pw"}
	link, _, err := portableLink(srv)
	if err == nil {
		t.Fatalf("offered %q for a node with no portable leg", link)
	}
	for _, want := range []string{"no REALITY leg", "mole server repair old"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not tell the operator %q: %v", want, err)
		}
	}
}

// The contract with the importer, and the reason this test exists separately
// from the ones over StandbyURI: Hiddify's ray2sing reads security, pbk, sid,
// flow and sni off a vless link and nothing else. Renaming one of them would
// not fail anything mole does with its own link, and would silently break every
// client the link was handed to.
func TestPortableLinkCarriesWhatOtherClientsRead(t *testing.T) {
	link, _, err := portableLink(dualLegNode())
	if err != nil {
		t.Fatalf("portableLink: %v", err)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("portable link does not parse: %v", err)
	}
	if u.Scheme != "vless" {
		t.Fatalf("scheme = %q, want vless", u.Scheme)
	}
	if u.User.Username() != "u-1" {
		t.Errorf("uuid = %q, want it in the userinfo where every vless client reads it", u.User.Username())
	}
	q := u.Query()
	for key, want := range map[string]string{
		"security": "reality",
		"pbk":      "pbk-1",
		"sid":      "abcd1234",
		"sni":      "www.apple.com",
		"flow":     "xtls-rprx-vision",
		"fp":       "chrome",
		"type":     "tcp",
	} {
		if got := q.Get(key); got != want {
			t.Errorf("?%s = %q, want %q", key, got, want)
		}
	}
	// Nothing mole-only may ride along. An importer drops unknown parameters
	// without a word, so a ceiling in the link is worse than no ceiling: the
	// operator reads it there and believes it travelled.
	for _, banned := range []string{"pinnedcert", "upmbps", "downmbps", "peakstart", "peakend", "ports", "hop"} {
		if got := q.Get(banned); got != "" {
			t.Errorf("portable link carries the mole-only parameter %s=%s", banned, got)
		}
	}
}

// The hy2 variant, and the whole of its cost in one parameter. A third-party
// client has nowhere to put a pinned certificate, so it has to be told not to
// verify; Hiddify's ray2sing reads exactly insecure, sni, obfs, obfs-password
// and ech off a hysteria2 link, so these are the complete contract rather than
// a selection from it.
func TestPortableHy2LinkIsThePlainDialect(t *testing.T) {
	srv := dualLegNode()
	srv.SNI = "node-abc123.net"
	srv.Obfs = "salamander"
	srv.ObfsPassword = "obfskey"
	link := portableHy2Link(srv)

	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("portable hy2 link does not parse: %v", err)
	}
	if u.Scheme != "hysteria2" {
		t.Errorf("scheme = %q, want the widely accepted hysteria2", u.Scheme)
	}
	if u.User.Username() != "pw" {
		t.Errorf("tunnel password = %q, want it in the userinfo", u.User.Username())
	}
	if u.Hostname() != "1.2.3.4" || u.Port() != "443" {
		t.Errorf("address = %s:%s, want 1.2.3.4:443", u.Hostname(), u.Port())
	}
	if u.Fragment != "icn-1" {
		t.Errorf("fragment = %q, want the server name the importer labels it with", u.Fragment)
	}
	q := u.Query()
	for key, want := range map[string]string{
		"insecure":      "1",
		"sni":           "node-abc123.net",
		"obfs":          "salamander",
		"obfs-password": "obfskey",
	} {
		if got := q.Get(key); got != want {
			t.Errorf("?%s = %q, want %q", key, got, want)
		}
	}
	for _, banned := range []string{"pinnedcert", "upmbps", "downmbps", "peakstart", "peakend", "ports", "hop"} {
		if got := q.Get(banned); got != "" {
			t.Errorf("portable link carries the mole-only parameter %s=%s", banned, got)
		}
	}

	// It has to survive mole's own importer too — moving a node to a second
	// machine is `mole server link --hy2` on one and `mole server import` on the
	// other, and a link only one of the two can read is not a link.
	back, err := serverFromURI(link)
	if err != nil {
		t.Fatalf("mole cannot import its own portable link: %v", err)
	}
	if back.Password != "pw" || back.SNI != "node-abc123.net" || back.ObfsPassword != "obfskey" {
		t.Errorf("round-trip lost a credential: %+v", back)
	}
}

// A node recorded before pinning existed has no SNI of its own, and its
// certificate says bing.com. Server.URI() still falls back to that name for the
// clients that already hold it, but a link printed for a stranger must not
// re-publish the impersonation pinning retired — and with verification off the
// client needs no SNI to connect.
func TestPortableHy2LinkDoesNotImpersonateForALegacyNode(t *testing.T) {
	srv := &Server{Name: "old", IP: "1.2.3.4", Port: 443, Password: "pw"}
	link := portableHy2Link(srv)
	if strings.Contains(link, "bing.com") {
		t.Errorf("legacy link impersonates a known site: %s", link)
	}
	if !strings.Contains(link, "insecure=1") {
		t.Errorf("a link that carries no pin must say verification is off: %s", link)
	}
}

// An IPv6-only node needs its address bracketed or the link has no host in it
// that any parser can find.
func TestPortableHy2LinkBracketsAnIPv6OnlyNode(t *testing.T) {
	srv := &Server{Name: "v6", IPv6: "2401:c080:1c02::1", Port: 443, Password: "pw", SNI: "node-x.net"}
	u, err := url.Parse(portableHy2Link(srv))
	if err != nil {
		t.Fatalf("link does not parse: %v", err)
	}
	if u.Hostname() != "2401:c080:1c02::1" || u.Port() != "443" {
		t.Errorf("address = %s:%s, want the unbracketed pair back", u.Hostname(), u.Port())
	}
}
