package cmd

import (
	"os"
	"testing"

	"github.com/LeonTing1010/mole/config"
	"github.com/LeonTing1010/mole/core"
)

// Not a unit test: it builds the config for the node actually on this machine
// and asserts the gate materialises for it. Skipped wherever there is no
// server list, so CI and a fresh clone are unaffected.
//
// It exists because every other test here proves the wiring for a URI the test
// wrote itself. The live node's URI carries obfs, a pinned cert and a ladder
// this file does not choose, and those are the parts a synthetic case cannot
// vouch for.
func TestLiveNodeGetsTheLegGate(t *testing.T) {
	if _, err := os.Stat(os.Getenv("HOME") + "/.mole/servers.json"); err != nil {
		t.Skip("no server list on this machine")
	}
	servers, err := loadServers()
	if err != nil || len(servers) == 0 {
		t.Skip("no servers")
	}
	for _, s := range servers {
		if !s.Active {
			continue
		}
		if s.StandbyURI() == "" {
			t.Skipf("active node %s carries no REALITY leg", s.Name)
		}
		cfg, err := config.BuildWithStandby(s.URI(), s.StandbyURI())
		if err != nil {
			t.Fatalf("building the live node's config: %v", err)
		}
		var listen int
		for _, in := range cfg.Inbounds {
			if in.Tag == core.LegGateInboundTag {
				listen = in.ListenPort
			}
		}
		if listen != core.LegGatePort {
			t.Fatalf("live node %s: gate forwarder listens on %d, want %d", s.Name, listen, core.LegGatePort)
		}
		var target string
		for _, r := range cfg.Route.Rules {
			for _, in := range r.Inbound {
				if in == core.LegGateInboundTag {
					target = r.Outbound
				}
			}
		}
		if target != core.Hy2ProbeSelectorTag {
			t.Fatalf("live node %s: gate routed to %q, not its independent UDP selector", s.Name, target)
		}
		var members []string
		for _, out := range cfg.Outbounds {
			if out.Tag == target && out.Type == "selector" {
				members = out.Outbounds
			}
		}
		if len(members) == 0 {
			t.Fatal("live UDP probe selector has no usable members")
		}
		for _, member := range members {
			found := false
			for _, out := range cfg.Outbounds {
				if out.Tag == member && out.Type == "hysteria2" {
					found = true
				}
			}
			if !found {
				t.Fatalf("live UDP probe can measure another transport via %q", member)
			}
		}
		t.Logf("live node %s: gate 127.0.0.1:%d -> %s", s.Name, listen, target)
	}
}
