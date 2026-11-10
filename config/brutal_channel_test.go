package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeonTing1010/mole/core"
)

const testRealityURI = "vless://00000000-0000-4000-8000-000000000000@198.51.100.10:443" +
	"?encryption=none&security=reality&type=tcp&flow=xtls-rprx-vision&fp=chrome" +
	"&sni=www.apple.com&pbk=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA&sid=0123456789abcdef#icn-test"

const testHy2URI = "hysteria2://pw@1.2.3.4:443?insecure=1#icn-test"

func brutalInbound(cfg *SingboxConfig) *InboundConfig {
	for i := range cfg.Inbounds {
		if cfg.Inbounds[i].Tag == core.BrutalInboundTag {
			return &cfg.Inbounds[i]
		}
	}
	return nil
}

// The keeper listens on the NODE's loopback, and the TUN never captures
// loopback here, so the only way to name it is to write the address on the far
// side of the leg. That is what the forwarder does.
func TestBrutalForwarderReachesTheNodeLoopback(t *testing.T) {
	cfg, err := BuildWithStandby(testHy2URI, testRealityURI)
	if err != nil {
		t.Fatal(err)
	}
	in := brutalInbound(cfg)
	if in == nil {
		t.Fatal("no brutal forwarder inbound; the rate has no way to reach the node")
	}
	if in.Listen != "127.0.0.1" {
		t.Errorf("listen = %q — the forwarder must not be reachable off this machine", in.Listen)
	}
	if in.ListenPort != core.BrutalKeeperPort || in.OverridePort != core.BrutalKeeperPort {
		t.Errorf("ports = %d -> %d, want %d both ends", in.ListenPort, in.OverridePort, core.BrutalKeeperPort)
	}
	if in.OverrideAddress != "127.0.0.1" {
		t.Errorf("override_address = %q, want the node's own loopback", in.OverrideAddress)
	}
}

// The rate describes the REALITY leg, so it has to travel over the REALITY leg
// — bound by inbound tag, not by the proxy selector, or a push during a
// fallback would go out over hy2 and pace nothing.
func TestBrutalRuleBindsTheRealityLegAndOutranksTheLoopbackDirect(t *testing.T) {
	cfg, err := BuildWithStandby(testHy2URI, testRealityURI)
	if err != nil {
		t.Fatal(err)
	}
	brutalAt, loopbackAt := -1, -1
	for i, r := range cfg.Route.Rules {
		if len(r.Inbound) == 1 && r.Inbound[0] == core.BrutalInboundTag {
			if r.Outbound != core.RealityLegTag {
				t.Errorf("brutal rule targets %q, want %q", r.Outbound, core.RealityLegTag)
			}
			if brutalAt < 0 {
				brutalAt = i
			}
		}
		for _, c := range r.IPCIDR {
			if c == "127.0.0.0/8" && loopbackAt < 0 {
				loopbackAt = i
			}
		}
	}
	if brutalAt < 0 {
		t.Fatal("no route rule binds the forwarder to the REALITY leg")
	}
	if loopbackAt < 0 {
		t.Fatal("expected a 127.0.0.0/8 direct rule to order against")
	}
	if brutalAt > loopbackAt {
		t.Errorf("brutal rule at %d sits behind the 127.0.0.0/8 direct rule at %d — "+
			"the push would be answered by this machine instead of the node", brutalAt, loopbackAt)
	}
}

