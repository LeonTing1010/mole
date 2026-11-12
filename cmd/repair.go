package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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

func runRepair(cmd *cobra.Command, args []string) error {
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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
		return verifyRecordedNode(ctx, *srv, 20*time.Second)
	}

	return repairNode(ctx, srv, os.Stdout)
}

// repairNode opens a temporary SSH path, waits for boot installation, repairs
// the failed requested protocols, and verifies both legs. It is shared by `mole server
// repair` and the deploy-time auto-repair fallback, so a deploy that ends with
// one leg up and one refusing heals itself instead of leaving a billing
// instance for the operator to repair by hand.
//
// Output goes to w; runRepair passes os.Stdout, finishDeploy passes the
// deployment log writer so the repair trail lands in the receipt.
func repairNode(parent context.Context, srv *Server, w io.Writer) error {
	if err := parent.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, 12*time.Minute)
	defer cancel()
	probeCtx, probeCancel := context.WithTimeout(ctx, 15*time.Second)
	before, _ := verifyNode(probeCtx, *srv, w)
	probeCancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if before.Status == "ready" {
		return nil
	}
	plan, err := prepareNodeRepair(*srv, before)
	if err != nil {
		return err
	}

	keyPath, err := localSSHKeyPath()
	if err != nil {
		return err
	}
	ifi, err := utils.DefaultPhysicalInterface()
	if err != nil {
		return fmt.Errorf("no physical uplink to reach the node from: %w", err)
	}

	fmt.Fprintf(w, "🔧 Repairing %s (%s)\n", srv.Name, srv.IP)
	myIP, err := directPublicIP(ctx)
	if err != nil {
		return fmt.Errorf("could not learn this machine's public address (needed for the firewall rule): %w", err)
	}
	groupID, err := ensureFirewallGroupContext(ctx)
	if err != nil {
		return fmt.Errorf("firewall group: %w", err)
	}
	ruleID, owned, err := addFirewallRule(ctx, groupID, "tcp", "22", myIP, "mole repair (temporary)")
	if err != nil {
		return fmt.Errorf("open tcp/22 for %s: %w", myIP, err)
	}
	if owned {
		fmt.Fprintf(w, "   opened tcp/22 for %s (rule %d) — closed again when this finishes\n", myIP, ruleID)
	} else {
		fmt.Fprintln(w, "   using existing tcp/22 access; leaving its rule unchanged")
	}
	defer func() {
		if !owned {
			return
		}
		if derr := deleteFirewallRule(groupID, ruleID); derr != nil {
			fmt.Fprintf(w, "⚠️  could not close tcp/22 (rule %d): %v — delete it in the Vultr console\n", ruleID, derr)
		} else {
			fmt.Fprintln(w, "   closed tcp/22 again")
		}
	}()
	if err := utils.AllowDirectHostContext(ctx, srv.IP); err != nil {
		return err
	}

	ssh := func(script string) (string, error) {
		return sshRun(ctx, keyPath, ifi.Name, srv.IP, script, 11*time.Minute)
	}
	fmt.Fprint(w, "   waiting for SSH")
	var lastErr error
	for i := 0; i < 24; i++ {
		if _, lastErr = ssh("true"); lastErr == nil {
			break
		}
		fmt.Fprint(w, ".")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	fmt.Fprintln(w)
	if lastErr != nil {
		return fmt.Errorf("SSH never connected: %v (the rule can take a minute or two to propagate; try again)", lastErr)
	}

	fmt.Fprintln(w, "   waiting for first boot to finish, then repairing failed protocols...")
	out, repairErr := ssh(plan.script)
	// Identity replacement is acknowledged under the node lock. Persist it even
	// if a later service start failed, so retries use the identity actually written.
	changed := false
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		switch line {
		case "MOLE_HY2_IDENTITY_REPLACED":
			srv.TLSCertPEM, srv.SNI = plan.hy2.CertPEM, plan.hy2.SNI
			srv.Obfs, srv.ObfsPassword = "salamander", plan.hy2.ObfsPassword
			changed = true
		case "MOLE_REALITY_IDENTITY_REPLACED":
			srv.UUID, srv.RealityPubKey = plan.reality.UUID, plan.reality.PublicKey
			srv.RealityShortID, srv.RealityDest = plan.reality.ShortID, plan.reality.Dest
			changed = true
		default:
			if line != "" {
				fmt.Fprintln(w, "   | "+line)
			}
		}
	}
	if changed {
		srv.Verification = nil
		if err := changeServer(srv.InstanceID, srv.Name, func(current *Server) {
			current.TLSCertPEM, current.SNI = srv.TLSCertPEM, srv.SNI
			current.Obfs, current.ObfsPassword = srv.Obfs, srv.ObfsPassword
			current.UUID, current.RealityPubKey = srv.UUID, srv.RealityPubKey
			current.RealityShortID, current.RealityDest = srv.RealityShortID, srv.RealityDest
			current.Verification = nil
		}); err != nil {
			return errors.Join(repairErr, err)
		}
		fmt.Fprintln(w, "   restored missing identity; restart the client if it was running")
	}
	if repairErr != nil {
		return fmt.Errorf("repair script failed: %w", repairErr)
	}
	checkCtx, checkCancel := context.WithTimeout(ctx, 45*time.Second)
	defer checkCancel()
	result, verifyErr := verifyNode(checkCtx, *srv, w)
	storeErr := changeServer(srv.InstanceID, srv.Name, func(current *Server) { current.Verification = &result })
	return errors.Join(verifyErr, storeErr)
}

