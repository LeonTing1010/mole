package utils

import (
	"encoding/json"
	"errors"
	"fmt"
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
	return systemDNSSettings().takeOver()
}

type dnsSettings struct {
	path string
	list func() ([]string, error)
	get  func(string) ([]string, error)
	set  func(string, []string) error
}

func systemDNSSettings() dnsSettings {
	return dnsSettings{dnsBackupPath(), listNetworkServices, getDNS, setDNS}
}

// Persist before mutation; preserve originals left by a crashed daemon.
func (m dnsSettings) takeOver() error {
	services, err := m.list()
	if err != nil {
		return err
	}
	backup, err := m.readBackup()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, svc := range services {
		if _, exists := backup[svc]; exists {
			continue
		}
		dns, err := m.get(svc)
		if err != nil {
			return fmt.Errorf("read DNS for %s: %w", svc, err)
		}
		backup[svc] = stripTunGateway(dns)
	}
	if err := m.saveBackup(backup); err != nil {
		return fmt.Errorf("save DNS backup: %w", err)
	}
	var errs []error
	for _, svc := range services {
		if err := m.set(svc, []string{tunGatewayDNS}); err != nil {
			errs = append(errs, fmt.Errorf("set DNS for %s: %w", svc, err))
		}
	}
	return errors.Join(errs...)
}

func (m dnsSettings) readBackup() (map[string][]string, error) {
	backup := make(map[string][]string)
	data, err := os.ReadFile(m.path)
	if err != nil {
		return backup, err
	}
	if err := json.Unmarshal(data, &backup); err != nil {
		return nil, fmt.Errorf("decode DNS backup: %w", err)
	}
	if backup == nil {
		return nil, fmt.Errorf("invalid null DNS backup")
	}
	return backup, nil
}

func (m dnsSettings) saveBackup(backup map[string][]string) error {
	data, err := json.MarshalIndent(backup, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(m.path), ".dns-backup-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), m.path); err != nil {
		return err
	}
	ChownToMoleDirOwner(m.path)
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

// RestoreDNS reports incomplete recovery and retains its backup for retry.
// With no backup, recover a dead TUN resolver using DHCP defaults.
func RestoreDNS() error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	return systemDNSSettings().restore()
}

func (m dnsSettings) restore() error {
	backup, err := m.readBackup()
	if errors.Is(err, os.ErrNotExist) {
		services, err := m.list()
		if err != nil {
			return err
		}
		var errs []error
		for _, svc := range services {
			current, err := m.get(svc)
			if err != nil {
				errs = append(errs, fmt.Errorf("read DNS for %s: %w", svc, err))
				continue
			}
			clean := stripTunGateway(current)
			if len(clean) == len(current) {
				continue
			}
			if len(clean) == 0 {
				clean = []string{"Empty"}
			}
			if err := m.set(svc, clean); err != nil {
				errs = append(errs, fmt.Errorf("restore DNS for %s: %w", svc, err))
			}
		}
		return errors.Join(errs...)
	}
	if err != nil {
		return err
	}
	pending := make(map[string][]string)
	var errs []error
	for svc, dns := range backup {
		original := dns
		if len(dns) == 0 {
			dns = []string{"Empty"}
		}
		if err := m.set(svc, dns); err != nil {
			pending[svc] = original
			errs = append(errs, fmt.Errorf("restore DNS for %s: %w", svc, err))
		}
	}
	if len(pending) > 0 {
		return errors.Join(errors.Join(errs...), m.saveBackup(pending))
	}
	return os.Remove(m.path)
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

// setDNS points one network service at the given resolvers.
//
// Skips sudo when already root, the way pfctl() does. Functionally this changes
// nothing today — sudo invoked by root never authenticates — but the daemon
// restores DNS from a context where asking a human for anything is impossible:
// it runs unattended, and after a destroy-on-lid-close it runs with the lid shut.
// Code on that path should not read as though it might prompt.
func setDNS(service string, dns []string) error {
	args := append([]string{"-setdnsservers", service}, dns...)
	var cmd *exec.Cmd
	if os.Geteuid() == 0 {
		cmd = exec.Command("networksetup", args...)
	} else {
		cmd = exec.Command("sudo", append([]string{"networksetup"}, args...)...)
	}
	return cmd.Run()
}
