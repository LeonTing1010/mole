package cmd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
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
func deployViaVultr(name, region, plan, script string, session *deploySession, ctx context.Context) (instanceID string, err error) {
	// Same key resolution as every other server command: the environment first,
	// then the file the daemon reads. Deploy used to look only at the
	// environment, so `mole server destroy` worked while `mole server deploy`
	// demanded an export of a key the machine already held.
	if _, err := vultrKey(); err != nil {
		return "", err
	}

	if err = session.stage("creating_startup_script", nil); err != nil {
		return "", err
	}
	scriptID, err := createStartupScript(name+"-"+session.record.ID, script)
	if err != nil {
		_ = session.stage("startup_script_unconfirmed", err)
		return "", fmt.Errorf("create startup script: %w", err)
	}

	session.record.ScriptID = scriptID
	if err = session.stage("preparing_firewall", nil); err != nil {
		return "", err
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
		"script_id":   scriptID,
		"backups":     "disabled",
		"enable_ipv6": true,
	}
	if fwID != "" {
		body["firewall_group_id"] = fwID
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if err = session.stage("creating_instance", nil); err != nil {
		return "", err
	}
	instResp, err := serverAPIWithin(60*time.Second, "POST", "/instances", body)
	if err != nil {
		_ = session.stage("instance_creation_unconfirmed", err)
		return "", fmt.Errorf("create instance outcome unconfirmed; inspect deployment %s before creating again: %w", session.record.ID, err)
	}
	var inst struct {
		Instance struct {
			ID string `json:"id"`
		} `json:"instance"`
	}
	if err := json.Unmarshal(instResp, &inst); err != nil {
		_ = session.stage("instance_creation_unconfirmed", err)
		return "", err
	}
	if inst.Instance.ID == "" {
		_ = session.stage("instance_creation_unconfirmed", fmt.Errorf("missing instance ID"))
		return "", fmt.Errorf("Vultr returned no instance id; creation outcome unknown")
	}
	return inst.Instance.ID, nil
}

// createStartupScript retries a script creation only after a complete lookup.
// Reuse additionally requires identical content: a name is not an identity.
// Instance creation remains single-shot because an unknown POST can bill.
func createStartupScript(name, script string) (string, error) {
	payload := map[string]any{
		"name":   name,
		"type":   "boot",
		"script": base64.StdEncoding.EncodeToString([]byte(script)),
	}

	// Allow a few verify-before-retry rounds. The inner POST call itself is
	// still single-shot (serverAPIWithin does not retry POST), so each round
	// is one POST + optional one GET for verification.
	const maxRounds = 3
	var lastErr error
	for round := 1; round <= maxRounds; round++ {
		resp, err := scriptAPICall(60*time.Second, "POST", "/startup-scripts", payload)
		if err == nil {
			var created struct {
				StartupScript struct {
					ID string `json:"id"`
				} `json:"startup_script"`
			}
			if err := json.Unmarshal(resp, &created); err != nil {
				return "", err
			}
			if created.StartupScript.ID == "" {
				return "", fmt.Errorf("Vultr returned no startup-script id")
			}
			return created.StartupScript.ID, nil
		}

		// Only server-side errors are candidates for retry. A 4xx means Vultr
		// received and rejected the request — repeating it will get the same
		// answer. Transport errors (timeout, EOF, DNS) are also not retried
		// here; serverAPIWithin already retries those on its own for idempotent
		// calls, and we'd be doubling up on the wrong layer.
		var se *ServerError
		if !errors.As(err, &se) || se.Status < 500 {
			return "", fmt.Errorf("create startup script: %w", err)
		}
		lastErr = err

		// Before retrying, check whether the script actually got created — a
		// 502/503/504 can leave the resource in either state.
		id, found, verr := findStartupScriptByName(name)
		if verr != nil {
			return "", fmt.Errorf("startup-script creation outcome unknown; verification failed: %w", verr)
		}
		if found {
			data, err := scriptAPICall(30*time.Second, "GET", "/startup-scripts/"+url.PathEscape(id), nil)
			if err != nil {
				return "", fmt.Errorf("startup-script content unverified: %w", err)
			}
			var existing struct {
				StartupScript struct {
					Script string `json:"script"`
				} `json:"startup_script"`
			}
			if err := json.Unmarshal(data, &existing); err != nil {
				return "", err
			}
			if existing.StartupScript.Script != payload["script"] {
				return "", fmt.Errorf("startup script %q exists with different content; refusing to reuse its identity", name)
			}
			fmt.Printf("Startup script recovered after HTTP %d; identity content verified.\n", se.Status)
			return id, nil
		}
		if round < maxRounds {
			time.Sleep(time.Duration(round) * 2 * time.Second)
		}
	}
	return "", fmt.Errorf("create startup script: %w (creation not confirmed after complete lookups)", lastErr)
}

// findStartupScriptByName lists Vultr startup scripts and returns the id of
// the first one whose name matches, or ("", false, nil) if none exists.
func findStartupScriptByName(name string) (string, bool, error) {
	path := "/startup-scripts?per_page=100"
	seen := map[string]bool{}
	for path != "" {
		if seen[path] || len(seen) >= 1000 {
			return "", false, fmt.Errorf("startup-script pagination did not terminate")
		}
		seen[path] = true
		resp, err := scriptAPICall(30*time.Second, "GET", path, nil)
		if err != nil {
			return "", false, err
		}
		var wrapper struct {
			StartupScripts []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"startup_scripts"`
			Meta struct {
				Links struct {
					Next string `json:"next"`
				} `json:"links"`
			} `json:"meta"`
		}
		if err := json.Unmarshal(resp, &wrapper); err != nil {
			return "", false, err
		}
		for _, script := range wrapper.StartupScripts {
			if script.Name == name {
				return script.ID, true, nil
			}
		}
		next := wrapper.Meta.Links.Next
		if next == "" {
			break
		}
		u, err := url.Parse(next)
		if err != nil {
			return "", false, err
		}
		if u.Host != "" && u.Host != "api.vultr.com" {
			return "", false, fmt.Errorf("unexpected pagination origin")
		}
		if u.Path == "" {
			u.Path = "/startup-scripts"
		}
		u.Path = strings.TrimPrefix(u.Path, "/v2")
		if u.Path != "/startup-scripts" {
			return "", false, fmt.Errorf("unexpected pagination path %q", u.Path)
		}
		path = u.RequestURI()
	}
	return "", false, nil
}

// scriptAPICall is the thin wrapper createStartupScript and
// findStartupScriptByName use to reach Vultr. A var (not a fixed call) so
// tests can swap it for a plain-http.Client call against a local httptest
// server — serverAPIWithin's bound-physical-interface client can't reach a
// loopback address, and every real POST in this file still goes through
// serverAPIWithin directly.
var scriptAPICall = func(budget time.Duration, method, path string, body any) ([]byte, error) {
	return serverAPIWithin(budget, method, path, body)
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

// dualBootScript prepares SSH recovery before either independent installer.
// Both children inherit one absolute deadline and are joined before script exit.
// Deployment readiness is still determined by external authenticated HTTPS.
func dualBootScript(password string, port, downMbps int, id *serverIdentity, rid *realityIdentity) string {
	return "#!/bin/bash\n" + nodeBootPrelude + nodeBootPrereq + sshHardeningScript(id.SSHPubKey) + nodeNetworkTuning +
		"\n(\n" + hy2ServerSnippet(password, port, id) + "\n) &\nhy2_pid=$!\n" +
		"(\n" + realityServerSnippet(port, downMbps, rid) + "\n) &\nreality_pid=$!\n" +
		"wait \"$hy2_pid\" || true\nwait \"$reality_pid\" || true\n"
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
// The two passwords are quoted YAML scalars, not bare ones. A bare scalar that
// is nothing but digits resolves as a number: harmless while the password was
// 32 hex characters, because a 32-digit integer overflows int64 and the parser
// falls back to a string — but tunnelPassword puts a '.' in it, and the same
// accident then resolves as a float and arrives on the server as something
// other than the password the client holds. Quoting removes the coincidence
// rather than relying on one.
//
// SSH is switched to key-only when we have a key to install. Vultr hands out a
// generated root password through its own API for a window after provisioning,
// so the account API key is transitively root on every node — and from root,
// the tunnel password, the obfs key and the TLS private key are all readable.
// Installing the key before disabling passwords keeps the order safe; the Vultr
// web console remains the backstop either way.
func hy2BootScript(password string, port int, id *serverIdentity) string {
	return "#!/bin/bash\n" + nodeBootPrelude + nodeBootPrereq + sshHardeningScript(id.SSHPubKey) + nodeNetworkTuning + hy2ServerSnippet(password, port, id)
}

// hy2ServerSnippet keeps the required leg independent from the optional cleanup.
func hy2ServerSnippet(password string, port int, id *serverIdentity) string {
	return fmt.Sprintf(`
# The hysteria2 segment runs in its own subshell. "set -e" still holds inside
# it, but a failure now terminates only this segment: the REALITY segment
# can still run independently, and the node keeps SSH access either way. One
# broken leg should cost one leg, not the machine.
(
%s
echo "mole stage hy2 installing $(date -Is)"
mole_install_hy2
install -d -m 755 /etc/hysteria
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
# QUIC packet sizing follows Hysteria defaults; no unsupported top-level MTU key.
tls:
  cert: /etc/hysteria/server.crt
  key: /etc/hysteria/server.key
auth:
  type: password
  password: "%s"
obfs:
  type: salamander
  salamander:
    password: "%s"
EOF
chown root:hysteria /etc/hysteria/config.yaml
chmod 640 /etc/hysteria/config.yaml
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
mole_run systemctl enable --now hysteria-server
mole_run systemctl is-active --quiet hysteria-server
echo "mole stage hy2 service_active $(date -Is)"
# Trim Ubuntu cloud-image fat. Every VPS ships with a pile of services that a
# forwarder never needs — daemons that probe modems, schedule disk IO,
# subscribe to telemetry, or update packages unattended. Disabling them after
# the tunnel is up removes ~50MB of steady-state RSS and, more importantly,
# prevents them from CPU-spiking during apt runs or probe storms that would
# otherwise steal cycles from hy2. The list is conservative (only services
# Vultr instances have been observed running) and everything is "|| true" so a
# missing binary never aborts the boot script.
mole_run systemctl disable --now snapd snapd.socket snapd.seeded.service unattended-upgrades ModemManager multipathd open-iscsi iscsid apport apport-gather thermald vgauth ubuntu-advantage ua-reboot-cmds pollinate console-setup.service keyboard-setup.service getty@tty1.service sysstat rsyslog cron secureboot-db.service 2>/dev/null || true
)
if ! systemctl is-active --quiet hysteria-server 2>/dev/null; then
  echo "HY2 LEG NOT RUNNING: hysteria-server is not active. SSH access and the REALITY segment are unaffected; run 'mole server repair' to reinstall it." >&2
fi`,
		nodeInstallFunctions,
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
	)
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
func realityBootScript(port, downMbps int, id *realityIdentity) string {
	// Key first, for the same reason hy2BootScript puts it first: a REALITY
	// install that fails must not also cost the ability to log in and fix it.
	// The snippet carries its own subshell and "set -e", so this script needs no
	// top-level one.
	return "#!/bin/bash\n" + nodeBootPrelude + nodeBootPrereq + sshHardeningScript(id.SSHPubKey) + nodeNetworkTuning + "\n" + realityServerSnippet(port, downMbps, id)
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
//
// The direct outbound carries a `domain_resolver` pinned to `prefer_ipv6`, and
// that is the line deciding which address a site sees. The client hands domains
// across the leg, not addresses — its DNS answers every foreign name with a
// fakeip and sing-box recovers the real name from the sniffed SNI — so the
// client's own `ipv4_only` strategy never gets a vote on a dual-stack
// destination. Measured on the icn node 2026-09-27: an AAAA-only probe exited
// from the node's own 2401:c080:… while cloudflare.com, which has both records,
// exited from its 141.164.35.8. Same node, same tunnel, two exit families.
//
// In sing-box 1.14.4 this direct dialer uses parallel address fallback. Keep
// IPv6 preference separate from the client's IPv4-only node connection; verify
// both exit families and dead-AAAA fallback when upgrading the engine.
//
// The `dns.servers` entry above exists only so the resolver has a tag to name;
// `local` is what the node was already resolving with. It is spelled this way
// rather than as `domain_strategy` on the outbound because that field is a hard
// FATAL in the sing-box the node installs (deprecated in 1.12, gone in 1.14) —
// and a config sing-box refuses to start leaves the node with no REALITY leg at
// all, which is the failure `mole server repair` exists to clean up.
func realityServerSnippet(port, downMbps int, id *realityIdentity) string {
	return fmt.Sprintf(`
# sing-box arrives as a .deb, and on a fresh Ubuntu the first minutes of boot
# belong to apt-daily/unattended-upgrades, which hold the dpkg frontend lock.
# The icn-1788515745 node (2026-09-04) lost that race: install.sh printed
# "dpkg frontend lock was locked by another process", the enable failed with
# "Unit file sing-box.service does not exist", and the node ran for 17 hours
# with TCP/443 refusing — the REALITY leg had never existed, and every
# fallback re-test said "still will not dial". So: wait for the lock, retry
# the install, and only then write the config (writing it first turns the
# package's own conffile into an interactive prompt that kills dpkg).
#
# The subshell isolates this leg. When appended after the hysteria2 segment, a
# failure here must not abort the rest of the boot script; when it stands alone,
# the SSH key is already installed. Either way the machine stays reachable.
(
%s
echo "mole stage reality installing $(date -Is)"
mole_install_singbox
install -d -m 755 /etc/sing-box
cat > /etc/sing-box/config.json << 'EOF'
{
  "log": {"level": "warn"},
  "dns": {"servers": [{"type": "local", "tag": "local"}]},
  "inbounds": [
    {
      "type": "vless",
      "tag": "in",
      "listen": "::",
      "listen_port": %d,
      "tcp_fast_open": true,
      "users": [{"uuid": "%s", "flow": "xtls-rprx-vision"}],
      "multiplex": {"enabled": true},
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
  "outbounds": [{"type": "direct", "tag": "direct", "domain_resolver": {"server": "local", "strategy": "prefer_ipv6"}}]
}
EOF
chown root:sing-box /etc/sing-box/config.json
chmod 640 /etc/sing-box/config.json
if command -v ufw >/dev/null 2>&1; then ufw allow %d/tcp; fi
iptables -I INPUT -p tcp --dport %d -j ACCEPT 2>/dev/null || true
sing-box check -c /etc/sing-box/config.json
mole_run systemctl enable --now sing-box
mole_run systemctl is-active --quiet sing-box
echo "mole stage reality service_active $(date -Is)"
%s
)
if ! systemctl is-active --quiet sing-box 2>/dev/null; then
  echo "REALITY LEG NOT RUNNING: sing-box is not active. The hysteria2 leg and SSH access are unaffected; run 'mole server repair' to reinstall it." >&2
fi
`,
		nodeInstallFunctions,
		port, id.UUID, id.Dest, id.Dest, id.PrivateKey, id.ShortID,
		port, port, brutalKeeperSnippet(downMbps))
}
