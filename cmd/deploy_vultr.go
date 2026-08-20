package cmd

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// deployViaVultr provisions a node by talking to the Vultr API from this
// machine. It is the only provisioning path.
//
// A Cloudflare Worker used to offer a second one, and could never actually
// work: Vultr's API access control allowlists source addresses and a Worker has
// no stable egress to allowlist, so every provisioning call from it came back
// 403. Removing it also took the root-scoped Vultr key out of an
// internet-facing service — the key now exists only on machines that already
// hold it.
//
// Creating an instance is not idempotent, which is the other reason to have
// exactly one path: a failure that leaves the outcome unknown must never be
// retried down an alternative route, or you pay for two servers and adopt
// neither.
func deployViaVultr(name, region, plan string, port int, id *serverIdentity) (instanceID, password string, err error) {
	key := strings.TrimSpace(os.Getenv("VULTR_API_KEY"))
	if key == "" {
		return "", "", fmt.Errorf("VULTR_API_KEY is not set")
	}
	password, err = randomHexString(16)
	if err != nil {
		return "", "", err
	}

	script := hy2BootScript(password, port, id)
	scriptResp, err := serverAPIWithin(60*time.Second, "POST", "/startup-scripts", map[string]any{
		"name":   name,
		"type":   "boot",
		"script": base64.StdEncoding.EncodeToString([]byte(script)),
	})
	if err != nil {
		return "", "", fmt.Errorf("create startup script: %w", err)
	}
	var created struct {
		StartupScript struct {
			ID string `json:"id"`
		} `json:"startup_script"`
	}
	if err := json.Unmarshal(scriptResp, &created); err != nil {
		return "", "", err
	}
	if created.StartupScript.ID == "" {
		return "", "", fmt.Errorf("Vultr returned no startup-script id")
	}

	// Attach the firewall at creation time. Applying it afterwards leaves a
	// window where the node boots with every port open to the internet, and a
	// hardening step that has to be remembered separately is one that will
	// eventually be forgotten.
	fwID, fwErr := ensureFirewallGroup()
	if fwErr != nil {
		fmt.Printf("⚠️  could not prepare the Vultr firewall group: %v\n", fwErr)
		fmt.Println("    The node will boot with no cloud firewall — attach one before relying on it.")
	}

	body := map[string]any{
		"region":      region,
		"plan":        plan,
		"os_id":       2284, // Ubuntu 24.04
		"label":       name,
		"script_id":   created.StartupScript.ID,
		"backups":     "disabled",
		"enable_ipv6": true,
	}
	if fwID != "" {
		body["firewall_group_id"] = fwID
	}
	instResp, err := serverAPIWithin(60*time.Second, "POST", "/instances", body)
	if err != nil {
		return "", "", fmt.Errorf("create instance: %w", err)
	}
	var inst struct {
		Instance struct {
			ID string `json:"id"`
		} `json:"instance"`
	}
	if err := json.Unmarshal(instResp, &inst); err != nil {
		return "", "", err
	}
	if inst.Instance.ID == "" {
		return "", "", fmt.Errorf("Vultr returned no instance id")
	}
	return inst.Instance.ID, password, nil
}

// moleFirewallGroup is the description used to find (or create) the one
// firewall group every mole node shares.
const moleFirewallGroup = "mole-nodes"

// ensureFirewallGroup returns the id of the shared firewall group, creating it
// with the right rules if it doesn't exist yet. Idempotent: an existing group
// is reused untouched, so hand-edited rules survive.
//
// Vultr firewall groups are default-deny inbound and stateful, so the rules
// below are the entire inbound surface: the tunnel, and ICMP. ICMP is not
// decorative — `ping -b en0 <node>` is the one check that separates a dark path
// from a flooded Brutal ceiling, and losing it would leave the two
// indistinguishable. Nothing opens SSH: nodes are key-only, and the Vultr web
// console remains the way in when something is truly broken.
func ensureFirewallGroup() (string, error) {
	listed, err := serverAPIWithin(30*time.Second, "GET", "/firewalls", nil)
	if err != nil {
		return "", err
	}
	var groups struct {
		FirewallGroups []struct {
			ID          string `json:"id"`
			Description string `json:"description"`
		} `json:"firewall_groups"`
	}
	if err := json.Unmarshal(listed, &groups); err != nil {
		return "", err
	}
	for _, g := range groups.FirewallGroups {
		if g.Description == moleFirewallGroup {
			return g.ID, nil
		}
	}

	createdResp, err := serverAPIWithin(30*time.Second, "POST", "/firewalls",
		map[string]any{"description": moleFirewallGroup})
	if err != nil {
		return "", err
	}
	var g struct {
		FirewallGroup struct {
			ID string `json:"id"`
		} `json:"firewall_group"`
	}
	if err := json.Unmarshal(createdResp, &g); err != nil {
		return "", err
	}
	id := g.FirewallGroup.ID
	if id == "" {
		return "", fmt.Errorf("Vultr returned no firewall-group id")
	}

	for _, rule := range []map[string]any{
		{"ip_type": "v4", "protocol": "udp", "subnet": "0.0.0.0", "subnet_size": 0, "port": "443", "notes": "hysteria2"},
		{"ip_type": "v6", "protocol": "udp", "subnet": "::", "subnet_size": 0, "port": "443", "notes": "hysteria2 v6"},
		{"ip_type": "v4", "protocol": "icmp", "subnet": "0.0.0.0", "subnet_size": 0, "notes": "path diagnostics"},
	} {
		if _, err := serverAPIWithin(30*time.Second, "POST", "/firewalls/"+id+"/rules", rule); err != nil {
			return "", fmt.Errorf("add firewall rule %v: %w", rule["notes"], err)
		}
	}
	return id, nil
}

