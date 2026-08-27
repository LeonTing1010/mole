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
// The boot script is rendered by the caller, because what a node needs
// installed is decided by the protocol it will speak and by identity material
// that is minted on this machine — neither of which this function should have
// an opinion about.
func deployViaVultr(name, region, plan, script string) (instanceID string, err error) {
	key := strings.TrimSpace(os.Getenv("VULTR_API_KEY"))
	if key == "" {
		return "", fmt.Errorf("VULTR_API_KEY is not set")
	}

	scriptResp, err := serverAPIWithin(60*time.Second, "POST", "/startup-scripts", map[string]any{
		"name":   name,
		"type":   "boot",
		"script": base64.StdEncoding.EncodeToString([]byte(script)),
	})
	if err != nil {
		return "", fmt.Errorf("create startup script: %w", err)
	}
	var created struct {
		StartupScript struct {
			ID string `json:"id"`
		} `json:"startup_script"`
	}
	if err := json.Unmarshal(scriptResp, &created); err != nil {
		return "", err
	}
	if created.StartupScript.ID == "" {
		return "", fmt.Errorf("Vultr returned no startup-script id")
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
		return "", fmt.Errorf("create instance: %w", err)
	}
	var inst struct {
		Instance struct {
			ID string `json:"id"`
		} `json:"instance"`
	}
	if err := json.Unmarshal(instResp, &inst); err != nil {
		return "", err
	}
	if inst.Instance.ID == "" {
		return "", fmt.Errorf("Vultr returned no instance id")
	}
	return inst.Instance.ID, nil
}

// moleFirewallGroup is the description used to find (or create) the one
// firewall group every mole node shares.
const moleFirewallGroup = "mole-nodes"

// moleFirewallRules is the entire inbound surface of a mole node. Vultr
// firewall groups are default-deny and stateful, so this list is exhaustive.
//
// Both tunnel protocols are open on every node, on purpose. UDP/443 carries
// hysteria2 and TCP/443 carries VLESS+REALITY, and a node has to be able to
// switch legs — the whole reason the REALITY leg exists is that UDP/443 on this
// path gets selectively throttled, which is discovered while the node is
// already running. ICMP is not decorative either: `ping -b en0 <node>` is the
// one check that separates a dark path from a flooded Brutal ceiling, and
// losing it leaves the two indistinguishable.
//
// Nothing opens SSH: nodes are key-only, and the Vultr web console remains the
// way in when something is truly broken.
var moleFirewallRules = []map[string]any{
	{"ip_type": "v4", "protocol": "udp", "subnet": "0.0.0.0", "subnet_size": 0, "port": "443", "notes": "hysteria2"},
	{"ip_type": "v6", "protocol": "udp", "subnet": "::", "subnet_size": 0, "port": "443", "notes": "hysteria2 v6"},
	{"ip_type": "v4", "protocol": "tcp", "subnet": "0.0.0.0", "subnet_size": 0, "port": "443", "notes": "vless reality"},
	{"ip_type": "v6", "protocol": "tcp", "subnet": "::", "subnet_size": 0, "port": "443", "notes": "vless reality v6"},
	{"ip_type": "v4", "protocol": "icmp", "subnet": "0.0.0.0", "subnet_size": 0, "notes": "path diagnostics"},
}

// ensureFirewallGroup returns the id of the shared firewall group, creating it
// if it doesn't exist and adding any rule from moleFirewallRules that is
// missing.
//
// It reconciles rather than merely creating, because the rule set grows: a
// group created before the REALITY leg existed opens UDP/443 only, and a
// REALITY node attached to it would boot listening on a port the cloud firewall
// silently drops — a node that looks deployed, answers ping, and refuses every
// connection. Reconciling is additive: rules added by hand are never removed.
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
			return g.ID, ensureFirewallRules(g.ID)
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
	return id, ensureFirewallRules(id)
}

