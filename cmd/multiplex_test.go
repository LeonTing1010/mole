package cmd

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

// Quality/what-x-what: capability survives server storage, export, and import.
func TestRealityMultiplexCapabilityRoundTrip(t *testing.T) {
	// Why: an unadvertised node must retain its original non-multiplexed URI.
	for _, enabled := range []bool{false, true} {
		data, _ := json.Marshal(map[string]any{
			"proto": protoReality, "ip": "192.0.2.1", "port": 443,
			"uuid":               "00000000-0000-4000-8000-000000000000",
			"reality_public_key": "pub", "reality_multiplex": enabled,
		})
		var srv Server
		if err := json.Unmarshal(data, &srv); err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(srv.URI())
		if (u.Query().Get("mux") == "smux") != enabled {
			t.Fatalf("capability %t lost in URI", enabled)
		}
		imported, err := realityServerFromURI(u)
		if err != nil {
			t.Fatal(err)
		}
		u2, _ := url.Parse(imported.URI())
		if u2.Query().Get("mux") != u.Query().Get("mux") {
			t.Fatal("import lost the multiplex capability")
		}
	}
}

func TestNodeExplicitlyAcceptsMultiplexAndKeepsVision(t *testing.T) {
	// Quality/what: global implicit mux is deprecated and rejects these requests.
	// Why: a client-only opt-in previously looked like Vision incompatibility.
	script := realityServerSnippet(443, 100, &realityIdentity{UUID: "legacy"})
	const start = "cat > /etc/sing-box/config.json << 'EOF'\n"
	_, rest, _ := strings.Cut(script, start)
	text, _, _ := strings.Cut(rest, "\nEOF\n")
	var cfg struct {
		Inbounds []struct {
			Multiplex struct{ Enabled bool } `json:"multiplex"`
			Users     []struct{ UUID, Flow string }
		}
	}
	if err := json.Unmarshal([]byte(text), &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Inbounds) != 1 || !cfg.Inbounds[0].Multiplex.Enabled {
		t.Fatal("node does not explicitly accept multiplex")
	}
	if len(cfg.Inbounds[0].Users) != 1 || cfg.Inbounds[0].Users[0].UUID != "legacy" || cfg.Inbounds[0].Users[0].Flow != "xtls-rprx-vision" {
		t.Fatal("multiplex changed the legacy Vision identity")
	}
}
