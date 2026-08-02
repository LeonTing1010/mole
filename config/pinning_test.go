package config

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A throwaway certificate. Only its shape matters here — that it survives the
// URI, lands in the right field, and is accepted by sing-box.
const testCertPEM = `-----BEGIN CERTIFICATE-----
MIIBhTCCASugAwIBAgIQIRi6zePL6mKjOipn+dNuaTAKBggqhkjOPQQDAjASMRAw
DgYDVQQKEwdBY21lIENvMB4XDTE3MTAyMDE5NDMwNloXDTE4MTAyMDE5NDMwNlow
EjEQMA4GA1UEChMHQWNtZSBDbzBZMBMGByqGSM49AgEGCCqGSM49AwEHA0IABD0d
7VNhbWvZLWPuj/RtHFjvtJBEwOkhbN/BnnE8rnZR8+sbwnc/KhCk3FhnpHZnQz7B
5aETbbIgmuvewdjvSBSjYzBhMA4GA1UdDwEB/wQEAwICpDATBgNVHSUEDDAKBggr
BgEFBQcDATAPBgNVHRMBAf8EBTADAQH/MCkGA1UdEQQiMCCCDmxvY2FsaG9zdDo1
NDUzgg4xMjcuMC4wLjE6NTQ1MzAKBggqhkjOPQQDAgNIADBFAiEA2zpJEPQyz6/l
Wf86aX6PepsntZv2GYlA5UpabfT2EZICICpJ5h/iI+i341gBmLiAFQOyTDT+/wQc
6MF9+Yw1Yy0t
-----END CERTIFICATE-----`

func pinnedURI(extra string) string {
	return "hy2://pw@45.32.28.60:443?sni=node-abc123.net&pinnedcert=" +
		base64.RawURLEncoding.EncodeToString([]byte(testCertPEM)) + extra
}

func TestPinnedCertLandsInTLSConfig(t *testing.T) {
	o, err := parseServerURL(pinnedURI(""))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if o.TLS == nil {
		t.Fatal("no TLS config emitted")
	}
	if o.TLS.Insecure {
		t.Error("a pinned outbound must not skip verification")
	}
	if len(o.TLS.Certificate) == 0 {
		t.Fatal("certificate not carried into the outbound")
	}
	joined := strings.Join(o.TLS.Certificate, "\n")
	if joined != testCertPEM {
		t.Errorf("certificate mangled in transit:\n%s", joined)
	}
	if o.TLS.ServerName != "node-abc123.net" {
		t.Errorf("SNI = %q, want the name the cert is issued for", o.TLS.ServerName)
	}
}

// A stale link may carry both. The pin is strictly stronger than the flag, so
// it has to win — the reverse would let an old URI quietly keep the hole open.
func TestPinOverridesInsecureFlag(t *testing.T) {
	o, err := parseServerURL(pinnedURI("&insecure=1"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if o.TLS.Insecure {
		t.Error("insecure=1 survived alongside a pinned certificate")
	}
	if len(o.TLS.Certificate) == 0 {
		t.Error("certificate dropped when insecure=1 was also present")
	}
}

// Servers provisioned before pinning must keep working exactly as before,
// including the part we dislike — failing them closed would strand the user.
func TestLegacyInsecureURIStillParses(t *testing.T) {
	o, err := parseServerURL("hy2://pw@45.32.28.60:443?insecure=1&sni=bing.com")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !o.TLS.Insecure {
		t.Error("legacy unpinned server lost its insecure flag and would now fail to connect")
	}
	if len(o.TLS.Certificate) != 0 {
		t.Error("legacy server somehow acquired a pinned certificate")
	}
}

// A corrupt pin must not silently become "trust anything".
func TestCorruptPinDoesNotDowngradeToInsecure(t *testing.T) {
	o, err := parseServerURL("hy2://pw@45.32.28.60:443?sni=node-abc123.net&pinnedcert=!!!not-base64!!!")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if o.TLS != nil && o.TLS.Insecure {
		t.Error("a corrupt pin turned into a verification bypass")
	}
}

func TestValidateFlagsUnpinnedInsecureTLS(t *testing.T) {
	insecure := &SingboxConfig{Outbounds: []OutboundConfig{
		{Type: "hysteria2", Tag: "proxy", TLS: &TLSConfig{Enabled: true, Insecure: true}},
	}}
	if !insecureProxyTLS(insecure) {
		t.Error("an unverified outbound was not flagged")
	}
	pinned := &SingboxConfig{Outbounds: []OutboundConfig{
		{Type: "hysteria2", Tag: "proxy", TLS: &TLSConfig{Enabled: true, Certificate: []string{testCertPEM}}},
	}}
	if insecureProxyTLS(pinned) {
		t.Error("a pinned outbound was flagged as unverified")
	}
}

// The unit tests above prove mole emits the fields it means to. This one proves
// sing-box accepts them: the `certificate` field takes an array of PEM *lines*,
// and a shape mismatch here would only ever surface as a tunnel that refuses to
// start — at the exact moment the user has no working network to debug with.
func TestSingboxAcceptsThePinnedOutbound(t *testing.T) {
	bin, err := exec.LookPath("sing-box")
	if err != nil {
		t.Skip("sing-box not installed; skipping real-binary validation")
	}

	o, err := parseServerURL(pinnedURI("&obfs=salamander&obfs-password=deadbeef"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if o.Obfs == nil || o.Obfs.Password != "deadbeef" {
		t.Fatal("obfs did not survive parsing")
	}

	cfg := map[string]any{"outbounds": []any{o}}
	blob, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(t.TempDir(), "pinned.json")
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if out, err := exec.Command(bin, "check", "-c", path).CombinedOutput(); err != nil {
		t.Fatalf("sing-box rejected the pinned outbound: %v\n%s\n%s", err, out, blob)
	}
}
