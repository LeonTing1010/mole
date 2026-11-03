package config

import (
	"testing"

	"github.com/LeonTing1010/mole/core"
)

// Quality/what-x-what: dual routing must preserve single-mode routing by default.
func TestDualNodeGetsFlowSelectorsWithSafeDefaults(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// Why: installing support must not silently activate an untested UDP leg.
	cfg, err := BuildWithStandby(testHy2URI, testRealityURI)
	if err != nil {
		t.Fatal(err)
	}
	lanes := 0
	for _, o := range cfg.Outbounds {
		if o.Tag == "flow-0" || o.Tag == "flow-1" || o.Tag == "flow-2" || o.Tag == "flow-3" || o.Tag == "flow-4" || o.Tag == "flow-5" || o.Tag == "flow-6" || o.Tag == "flow-7" {
			lanes++
			if o.Type != "selector" || o.Default != core.ProxySelectorTag {
				t.Fatal("single-mode behavior changed")
			}
		}
	}
	if lanes != 8 {
		t.Fatalf("got %d flow selectors, want 8", lanes)
	}
	for _, r := range cfg.Route.Rules {
		if r.Outbound == "flow-0" {
			return
		}
	}
	t.Fatal("flow selectors are not wired into foreign routes")
}

// Quality/what-x-what: domain-only and IPv6 traffic cannot bypass dual routing.
func TestDualRoutingIncludesTheFinalDomainOnlyFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// Why: live TUN sniffing removes the fake destination IP before matching CIDRs.
	cfg, err := BuildWithStandby(testHy2URI, testRealityURI)
	if err != nil {
		t.Fatal(err)
	}
	for lane := 0; lane < core.BalanceLanes; lane++ {
		found := false
		for _, r := range cfg.Route.Rules {
			if r.Outbound == core.BalanceLaneTag(lane) && len(r.IPCIDR) == 0 && len(r.RuleSet) == 0 && len(r.DomainSuffix) == 0 && len(r.DomainKeyword) == 0 && len(r.Inbound) == 0 && len(r.SourcePortRange) > 0 {
				found = true
			}
		}
		if !found {
			t.Fatalf("flow %d has no final fallback for domain-only traffic", lane)
		}
	}
}
