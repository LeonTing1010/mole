package config

import (
	"strings"
	"testing"

	"github.com/LeonTing1010/mole/core"
)

// Quality/what-x-what: both protocols use one traffic decision, without shards.
func TestPrimaryFallbackConfigUsesOneTrafficSelector(t *testing.T) {
	// Why: near-equal throughput does not justify per-source-port route machinery.
	t.Setenv("HOME", t.TempDir())
	cfg, err := BuildWithStandby(testHy2URI, testRealityURI)
	if err != nil {
		t.Fatal(err)
	}
	primary := false
	for _, outbound := range cfg.Outbounds {
		if strings.HasPrefix(outbound.Tag, "flow-") {
			t.Fatal("retired allocation selectors are still generated")
		}
		if outbound.Tag == core.ProxySelectorTag {
			primary = outbound.Default == core.RealityLegTag
		}
	}
	if !primary || cfg.Route.Final != core.ProxySelectorTag {
		t.Fatal("traffic does not default to the REALITY primary selector")
	}
	for _, rule := range cfg.Route.Rules {
		if strings.HasPrefix(rule.Outbound, "flow-") || len(rule.SourcePortRange) > 0 {
			t.Fatal("generated routing still shards source ports")
		}
	}
}
