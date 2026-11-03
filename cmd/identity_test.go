package cmd

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/LeonTing1010/mole/config"
	"github.com/LeonTing1010/mole/core"
)

// The identity is only worth anything if a client that trusts nothing but the
// pinned certificate can actually complete a handshake with a server holding
// the matching key. This is the check that would have caught the old
// provisioning script: it issued a Common-Name-only certificate, and Go dropped
// CN fallback in 1.15, so pinning it could never have verified. That script
// survived because it was always paired with verification turned off.
func TestPinnedIdentityCompletesTLSHandshake(t *testing.T) {
	id, err := newServerIdentity()
	if err != nil {
		t.Fatalf("newServerIdentity: %v", err)
	}

	serverCert, err := tls.X509KeyPair([]byte(id.CertPEM), []byte(id.KeyPEM))
	if err != nil {
		t.Fatalf("server keypair: %v", err)
	}

	// The client trusts exactly one certificate — the pin — and nothing else.
	// This mirrors what sing-box does with tls.certificate.
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(id.CertPEM)) {
		t.Fatal("pinned certificate did not parse as PEM")
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{serverCert}})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	errCh := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer conn.Close()
		errCh <- conn.(*tls.Conn).Handshake()
	}()

	client, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{
		RootCAs:    pool,
		ServerName: id.SNI,
	})
	if err != nil {
		t.Fatalf("pinned handshake failed (a CN-only cert with no SAN fails exactly here): %v", err)
	}
	client.Close()
	if err := <-errCh; err != nil {
		t.Fatalf("server side handshake: %v", err)
	}
}

// A certificate that verifies only because the name happens to match one we
// chose is not proof of pinning. Verify the pin is load-bearing: a *different*
// identity's certificate must be rejected even though it is equally valid and
// carries a SAN of its own.
func TestPinRejectsADifferentCertificate(t *testing.T) {
	server, err := newServerIdentity()
	if err != nil {
		t.Fatalf("newServerIdentity: %v", err)
	}
	other, err := newServerIdentity()
	if err != nil {
		t.Fatalf("newServerIdentity: %v", err)
	}

	serverCert, err := tls.X509KeyPair([]byte(server.CertPEM), []byte(server.KeyPEM))
	if err != nil {
		t.Fatalf("server keypair: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(other.CertPEM)) // pin the wrong one

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{serverCert}})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		if conn, err := ln.Accept(); err == nil {
			conn.Close()
		}
	}()

	conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{
		RootCAs:    pool,
		ServerName: server.SNI,
	})
	if err == nil {
		conn.Close()
		t.Fatal("handshake succeeded against a certificate that was not the pinned one")
	}
}

// The old script named a famous site it had no relationship to, which both
// contradicts the destination address and hands an active prober a self-signed
// certificate for a site that plainly does not issue its own.
func TestIdentityDoesNotImpersonateAKnownSite(t *testing.T) {
	id, err := newServerIdentity()
	if err != nil {
		t.Fatalf("newServerIdentity: %v", err)
	}
	for _, brand := range []string{"bing.com", "microsoft.com", "google.com", "apple.com", "cloudflare.com"} {
		if strings.Contains(strings.ToLower(id.SNI), brand) {
			t.Errorf("SNI %q impersonates %s", id.SNI, brand)
		}
	}
	if id.ObfsPassword == "" {
		t.Error("obfs password is empty — the server would obfuscate with a key the client lacks")
	}
}

// Two deploys must never share TLS material or an obfs key: one compromised
// node would otherwise let its holder impersonate every other node.
func TestIdentitiesAreUnique(t *testing.T) {
	a, err := newServerIdentity()
	if err != nil {
		t.Fatalf("newServerIdentity: %v", err)
	}
	b, err := newServerIdentity()
	if err != nil {
		t.Fatalf("newServerIdentity: %v", err)
	}
	if a.CertPEM == b.CertPEM || a.KeyPEM == b.KeyPEM {
		t.Error("two identities share TLS material")
	}
	if a.ObfsPassword == b.ObfsPassword || a.SNI == b.SNI {
		t.Error("two identities share an obfs password or SAN")
	}
}

