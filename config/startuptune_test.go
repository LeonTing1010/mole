package config

import (
	"slices"
	"testing"

	"github.com/LeonTing1010/mole/core"
)

func TestStartupTuneUsesPrivateKeeperRoute(t *testing.T) {
	cfg := SingboxConfig{
		Outbounds: []OutboundConfig{
			{Type: "selector", Tag: "proxy", Outbounds: []string{"proxy-bw-100", "proxy-bw-200", "proxy-reality"}, Default: "proxy-reality"},
			{Type: "hysteria2", Tag: "proxy-bw-100"},
			{Type: "hysteria2", Tag: "proxy-bw-200"},
			{Type: "vless", Tag: "proxy-reality"},
		},
		Route: RouteConfig{Final: "proxy"},
	}
	cfg.EnableStartupTune()
	probe := cfg.Outbounds[len(cfg.Outbounds)-1]
	if probe.Tag != core.StartupTuneSelector || !slices.Equal(probe.Outbounds, []string{"proxy-bw-100", "proxy-bw-200"}) {
		t.Fatalf("probe should select only actual HY2 rungs: %+v", probe)
	}
	in := cfg.Inbounds[0]
	if in.Listen != "127.0.0.1" || in.OverrideAddress != "127.0.0.1" || in.OverridePort != core.BrutalKeeperPort || in.ListenPort != core.StartupTunePort {
		t.Fatalf("probe should reach only the remote keeper through the local listener: %+v", in)
	}
	rule := cfg.Route.Rules[0]
	if !slices.Equal(rule.Inbound, []string{in.Tag}) || rule.Outbound != probe.Tag || cfg.Route.Final != "proxy" || cfg.Outbounds[0].Default != "proxy-reality" {
		t.Fatalf("test route must have its own selector: %+v", rule)
	}
}
