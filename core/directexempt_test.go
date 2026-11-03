package core

import (
	"testing"

	"github.com/LeonTing1010/mole/utils"
)

// The learner opens holes in a fail-closed ruleset, so what it REFUSES matters
// more than what it accepts. Each rejection below is a way the table could grow
// past the rules that justify it.
func TestDirectExemptCandidatesGate(t *testing.T) {
	set := utils.DirectDomainSet{Suffixes: []string{"npmjs.org", "upwork.com"}}
	conns := []Connection{
		// The case this exists for: sing-box dialed it direct, PF dropped it.
		{Chains: []string{"direct"}, Host: "registry.npmjs.org", DestIP: "104.16.0.35"},
		// Rode the tunnel — the VPS pass rule covers it, and its DestIP is empty
		// anyway because the tunnel carries a domain, not an address.
		{Chains: []string{"proxy-reality", "proxy"}, Host: "github.com", DestIP: ""},
		// Direct, but for a domain nobody wrote a direct rule for. If this ever
		// happens the router is wrong, and a hole is the last thing it needs.
		{Chains: []string{"direct"}, Host: "api.anthropic.com", DestIP: "160.79.104.10"},
		// A raw-IP dial: no name, nothing to check against the rules.
		{Chains: []string{"direct"}, Host: "", DestIP: "117.185.25.32"},
		// A chain whose FINAL outbound is the proxy. Only chains[0] opens the
		// socket; matching "direct" anywhere in the chain would exempt an
		// address the packets never travel to.
		{Chains: []string{"proxy", "direct"}, Host: "www.upwork.com", DestIP: "104.18.128.226"},
		{Chains: nil, Host: "www.upwork.com", DestIP: "104.18.129.226"},
	}
	got := directExemptCandidates(conns, set)
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	if got[0].Host != "registry.npmjs.org" || got[0].IP != "104.16.0.35" {
		t.Fatalf("got %+v, want registry.npmjs.org/104.16.0.35", got[0])
	}
}
