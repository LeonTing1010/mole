package cmd

import (
	"encoding/json"
	"os/exec"
	"testing"
)

// Safety/what-x-what: losing a kernel route invalidates a previous rate receipt.
func TestKeeperRepairsLostKernelState(t *testing.T) {
	source, _ := json.Marshal(brutalKeeperPy)
	script := `import json,sys,types
ns={'__name__':'keeper_test'}
exec(json.loads(sys.argv[1]),ns)
ns['read_rate']=lambda:600
ns['module_ok']=lambda:(True,'')
ns['peers']=lambda:{'192.0.2.1'}
kernel={'present':False,'fail':False}
def run(args,**kwargs):
 if args[:2]==['brutalctl','add']:
  if kernel['fail']:return types.SimpleNamespace(returncode=1,stderr='route unavailable',stdout='')
  kernel['present']=True
  return types.SimpleNamespace(returncode=0,stderr='',stdout='')
 raise AssertionError(args)
ns['subprocess'].run=run
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
