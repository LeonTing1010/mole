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
	backup := make(map[string][]string, len(services))
	for _, svc := range services {
		dns, _ := getDNS(svc)
		backup[svc] = dns
	}
	data, _ := json.MarshalIndent(backup, "", "  ")
	_ = os.WriteFile(dnsBackupPath(), data, 0644)
	for _, svc := range services {
		_ = setDNS(svc, []string{tunGatewayDNS})
	}
	return nil
}

// RestoreDNS puts every network service's DNS back to what TakeOverDNS
// recorded. Safe to call even if no backup exists.
func RestoreDNS() {
	if runtime.GOOS != "darwin" {
		return
	}
	data, err := os.ReadFile(dnsBackupPath())
	if err != nil {
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
