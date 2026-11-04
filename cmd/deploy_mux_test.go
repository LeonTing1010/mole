package cmd

import (
	"github.com/LeonTing1010/mole/utils"
	"strings"
	"testing"
	"time"
)

// Quality/what-x-what: new nodes use the native capability their boot script installs.
func TestFreshDeploymentOptsIntoNativeReuseWithoutChangingImports(t *testing.T) {
	// Why: daily replacement must not silently lose a qualified reuse option.
	id, err := newRealityIdentity("")
	if err != nil {
		t.Fatal(err)
	}
	for _, proto := range []string{protoReality, protoHysteria2} {
		s := Server{Name: "test", Proto: proto, IP: "192.0.2.10", Port: 443}
		configureDeployedReality(&s, id)
		uri := s.URI()
		if proto == protoHysteria2 {
			uri = s.StandbyURI()
		}
		if !s.RealityMultiplex || !strings.Contains(uri, "mux=smux") || s.UUID != id.UUID || s.RealityPubKey != id.PublicKey {
			t.Fatalf("fresh %s node lost authenticated native reuse", proto)
		}
		if !strings.Contains(realityServerSnippet(443, 400, id), `"multiplex": {"enabled": true}`) {
			t.Fatal("new profile opted into unsupported server capability")
		}
	}
	legacy := Server{Name: "legacy", Proto: protoReality, IP: "192.0.2.11", Port: 443, UUID: id.UUID, RealityPubKey: id.PublicKey, RealityDest: id.Dest}
	back, err := serverFromURI(legacy.URI())
	if err != nil {
		t.Fatal(err)
	}
	if back.RealityMultiplex || strings.Contains(back.URI(), "mux=") {
		t.Fatal("legacy import was opted in without capability evidence")
	}
}

// Quality/what: clean synthetic samples cannot hide observed application failures.
func TestDialStatusSeparatesApplicationEvidenceFromProbeVotes(t *testing.T) {
	// Why: operators need visibility without mislabeling raw sightings as an error rate.
	s := &utils.State{ApplicationNodeDialFailures: 3, LastApplicationNodeDialFailureAt: time.Now()}
	for i := 0; i < utils.DialWindow; i++ {
		s.RecordDial(100, nil)
	}
	text := dialStatus(s)
	if !strings.Contains(text, "3 application node-dial failures observed") || !strings.Contains(text, "0 failed") {
		t.Fatal(text)
	}
}
