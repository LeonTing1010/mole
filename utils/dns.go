package utils

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// tunGatewayDNS must match the first address configured on the TUN inbound.
const tunGatewayDNS = "172.19.0.1"

func dnsBackupPath() string { return filepath.Join(MoleDir(), "dns-backup.json") }

// PreferredDirectDNS returns the resolver used for direct (China) domains.
//
// Pinned to AliDNS 223.5.5.5. We deliberately do NOT pick by query latency:
// that optimizes for how fast a resolver answers, which is unrelated to — and
// often worse for — CDN-edge proximity. In practice the latency race always
// picked Baidu 180.76.76.76 (it answers baidu.com fastest), and Baidu handed
// back far/congested Tencent edges: mp.weixin.qq.com loaded at ~15 KB/s
// (TTFB ~1-3s) vs ~300 KB/s (TTFB ~0.15s) on the AliDNS edge — a ~15x
// difference. AliDNS returns well-localized edges and is already the resolver
// the rest of mole assumes (see directResolver in ipinfo.go, route bypass rules).
func PreferredDirectDNS() string {
	return "223.5.5.5"
}

// TakeOverDNS points every active macOS network service at the TUN gateway,
// after backing up the previous settings. Needed because unprivileged
// sing-box TUN doesn't rewrite system DNS config, so macOS' native resolvers
// (Private Relay, system DoH, DHCP-configured DNS) skip the tunnel and get
// poisoned answers. No-op on non-macOS platforms.
func TakeOverDNS() error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	services, err := listNetworkServices()
	if err != nil {
		return err
	}
	// Only capture a backup when one doesn't already exist. A leftover backup
	// means a previous run was killed before RestoreDNS could run: the current
	// system DNS is already the TUN gateway, so re-capturing now would record
	// 172.19.0.1 as each service's "previous" resolver and a later RestoreDNS
	// would strand every service on a dead TUN gateway — breaking all DNS until
	// the user manually resets it. The existing backup still holds the real
	// pre-takeover values, so leave it untouched.
	if _, err := os.Stat(dnsBackupPath()); err != nil {
		backup := make(map[string][]string, len(services))
		for _, svc := range services {
			dns, _ := getDNS(svc)
			// Belt-and-suspenders: never record the TUN gateway itself, even if
			// the backup file was somehow removed mid-run.
			backup[svc] = stripTunGateway(dns)
		}
		data, _ := json.MarshalIndent(backup, "", "  ")
		_ = os.WriteFile(dnsBackupPath(), data, 0644)
	}
	for _, svc := range services {
		_ = setDNS(svc, []string{tunGatewayDNS})
	}
	return nil
}

// stripTunGateway drops the TUN gateway from a captured DNS list so that a
// half-cleaned previous run (system DNS already pointed at the gateway) can't
// get 172.19.0.1 recorded as a real upstream and later restored as one. An
// empty result restores as "Empty" (DHCP defaults), which RestoreDNS handles.
func stripTunGateway(dns []string) []string {
	out := make([]string, 0, len(dns))
	for _, d := range dns {
		if strings.TrimSpace(d) != tunGatewayDNS {
			out = append(out, d)
		}
	}
	return out
}

// RestoreDNS puts every network service's DNS back to what TakeOverDNS
// recorded. If no backup exists (e.g., after an unclean shutdown), it
// resets every active network service to "Empty" so the system falls back to
// DHCP-provided DNS instead of leaving the TUN gateway (172.19.0.1) as the
// sole resolver, which breaks all network access when the TUN is gone.
// Safe to call even if no backup exists.
func RestoreDNS() {
	if runtime.GOOS != "darwin" {
		return
	}
	data, err := os.ReadFile(dnsBackupPath())
	if err != nil {
		// No backup — reset all services to DHCP defaults.
		services, err := listNetworkServices()
		if err != nil {
			return
		}
		for _, svc := range services {
			_ = setDNS(svc, []string{"Empty"})
		}
		return
	}
	var backup map[string][]string
	if json.Unmarshal(data, &backup) != nil {
		return
	}
	for svc, dns := range backup {
		if len(dns) == 0 {
			dns = []string{"Empty"}
		}
		_ = setDNS(svc, dns)
	}
	_ = os.Remove(dnsBackupPath())
}

func listNetworkServices() ([]string, error) {
	out, err := exec.Command("networksetup", "-listallnetworkservices").Output()
	if err != nil {
		return nil, err
	}
	var services []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		// Skip the header line and disabled services (prefixed with *).
		if line == "" || strings.HasPrefix(line, "*") || strings.HasPrefix(line, "An asterisk") {
			continue
		}
		services = append(services, line)
	}
	return services, nil
}

func getDNS(service string) ([]string, error) {
	out, err := exec.Command("networksetup", "-getdnsservers", service).Output()
	if err != nil {
		return nil, err
	}
	s := strings.TrimSpace(string(out))
	if strings.Contains(s, "aren't any DNS Servers") {
		return nil, nil
	}
	var dns []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			dns = append(dns, line)
		}
	}
	return dns, nil
}

func setDNS(service string, dns []string) error {
	args := append([]string{"networksetup", "-setdnsservers", service}, dns...)
	return exec.Command("sudo", args...).Run()
}
