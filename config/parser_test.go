package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeonTing1010/mole/core"
)

// TestBuildConfigValid tests that generated config is valid and doesn't reference missing rule-sets.
func TestBuildConfigValid(t *testing.T) {
	// Test with a sample hy2 URI
	uri := "hy2://test@example.com:443?sni=bing.com&insecure=1#test-server"

	cfg, err := Build(uri)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	// Verify no remote rule-sets are referenced (only local ones allowed)
	for _, rs := range cfg.Route.RuleSet {
		if rs.Type == "remote" {
			t.Errorf("Config references remote rule-set %s - should use local only", rs.Tag)
		}
	}

	// NOTE: DNS rules deliberately MAY use rule_set. An earlier assertion here
	// required domain_suffix instead, but that was the shape left behind when
	// geosite-cn was removed in 2e9a278 — a workaround frozen into a rule. Hand
	// suffix lists cannot classify domestic sites on .com domains, which is how
	// baidu/bilibili/xiaohongshu ended up routed abroad. What actually has to
	// hold is that every referenced rule-set is local, declared and present on
	// disk; TestRuleSetsAreLocalAndPresent enforces that.

	// Verify config can be serialized
	_, err = json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Config serialization failed: %v", err)
	}
}

// TestDNSRulesValid tests that DNS rules are properly configured.
func TestDNSRulesValid(t *testing.T) {
	uri := "hy2://test@example.com:443?sni=bing.com&insecure=1#test-server"

	cfg, err := Build(uri)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	// Must have at least one DNS rule for Chinese domains. .cn stays hardcoded
	// in parser.go; qq.com now arrives via builtin-rules.json (no leading dot).
	hasChineseDomainRule := false
	for _, rule := range cfg.DNS.Rules {
		for _, suffix := range rule.DomainSuffix {
			if suffix == ".cn" || suffix == "qq.com" {
				hasChineseDomainRule = true
				break
			}
		}
	}

	if !hasChineseDomainRule {
		t.Error("DNS rules missing Chinese domain suffixes (.cn, qq.com, etc.)")
	}

	// Must have both dns-direct and dns-remote servers
	hasDirect := false
	hasRemote := false
	for _, s := range cfg.DNS.Servers {
		if s.Tag == "dns-direct" {
			hasDirect = true
		}
		if s.Tag == "dns-remote" {
			hasRemote = true
		}
	}

	if !hasDirect {
		t.Error("Missing dns-direct server")
	}
	if !hasRemote {
		t.Error("Missing dns-remote server")
	}
}

// TestRouteRulesValid tests that route rules are properly configured.
func TestRouteRulesValid(t *testing.T) {
	uri := "hy2://test@example.com:443?sni=bing.com&insecure=1#test-server"

	cfg, err := Build(uri)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	// Must have a catch-all rule at the end
	hasCatchAll := false
	for _, rule := range cfg.Route.Rules {
		if len(rule.IPCIDR) > 0 && rule.IPCIDR[0] == "0.0.0.0/0" {
			hasCatchAll = true
			if rule.Outbound != "proxy" {
				t.Errorf("Catch-all rule should route to proxy, got %s", rule.Outbound)
			}
		}
	}

	if !hasCatchAll {
		t.Error("Missing catch-all route rule (0.0.0.0/0)")
	}

	// Must have geoip-cn rule for domestic IPs
	hasGeoIPCN := false
	for _, rule := range cfg.Route.Rules {
		for _, rs := range rule.RuleSet {
			if rs == "geoip-cn" {
				hasGeoIPCN = true
				if rule.Outbound != "direct" {
					t.Errorf("geoip-cn should route to direct, got %s", rule.Outbound)
				}
			}
		}
	}

	if !hasGeoIPCN {
		t.Error("Missing geoip-cn route rule")
	}
}