// Servers recorded before pinning existed have no certificate to offer. Their
// URI must come out exactly as it always did, or `mole up` would rebuild a
// working tunnel into a different one.
func TestLegacyServerURIIsUnchanged(t *testing.T) {
	s := Server{Name: "nrt-1", IP: "45.32.28.60", Port: 443, Password: "pw"}
	got := s.URI()
	// The identity half of the URI is still frozen. The ceiling half is now
	// always present: an entry with no ceiling on file used to leave the parser
	// to invent one, and it invented a different one than the supervisor
	// scheduled over, which put the tunnel on the top rung with no way down.
	want := "hy2://pw@45.32.28.60:443?insecure=1&sni=bing.com" +
		"&upmbps=5&downmbps=20&peakdownmbps=5&peakupmbps=2&peakstart=12&peakend=2#nrt-1"
	if got != want {
		t.Errorf("legacy URI changed:\n got %q\nwant %q", got, want)
	}
}

// The regression that made this whole family of tests necessary: the ceiling the
// config dials and the ceiling the supervisor schedules over must be the same
// numbers, for every server, tuned or not. When they diverged, sing-box
// materialised a ladder topping out at 50 Mbps and defaulted to that top rung,
// while Schedule() reported no ladder at all — so the bandwidth scheduler never
// started, `mole ceiling` refused to run, and nothing could bring it down.
func TestURIAndScheduleDescribeTheSameCeiling(t *testing.T) {
	cases := []struct {
		name string
		srv  Server
	}{
		{"untuned", Server{Name: "s", IP: "1.2.3.4", Port: 443, Password: "pw"}},
		{"down only", Server{Name: "s", IP: "1.2.3.4", Port: 443, Password: "pw", DownMbps: 12}},
		{"fully tuned", Server{
			Name: "s", IP: "1.2.3.4", Port: 443, Password: "pw",
			UpMbps: 8, DownMbps: 30, PeakUpMbps: 3, PeakDownMbps: 8,
			PeakStartHour: 19, PeakEndHour: 23,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Build(tc.srv.URI())
			if err != nil {
				t.Fatalf("config.Build: %v", err)
			}
			sched := tc.srv.Schedule()
			want := sched.Member(time.Now())

			var sel *config.OutboundConfig
			members := map[string]config.OutboundConfig{}
			for i, o := range cfg.Outbounds {
				if o.Tag == core.ProxySelectorTag {
					sel = &cfg.Outbounds[i]
				}
				members[o.Tag] = o
			}
			if sel == nil || sel.Type != "selector" {
				t.Fatalf("no proxy selector built for %s", tc.srv.URI())
			}
			if sel.Default != want {
				t.Errorf("selector starts on %q but the schedule says %q", sel.Default, want)
			}
			// Every ceiling the scheduler can ever name must exist as an outbound,
			// or SelectProxy names a member sing-box does not have.
			for _, down := range sched.Rungs() {
				tag := core.BandwidthRungTag(down)
				m, ok := members[tag]
				if !ok {
					t.Errorf("schedule offers rung %d but %s was not built", down, tag)
					continue
				}
				if m.DownMbps != down {
					t.Errorf("%s declares down=%d, want %d", tag, m.DownMbps, down)
				}
			}
		})
	}
}

// A pinned server must stop advertising insecure=1 — leaving it in would let a
// parser that reads the flag first keep skipping verification.
func TestPinnedServerURIDropsInsecureAndRoundTrips(t *testing.T) {
	id, err := newServerIdentity()
	if err != nil {
		t.Fatalf("newServerIdentity: %v", err)
	}
	s := Server{
		Name: "nrt-2", IP: "45.32.28.60", Port: 443, Password: "pw",
		TLSCertPEM: id.CertPEM, SNI: id.SNI,
		Obfs: "salamander", ObfsPassword: id.ObfsPassword,
	}
	uri := s.URI()
	if strings.Contains(uri, "insecure=1") {
		t.Errorf("pinned URI still carries insecure=1: %s", uri)
	}
	if !strings.Contains(uri, "pinnedcert=") {
		t.Errorf("pinned URI carries no certificate: %s", uri)
	}

	back, err := serverFromURI(uri)
	if err != nil {
		t.Fatalf("serverFromURI: %v", err)
	}
	if back.TLSCertPEM != id.CertPEM {
		t.Error("certificate did not survive the URI round-trip")
	}
	if back.SNI != id.SNI {
		t.Errorf("SNI round-trip: got %q want %q", back.SNI, id.SNI)
	}
	if back.ObfsPassword != id.ObfsPassword {
		t.Error("obfs password did not survive the URI round-trip")
	}
	if back.URI() != uri {
		t.Errorf("URI is not stable across a round-trip:\n got %q\nwant %q", back.URI(), uri)
	}
}