// The rate push is on its own REALITY leg WITHOUT multiplex, so its dial can
// never become a session in the mux pool. TCP Brutal only classifies sockets
// created after the rule exists; a pool session dialled before the first
// successful push would stay on bbr for its whole life.
func TestBrutalRateChannelAvoidsTheMuxPool(t *testing.T) {
	// Production asks for the pool explicitly; without the query parameter the
	// REALITY leg is not multiplexed at all, and there would be nothing to avoid.
	base, _, _ := strings.Cut(testRealityURI, "#")
	cfg, err := BuildWithStandby(testHy2URI, base+"&mux=smux#icn-test")
	if err != nil {
		t.Fatal(err)
	}
	var rateIn *InboundConfig
	for i := range cfg.Inbounds {
		if cfg.Inbounds[i].Tag == core.BrutalRateInboundTag {
			rateIn = &cfg.Inbounds[i]
		}
	}
	if rateIn == nil {
		t.Fatal("no rate channel; the push would have to travel the mux pool")
	}
	if rateIn.Listen != "127.0.0.1" || rateIn.ListenPort != core.BrutalRatePort || rateIn.OverridePort != core.BrutalKeeperPort {
		t.Errorf("rate channel = %s:%d -> %d, want 127.0.0.1:%d -> %d",
			rateIn.Listen, rateIn.ListenPort, rateIn.OverridePort, core.BrutalRatePort, core.BrutalKeeperPort)
	}

	var rateOut, realityOut *OutboundConfig
	for i := range cfg.Outbounds {
		switch cfg.Outbounds[i].Tag {
		case core.RealityRateLegTag:
			rateOut = &cfg.Outbounds[i]
		case core.RealityLegTag:
			realityOut = &cfg.Outbounds[i]
		}
	}
	if rateOut == nil {
		t.Fatal("rate channel has no REALITY outbound")
	}
	if rateOut.Multiplex != nil {
		t.Error("the rate outbound is multiplexed; its dial could take the pool's first session")
	}
	if realityOut == nil || realityOut.Multiplex == nil {
		t.Fatal("precondition: the traffic REALITY leg should be multiplexed")
	}
	if rateOut.Server != realityOut.Server || rateOut.UUID != realityOut.UUID || rateOut.Flow != realityOut.Flow {
		t.Error("the rate outbound does not describe the same REALITY leg as the traffic outbound")
	}

	routed, routedAt := false, -1
	for i, r := range cfg.Route.Rules {
		if len(r.Inbound) == 1 && r.Inbound[0] == core.BrutalRateInboundTag {
			routed, routedAt = true, i
			if r.Outbound != core.RealityRateLegTag {
				t.Errorf("rate rule targets %q, want %q", r.Outbound, core.RealityRateLegTag)
			}
		}
	}
	if !routed {
		t.Fatal("no route rule binds the rate channel to its REALITY outbound")
	}
	for i, r := range cfg.Route.Rules {
		for _, c := range r.IPCIDR {
			if c == "127.0.0.0/8" && i < routedAt {
				t.Fatalf("127.0.0.0/8 direct at %d answers the rate dial before the rule at %d", i, routedAt)
			}
			if c == "127.0.0.0/8" {
				return
			}
		}
	}
}

// sing-box treats a rule naming an unknown outbound as fatal at startup, so a
// single-leg config that mentioned proxy-reality would not slow the tunnel
// down, it would stop it existing.
func TestNoRealityLegMeansNoForwarderAndNoDanglingRule(t *testing.T) {
	cfg, err := Build(testHy2URI)
	if err != nil {
		t.Fatal(err)
	}
	if in := brutalInbound(cfg); in != nil {
		t.Error("a single-leg config grew a forwarder for a leg it does not have")
	}
	for i := range cfg.Inbounds {
		if cfg.Inbounds[i].Tag == core.BrutalRateInboundTag {
			t.Error("a single-leg config grew a rate channel for a leg it does not have")
		}
	}
	tags := map[string]bool{}
	for _, o := range cfg.Outbounds {
		tags[o.Tag] = true
	}
	for i, r := range cfg.Route.Rules {
		if r.Outbound != "" && !tags[r.Outbound] {
			t.Errorf("route rule %d targets outbound %q, which this config does not define", i, r.Outbound)
		}
	}
}

// The strongest available judge: sing-box itself, on the two-leg config.
func TestTwoLegConfigWithForwarderPassesSingboxCheck(t *testing.T) {
	sb, err := exec.LookPath("sing-box")
	if err != nil {
		t.Skip("环境无 sing-box，跳过端到端校验")
	}
	realMole := MoleDirForTest()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".mole"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyIfExists(t, filepath.Join(realMole, "geoip-cn.srs"), filepath.Join(home, ".mole", "geoip-cn.srs"))
	copyIfExists(t, filepath.Join(realMole, "geosite-cn.srs"), filepath.Join(home, ".mole", "geosite-cn.srs"))

	cfg, err := BuildWithStandby(testHy2URI, testRealityURI)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".mole", "sing-box-config.json")
	if err := Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(sb, "check", "-c", path).CombinedOutput(); err != nil {
		t.Fatalf("sing-box rejected the config: %v\n%s", err, out)
	}
}