// TestDNSResolverBypassBeforeHijack pins the invariant whose absence made
// `mole status` exit-IP lookups fail: DNS queries to our direct resolvers
// (223.5.5.5 / 1.1.1.1) must be routed `direct` by a rule that comes BEFORE
// the catch-all `hijack-dns` rule. Without it, the CLI's own direct DNS
// queries get hijacked by the TUN DNS and answered with FakeIP (198.18.x.x),
// so the exit-IP probe connects to a dead 198.18 address.
func TestDNSResolverBypassBeforeHijack(t *testing.T) {
	uri := "hy2://test@example.com:443?sni=bing.com&insecure=1#test-server"

	cfg, err := Build(uri)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	bypassIdx, hijackIdx := -1, -1
	for i, rule := range cfg.Route.Rules {
		if rule.Action == "hijack-dns" {
			hijackIdx = i
		}
		if rule.Protocol == "dns" && rule.Outbound == "direct" {
			hasAli, hasCF := false, false
			for _, c := range rule.IPCIDR {
				if c == "223.5.5.5/32" {
					hasAli = true
				}
				if c == "1.1.1.1/32" {
					hasCF = true
				}
			}
			if hasAli && hasCF {
				bypassIdx = i
			}
		}
	}

	if bypassIdx == -1 {
		t.Fatal("missing dns-resolver bypass rule (protocol:dns, ip_cidr 223.5.5.5/1.1.1.1 → direct)")
	}
	if hijackIdx == -1 {
		t.Fatal("missing hijack-dns route rule")
	}
	if bypassIdx > hijackIdx {
		t.Errorf("dns-resolver bypass rule (idx %d) must come before hijack-dns (idx %d)", bypassIdx, hijackIdx)
	}
}

const testURI = "hy2://test@example.com:443?sni=bing.com&insecure=1#test-server"

// TestRuleSetsAreLocalAndPresent guards the hazard that once got geosite-cn
// deleted outright (2e9a278, "avoid download failures"): a config that names a
// rule-set sing-box cannot load. sing-box treats that as fatal, so it does not
// degrade — the tunnel simply refuses to start, during startup, which is exactly
// when the user has no working network to debug it with.
//
// The rule is therefore NOT "don't use geosite" — that threw away a needed
// capability to dodge a failure mode. It is: every rule-set referenced must be
// declared, every declared rule-set must be a local file that exists, and none
// may be fetched by sing-box itself (that would put a download on the cold-start
// critical path, the original chicken-and-egg deadlock EnsureRuleSets exists to
// prevent).
func TestRuleSetsAreLocalAndPresent(t *testing.T) {
	cfg, err := Build(testURI)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	declared := map[string]bool{}
	for _, rs := range cfg.Route.RuleSet {
		declared[rs.Tag] = true
		if rs.Type != "local" {
			t.Errorf("rule-set %s has type %q — must be local so startup never waits on a download", rs.Tag, rs.Type)
		}
		if rs.URL != "" || rs.DownloadDetour != "" {
			t.Errorf("rule-set %s would be fetched by sing-box (url=%q detour=%q); prefetch it in utils.EnsureRuleSets instead", rs.Tag, rs.URL, rs.DownloadDetour)
		}
		if rs.Path == "" {
			t.Errorf("rule-set %s declares no path", rs.Tag)
			continue
		}
		if st, err := os.Stat(rs.Path); err != nil || st.Size() == 0 {
			t.Errorf("rule-set %s points at %s, which sing-box could not load (%v) — the config must omit a rule-set whose file is missing, not reference it", rs.Tag, rs.Path, err)
		}
	}

	for _, rule := range cfg.Route.Rules {
		for _, rs := range rule.RuleSet {
			if !declared[rs] {
				t.Errorf("route rule references undeclared rule-set %q", rs)
			}
		}
	}
	for _, rule := range cfg.DNS.Rules {
		for _, rs := range rule.RuleSet {
			if !declared[rs] {
				t.Errorf("DNS rule references undeclared rule-set %q", rs)
			}
		}
	}
}

