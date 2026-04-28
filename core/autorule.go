package core

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/LeonTing1010/mole/config"
	"github.com/LeonTing1010/mole/utils"
)

// AutoRuleManager automatically maintains direct routing rules.
type AutoRuleManager struct {
	mu        sync.Mutex
	rulesPath string
}

// NewAutoRuleManager creates a new rule manager.
func NewAutoRuleManager() *AutoRuleManager {
	return &AutoRuleManager{
		rulesPath: utils.CustomRulesPath(),
	}
}

// AddDomainRule adds a domain suffix rule for direct connection.
func (a *AutoRuleManager) AddDomainRule(domain string) error {
	// Normalize domain
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	rules := a.loadRules()

	// Check if already exists
	for _, r := range rules {
		for _, d := range r.DomainSuffix {
			if d == domain {
				return nil // Already exists
			}
		}
	}

	// Add new rule
	rules = append(rules, config.RouteRule{
		DomainSuffix: []string{domain},
		Outbound:     "direct",
	})

	fmt.Printf("📝 Auto-added direct rule for domain: %s\n", domain)
	return a.saveRules(rules)
}

// AddIPRule adds an IP CIDR rule for direct connection.
func (a *AutoRuleManager) AddIPRule(ip string) error {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	rules := a.loadRules()

	// Check if already exists
	cidr := ip + "/32"
	for _, r := range rules {
		for _, c := range r.IPCIDR {
			if c == cidr {
				return nil // Already exists
			}
		}
	}

	// Add new rule
	rules = append(rules, config.RouteRule{
		IPCIDR:   []string{cidr},
		Outbound: "direct",
	})

	fmt.Printf("📝 Auto-added direct rule for IP: %s\n", ip)
	return a.saveRules(rules)
}

// loadRules loads custom rules from file.
func (a *AutoRuleManager) loadRules() []config.RouteRule {
	data, err := os.ReadFile(a.rulesPath)
	if err != nil {
		return []config.RouteRule{}
	}

	var rules []config.RouteRule
	if err := json.Unmarshal(data, &rules); err != nil {
		return []config.RouteRule{}
	}
	return rules
}

// saveRules saves rules to file.
func (a *AutoRuleManager) saveRules(rules []config.RouteRule) error {
	data, err := json.MarshalIndent(rules, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(a.rulesPath, data, 0644)
}

// Global instance
var autoRuleMgr = NewAutoRuleManager()

// AutoAddDomain is a convenience function to add a domain rule.
func AutoAddDomain(domain string) {
	_ = autoRuleMgr.AddDomainRule(domain)
}

// AutoAddIP is a convenience function to add an IP rule.
func AutoAddIP(ip string) {
	_ = autoRuleMgr.AddIPRule(ip)
}
