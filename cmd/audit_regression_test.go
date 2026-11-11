package cmd

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/LeonTing1010/mole/utils"
	"golang.org/x/crypto/curve25519"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Quality/what. Why: rate refreshes cannot silently undo the operator's tuned gain.
func TestKeeperRateWithoutGainPreservesGain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keeper.py")
	if err := os.WriteFile(path, []byte(brutalKeeperPy), 0600); err != nil {
		t.Fatal(err)
	}
	script := `import runpy,sys
m=runpy.run_path(sys.argv[1]);g=m['H'].do_GET.__globals__
assert g['read_rate']()==77, g['read_rate']()
assert g['read_gain']()==15, g['read_gain']()
g['write_rate']=lambda n:None
g['write_gain'](20)
g['apply']=lambda *a:(1,1,'')
h=object.__new__(m['H']);h.path='/rate?mbps=100';h.reply=lambda *a:None
h.do_GET()
assert g['read_gain']()==20, g['read_gain']()
`
	p := exec.Command("python3", "-c", script, path)
	p.Env = append(os.Environ(), "MOLE_BRUTAL_RATE_FILE="+filepath.Join(dir, "rate"), "MOLE_BRUTAL_GAIN_FILE="+filepath.Join(dir, "gain"), "MOLE_DEFAULT_MBPS=77", "MOLE_DEFAULT_GAIN=15")
	if out, err := p.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
}

// Safety/what-x-what. Why: tuning B must not modify or measure a running tunnel to A.
func TestGainTuneRejectsNodeOtherThanRunningDaemon(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := writePrivateJSON(utils.ServersPath(), []Server{{Name: "B", IP: "192.0.2.2", Port: 443}}); err != nil {
		t.Fatal(err)
	}
	if err := utils.WriteState(&utils.State{Server: "A", PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	old := http.DefaultTransport
	http.DefaultTransport = auditRejectTransport{}
	defer func() { http.DefaultTransport = old }()
	err := runGainTune(nil, []string{"B"})
	if err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("expected running-node mismatch, got %v", err)
	}
}

// Quality/what. Why: a rejected gain change cannot qualify as a measured candidate.
func TestGainScanRejectsUnappliedGain(t *testing.T) {
	bulk := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/gain") {
			w.WriteHeader(503)
			fmt.Fprint(w, "applied=0\n")
			return
		}
		bulk++
		w.WriteHeader(500)
	}))
	defer s.Close()
	_, _, _, err := scanAndPickGain(s.URL)
	if err == nil || bulk != 0 {
		t.Fatalf("unapplied gains measured: bulk=%d err=%v", bulk, err)
	}
}

type auditRejectTransport struct{}

func (auditRejectTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("unexpected network request")
}

// Safety/what-x-what. Why: standalone links must authenticate the generated server
// and must be consumable by the real CLI importer, without revealing its private key.
func TestStandaloneLinksMatchIdentityAndImport(t *testing.T) {
	root := t.TempDir()
	p := exec.Command("python3", "../deploy/configure.py", "--root", root, "--ip", "192.0.2.4")
	for _, env := range os.Environ() {
		key, _, _ := strings.Cut(env, "=")
		if key != "REALITY_PRIV" && key != "REALITY_SHORT" && key != "USER_UUID" && key != "HY2_PASSWORD" {
			p.Env = append(p.Env, env)
		}
	}
	if out, err := p.CombinedOutput(); err != nil {
		t.Fatalf("configure: %v: %s", err, out)
	}
	var identity struct {
		Private  string `json:"reality_private_key"`
		UUID     string `json:"uuid"`
		Short    string `json:"short_id"`
		Password string `json:"hy2_password"`
	}
	data, err := os.ReadFile(filepath.Join(root, "etc/mole/identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &identity); err != nil {
		t.Fatal(err)
	}
	var links struct {
		Reality string `json:"reality_uri"`
		HY2     string `json:"hy2_uri"`
	}
	data, err = os.ReadFile(filepath.Join(root, "etc/mole/config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &links); err != nil {
		t.Fatal(err)
	}
	reality, err := serverFromURI(links.Reality)
	if err != nil {
		t.Fatal(err)
	}
	private, err := base64.RawURLEncoding.DecodeString(identity.Private)
	if err != nil {
		t.Fatal(err)
	}
	public, err := curve25519.X25519(private, curve25519.Basepoint)
	if err != nil {
		t.Fatal(err)
	}
	if reality.RealityPubKey != base64.RawURLEncoding.EncodeToString(public) || reality.UUID != identity.UUID || reality.RealityShortID != identity.Short || !reality.RealityMultiplex {
		t.Fatal("REALITY URI disagrees with serving identity")
	}
	hy2, err := serverFromURI(links.HY2)
	if err != nil {
		t.Fatal(err)
	}
	if hy2.Password != identity.Password || hy2.ObfsPassword != identity.Password {
		t.Fatal("hy2 URI disagrees with serving identity")
	}
}