// TestGeositeCNDegradesWhenAbsent pins the fallback that makes depending on
// geosite-cn safe: with the file missing, the config must come out with no trace
// of it — not a dangling reference, and not a DNS rule pointing at a rule-set
// that was never declared. A prefetch failure should cost domestic-domain
// classification accuracy, nothing more.
func TestGeositeCNDegradesWhenAbsent(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // fresh ~/.mole: no rule-sets prefetched

	cfg, err := Build(testURI)
	if err != nil {
		t.Fatalf("Build must succeed without geosite-cn on disk, got: %v", err)
	}

	for _, rs := range cfg.Route.RuleSet {
		if contains(rs.Tag, "geosite") {
			t.Errorf("declared geosite rule-set %q despite the file being absent", rs.Tag)
		}
	}
	for _, rule := range cfg.DNS.Rules {
		for _, rs := range rule.RuleSet {
			if contains(rs, "geosite") {
				t.Errorf("DNS rule references geosite rule-set %q despite the file being absent", rs)
			}
		}
	}
	// The fakeip catch-all is what everything falls through to; losing it would
	// turn a degraded classifier into no resolution at all.
	var hasFakeIP bool
	for _, rule := range cfg.DNS.Rules {
		if rule.Server == "dns-fake" {
			hasFakeIP = true
		}
	}
	if !hasFakeIP {
		t.Error("degraded config lost its fakeip catch-all")
	}
}

// TestGeositeCNUsedWhenPresent is the other half: when the rule-set IS on disk,
// domestic-domain classification must actually be wired to it, and ahead of the
// fakeip catch-all — behind it the catch-all would win and the rule would be
// dead weight.
func TestGeositeCNUsedWhenPresent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".mole")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"geoip-cn.srs", "geosite-cn.srs"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("stub"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	cfg, err := Build(testURI)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	geositeAt, fakeipAt := -1, -1
	for i, rule := range cfg.DNS.Rules {
		for _, rs := range rule.RuleSet {
			if rs == "geosite-cn" && geositeAt < 0 {
				geositeAt = i
				if rule.Server != "dns-direct" {
					t.Errorf("geosite-cn resolves via %q, want dns-direct — a fake IP would strand it behind the 198.18.0.0/15→proxy rule", rule.Server)
				}
			}
		}
		if rule.Server == "dns-fake" && fakeipAt < 0 {
			fakeipAt = i
		}
	}
	if geositeAt < 0 {
		t.Fatal("geosite-cn is on disk but no DNS rule uses it")
	}
	if fakeipAt >= 0 && geositeAt > fakeipAt {
		t.Errorf("geosite-cn rule is at %d, after the fakeip catch-all at %d — it would never match", geositeAt, fakeipAt)
	}
}

