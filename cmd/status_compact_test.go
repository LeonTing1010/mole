package cmd

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

// healthyStatusState is the smallest state that should collapse to the compact
// view: everything answered, nothing latched, no outstanding lid debt.
func healthyStatusState() *utils.State {
	st := &utils.State{
		Mode:             utils.ModeProxy,
		LastProbeVerdict: utils.ProbeAlive,
		LastProbeAt:      time.Now(),
		LastLatencyMs:    108,
		LastKeepaliveAt:  time.Now(),
		LastKeepaliveMs:  49,
		LinkGateway:      "192.168.1.1",
		LinkLastAt:       time.Now(),
		LidDestroy:       utils.LidDestroyArmed,
	}
	for i := 0; i < utils.DialWindow; i++ {
		st.DialSamples = append(st.DialSamples, utils.DialSample{Ms: 200, OK: true})
	}
	return st
}

func TestStatusHealthyCompactGate(t *testing.T) {
	if !statusHealthy(healthyStatusState()) {
		t.Fatal("a fully healthy state must use the compact view")
	}

	cases := []struct {
		name   string
		mutate func(*utils.State)
	}{
		{"window still filling", func(s *utils.State) { s.DialSamples = s.DialSamples[:3] }},
		{"a slow dial", func(s *utils.State) { s.DialSamples[0].Ms = utils.DialSlowMs + 1 }},
		{"a failed dial", func(s *utils.State) { s.DialSamples[0].OK = false }},
		{"local link bad", func(s *utils.State) { s.LinkBad = true }},
		{"keepalive failing", func(s *utils.State) { s.LastKeepaliveError = "i/o timeout" }},
		{"pacing unconfirmed", func(s *utils.State) { s.RealityPacingState = "unconfirmed" }},
		{"probe silent streak", func(s *utils.State) {
			s.LastProbeVerdict = utils.ProbeSilent
			s.ConsecutiveFails = utils.ProbeSilentThreshold
		}},
		{"lid destroy off", func(s *utils.State) { s.LidDestroy = utils.LidDestroyOff }},
		{"lid debt pending", func(s *utils.State) { s.LidDestroy = utils.LidDestroyPending }},
		{"not proxy mode", func(s *utils.State) { s.Mode = utils.Mode("direct") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := healthyStatusState()
			tc.mutate(st)
			if statusHealthy(st) {
				t.Fatal("must fall back to the detailed view")
			}
		})
	}
}

// The compact view is only allowed to hide telemetry, never a problem: if the
// gate is wrong the reader sees "healthy" over a degraded tunnel.
func TestStatusHealthyMatchesDetailedVerdict(t *testing.T) {
	st := healthyStatusState()
	st.SlowNewFlows = true
	if statusHealthy(st) {
		t.Fatal("SlowNewFlows must not render as healthy")
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	w.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// A state that reads as an outage must end in something the reader can do, not
// just a description of the outage.
func TestPrintHintsActionable(t *testing.T) {
	st := healthyStatusState()
	st.LinkBad = true
	out := captureStdout(t, func() { printHints(st) })
	if !strings.Contains(out, "Wi-Fi") {
		t.Fatalf("local-link hint missing: %q", out)
	}

	st = healthyStatusState()
	st.KeepaliveFails = utils.KeepaliveFailThreshold
	out = captureStdout(t, func() { printHints(st) })
	if !strings.Contains(out, "mole server deploy") {
		t.Fatalf("path-dead hint missing: %q", out)
	}

	st = healthyStatusState()
	st.DialSamples[0].OK = false
	out = captureStdout(t, func() { printHints(st) })
	if !strings.Contains(out, "1/20") {
		t.Fatalf("dial hint missing: %q", out)
	}
}
