package cmd

import (
	"encoding/json"
	"os/exec"
	"testing"
)

// Safety/what-x-what: losing a kernel route invalidates a previous rate receipt.
// 2026-11-10: apply() now writes /proc/net/tcp_brutal/rules directly instead
// of shelling out to brutcalctl, so the harness simulates the /proc file.
func TestKeeperRepairsLostKernelState(t *testing.T) {
	source, _ := json.Marshal(brutalKeeperPy)
	script := `import json,sys,types
ns={'__name__':'keeper_test'}
exec(json.loads(sys.argv[1]),ns)
ns['read_rate']=lambda:600
ns['read_gain']=lambda:15
ns['module_ok']=lambda:(True,'')
ns['peers']=lambda:{'192.0.2.1'}
kernel={'present':False,'fail':False,'writes':[]}
class FakeProc:
    def __init__(self,k):self.k=k
    def __enter__(self):
        if self.k['fail']:raise OSError('route unavailable')
        self.k['present']=True
        return self
    def __exit__(self,*a):pass
    def write(self,data):self.k['writes'].append(data)
orig_open=open
def fake_open(path,mode='r'):
    if path==ns['PROC_RULES']:return FakeProc(kernel)
    return orig_open(path,mode)
ns['open']=fake_open
orig_exists=ns['os'].path.exists
ns['os'].path.exists=lambda p: True if p==ns['PROC_RULES'] else orig_exists(p)
assert ns['apply']()[0]==1 and kernel['present']
kernel['present']=False
assert ns['apply']()[0]==1 and kernel['present'], 'cached receipt hid lost kernel state'
kernel.update(present=False,fail=True)
assert ns['apply']()[0]==0, 'failed repair certified as applied'
`
	if out, err := exec.Command("python3", "-c", script, string(source)).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
