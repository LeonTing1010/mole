package config

import "github.com/LeonTing1010/mole/core"

func flowSelectors() []OutboundConfig {
	var selectors []OutboundConfig
	for lane := 0; lane < core.BalanceLanes; lane++ {
		selectors = append(selectors, OutboundConfig{
			Type: "selector", Tag: core.BalanceLaneTag(lane), Default: core.ProxySelectorTag,
			Outbounds: []string{core.ProxySelectorTag, core.RealityLegTag, core.Hy2ProbeSelectorTag},
		})
	}
	return selectors
}

func flowRoutes(rules []RouteRule) []RouteRule {
	var out []RouteRule
	for _, rule := range rules {
		if rule.Outbound == core.ProxySelectorTag && !rule.Invert && len(rule.SourcePortRange) == 0 {
			for lane := 0; lane < core.BalanceLanes; lane++ {
				shard := rule
				shard.SourcePortRange = core.BalancePortRanges(lane)
				shard.Outbound = core.BalanceLaneTag(lane)
				out = append(out, shard)
			}
		}
		// Retain source-port-zero and explicitly scoped/inverted rules exactly.
		out = append(out, rule)
	}
	// Sniffing a FakeIP flow restores its domain and clears the synthetic IP.
	// Such traffic misses every CIDR route and would otherwise go straight to
	// route.final, bypassing all lane selectors. Match source ports alone at
	// the end, after domestic/direct and all explicit routing decisions.
	for lane := 0; lane < core.BalanceLanes; lane++ {
		out = append(out, RouteRule{
			SourcePortRange: core.BalancePortRanges(lane), Outbound: core.BalanceLaneTag(lane),
		})
	}
	return out
}
