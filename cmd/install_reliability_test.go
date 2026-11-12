package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Safety / what: failed or corrupted downloads cannot advance installation.
func TestRequiredDownloadFailureStopsInstallation(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		dir := t.TempDir()
		curl := "#!/bin/sh\nexit 22\n"
		if corrupt {
			curl = "#!/bin/sh\nwhile [ $# -gt 0 ]; do if [ \"$1\" = -o ]; then shift; echo corrupted > \"$1\"; fi; shift; done\n"
		}
		if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(curl), 0700); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", "-c", nodeInstallFunctions+"\nmole_download https://invalid.test/file 0000000000000000000000000000000000000000000000000000000000000000 \"$PWD/artifact\"\necho INSTALL_CONTINUED\n")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "MOLE_DOWNLOAD_ATTEMPTS=1")
		out, err := cmd.CombinedOutput()
		if err == nil || strings.Contains(string(out), "INSTALL_CONTINUED") {
			t.Fatalf("corrupt=%v err=%v out=%s", corrupt, err, out)
		}
	}
}

// Quality / what-x-what: optional optimization cannot gate the REALITY listener.
func TestRealityStartsBeforeOptionalOptimization(t *testing.T) {
	rid := &realityIdentity{UUID: "u", PrivateKey: "key", ShortID: "aabb", Dest: "www.apple.com"}
	s := realityServerSnippet(443, 80, rid)
	start, optional := strings.Index(s, "systemctl enable --now sing-box"), strings.Index(s, "systemctl start --no-block mole-brutal-install")
	if start < 0 || optional < 0 || start > optional {
		t.Fatal("optional install blocks REALITY startup")
	}
}

// Safety / what-x-what: strict boot umask must not lock service users out.
func TestBootConfigurationRemainsReadableByServiceGroup(t *testing.T) {
	id, err := newServerIdentity()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, d := range []string{"etc/systemd/system", "etc/hysteria", "bin"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"systemctl", "ufw", "iptables", "sysctl", "chown"} {
		if err := os.WriteFile(filepath.Join(root, "bin", name), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	script := strings.Replace(sandboxBootPrerequisites(hy2BootScript("test-password", 443, id)), nodeInstallFunctions, "set -euo pipefail\nmole_install_hy2(){ :; }\nmole_run(){ \"$@\"; }\n", 1)
	script = strings.NewReplacer("/usr/local/lib", root+"/usr/local/lib", "/etc/", root+"/etc/", "/var/log/", root+"/var/log/", "/root/", root+"/root/").Replace(script)
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+filepath.Join(root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("boot sandbox: %v %s", err, out)
	}
	cfg, err := os.Stat(filepath.Join(root, "etc/hysteria/config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode().Perm() != 0640 {
		t.Fatalf("service configuration permissions=%o, want 640", cfg.Mode().Perm())
	}
	dir, err := os.Stat(filepath.Join(root, "etc/hysteria"))
	if err != nil {
		t.Fatal(err)
	}
	if dir.Mode().Perm()&0050 != 0050 {
		t.Fatalf("service directory cannot be traversed: %o", dir.Mode().Perm())
	}
}

// Safety / audit: package locks are waited for, never removed or killed, and
// their wait consumes the same required-install deadline as package execution.
func TestPackageLockWaitUsesInstallationDeadline(t *testing.T) {
	script := nodeInstallFunctions + "\nMOLE_INSTALL_DEADLINE=1\nfuser(){ return 0; }\nmole_wait_dpkg\necho INSTALL_CONTINUED\n"
	out, err := exec.Command("bash", "-c", script).CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 124 || strings.Contains(string(out), "INSTALL_CONTINUED") {
		t.Fatalf("lock wait did not honor budget: err=%v out=%s", err, out)
	}
}
