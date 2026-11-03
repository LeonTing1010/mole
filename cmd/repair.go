package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/LeonTing1010/mole/utils"
)

// `mole server repair`: put a node's servers back the way the boot script
// meant to, over SSH, from this machine.
//
// What it is for: a node whose REALITY port REFUSES. The boot script installs
// sing-box from a .deb, and on a fresh Ubuntu the first minutes belong to
// apt's own timers, which hold the dpkg lock; the 2026-09-04 node lost that
// race and ran seventeen hours with one leg. The boot script now waits and
// retries, and this is the remedy for the node that still slips through — and
// for any other reason a unit is down that a `systemctl enable --now` fixes.
//
// How it gets in: the nodes accept nothing but UDP/443, TCP/443 and ICMP from
// the internet (see ensureFirewallRules), so SSH means a temporary tcp/22 rule
// for this machine's public address, added through the API before and deleted
// after. The SSH session is bound to the physical interface, because an
// unbound one rides the TUN to the very node it is trying to reach and
// arrives from the node's own address — measured, twice (items 12 and 42 in
// the project notes).
var serverRepairCmd = &cobra.Command{
	Use:   "repair [name]",
	Short: "Reinstall a node's servers over SSH when a leg is refusing",
	Long: `Reinstalls sing-box (REALITY, TCP/443) and re-enables hysteria2 on a node,
over SSH from this machine. Opens tcp/22 for your public address in the
Vultr firewall group for the duration and closes it after.

Use it when "mole status" says the REALITY server is not listening, or when a
deploy ended with a leg refusing.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runRepair,
}

var repairCheckOnly bool

func init() {
	serverCmd.AddCommand(serverRepairCmd)
	serverRepairCmd.Flags().BoolVar(&repairCheckOnly, "check", false, "only probe both legs from outside the tunnel; change nothing")
}

func runRepair(_ *cobra.Command, args []string) error {
	servers, err := loadServers()
	if err != nil {
		return err
	}
	var srv *Server
	if len(args) == 1 {
		if i := indexByName(servers, args[0]); i >= 0 {
			srv = &servers[i]
		} else {
			return fmt.Errorf("no server named %q", args[0])
		}
	} else {
		srv, err = ActiveServer()
		if err != nil {
			return err
		}
	}
	if srv.IP == "" && srv.IPv6 == "" {
		return fmt.Errorf("%s has no address yet", srv.Name)
	}
	if repairCheckOnly {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return verifyRecordedNode(ctx, *srv, 20*time.Second)
	}

	keyPath, err := localSSHKeyPath()
	if err != nil {
		return err
	}
	ifi, err := utils.DefaultPhysicalInterface()
	if err != nil {
		return fmt.Errorf("no physical uplink to reach the node from: %w", err)
	}

	fmt.Printf("🔧 Repairing %s (%s)\n", srv.Name, srv.IP)
	myIP, err := directPublicIP()
	if err != nil {
		return fmt.Errorf("could not learn this machine's public address (needed for the firewall rule): %w", err)
	}
	groupID, err := ensureFirewallGroup()
	if err != nil {
		return fmt.Errorf("firewall group: %w", err)
	}
	ruleID, err := addFirewallRule(groupID, "tcp", "22", myIP, "mole repair (temporary)")
	if err != nil {
		return fmt.Errorf("open tcp/22 for %s: %w", myIP, err)
	}
	fmt.Printf("   opened tcp/22 for %s (rule %d) — closed again when this finishes\n", myIP, ruleID)
	defer func() {
		if derr := deleteFirewallRule(groupID, ruleID); derr != nil {
			fmt.Printf("⚠️  could not close tcp/22 (rule %d): %v — delete it in the Vultr console\n", ruleID, derr)
		} else {
			fmt.Println("   closed tcp/22 again")
		}
	}()
	_ = utils.AllowDirectHost(srv.IP)

	ssh := func(script string) (string, error) {
		return sshRun(keyPath, ifi.Name, srv.IP, script, 3*time.Minute)
	}
	fmt.Print("   waiting for SSH")
	var lastErr error
	for i := 0; i < 24; i++ {
		if _, lastErr = ssh("true"); lastErr == nil {
			break
		}
		fmt.Print(".")
		time.Sleep(5 * time.Second)
	}
	fmt.Println()
	if lastErr != nil {
		return fmt.Errorf("SSH never connected: %v (the rule can take a minute or two to propagate; try again)", lastErr)
	}

	// Whether the REALITY config survived decides whether the identity does.
	// The private key lives only on the node, so a missing config means a new
	// identity, a new public key for the client, and a restart to pick it up.
	out, _ := ssh("test -s /etc/sing-box/config.json && echo present || echo missing")
	configMissing := strings.TrimSpace(out) == "missing"
	var configBlock string
	if configMissing && !srv.isReality() && srv.hasStandbyLeg() || configMissing && srv.isReality() {
		rid, err := newRealityIdentity(srv.RealityDest)
		if err != nil {
			return fmt.Errorf("mint a new REALITY identity: %w", err)
		}
		port := int(srv.RealityPort)
		if port == 0 {
			port = int(srv.Port)
		}
		configBlock = realityConfigSnippet(port, rid)
		srv.UUID, srv.RealityPubKey, srv.RealityShortID, srv.RealityDest = rid.UUID, rid.PublicKey, rid.ShortID, rid.Dest
		if err := changeServer(srv.InstanceID, srv.Name, func(current *Server) {
			current.UUID = srv.UUID
			current.RealityPubKey = srv.RealityPubKey
			current.RealityShortID = srv.RealityShortID
			current.RealityDest = srv.RealityDest
			current.Verification = nil
		}); err != nil {
			return err
		}

		fmt.Println("   REALITY config was missing on the node — minted a new identity (servers.json updated; restart mole afterwards)")
	}

	fmt.Println("   reinstalling sing-box and re-enabling both units...")
	out, err = ssh(repairScript(configBlock, srv.DownMbps))
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line != "" {
			fmt.Println("   | " + line)
		}
	}
	if err != nil {
		return fmt.Errorf("repair script failed: %w", err)
	}

	hy2, reality := verifyNodeLegs(*srv, 45*time.Second)
	if !reality.ok() && srv.realityAddr() != "" {
		return fmt.Errorf("REALITY still %s after the repair — see the script output above", reality.Verdict)
	}
	if !hy2.ok() && !srv.isReality() {
		return fmt.Errorf("hysteria2 still %s after the repair — see the script output above", hy2.Verdict)
	}
	if configBlock != "" {
		fmt.Println("↻  The client now needs the new REALITY key: run `mole down && mole up`.")
	}
	return nil
}

// repairScript is the node-side half. It is the install part of the boot
// script with its failure modes handled explicitly: the dpkg lock waited for,
// the install retried, the package's conffile prompt suppressed (dpkg asks
// interactively when /etc/sing-box/config.json already exists, and a
// non-interactive stdin leaves the package half-configured with no service
// user — the 217/USER failure seen 2026-09-05), and the postinst finished
// before the unit is started.
func repairScript(configBlock string, downMbps int) string {
	return nodeInstallFunctions + `
mole_install_singbox
` + configBlock + `
test -s /etc/sing-box/config.json || { echo "no /etc/sing-box/config.json"; exit 4; }
sing-box check -c /etc/sing-box/config.json
PORT=$(grep -o '"listen_port": *[0-9]*' /etc/sing-box/config.json | grep -o '[0-9]*$')
iptables -C INPUT -p tcp --dport "$PORT" -j ACCEPT 2>/dev/null || iptables -I INPUT -p tcp --dport "$PORT" -j ACCEPT 2>/dev/null || true
systemctl enable --now sing-box >/dev/null 2>&1 || true
systemctl restart sing-box
systemctl enable --now hysteria-server >/dev/null 2>&1 || true
sleep 2
echo "sing-box: $(systemctl is-active sing-box)   hysteria-server: $(systemctl is-active hysteria-server)"
ss -lntup 2>/dev/null | grep -E ':443 ' | awk '{print $1, $5, $7}'
set +e
` + brutalKeeperSnippet(downMbps) + `
echo "brutal: module $(lsmod | grep -q '^brutal' && echo loaded || echo MISSING)   keeper: $(systemctl is-active mole-brutal-keeper)"
`
}

// realityConfigSnippet renders just the config-file part of the REALITY
// server install, for a node whose config is gone.
func realityConfigSnippet(port int, id *realityIdentity) string {
	full := realityServerSnippet(port, 0, id)
	start := strings.Index(full, "install -d -m 755 /etc/sing-box")
	end := strings.Index(full, "if command -v ufw")
	if start < 0 || end < 0 || end < start {
		return ""
	}
	return full[start:end]
}

// sshRun executes script on the node as root, bound to ifname.
func sshRun(keyPath, ifname, ip, script string, budget time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh",
		"-i", keyPath,
		"-o", "BindInterface="+ifname,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=8",
		"root@"+ip, "bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// localSSHKeyPath is the private half of localSSHPubKey.
func localSSHKeyPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
		p := filepath.Join(home, ".ssh", name)
		if _, err := os.Stat(p); err == nil {
			if _, perr := os.Stat(p + ".pub"); perr == nil {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("no SSH key pair in ~/.ssh (the node accepts the key that was installed at deploy)")
}

// directPublicIP asks a domestic echo service, over the physical interface,
// what address this machine appears from. Domestic on purpose: the kill switch
// fails foreign egress closed on that interface, and a fake-IP answer from
// inside the tunnel would name the VPS.
func directPublicIP() (string, error) {
	c, err := utils.NewBoundHTTPClient(8 * time.Second)
	if err != nil {
		return "", err
	}
	ipv4 := regexp.MustCompile(`\b(\d{1,3}\.){3}\d{1,3}\b`)
	var lastErr error
	for _, u := range []string{"http://members.3322.org/dyndns/getip", "https://myip.ipip.net"} {
		resp, err := c.Get(u)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if ip := ipv4.FindString(string(body)); ip != "" {
			return ip, nil
		}
		lastErr = fmt.Errorf("%s: no address in %q", u, strings.TrimSpace(string(body)))
	}
	return "", lastErr
}

// addFirewallRule adds one inbound accept rule to the group and returns its id.
// If the rule already exists (same protocol+port+subnet), it returns the existing id instead of failing.
func addFirewallRule(groupID, protocol, port, ipv4, notes string) (int, error) {
	body := map[string]any{
		"ip_type": "v4", "protocol": protocol, "port": port,
		"subnet": ipv4, "subnet_size": 32, "notes": notes,
	}
	resp, err := serverAPIWithin(30*time.Second, "POST", "/firewalls/"+groupID+"/rules", body)
	if err != nil {
		// Vultr returns 400 {"error":"This rule is already defined"} for duplicates.
		// In that case, find the existing rule id and return it so the rest of
		// repair can proceed (the defer still closes it — no harm re-deleting).
		if strings.Contains(err.Error(), "already defined") {
			return findExistingFirewallRule(groupID, protocol, port, ipv4)
		}
		return 0, err
	}
	var parsed struct {
		Rule struct {
			ID int `json:"id"`
		} `json:"firewall_rule"`
	}
	if err := json.Unmarshal(resp, &parsed); err != nil || parsed.Rule.ID == 0 {
		return 0, fmt.Errorf("unexpected reply: %s", strings.TrimSpace(string(resp)))
	}
	return parsed.Rule.ID, nil
}

// findExistingFirewallRule GETs all rules on the group and returns the id of the
// first inbound accept rule matching protocol+port+subnet, or 0 if none matches.
func findExistingFirewallRule(groupID, protocol, port, ipv4 string) (int, error) {
	resp, err := serverAPIWithin(30*time.Second, "GET", "/firewalls/"+groupID+"/rules?per_page=100", nil)
	if err != nil {
		return 0, err
	}
	var parsed struct {
		Rules []struct {
			ID       int    `json:"id"`
			Action   string `json:"action"`
			Protocol string `json:"protocol"`
			Port     string `json:"port"`
			Subnet   string `json:"subnet"`
		} `json:"firewall_rules"`
	}
	if err := json.Unmarshal(resp, &parsed); err != nil {
		return 0, fmt.Errorf("unexpected GET reply: %s", strings.TrimSpace(string(resp)))
	}
	for _, r := range parsed.Rules {
		if r.Action == "accept" && r.Protocol == protocol && r.Port == port && r.Subnet == ipv4 {
			return r.ID, nil
		}
	}
	return 0, fmt.Errorf("rule already defined but couldn't find it on the group")
}

func deleteFirewallRule(groupID string, ruleID int) error {
	_, err := serverAPIWithin(30*time.Second, "DELETE", fmt.Sprintf("/firewalls/%s/rules/%d", groupID, ruleID), nil)
	return err
}