// The certificate rides in a query parameter, so its encoding must survive URL
// parsing without escaping — base64url without padding is chosen for exactly
// that reason.
func TestPinnedCertEncodingIsURLSafe(t *testing.T) {
	id, err := newServerIdentity()
	if err != nil {
		t.Fatalf("newServerIdentity: %v", err)
	}
	enc := base64.RawURLEncoding.EncodeToString([]byte(id.CertPEM))
	if strings.ContainsAny(enc, "+/=&#?") {
		t.Errorf("encoded certificate contains characters that need escaping: %q", enc)
	}
}

// These are real 32-hex passwords of the shape mole used to mint, and what an
// importer that base64-decodes a link's userinfo makes of them. The first two
// decode to 24 bytes with a colon in them, so the importer authenticates with
// the two halves of the decoded noise and the server reports a wrong password —
// with nothing anywhere saying the credential was rewritten on the way in.
// 11.8% of hex passwords landed there. If this test ever passes because the
// predicate stopped recognising them, the password change below is unguarded.
func TestBase64RewritesFlagsTheCredentialsItMangles(t *testing.T) {
	mangled := []string{
		"ff9243a8f506b40928b5b7a767c76fb0", // colon at decoded index 8
		"e8a7f770d9106fd287db7f1adbc60926", // colon at decoded index 20
	}
	for _, pw := range mangled {
		if !base64Rewrites(pw) {
			t.Errorf("%s is rewritten by such an importer but not flagged", pw)
		}
	}
	// A colon-free decode leaves the credential alone, so the predicate must not
	// fire on every hex string — only on the ones that actually break.
	if base64Rewrites("00000000000000000000000000000000") {
		t.Error("a colon-free decode flagged as rewritten")
	}
	// And the '.' makes the decode fail outright, which is the whole remedy.
	if base64Rewrites("ff9243a8f506b409.28b5b7a767c76fb0") {
		t.Error("a password carrying '.' is not base64 at all, so nothing can rewrite it")
	}
}

func TestTunnelPasswordIsNotBase64Shaped(t *testing.T) {
	for i := 0; i < 4096; i++ {
		pw, err := tunnelPassword()
		if err != nil {
			t.Fatalf("tunnelPassword: %v", err)
		}
		if base64Rewrites(pw) {
			t.Fatalf("%q can still be rewritten by an importer that decodes userinfo", pw)
		}
		if n := strings.Count(pw, "."); n != 1 {
			t.Fatalf("%q carries %d dots, want exactly 1", pw, n)
		}
		// A leading '.' is YAML's .inf/.nan indicator, and the password is
		// interpolated into the node's config.yaml.
		if pw[0] == '.' {
			t.Fatalf("%q starts with the dot", pw)
		}
		if len(pw) != 33 {
			t.Fatalf("%q is %d chars, want 33 (32 hex + the dot)", pw, len(pw))
		}
		if body := strings.Replace(pw, ".", "", 1); !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(body) {
			t.Fatalf("%q is not hex around its dot", pw)
		}
	}
}

// The password reaches the node twice — once in the boot script's YAML and once
// in the client's own config, built from the URI — and the two have to agree or
// the tunnel is dark with both sides reporting a bad password. A '.' is
// unreserved in RFC 3986, so it needs no escaping in the userinfo and the
// round trip is the identity.
func TestTunnelPasswordSurvivesTheURIRoundTrip(t *testing.T) {
	pw, err := tunnelPassword()
	if err != nil {
		t.Fatalf("tunnelPassword: %v", err)
	}
	s := Server{Name: "icn-1", IP: "1.2.3.4", Port: 443, Password: pw}
	uri := s.URI()
	back, err := serverFromURI(uri)
	if err != nil {
		t.Fatalf("serverFromURI: %v", err)
	}
	if back.Password != pw {
		t.Errorf("password did not survive the URI: got %q want %q", back.Password, pw)
	}
	if back.URI() != uri {
		t.Errorf("URI is not stable across a round-trip:\n got %q\nwant %q", back.URI(), uri)
	}
}

