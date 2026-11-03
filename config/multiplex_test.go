package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// Quality/what: reuse is opt-in because older nodes reject multiplex requests.
func TestRealityMultiplexRequiresExplicitCapability(t *testing.T) {
	// Why: reducing new SYNs must not strand existing, unupgraded nodes.
	for _, tc := range []struct {
		query string
		want  bool
	}{{"", false}, {"&mux=smux", true}} {
		base, _, _ := strings.Cut(testRealityURI, "#")
		o, err := parseServerURL(base + tc.query)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(o)
		var fields map[string]json.RawMessage
		json.Unmarshal(data, &fields)
		if _, ok := fields["multiplex"]; ok != tc.want {
			t.Fatalf("query %q: multiplex present=%t, want %t", tc.query, ok, tc.want)
		}
		if tc.want {
			var mux struct {
				Enabled        bool   `json:"enabled"`
				Protocol       string `json:"protocol"`
				MaxConnections int    `json:"max_connections"`
				MinStreams     int    `json:"min_streams"`
			}
			json.Unmarshal(fields["multiplex"], &mux)
			if !mux.Enabled || mux.Protocol != "smux" || mux.MaxConnections != 8 || mux.MinStreams != 1 {
				t.Fatalf("unexpected native connection pool: %+v", mux)
			}
			if o.Flow != "xtls-rprx-vision" || !o.TCPFastOpen || o.TLS.Insecure {
				t.Fatal("multiplex changed Vision, Fast Open, or authenticated TLS")
			}
		}
	}
}

func TestRealityMultiplexRejectsUnknownProtocol(t *testing.T) {
	// Quality/what: a misspelled opt-in must not silently select another transport.
	base, _, _ := strings.Cut(testRealityURI, "#")
	if _, err := parseServerURL(base + "&mux=typo"); err == nil {
		t.Fatal("unknown multiplex protocol accepted")
	}
}