// TestHysteria2CongestionControl pins the three bandwidth paths: absent params
// keep the Brutal default, positive params override it, and a negative sentinel
// disables Brutal so the fields are omitted and hysteria2 falls back to BBR.
func TestHysteria2CongestionControl(t *testing.T) {
	// Resolves the outbound that actually carries the effective Brutal ceiling.
	// With a ceiling configured, `proxy` is a selector over the bandwidth ladder
	// and the live ceiling lives on its default member; in BBR mode there is no
	// ladder and `proxy` is the hy2 outbound itself. Following the selector keeps
	// these assertions about the ceiling the URI asked for, not about the shape
	// the config happens to use to express it.
	proxyOut := func(t *testing.T, uri string) *OutboundConfig {
		t.Helper()
		cfg, err := Build(uri)
		if err != nil {
			t.Fatalf("Build failed: %v", err)
		}
		byTag := func(tag string) *OutboundConfig {
			for i := range cfg.Outbounds {
				if cfg.Outbounds[i].Tag == tag {
					return &cfg.Outbounds[i]
				}
			}
			return nil
		}
		o := byTag("proxy")
		if o == nil {
			t.Fatal("no proxy outbound in config")
		}
		if o.Type != "selector" {
			return o
		}
		member := byTag(o.Default)
		if member == nil {
			t.Fatalf("proxy selector default %q is not among the outbounds", o.Default)
		}
		return member
	}

	t.Run("default is Brutal 20/50", func(t *testing.T) {
		o := proxyOut(t, "hy2://p@example.com:443?sni=bing.com&insecure=1#s")
		if o.UpMbps != 20 || o.DownMbps != 50 {
			t.Errorf("got up=%d down=%d, want 20/50", o.UpMbps, o.DownMbps)
		}
	})

	t.Run("positive params pin Brutal ceiling", func(t *testing.T) {
		o := proxyOut(t, "hy2://p@example.com:443?sni=bing.com&insecure=1&upmbps=4&downmbps=8#s")
		if o.UpMbps != 4 || o.DownMbps != 8 {
			t.Errorf("got up=%d down=%d, want 4/8", o.UpMbps, o.DownMbps)
		}
	})

	t.Run("negative sentinel disables Brutal (BBR)", func(t *testing.T) {
		o := proxyOut(t, "hy2://p@example.com:443?sni=bing.com&insecure=1&upmbps=-1&downmbps=-1#s")
		if o.UpMbps != 0 || o.DownMbps != 0 {
			t.Errorf("got up=%d down=%d, want 0/0 (omitted → BBR)", o.UpMbps, o.DownMbps)
		}
		// 0 with omitempty must drop the fields from the serialized outbound,
		// otherwise sing-box still sees Brutal=0 rather than no Brutal at all.
		b, err := json.Marshal(o)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if s := string(b); containsHelper(s, "up_mbps") || containsHelper(s, "down_mbps") {
			t.Errorf("BBR outbound must omit bandwidth fields, got: %s", s)
		}
	})
}

// TestBandwidthLadder pins the shape `mole ceiling` depends on: the route target
// `proxy` is a selector, every rung it lists exists as a real outbound carrying
// that ceiling, and its default is the ceiling the clock wants right now. A
// selector naming a member that isn't in the config would make sing-box reject
// it — or make every scheduler tick fail — so this is the integration guard for
// the invariant core.TestMemberIsAlwaysARung checks in the abstract.
func TestBandwidthLadder(t *testing.T) {
	cfg, err := Build("hy2://p@example.com:443?sni=bing.com&insecure=1&upmbps=5&downmbps=20&peakdownmbps=5&peakupmbps=2&peakstart=18&peakend=0#s")
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	byTag := map[string]*OutboundConfig{}
	for i := range cfg.Outbounds {
		byTag[cfg.Outbounds[i].Tag] = &cfg.Outbounds[i]
	}

	sel := byTag["proxy"]
	if sel == nil {
		t.Fatal("no proxy outbound")
	}
	if sel.Type != "selector" {
		t.Fatalf("proxy is %q, want a selector over the ladder", sel.Type)
	}
	if len(sel.Outbounds) < 2 {
		t.Fatalf("ladder has %d members, want several", len(sel.Outbounds))
	}

	for _, member := range sel.Outbounds {
		o := byTag[member]
		if o == nil {
			t.Errorf("selector lists %q but no such outbound exists", member)
			continue
		}
		if o.Type != "hysteria2" {
			t.Errorf("rung %q is %q, want hysteria2", member, o.Type)
		}
		if want := fmt.Sprintf("proxy-bw-%d", o.DownMbps); member != want {
			t.Errorf("rung %q carries down=%d, so it should be tagged %q", member, o.DownMbps, want)
		}
		if o.UpMbps <= 0 {
			t.Errorf("rung %q has up_mbps=%d — zero drops it out of Brutal into BBR", member, o.UpMbps)
		}
	}

	if byTag[sel.Default] == nil {
		t.Errorf("selector default %q is not among the outbounds", sel.Default)
	}
	sched := core.BandwidthSchedule{
		OffUp: 5, OffDown: 20, PeakUp: 2, PeakDown: 5, StartHour: 18, EndHour: 0,
	}
	if want := sched.Member(time.Now()); sel.Default != want {
		t.Errorf("selector default = %q, want %q (the ceiling the clock wants at build time)", sel.Default, want)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