// A plan contains candidate identities, used only when configuration is still
// missing after the shared boot/repair lock has been acquired. Existing keys
// are never rotated just because protocol verification was slow.
type nodeRepairPlan struct {
	script  string
	hy2     *serverIdentity
	reality *realityIdentity
}

func prepareNodeRepair(srv Server, result nodeVerification) (nodeRepairPlan, error) {
	plan := nodeRepairPlan{script: "set -euo pipefail\n" + nodeInstallLock + nodeInstallFunctions + nodeNetworkTuning + "\n"}
	h, r := wantsLegs(srv)
	if h && !result.HY2.ok() {
		id, err := newServerIdentity()
		if err != nil {
			return plan, err
		}
		plan.hy2 = id
		if srv.Obfs == "salamander" && srv.ObfsPassword != "" {
			id.ObfsPassword = srv.ObfsPassword
		}
		full := hy2ServerSnippet(srv.Password, int(srv.Port), id)
		start := strings.Index(full, "install -d -m 755 /etc/hysteria")
		network := strings.Index(full, "if command -v ufw")
		end := strings.Index(full, "mole_run systemctl enable --now hysteria-server")
		if start < 0 || network < start || end < network {
			return plan, fmt.Errorf("HY2 recovery template is incomplete")
		}
		plan.script += `
mole_install_hy2
if ! test -s /etc/hysteria/config.yaml || ! test -s /etc/hysteria/server.key || ! test -s /etc/hysteria/server.crt; then
` + full[start:network] + `
echo MOLE_HY2_IDENTITY_REPLACED
fi
` + full[network:end] + `
mole_run systemctl enable --now hysteria-server
mole_run systemctl restart hysteria-server
mole_run systemctl is-active --quiet hysteria-server
`
	}
	if r && !result.Reality.ok() {
		id, err := newRealityIdentity(srv.RealityDest)
		if err != nil {
			return plan, err
		}
		plan.reality = id
		port := int(srv.RealityPort)
		if port == 0 {
			port = int(srv.Port)
		}
		plan.script += `
mole_install_singbox
if ! test -s /etc/sing-box/config.json; then
` + realityConfigSnippet(port, id) + `
echo MOLE_REALITY_IDENTITY_REPLACED
fi
sing-box check -c /etc/sing-box/config.json
` + fmt.Sprintf("iptables -C INPUT -p tcp --dport %d -j ACCEPT 2>/dev/null || iptables -I INPUT -p tcp --dport %d -j ACCEPT\n", port, port) + `
mole_run systemctl enable --now sing-box
mole_run systemctl restart sing-box
mole_run systemctl is-active --quiet sing-box
` + brutalKeeperSnippet(srv.DownMbps)
	}
	return plan, nil
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
func sshRun(parent context.Context, keyPath, ifname, ip, script string, budget time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(parent, budget)
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
	return string(out), errors.Join(ctx.Err(), err)
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
func directPublicIP(ctx context.Context) (string, error) {
	c, err := utils.NewBoundHTTPClient(8 * time.Second)
	if err != nil {
		return "", err
	}
	ipv4 := regexp.MustCompile(`\b(\d{1,3}\.){3}\d{1,3}\b`)
	var lastErr error
	for _, u := range []string{"http://members.3322.org/dyndns/getip", "https://myip.ipip.net"} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return "", err
		}
		resp, err := c.Do(req)
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
func addFirewallRule(ctx context.Context, groupID, protocol, port, ipv4, notes string) (int, bool, error) {
	body := map[string]any{
		"ip_type": "v4", "protocol": protocol, "port": port,
		"subnet": ipv4, "subnet_size": 32, "notes": notes,
	}
	resp, err := serverAPIContext(ctx, 30*time.Second, "POST", "/firewalls/"+groupID+"/rules", body)
	if err != nil {
		// Vultr returns 400 {"error":"This rule is already defined"} for duplicates.
		// In that case, find the existing rule id and return it so the rest of
		// repair can proceed without claiming ownership or deleting that rule.
		if strings.Contains(err.Error(), "already defined") {
			id, err := findExistingFirewallRule(ctx, groupID, protocol, port, ipv4)
			return id, false, err
		}
		return 0, false, err
	}
	var parsed struct {
		Rule struct {
			ID int `json:"id"`
		} `json:"firewall_rule"`
	}
	if err := json.Unmarshal(resp, &parsed); err != nil || parsed.Rule.ID == 0 {
		return 0, false, fmt.Errorf("unexpected reply: %s", strings.TrimSpace(string(resp)))
	}
	return parsed.Rule.ID, true, nil
}

// findExistingFirewallRule GETs all rules on the group and returns the id of the
// first inbound accept rule matching protocol+port+subnet, or 0 if none matches.
func findExistingFirewallRule(ctx context.Context, groupID, protocol, port, ipv4 string) (int, error) {
	resp, err := serverAPIContext(ctx, 30*time.Second, "GET", "/firewalls/"+groupID+"/rules?per_page=100", nil)
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
