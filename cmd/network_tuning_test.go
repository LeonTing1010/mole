package cmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Quality/audit: configuration intent must not hide an unchanged physical qdisc.
func TestNetworkTuningReportsEffectiveQueue(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	os.Mkdir(bin, 0700)
	for name, body := range map[string]string{"ip": `echo '[{"dev":"eth0"}]'`, "tc": `echo '[{"kind":"fq_codel","root":true}]'`, "sysctl": `case "$2" in net.core.default_qdisc) echo fq;; net.ipv4.tcp_congestion_control) echo bbr;; *) echo 3;; esac`, "systemctl": `echo active`, "sing-box": `echo sing-box-1.14.4`, "hysteria": `echo hysteria-2.13.0`, "ss": `echo brutal`} {
		os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0700)
	}
	script := strings.ReplaceAll(nodeNetworkStatePython, "/var/log/mole/network-apply.status", filepath.Join(root, "apply.status"))
	os.WriteFile(filepath.Join(root, "apply.status"), []byte("applied"), 0600)
	cmd := exec.Command("python3", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var report nodeTuningReport
	if err = json.Unmarshal(out, &report); err != nil {
		t.Fatal(string(out), err)
	}
	if report.Status != "degraded" || report.Qdisc != "fq_codel" || report.DefaultQdisc != "fq" {
		t.Fatal(string(out))
	}
}

// Quality/what-x-what: the same tuning stage is present for every provisioning path.
func TestAllBootProfilesIncludeNetworkObservation(t *testing.T) {
	id := &serverIdentity{}
	rid := &realityIdentity{}
	for name, script := range map[string]string{"hy2": hy2BootScript("test", 443, id), "reality": realityBootScript(443, 400, rid), "dual": dualBootScript("test", 443, 400, id, rid), "repair": repairScript("", 400)} {
		if strings.Count(script, "MOLE_NETWORK_STATE_EOF") != 2 {
			t.Fatalf("%s lacks one shared observation stage", name)
		}
	}
}

// Quality/what: the server listener must opt in, kernel capability alone is insufficient.
func TestRealityListenerEnablesTFO(t *testing.T) {
	script := realityServerSnippet(443, 400, &realityIdentity{})
	start := strings.Index(script, "{\n  \"log\"")
	end := strings.Index(script[start:], "\nEOF") + start
	var cfg struct {
		Inbounds []struct {
			TFO bool `json:"tcp_fast_open"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal([]byte(script[start:end]), &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Inbounds) != 1 || !cfg.Inbounds[0].TFO {
		t.Fatal("server TFO disabled")
	}
}

// Quality/what: legacy or partial responses cannot certify tuning readiness.
func TestTuningReadbackRequiresEffectiveState(t *testing.T) {
	for _, data := range []string{`404 page not found`, `{"status":"ready"}`, `{}`} {
		if r := decodeNodeTuning([]byte(data)); r.Status != "unobserved" {
			t.Fatalf("accepted incomplete state: %s", data)
		}
	}
}
