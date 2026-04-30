package config

import (
	"encoding/json"
	"os"
	"testing"
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

	// Verify DNS rules don't use rule_set (we use domain_suffix instead)
	for _, rule := range cfg.DNS.Rules {
		if len(rule.RuleSet) > 0 {
			t.Errorf("DNS rule references rule_set %v - should use domain_suffix", rule.RuleSet)
		}
	}

	// Verify config can be serialized
	_, err = json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Config serialization failed: %v", err)
	}
}

// TestConfigReferencesExistingRuleSets tests that all referenced rule-sets exist on disk.
func TestConfigReferencesExistingRuleSets(t *testing.T) {
	uri := "hy2://test@example.com:443?sni=bing.com&insecure=1#test-server"

	cfg, err := Build(uri)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	for _, rs := range cfg.Route.RuleSet {
		if rs.Type == "local" && rs.Path != "" {
			if _, err := os.Stat(rs.Path); os.IsNotExist(err) {
				t.Errorf("Rule-set %s references non-existent file: %s", rs.Tag, rs.Path)
			}
		}
	}
}

// TestDNSRulesValid tests that DNS rules are properly configured.
func TestDNSRulesValid(t *testing.T) {
	uri := "hy2://test@example.com:443?sni=bing.com&insecure=1#test-server"

	cfg, err := Build(uri)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	// Must have at least one DNS rule for Chinese domains
	hasChineseDomainRule := false
	for _, rule := range cfg.DNS.Rules {
		for _, suffix := range rule.DomainSuffix {
			if suffix == ".cn" || suffix == ".qq.com" {
				hasChineseDomainRule = true
				break
			}
		}
	}

	if !hasChineseDomainRule {
		t.Error("DNS rules missing Chinese domain suffixes (.cn, .qq.com, etc.)")
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

// TestConfigDoesNotUseGeosite tests that config doesn't use geosite rule-sets.
func TestConfigDoesNotUseGeosite(t *testing.T) {
	uri := "hy2://test@example.com:443?sni=bing.com&insecure=1#test-server"

	cfg, err := Build(uri)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	// Check route rules
	for _, rule := range cfg.Route.Rules {
		for _, rs := range rule.RuleSet {
			if contains(rs, "geosite") {
				t.Errorf("Route rule references geosite rule-set: %s", rs)
			}
		}
	}

	// Check rule-sets
	for _, rs := range cfg.Route.RuleSet {
		if contains(rs.Tag, "geosite") {
			t.Errorf("Config includes geosite rule-set: %s", rs.Tag)
		}
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
