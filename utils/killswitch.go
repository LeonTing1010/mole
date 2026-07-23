package utils

import (
	_ "embed"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The kill switch is a PF anchor that makes "foreign traffic must ride the
// tunnel" a property of the HOST, not of the sing-box process. sing-box's
// auto_route only protects while sing-box is alive: the moment it dies, its
// routes vanish and every foreign connection silently falls back to the
// physical interface — traffic doesn't stop, it LEAKS. The anchor outlives
// the process: physical-interface egress is blocked except for the VPS
// endpoint, Chinese IPv4 space, and local infrastructure, so a dead sing-box
// means foreign = blocked (fail-closed) while domestic direct keeps working.
//
// The anchor lives under com.apple/* because the stock /etc/pf.conf already
// evaluates that wildcard — rules load into it without editing any system
// file. Rules are scoped to physical interfaces (en*/bridge*), so utun
// traffic is never touched.
//
// Deliberate consequence: if the daemon is SIGKILL'd, nothing unloads the
// anchor and foreign stays blocked until the next `mole up`/`mole down`.
// That is the feature — the alternative is leaking until someone notices.
const pfAnchor = "com.apple/mole"

// cnCIDRURL is the canonical China IPv4 CIDR list (APNIC-derived, one CIDR
// per line). Same lineage of data as the geoip-cn rule-set sing-box routes
// by, so the PF table and sing-box agree on what "China" means.
const cnCIDRURL = "https://raw.githubusercontent.com/17mon/china_ip_list/master/china_ip_list.txt"

// cnCIDRMaxAge mirrors geosite-cn's refresh cadence. CN address allocations
// move slowly; weekly is plenty.
const cnCIDRMaxAge = 7 * 24 * time.Hour

// cnCIDRMinEntries rejects a fetched list that is implausibly small. A
// truncated download must never become the PF table: the table is what keeps
// domestic traffic FLOWING while everything else is blocked, so a bad table
// converts "kill switch" into "kill everything".
const cnCIDRMinEntries = 1000

//go:embed cn-cidr.txt
var embeddedCNCIDRs string

// CNCIDRPath is the on-disk China IPv4 list the PF table loads from.
func CNCIDRPath() string { return filepath.Join(MoleDir(), "cn-cidr.txt") }

func pfRulesPath() string { return filepath.Join(MoleDir(), "pf-killswitch.conf") }

// pfTokenPath stores the pfctl -E reference token so `mole down` can release
// our enable reference (-X) instead of force-disabling PF for everyone.
func pfTokenPath() string { return filepath.Join(MoleDir(), "pf-token") }

// EnsureCNCIDRs makes sure a valid China CIDR list exists on disk, mirroring
// EnsureRuleSets: fetch when missing or stale, validate before replacing,
// fall back to the cached then the embedded copy. Never blocks startup.
func EnsureCNCIDRs() {
	path := CNCIDRPath()
	if st, err := os.Stat(path); err == nil && st.Size() > 0 && time.Since(st.ModTime()) < cnCIDRMaxAge {
		return
	}
	if err := fetchCNCIDRs(path); err != nil {
		if st, statErr := os.Stat(path); statErr == nil && st.Size() > 0 {
			fmt.Printf("⚠️  CN CIDR list refresh failed: %v (using cached copy)\n", err)
			return
		}
		fmt.Printf("⚠️  CN CIDR list fetch failed: %v (using embedded copy)\n", err)
		_ = os.WriteFile(path, []byte(embeddedCNCIDRs), 0644)
	}
}

func fetchCNCIDRs(dst string) error {
	client := newDirectHTTPClient(30 * time.Second)
	resp, err := client.Get(cnCIDRURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	valid := filterIPv4CIDRs(string(body))
	if len(valid) < cnCIDRMinEntries {
		return fmt.Errorf("only %d valid CIDRs (want ≥%d) — refusing truncated list", len(valid), cnCIDRMinEntries)
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(valid, "\n")+"\n"), 0644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// filterIPv4CIDRs keeps only lines that parse as IPv4 CIDRs. The PF table is
// IPv4-only by design: IPv6 egress on physical interfaces is blocked outright
// (link-local excepted), which is also mole's AAAA-leak posture.
func filterIPv4CIDRs(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ip, _, err := net.ParseCIDR(line)
		if err != nil || ip.To4() == nil {
			continue
		}
		out = append(out, line)
	}
	return out
}

// physicalInterfaces returns every present en*/bridge* interface. Down
// interfaces are included on purpose: an idle USB NIC that comes up later
// must already be covered. Interfaces that APPEAR after load (rare) are not
// covered until the next `mole up`.
func physicalInterfaces() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return []string{"en0"}
	}
	var out []string
	for _, ifc := range ifaces {
		if strings.HasPrefix(ifc.Name, "en") || strings.HasPrefix(ifc.Name, "bridge") {
			out = append(out, ifc.Name)
		}
	}
	if len(out) == 0 {
		out = []string{"en0"}
	}
	return out
}

