package cmd

import (
	"context"
	"errors"
	"github.com/LeonTing1010/mole/utils"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Safety/what-x-what. Why: a stuck package manager must not consume recovery access.
func TestBootPrerequisiteDeadlinePreservesSSH(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"bin", "run", "etc", "root", "var/log"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{"fuser": "exit 0", "flock": "exit 0", "systemctl": "exit 0"} {
		if err := os.WriteFile(filepath.Join(root, "bin", name), []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	id := &serverIdentity{SSHPubKey: "ssh-ed25519 recovery-test"}
	script := hy2BootScript("test", 443, id)
	script = strings.NewReplacer("/run/", root+"/run/", "/root/", root+"/root/", "/etc/", root+"/etc/", "/var/log/", root+"/var/log/").Replace(script)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", script)
	cmd.WaitDelay = 100 * time.Millisecond
	cmd.Env = append(os.Environ(), "PATH="+root+"/bin:"+os.Getenv("PATH"), "MOLE_PREREQ_BUDGET=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Errorf("prerequisites ignored their deadline: %v %s", err, out)
	}
	if err == nil {
		t.Error("held lock unexpectedly advanced installation")
	}
	key, kerr := os.ReadFile(filepath.Join(root, "root/.ssh/authorized_keys"))
	if kerr != nil || !strings.Contains(string(key), id.SSHPubKey) {
		t.Errorf("SSH recovery missing: %v", kerr)
	}
}

// Safety/what. Why: recovery cannot run package scripts after its budget expired.
func TestExpiredBudgetSkipsPackageConfiguration(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "configuration-ran")
	script := "export MOLE_TEST_MARKER=" + marker + "\n" + nodeInstallFunctions + `
MOLE_INSTALL_DEADLINE=1
uname(){ echo x86_64; }
mktemp(){ echo /unused-mole-test.deb; }
mole_download(){ :; }
fuser(){ return 1; }
dpkg(){ touch "$MOLE_TEST_MARKER"; return 1; }
mole_install_singbox
`
	out, err := exec.Command("bash", "-c", script).CombinedOutput()
	_, markerErr := os.Stat(marker)
	if err == nil || markerErr == nil {
		t.Fatalf("expired budget executed package configuration: %v %s", err, out)
	}
}

// Safety/audit. Why: real same-city nodes can have sub-5ms RTT; this cannot authorize deletion.
func TestLowRTTAloneCannotAuthorizeDestruction(t *testing.T) {
	replies := make([]time.Duration, vetProbes)
	for i := range replies {
		replies[i] = time.Millisecond
	}
	if destructive, why := interceptionVerdict(replies); destructive {
		t.Fatalf("healthy nearby node can be destroyed: %s", why)
	}
}

// Safety/what-x-what. Why: cancellation must precede credentials, network or cloud changes.
func TestCanceledRepairDoesNoWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := repairNode(ctx, &Server{}, os.Stdout)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("repair ignored cancellation: %v", err)
	}
}

// Safety/what. Why: Ctrl-C must stop a running SSH child and its retry loop.
func TestSSHRepairObservesParentCancellation(t *testing.T) {
	dir := t.TempDir()
	ssh := filepath.Join(dir, "ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\nexec sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := sshRun(ctx, "key", "en0", "192.0.2.1", "true", time.Minute)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("SSH not canceled promptly: %v", err)
	}
}

