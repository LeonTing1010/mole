package config

import (
	"testing"

	"github.com/LeonTing1010/mole/core"
	"github.com/LeonTing1010/mole/utils"
)

const abHy2URI = "hy2://pw@1.2.3.4:443?upmbps=5&downmbps=65&peakdownmbps=50&peakupmbps=5&peakstart=12&peakend=2"

func declareTestExperiment(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	e := &utils.Experiment{
		Name: "udp-port-20443",
		Arms: []utils.ExperimentArm{
			{Name: "p443"},
			{Name: "p20443", Port: 20443},
		},
	}
	if err := utils.WriteExperiment(e); err != nil {
		t.Fatal(err)
	}
}

// The safety property, and the only one that can do real damage: an arm must
// never be reachable by user traffic. It is materialized so the Clash API can
// dial it; putting it in the selector would let the bandwidth scheduler, a
// stall kick or `mole ceiling` route live requests through an experimental port
// — and then the experiment would be changing what it is measuring.
func TestExperimentArmsAreNeverSelectorMembers(t *testing.T) {
	declareTestExperiment(t)
	o, err := parseServerURL(abHy2URI)
	if err != nil {
		t.Fatal(err)
	}
	outs := buildProxyOutbounds(abHy2URI, "", o)
	if outs[0].Type != "selector" {
		t.Fatalf("first outbound should be the selector, got %+v", outs[0])
	}
	for _, m := range outs[0].Outbounds {
		if m == core.ExperimentArmTag("p443") || m == core.ExperimentArmTag("p20443") {
			t.Fatalf("A/B arm %q is a selector member — live traffic could be routed through it", m)
		}
	}
}

// ...but they must still be in the config, or there is nothing to probe.
func TestExperimentArmsAreMaterialized(t *testing.T) {
	declareTestExperiment(t)
	o, err := parseServerURL(abHy2URI)
	if err != nil {
		t.Fatal(err)
	}
	outs := buildProxyOutbounds(abHy2URI, "", o)
	byTag := map[string]OutboundConfig{}
	for _, x := range outs {
		byTag[x.Tag] = x
	}
	control, ok := byTag[core.ExperimentArmTag("p443")]
	if !ok {
		t.Fatal("control arm missing from the config — nothing to probe")
	}
	test, ok := byTag[core.ExperimentArmTag("p20443")]
	if !ok {
		t.Fatal("test arm missing from the config")
	}
	// The entire validity of the comparison rests on this: the two arms differ
	// in the port and in nothing else. A drift in the ceiling, the password or
	// obfs between arms would be measured as if it were the variable.
	if test.ServerPort != 20443 {
		t.Errorf("test arm did not take the port override: %d", test.ServerPort)
	}
	if control.ServerPort != 443 {
		t.Errorf("control arm should keep the live port, got %d", control.ServerPort)
	}
	if control.Server != test.Server {
		t.Errorf("arms differ in host (%q vs %q) — that is a second variable", control.Server, test.Server)
	}
	if control.DownMbps != test.DownMbps || control.UpMbps != test.UpMbps {
		t.Errorf("arms differ in ceiling (%d/%d vs %d/%d) — that is a second variable",
			control.UpMbps, control.DownMbps, test.UpMbps, test.DownMbps)
	}
	if control.Password != test.Password {
		t.Error("arms differ in password — one of them is measuring a rejected handshake")
	}
}

// No experiment declared must mean no extra outbounds at all, so the normal
// config is byte-identical to what it was before this feature existed.
func TestNoExperimentMeansNoArms(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	o, err := parseServerURL(abHy2URI)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range buildProxyOutbounds(abHy2URI, "", o) {
		if len(x.Tag) > 3 && x.Tag[:3] == "ab-" {
			t.Fatalf("materialized arm %q with no experiment declared", x.Tag)
		}
	}
}