// buildPFRules renders the anchor ruleset. Per physical interface, first
// match wins (quick): VPS endpoint (the tunnel itself + the health probe),
// local/broadcast infrastructure, DHCP, Chinese IPv4 space, IPv6 link-local
// — then block everything else, both families. utun/lo are never mentioned,
// so tunnel-bound traffic is untouched.
func buildPFRules(vpsIP string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# generated by mole — do not edit; unloaded by `mole down`\n")
	fmt.Fprintf(&b, "table <mole_cn4> persist file \"%s\"\n", CNCIDRPath())
	for _, ifc := range physicalInterfaces() {
		fmt.Fprintf(&b, "pass out quick on %s to %s\n", ifc, vpsIP)
		fmt.Fprintf(&b, "pass out quick on %s inet to { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 169.254.0.0/16, 100.64.0.0/10, 224.0.0.0/4, 255.255.255.255/32 }\n", ifc)
		fmt.Fprintf(&b, "pass out quick on %s proto udp from any port 68 to any port 67\n", ifc)
		fmt.Fprintf(&b, "pass out quick on %s inet to <mole_cn4>\n", ifc)
		fmt.Fprintf(&b, "pass out quick on %s inet6 to { fe80::/10, ff02::/16 }\n", ifc)
		fmt.Fprintf(&b, "block out quick on %s inet all\n", ifc)
		fmt.Fprintf(&b, "block out quick on %s inet6 all\n", ifc)
	}
	return b.String()
}

// LoadKillSwitch writes the ruleset, loads it into the anchor, and takes a
// PF enable reference. Requires root (the daemon has it).
func LoadKillSwitch(vpsIP string) error {
	if _, err := os.Stat(CNCIDRPath()); err != nil {
		// EnsureCNCIDRs runs in the parent, but the daemon must not depend on
		// that having succeeded — fall back to the embedded snapshot.
		if err := os.WriteFile(CNCIDRPath(), []byte(embeddedCNCIDRs), 0644); err != nil {
			return fmt.Errorf("write embedded CN list: %w", err)
		}
	}
	rules := buildPFRules(vpsIP)
	if err := os.WriteFile(pfRulesPath(), []byte(rules), 0644); err != nil {
		return fmt.Errorf("write rules: %w", err)
	}
	if out, err := pfctl("-a", pfAnchor, "-f", pfRulesPath()); err != nil {
		return fmt.Errorf("load anchor: %v (%s)", err, strings.TrimSpace(out))
	}
	out, err := pfctl("-E")
	if err != nil {
		_, _ = pfctl("-a", pfAnchor, "-F", "all")
		return fmt.Errorf("enable pf: %v (%s)", err, strings.TrimSpace(out))
	}
	// pfctl -E reports a reference token on stderr; keep it so unload can
	// release exactly our reference. A missing token is survivable — unload
	// then just flushes the anchor and leaves PF enabled (rules are gone, so
	// nothing is blocked).
	if m := regexp.MustCompile(`Token\s*:\s*(\d+)`).FindStringSubmatch(out); m != nil {
		_ = os.WriteFile(pfTokenPath(), []byte(m[1]), 0644)
	}
	return nil
}

// UnloadKillSwitch flushes the anchor and releases our PF enable reference.
// Safe to call when nothing is loaded; uses sudo when invoked unprivileged
// (same posture as RestoreDNS).
func UnloadKillSwitch() {
	_, _ = pfctl("-a", pfAnchor, "-F", "all")
	if data, err := os.ReadFile(pfTokenPath()); err == nil {
		if token := strings.TrimSpace(string(data)); token != "" {
			_, _ = pfctl("-X", token)
		}
		_ = os.Remove(pfTokenPath())
	}
}

// KillSwitchLoaded reports whether this machine currently has our anchor
// armed. Token-file based so unprivileged `mole status` can answer it.
func KillSwitchLoaded() bool {
	_, err := os.Stat(pfTokenPath())
	return err == nil
}

func pfctl(args ...string) (string, error) {
	var cmd *exec.Cmd
	if os.Geteuid() == 0 {
		cmd = exec.Command("pfctl", args...)
	} else {
		cmd = exec.Command("sudo", append([]string{"pfctl"}, args...)...)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}