// Quality/what-x-what. Why: rebuilding HY2 must restore its credentials and leave a healthy REALITY unit alone.
func TestRepairPlanRestoresOnlyFailedLeg(t *testing.T) {
	srv := Server{IP: "192.0.2.1", Port: 443, Password: "password", UUID: "u", RealityPubKey: "p"}
	plan, err := prepareNodeRepair(srv, nodeVerification{HY2: legReport{Verdict: utils.ProbeSilent}, Reality: legReport{Verdict: utils.ProbeAlive}})
	if err != nil {
		t.Fatal(err)
	}
	root := repairSandbox(t)
	runRepairSandbox(t, root, plan.script)
	if _, err := os.Stat(filepath.Join(root, "etc/hysteria/server.key")); err != nil {
		t.Fatalf("missing HY2 private key: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "installed-hy2")); err != nil {
		t.Fatal("HY2 binary was not restored")
	}
	if _, err := os.Stat(filepath.Join(root, "installed-reality")); !os.IsNotExist(err) {
		t.Fatal("healthy REALITY was reinstalled")
	}
	if _, err := os.Stat(filepath.Join(root, "restarted-sing-box")); !os.IsNotExist(err) {
		t.Fatal("healthy REALITY was restarted")
	}
}

func repairSandbox(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"bin", "run", "etc/hysteria", "etc/sing-box", "etc/systemd/system"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{
		"flock": "exit 0", "chown": "exit 0", "iptables": "exit 0", "ufw": "exit 0", "sing-box": "exit 0",
		"systemctl": `[ "$1" != restart ] || touch "$MOLE_TEST_ROOT/restarted-$2"; exit 0`,
	} {
		if err := os.WriteFile(filepath.Join(root, "bin", name), []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func sandboxRepairScript(root, script string) string {
	script = strings.ReplaceAll(script, nodeNetworkTuning, "\n: # optional tuning has its own sandbox tests\n")
	script = strings.ReplaceAll(script, brutalKeeperSnippet(0), "\n: # optional keeper has its own contract tests\n")
	script = strings.ReplaceAll(script, nodeInstallFunctions, `set -euo pipefail
mole_run(){ "$@"; }
mole_install_hy2(){ touch "$MOLE_TEST_ROOT/installed-hy2"; }
mole_install_singbox(){ touch "$MOLE_TEST_ROOT/installed-reality"; }
`)
	return strings.NewReplacer("/run/", root+"/run/", "/etc/", root+"/etc/", "/usr/local/", root+"/usr/local/", "/var/log/", root+"/var/log/").Replace(script)
}
func runRepairSandbox(t *testing.T, root, script string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", sandboxRepairScript(root, script))
	cmd.Env = append(os.Environ(), "MOLE_TEST_ROOT="+root, "PATH="+root+"/bin:"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("repair sandbox: %v %s", err, out)
	}
	return string(out)
}

// Safety/audit. Why: dpkg uses POSIX locks, and --audit succeeds while those locks are held.
func TestPackageLocksWithoutFuserWaitForActualOwner(t *testing.T) {
	root := t.TempDir()
	lock := filepath.Join(root, "dpkg-lock")
	script := strings.NewReplacer("/var/lib/dpkg/lock-frontend", lock, "/var/lib/dpkg/lock", lock, "/var/lib/apt/lists/lock", lock).Replace(nodeInstallFunctions) + `
command(){ if [ "$*" = '-v fuser' ]; then return 1; fi; builtin command "$@"; }
dpkg(){ return 0; }
sleep(){ command sleep .02; }
mole_wait_dpkg
`
	harness := `import fcntl,pathlib,subprocess,sys,time
p=pathlib.Path(sys.argv[1]);p.write_text('')
f=p.open('r+');fcntl.lockf(f,fcntl.LOCK_EX)
child=subprocess.Popen(['bash','-c',sys.argv[2]],stdout=subprocess.PIPE,stderr=subprocess.PIPE)
try:
 time.sleep(.2)
 assert child.poll() is None, 'installation advanced while dpkg lock was held'
 f.close()
 out,err=child.communicate(timeout=3)
 assert child.returncode==0,(out,err)
finally:
 f.close()
 if child.poll() is None: child.kill()
 child.communicate()
`
	cmd := exec.Command("python3", "-c", harness, lock, script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("POSIX lock wait failed: %v %s", err, out)
	}
}

// Safety/what-x-what. Why: boot must finish writing its original identity before repair checks for a missing one.
func TestRepairWaitsForBootAndPreservesIdentity(t *testing.T) {
	for _, proto := range []string{protoHysteria2, protoReality} {
		t.Run(proto, func(t *testing.T) {
			srv := Server{Proto: proto, Port: 443, Password: "password"}
			plan, err := prepareNodeRepair(srv, nodeVerification{})
			if err != nil {
				t.Fatal(err)
			}
			root := repairSandbox(t)
			shim := `#!/usr/bin/env python3
import fcntl
fcntl.flock(9,fcntl.LOCK_EX)
`
			if err := os.WriteFile(filepath.Join(root, "bin/flock"), []byte(shim), 0700); err != nil {
				t.Fatal(err)
			}
			script := sandboxRepairScript(root, plan.script)
			harness := `import fcntl,os,pathlib,subprocess,sys,time
root=pathlib.Path(sys.argv[1]);f=(root/'run/mole-install.lock').open('w');fcntl.flock(f,fcntl.LOCK_EX)
env=dict(os.environ,MOLE_TEST_ROOT=str(root),PATH=str(root/'bin')+':'+os.environ['PATH'])
child=subprocess.Popen(['bash','-c',sys.argv[2]],env=env,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
try:
 time.sleep(.2)
 assert not (root/'installed-hy2').exists(), 'repair modified node during boot'
 assert not (root/'installed-reality').exists(), 'repair modified node during boot'
 for name in ('config.yaml','server.key','server.crt'): (root/'etc/hysteria'/name).write_text('original identity')
 (root/'etc/sing-box/config.json').write_text('original reality identity')
 f.close()
 out,err=child.communicate(timeout=3)
 assert child.returncode==0,(out,err)
 assert b'MOLE_HY2_IDENTITY_REPLACED' not in out,out
 assert b'MOLE_REALITY_IDENTITY_REPLACED' not in out,out
 assert (root/'etc/sing-box/config.json').read_text()=='original reality identity'
 assert (root/'etc/hysteria/server.key').read_text()=='original identity'
finally:
 f.close()
 if child.poll() is None: child.kill()
 child.communicate()
`
			cmd := exec.Command("python3", "-c", harness, root, script)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("boot/repair exclusion failed: %v %s", err, out)
			}
		})
	}
}

// Safety/what-x-what. Why: losing the repaired verification receipt must not become success.
func TestVerificationPersistenceErrorsPropagate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(utils.MoleDir(), 0700); err != nil {
		t.Fatal(err)
	}
	srv := Server{Name: "test-node", InstanceID: "test-instance"}
	if err := appendServer(srv); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"receipt", "store"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			log, err := os.Create(filepath.Join(dir, "deploy.log"))
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			session := &deploySession{dir: dir, log: log}
			if failure == "receipt" {
				if err := os.Mkdir(filepath.Join(dir, "receipt.json"), 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(utils.ServersPath(), []byte("invalid JSON"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := recordNodeVerification(srv, session, nodeVerification{Status: "ready"}, nil); err == nil {
				t.Fatal("unpersisted success was accepted")
			}
		})
	}
}