// ensureFirewallRules adds every missing rule to an existing group. Matching is
// on what identifies a rule to Vultr — address family, protocol and port —
// rather than on the note, so a rule someone added by hand with a different
// description is recognised instead of being duplicated.
func ensureFirewallRules(groupID string) error {
	listed, err := serverAPIWithin(30*time.Second, "GET", "/firewalls/"+groupID+"/rules", nil)
	if err != nil {
		return err
	}
	var existing struct {
		FirewallRules []struct {
			IPType   string `json:"ip_type"`
			Protocol string `json:"protocol"`
			Port     string `json:"port"`
		} `json:"firewall_rules"`
	}
	if err := json.Unmarshal(listed, &existing); err != nil {
		return err
	}
	have := make(map[string]bool, len(existing.FirewallRules))
	for _, r := range existing.FirewallRules {
		have[firewallRuleKey(r.IPType, r.Protocol, r.Port)] = true
	}

	for _, rule := range moleFirewallRules {
		port, _ := rule["port"].(string)
		key := firewallRuleKey(rule["ip_type"].(string), rule["protocol"].(string), port)
		if have[key] {
			continue
		}
		if _, err := serverAPIWithin(30*time.Second, "POST", "/firewalls/"+groupID+"/rules", rule); err != nil {
			return fmt.Errorf("add firewall rule %v: %w", rule["notes"], err)
		}
	}
	return nil
}

// firewallRuleKey normalises Vultr's port spelling: a single port comes back as
// "443" on some responses and "443 - 443" on others, and ICMP carries no port
// at all.
func firewallRuleKey(ipType, protocol, port string) string {
	port = strings.NewReplacer(" ", "", "-", ":").Replace(port)
	if from, to, ok := strings.Cut(port, ":"); ok && from == to {
		port = from
	}
	return ipType + "/" + protocol + "/" + port
}

// The port-hopping range a fresh node is provisioned with, in the three
// spellings that need to agree: the client URI (`?ports=`), ufw, and iptables.
// They are derived from one pair so a change can never leave the client hopping
// onto ports the firewall drops.
const (
	hopPortLow  = 20000
	hopPortHigh = 50000
)

var (
	hopRangeURI = fmt.Sprintf("%d-%d", hopPortLow, hopPortHigh) // hysteria/`?ports=` spelling
	hopRangeUFW = fmt.Sprintf("%d:%d", hopPortLow, hopPortHigh) // ufw wants a colon
	hopRangeIPT = fmt.Sprintf("%d:%d", hopPortLow, hopPortHigh) // iptables --dport wants a colon
)

// hopIntervalSeconds is how often a client moves port when hopping is switched
// on. Not hysteria's 30s default: measured on this path, 30s cost 90% of
// throughput and 120s cost 28%, because every hop is a peer-address change the
// server's QUIC re-validates and Brutal restarts from. 120s is the least-bad
// setting for anyone who needs hopping at all.
const hopIntervalSeconds = 120

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
	sshHardening := sshHardeningScript(id.SSHPubKey)

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
if command -v ufw >/dev/null 2>&1; then ufw allow %d/udp; ufw allow %d/tcp; ufw allow %s/udp; fi
iptables -I INPUT -p udp --dport %d -j ACCEPT 2>/dev/null || true
iptables -I INPUT -p tcp --dport %d -j ACCEPT 2>/dev/null || true
# Port hopping. The listen port is left exactly as it was and the range is NAT'd
# onto it, so a client that hops and a client that doesn't both keep working —
# which is what makes this safe to roll out before any client knows about it.
# (hysteria's own "listen: :LOW-HIGH" would move the listener off %d instead,
# breaking every existing client the moment the server restarts.)
#
# Conntrack rewrites the reply source back to the port the client addressed, so
# the hop is invisible from the other end. See Server.HopPorts for why hopping
# earns its keep: an established five-tuple can stop being carried while new
# ones are fine, and no restart or ceiling change reaches that.
cat > /etc/systemd/system/mole-porthop.service << 'HOP_EOF'
[Unit]
Description=mole: redirect the hysteria2 port-hopping range to the listen port
After=network-online.target
Wants=network-online.target
Before=hysteria-server.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh -c 'iptables -t nat -C PREROUTING -p udp --dport %s -j REDIRECT --to-ports %d 2>/dev/null || iptables -t nat -A PREROUTING -p udp --dport %s -j REDIRECT --to-ports %d'
ExecStop=/bin/sh -c 'iptables -t nat -D PREROUTING -p udp --dport %s -j REDIRECT --to-ports %d 2>/dev/null || true'

