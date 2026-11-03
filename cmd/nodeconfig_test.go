package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The node's config is written by a boot script onto a box nobody can log into,
// and sing-box refuses to start on a config it rejects — which leaves the node
// with no REALITY leg, discovered hours later by a person rather than by the
// deploy. `domain_strategy` on an outbound became exactly that in sing-box 1.12
// (deprecated, then a hard FATAL), so the generated JSON is fed to the local
// sing-box instead of being eyeballed.
func TestNodeConfigPassesSingBoxCheck(t *testing.T) {
	bin, err := exec.LookPath("sing-box")
	if err != nil {
		t.Skip("sing-box not in $PATH")
	}
	rid := &realityIdentity{
		UUID:       "00000000-0000-4000-8000-000000000000",
		PrivateKey: "UuMBgl7MXTPx9inmQp2UCba9DTadeW58edN3jJ_eGnI",
		PublicKey:  "pub",
		ShortID:    "aabbccdd",
		Dest:       "www.apple.com",
	}
	snippet := realityServerSnippet(443, 100, rid)

	const start = "cat > /etc/sing-box/config.json << 'EOF'\n"
	i := strings.Index(snippet, start)
	if i < 0 {
		t.Fatal("snippet no longer writes /etc/sing-box/config.json via a heredoc")
	}
	rest := snippet[i+len(start):]
	j := strings.Index(rest, "\nEOF\n")
	if j < 0 {
		t.Fatal("heredoc is not terminated")
	}
	config := rest[:j+1]

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(bin, "check", "-c", path).CombinedOutput()
	if err != nil {
		t.Errorf("sing-box rejects the node config: %v\n%s\n--- config ---\n%s", err, out, config)
	}
}
