package cmd

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The keeper ships as a Python program inside a Go string, which means Go's
// compiler cannot see a single thing about it. A syntax error here is a node
// that deploys clean and silently never paces anything — the exact shape of
// the bug this whole channel was rebuilt for.
func TestKeeperIsValidPython(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	f := filepath.Join(t.TempDir(), "keeper.py")
	if err := os.WriteFile(f, []byte(brutalKeeperPy), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(py, "-m", "py_compile", f).CombinedOutput(); err != nil {
		t.Fatalf("keeper does not compile: %v\n%s", err, out)
	}
}

// The property the client now depends on: /rate can answer "no". Before
// 2026-09-09 it replied 200 whether or not brutalctl had installed anything,
// so the client's confirmation could not come back false and REALITY ran
// unpaced for days under a log line saying otherwise.
//
// Off the node there is no brutal module, which is precisely the failure being
// asserted — the keeper must say so rather than claim success.
func TestKeeperRefusesWhenItCannotPace(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "keeper.py")
	if err := os.WriteFile(f, []byte(brutalKeeperPy), 0o644); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)

	cmd := exec.Command(py, f)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("MOLE_BRUTAL_PORT=%d", port),
		"MOLE_BRUTAL_RATE_FILE="+filepath.Join(dir, "rate"),
		"MOLE_BRUTAL_GAIN_FILE="+filepath.Join(dir, "gain"),
		// brutcalctl stub is never reached when the module is absent — the
		// 503 must come from module_ok() failing, not a stubbed subprocess.
		"PATH="+stubBin(t, dir)+string(os.PathListSeparator)+os.Getenv("PATH"))
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	cmd.Stdout = &errBuf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() && errBuf.Len() > 0 {
			t.Logf("keeper output:\n%s", errBuf.String())
		}
	})
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitUp(t, base+"/health")

	reachable, err := http.Get(base + "/reachable")
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := io.ReadAll(reachable.Body)
	reachable.Body.Close()
	if reachable.StatusCode != 200 || !strings.Contains(string(rb), "reachable=1") || !strings.Contains(string(rb), "applied=0") {
		t.Fatalf("transport readiness conflates pacing: %s", rb)
	}
	resp, err := http.Get(base + "/rate?mbps=65")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %s, want 503 when nothing was paced\n%s", resp.Status, body)
	}
	if !strings.Contains(string(body), "applied=0") {
		t.Errorf("reply must report applied=0:\n%s", body)
	}
	if !strings.Contains(string(body), "detail=") {
		t.Errorf("a refusal has to name a reason:\n%s", body)
	}
	// The rate is still recorded, so a later apply picks it up.
	if !strings.Contains(string(body), "rate=65") {
		t.Errorf("the rate was not stored:\n%s", body)
	}
}

func stubBin(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "brutalctl"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitUp(t *testing.T, url string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if resp, err := http.Get(url); err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("keeper never came up")
}

// Exact host prefixes must follow the peer family; invalid ss rows cannot
// broaden a policy. Exercise the deployed Python rather than a Go duplicate.
func TestKeeperPacesNativeIPv6AndMappedIPv4(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(t.TempDir(), "keeper.py")
	if err := os.WriteFile(f, []byte(brutalKeeperPy), 0600); err != nil {
		t.Fatal(err)
	}
	script := `
import runpy,sys,types
m=runpy.run_path(sys.argv[1]);g=m['apply'].__globals__;calls=[]
rows='''0 0 192.0.2.1:443 198.51.100.4:34567
0 0 [2001:db8::1]:443 [2001:db8::20]:34568
0 0 [::ffff:192.0.2.1]:443 [::ffff:198.51.100.4]:34569
0 0 2001:db8::1:443 2001:db8::21:34570
0 0 192.0.2.1:443 999.1.2.3:99
0 0 [::]:443 [::]:12
0 0 [::]:443 [2001:db8::22]:bad
'''
def run(args,**kwargs):
 if args[0]=='ss':return types.SimpleNamespace(stdout=rows,returncode=0)
 raise AssertionError(args)
calls=[]
class FakeProc:
    def __enter__(self):return self
    def __exit__(self,*a):pass
    def write(self,data):calls.append(data)
orig_open=open
def fake_open(path,mode='r'):
    if path==g['PROC_RULES'] and mode=='w':return FakeProc()
    return orig_open(path,mode)
g['open']=fake_open;g['os'].path.exists=lambda p: True if p==g['PROC_RULES'] else orig_exists(p)
orig_exists=g['os'].path.exists
g['subprocess'].run=run;g['module_ok']=lambda:(True,'');g['read_rate']=lambda:500;g['read_gain']=lambda:15
n,peers,detail=g['apply'](True)
assert n==peers==3,(n,peers,detail,calls)
# Expect 3 /proc writes with rate=bytes+gain=15
import re
written=set()
for w in calls:
    m=re.match(r'add (\S+) rate=(\d+) gain=(\d+)',w)
    if m:written.add(m.group(1))
assert written=={'198.51.100.4/32','2001:db8::20/128','2001:db8::21/128'},calls
`
	if out, err := exec.Command(py, "-c", script, f).CombinedOutput(); err != nil {
		t.Fatalf("peer family policy: %v\n%s", err, out)
	}
}