[Install]
WantedBy=multi-user.target
HOP_EOF
systemctl daemon-reload
systemctl enable --now mole-porthop.service 2>/dev/null || true
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
		// ufw: base udp, base tcp, hop range
		port, port, hopRangeUFW,
		// iptables INPUT accepts for the base port
		port, port,
		// the porthop unit: base listen port in the config comment, then
		// check/append/delete triples of (range, port)
		port,
		hopRangeIPT, port, hopRangeIPT, port,
		hopRangeIPT, port,
		sshHardening)
}

// sshHardeningScript renders the key-only-root fragment shared by every boot
// script, or "" when this machine has no key to install. Vultr publishes a
// generated root password through its own API for a window after provisioning,
// so the account API key is transitively root on every node; installing the key
// before switching passwords off keeps the ordering safe, and the Vultr web
// console stays the backstop either way.
func sshHardeningScript(pubKey string) string {
	if strings.TrimSpace(pubKey) == "" {
		return ""
	}
	return fmt.Sprintf(`
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
`, strings.TrimSpace(pubKey))
}

// realityBootScript renders the first-boot script for a VLESS+REALITY node.
//
// REALITY is the standby leg for the failure mode the hysteria2 leg keeps
// hitting: this path is repeatedly QoS'd on UDP/443 while the physical link
// tests clean, which no ceiling change can fix. VLESS+REALITY rides TCP/443 and
// is, on the wire, a TLS 1.3 handshake to Dest carried out by the VPS on the
// client's behalf — same ClientHello a browser sends, same certificate that site
// really serves, so there is no self-signed leaf to catch and no obfuscation
// keying to keep in sync.
//
// The server is sing-box, the same implementation as the client. That is
// deliberate: an Xray server works equally well (both were measured working
// here), but running one binary on both ends means one version to reason about
// when a handshake starts failing.
//
// Note what is NOT in this script: no certificate, no key, no obfs password.
// The only secrets are the VLESS uuid and the REALITY private key, and the
// client never needs the latter.
func realityBootScript(port int, id *realityIdentity) string {
	return "#!/bin/bash\nset -e\n" + realityServerSnippet(port, id) + sshHardeningScript(id.SSHPubKey)
}

// realityServerSnippet installs and starts the REALITY server, with no shebang
// and no SSH hardening, so it can be appended to a hysteria2 boot script as well
// as standing on its own.
//
// Appending it is the normal case: every node mole deploys runs BOTH legs.
// hysteria2 owns UDP/443 and REALITY owns TCP/443 — different protocols, so
// they never contend for the port — and the cloud firewall group opens both on
// every node for exactly this reason. A standby leg that only existed on nodes
// deployed with a flag would be missing on the node that replaces this one, and
// with destroy-on-lid-close that is most mornings.
//
// The two servers are separate units (hysteria-server and sing-box) rather than
// one sing-box carrying both inbounds. That keeps the hysteria2 leg — the one
// carrying traffic almost all the time — byte-identical to what it was, so
// adding the standby cannot regress it.
func realityServerSnippet(port int, id *realityIdentity) string {
	return fmt.Sprintf(`
curl -fsSL https://sing-box.app/install.sh | sh
mkdir -p /etc/sing-box
cat > /etc/sing-box/config.json << 'EOF'
{
  "log": {"level": "warn"},
  "inbounds": [
    {
      "type": "vless",
      "tag": "in",
      "listen": "::",
      "listen_port": %d,
      "users": [{"uuid": "%s", "flow": "xtls-rprx-vision"}],
      "tls": {
        "enabled": true,
        "server_name": "%s",
        "reality": {
          "enabled": true,
          "handshake": {"server": "%s", "server_port": 443},
          "private_key": "%s",
          "short_id": ["%s"]
        }
      }
    }
  ],
  "outbounds": [{"type": "direct", "tag": "direct"}]
}
EOF
if command -v ufw >/dev/null 2>&1; then ufw allow %d/tcp; fi
iptables -I INPUT -p tcp --dport %d -j ACCEPT 2>/dev/null || true
systemctl enable --now sing-box
`,
		port, id.UUID, id.Dest, id.Dest, id.PrivateKey, id.ShortID,
		port, port)
}
