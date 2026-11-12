package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Quality / what-x-what: a slow protocol installer cannot hold the other one
// behind it. Why: users wait for the slower independent leg, not their sum.
func TestDualBootInstallersProgressIndependently(t *testing.T) {
	root := runDeployBootSandbox(t, "", true)
	for _, leg := range []string{"hy2", "reality"} {
		assertBootFile(t, root, "installer-passed-"+leg)
	}
	hy2, _ := os.ReadFile(filepath.Join(root, "deadline-hy2"))
	reality, _ := os.ReadFile(filepath.Join(root, "deadline-reality"))
	if len(hy2) == 0 || string(hy2) != string(reality) {
		t.Fatalf("installers must share one absolute deadline: hy2=%q reality=%q", hy2, reality)
	}
}

// Safety / what-x-what: an install failure must leave SSH recovery access and
// the other requested protocol usable. Why: one broken download costs one leg.
func TestDualBootFailurePreservesOtherLeg(t *testing.T) {
	for _, failed := range []string{"hy2", "reality"} {
		t.Run(failed, func(t *testing.T) {
			root := runDeployBootSandbox(t, failed, true)
			good, bad := "sing-box", "hysteria-server"
			if failed == "reality" {
				good, bad = bad, good
			}
			assertBootFile(t, root, "running-"+good)
			assertBootFile(t, root, "root/.ssh/authorized_keys")
			if _, err := os.Stat(filepath.Join(root, "running-"+bad)); !os.IsNotExist(err) {
				t.Fatalf("failed %s installation started its service: %v", failed, err)
			}
		})
	}
}

// Quality / what: optional cleanup must observe the tunnel already running.
// Why: stopping unrelated boot services cannot delay first usable HY2 traffic.
func TestBootStartsHY2BeforeServiceCleanup(t *testing.T) {
	root := runDeployBootSandbox(t, "", false)
	assertBootFile(t, root, "cleanup-ran")
	if _, err := os.Stat(filepath.Join(root, "cleanup-before-hy2")); !os.IsNotExist(err) {
		t.Fatal("optional cleanup ran before the HY2 service was available")
	}
}

func assertBootFile(t *testing.T, root, name string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, name)); err != nil {
		log, _ := os.ReadFile(filepath.Join(root, "var/log/mole/boot.log"))
		t.Fatalf("missing boot outcome %s: %v\n%s", name, err, log)
	}
}

func runDeployBootSandbox(t *testing.T, failed string, barrier bool) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"bin", "etc/systemd/system", "etc/sysctl.d", "etc/hysteria", "etc/sing-box"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, script string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, "bin", name), []byte("#!/bin/bash\nset -eu\n"+script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write("install-leg", `
test -s "$MOLE_TEST_ROOT/root/.ssh/authorized_keys"
echo "$MOLE_INSTALL_DEADLINE" > "$MOLE_TEST_ROOT/deadline-$1"
touch "$MOLE_TEST_ROOT/installer-entered-$1"
if [ "$MOLE_TEST_BARRIER" = true ]; then
  other=hy2; [ "$1" = hy2 ] && other=reality
  for attempt in $(seq 1 100); do
    [ ! -f "$MOLE_TEST_ROOT/installer-entered-$other" ] || break
    sleep .02
  done
  test -f "$MOLE_TEST_ROOT/installer-entered-$other"
fi
test "$MOLE_TEST_FAIL" != "$1"
touch "$MOLE_TEST_ROOT/installer-passed-$1"
`)
	write("systemctl", `
if [ "$1" = is-active ]; then
  for service in "$@"; do
    case "$service" in sing-box|hysteria-server) test -f "$MOLE_TEST_ROOT/running-$service"; exit $?;; esac
  done
fi
if [ "$1" = enable ]; then
  for service in "$@"; do
    case "$service" in sing-box|hysteria-server) touch "$MOLE_TEST_ROOT/running-$service";; esac
  done
fi
if [ "$1" = disable ]; then
  if [ ! -f "$MOLE_TEST_ROOT/running-hysteria-server" ]; then
    touch "$MOLE_TEST_ROOT/cleanup-before-hy2"
  fi
  touch "$MOLE_TEST_ROOT/cleanup-ran"
fi
`)
	for _, name := range []string{"ufw", "iptables", "chown", "sing-box"} {
		write(name, "exit 0\n")
	}
	write("sysctl", `test "${MOLE_TEST_SYSCTL_FAIL:-}" != 1`+"\n")
	write("date", `
if [ "${1:-}" = -Is ]; then exec /bin/date -u '+%Y-%m-%dT%H:%M:%SZ'; fi
exec /bin/date "$@"
`)
	id := &serverIdentity{CertPEM: "test certificate", KeyPEM: "test key", SSHPubKey: "ssh-ed25519 test", ObfsPassword: "test-obfs"}
	rid := &realityIdentity{UUID: "test-user", PrivateKey: "test-key", ShortID: "aabb", Dest: "www.apple.com"}
	script := dualBootScript("test-password", 443, 80, id, rid)
	script = sandboxBootPrerequisites(script)
	script = strings.ReplaceAll(script, nodeInstallFunctions, `
set -euo pipefail
mole_run(){ "$@"; }
mole_install_hy2(){ "$MOLE_TEST_ROOT/bin/install-leg" hy2; }
mole_install_singbox(){ "$MOLE_TEST_ROOT/bin/install-leg" reality; }
`)
	script = strings.ReplaceAll(script, brutalKeeperSnippet(80), "\n: # optional optimization sandbox\n")
	script = strings.NewReplacer("/usr/local/lib", root+"/usr/local/lib", "/etc/", root+"/etc/", "/var/log/", root+"/var/log/", "/root/", root+"/root/").Replace(script)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", script)
	cmd.Env = append(os.Environ(), "MOLE_TEST_ROOT="+root, "MOLE_TEST_FAIL="+failed,
		fmt.Sprintf("MOLE_TEST_BARRIER=%t", barrier),
		"PATH="+filepath.Join(root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("boot sandbox: %v\n%s", err, out)
	}
	return root
}

// Quality/what-x-what: unsupported optional sysctls must leave both protocols usable.
func TestTuningFailurePreservesProtocolReadiness(t *testing.T) {
	t.Setenv("MOLE_TEST_SYSCTL_FAIL", "1")
	root := runDeployBootSandbox(t, "", true)
	assertBootFile(t, root, "running-sing-box")
	assertBootFile(t, root, "running-hysteria-server")
	data, err := os.ReadFile(filepath.Join(root, "var/log/mole/network-apply.status"))
	if err != nil || strings.TrimSpace(string(data)) != "degraded" {
		t.Fatalf("apply failure lost: %s %v", data, err)
	}
}

// These sandboxes exercise independent protocol startup after prerequisites.
// Prerequisite failure, lock exclusion and deadline behavior have separate tests.
func sandboxBootPrerequisites(script string) string {
	script = strings.ReplaceAll(script, nodeBootPrereq, "\nexport MOLE_INSTALL_DEADLINE=$(($(date +%s)+300))\n")
	return strings.ReplaceAll(script, nodeInstallLock, "\n")
}