// The uuid is the credential in the link `mole server link` hands out, and its
// shape is not ours to choose: 36 characters, all of them in the base64url
// alphabet, length divisible by four — so it decodes every time. 14.6% of v4
// UUIDs decode to something containing a colon, and an importer then splits on
// it and authenticates with the halves. Resampling is the only defence that
// leaves the uuid canonical, which a VLESS server requires.
func TestRandomUUIDSurvivesAnImportersBase64Heuristic(t *testing.T) {
	canonical := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := make(map[string]bool, 4096)
	for i := 0; i < 4096; i++ {
		uuid, err := randomUUID()
		if err != nil {
			t.Fatalf("randomUUID: %v", err)
		}
		if !canonical.MatchString(uuid) {
			t.Fatalf("%q is not a canonical version-4 uuid", uuid)
		}
		if base64Rewrites(uuid) {
			t.Fatalf("%q decodes to something containing a colon — an importer would authenticate with the halves", uuid)
		}
		if seen[uuid] {
			t.Fatalf("%q minted twice", uuid)
		}
		seen[uuid] = true
	}
}

// The boot script runs once, unattended, on a machine reachable no other way.
// A quoting or heredoc mistake here doesn't fail loudly — it leaves a VPS that
// billed you and never came up.
func TestBootScriptIsValidBashAndCarriesTheIdentity(t *testing.T) {
	id, err := newServerIdentity()
	if err != nil {
		t.Fatalf("newServerIdentity: %v", err)
	}
	script := hy2BootScript("tunnel-password", 443, id)

	dir := t.TempDir()
	path := filepath.Join(dir, "boot.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("boot script is not valid bash: %v\n%s\n%s", err, out, script)
	}

	want := []string{
		strings.TrimSpace(id.CertPEM),
		strings.TrimSpace(id.KeyPEM),
		"type: salamander",
		id.ObfsPassword,
		"tunnel-password",
	}
	if id.SSHPubKey != "" {
		// Order is the safety property: the key must be installed before
		// passwords are refused, or first boot locks itself out.
		if i, j := strings.Index(script, "authorized_keys"), strings.Index(script, "PasswordAuthentication no"); i < 0 || j < 0 || i > j {
			t.Error("sshd hardening must install the key before disabling password auth")
		}
		// And before the first set -e. On 2026-10-31 the key was installed
		// last, a failure ahead of it ended the script, and the machine had
		// neither a key nor a way to log in. The line-anchored match skips the
		// mention of set -e in the comment above.
		if k, e := strings.Index(script, "authorized_keys"), strings.Index(script, "\nset -euo pipefail\n"); k < 0 || e < 0 || k > e {
			t.Error("the SSH key must be installed before the first set -e, or an early failure locks the machine")
		}
		want = append(want, strings.TrimSpace(id.SSHPubKey), "PermitRootLogin prohibit-password")
	} else if strings.Contains(script, "PasswordAuthentication no") {
		t.Error("password auth disabled with no key installed — the node would be unreachable")
	}
	for _, want := range want {
		if !strings.Contains(script, want) {
			t.Errorf("boot script is missing %q", want)
		}
	}
	// Under obfuscation nothing ever reaches the HTTP layer to be masqueraded at.
	if strings.Contains(script, "masquerade") {
		t.Error("obfuscated node still carries a masquerade block")
	}
	if strings.Contains(script, "bing.com") {
		t.Error("boot script still impersonates bing.com")
	}
}

// Both credentials land in the node's config.yaml as YAML scalars, and a bare
// scalar that is nothing but digits resolves as a number. While the password was
// 32 hex characters that was survivable by accident — a 32-digit integer
// overflows int64 and the parser falls back to a string — but tunnelPassword
// puts a '.' in it, and the same accident then resolves as a float. The node
// would come up authenticating against something other than the password the
// client holds, and both sides would report the other as wrong.
func TestBootScriptQuotesTheYAMLCredentials(t *testing.T) {
	id, err := newServerIdentity()
	if err != nil {
		t.Fatalf("newServerIdentity: %v", err)
	}
	pw, err := tunnelPassword()
	if err != nil {
		t.Fatalf("tunnelPassword: %v", err)
	}
	script := hy2BootScript(pw, 443, id)
	for _, want := range []string{`password: "` + pw + `"`, `password: "` + id.ObfsPassword + `"`} {
		if !strings.Contains(script, want) {
			t.Errorf("boot script does not carry the quoted scalar %s", want)
		}
	}
}