// hy2BootScript renders the first-boot script for a hysteria2 node carrying a
// client-minted identity: the certificate the client will pin, and salamander
// obfuscation keyed to a secret they share.
//
// Obfuscation is what keeps the node unremarkable — with it on there is no TLS
// ClientHello on the wire to fingerprint, and an active prober that sends
// anything else gets silence instead of a self-signed certificate confessing
// what this host is. The certificate is what keeps it safe: hysteria2 sends its
// password inside the TLS session, so an unverified peer walks away with both
// the password and the plaintext.
//
// No masquerade block: under obfuscation no prober ever reaches the HTTP layer
// to be masqueraded at.
//
// SSH is switched to key-only when we have a key to install. Vultr hands out a
// generated root password through its own API for a window after provisioning,
// so the account API key is transitively root on every node — and from root,
// the tunnel password, the obfs key and the TLS private key are all readable.
// Installing the key before disabling passwords keeps the order safe; the Vultr
// web console remains the backstop either way.
func hy2BootScript(password string, port int, id *serverIdentity) string {
	sshHardening := ""
	if id.SSHPubKey != "" {
		sshHardening = fmt.Sprintf(`
mkdir -p /root/.ssh && chmod 700 /root/.ssh
cat > /root/.ssh/authorized_keys << 'AUTHKEY_EOF'
%s
AUTHKEY_EOF
chmod 600 /root/.ssh/authorized_keys
mkdir -p /etc/ssh/sshd_config.d
cat > /etc/ssh/sshd_config.d/99-mole.conf << 'SSHD_EOF'
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin prohibit-password
SSHD_EOF
systemctl restart ssh 2>/dev/null || systemctl restart sshd 2>/dev/null || true
`, strings.TrimSpace(id.SSHPubKey))
	}

	return fmt.Sprintf(`#!/bin/bash
set -e
curl -fsSL https://get.hy2.sh/ | bash
mkdir -p /etc/hysteria
cat > /etc/hysteria/server.crt << 'CERT_EOF'
%s
CERT_EOF
cat > /etc/hysteria/server.key << 'KEY_EOF'
%s
KEY_EOF
chmod 600 /etc/hysteria/server.key
chown hysteria:hysteria /etc/hysteria/server.key /etc/hysteria/server.crt 2>/dev/null || true
cat > /etc/hysteria/config.yaml << 'EOF'
listen: :%d
tls:
  cert: /etc/hysteria/server.crt
  key: /etc/hysteria/server.key
auth:
  type: password
  password: %s
obfs:
  type: salamander
  salamander:
    password: %s
EOF
if command -v ufw >/dev/null 2>&1; then ufw allow %d/udp; ufw allow %d/tcp; fi
iptables -I INPUT -p udp --dport %d -j ACCEPT 2>/dev/null || true
iptables -I INPUT -p tcp --dport %d -j ACCEPT 2>/dev/null || true
cat > /etc/sysctl.d/99-mole.conf << 'SYSCTL_EOF'
# QUIC carries its whole receive window in one UDP socket buffer, and Linux
# defaults that buffer to 208KB. On this path — ~107ms to Seoul — the
# bandwidth-delay product at 100 Mbps is 1.34MB, so the default clamps the
# tunnel long before the link does. Measured 2026-08-20 on the icn node: the
# ceiling sweep plateaued at ~77 Mbps with the default and at ~85 with 16MB,
# and the rungs above 100 stopped adding anything either way. Hysteria raises
# the buffer itself at startup but cannot exceed rmem_max, and it does not
# always say so in the log.
net.core.rmem_max=16777216
net.core.wmem_max=16777216
SYSCTL_EOF
sysctl -p /etc/sysctl.d/99-mole.conf >/dev/null 2>&1 || true
systemctl enable --now hysteria-server
%s`,
		strings.TrimSpace(id.CertPEM),
		strings.TrimSpace(id.KeyPEM),
		port, password, id.ObfsPassword,
		port, port, port, port,
		sshHardening)
}
