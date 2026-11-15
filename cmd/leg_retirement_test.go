package cmd

import (
	"testing"

	"github.com/LeonTing1010/mole/utils"
)

// Safety/what-x-what: rejecting a retired mode preserves current user intent.
func TestRetiredBothCommandPreservesManualPin(t *testing.T) {
	// Why: asking for a removed mode must not silently activate a different one.
	t.Setenv("HOME", t.TempDir())
	srv := Server{Name: "test", Active: true, IP: "127.0.0.1", Port: 443, Password: "test", UUID: "test", RealityPubKey: "test", RealityShortID: "aa"}
	if err := saveServers([]Server{srv}); err != nil {
		t.Fatal(err)
	}
	if err := utils.WriteLeg(utils.LegHy2); err != nil {
		t.Fatal(err)
	}
	if err := runLeg(nil, []string{"both"}); err == nil {
		t.Fatal("retired mixed mode was accepted")
	}
	if utils.ReadLeg() != utils.LegHy2 {
		t.Fatal("rejected mode changed the saved pin")
	}
}

// Quality/what: supported protocol pins and automatic routing remain available.
func TestSupportedLegCommandsPersistOffline(t *testing.T) {
	// Why: retiring mixture optimization must preserve explicit protocol control.
	t.Setenv("HOME", t.TempDir())
	srv := Server{Name: "test", Active: true, IP: "127.0.0.1", Port: 443, Password: "test", UUID: "test", RealityPubKey: "test", RealityShortID: "aa"}
	if err := saveServers([]Server{srv}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ command, want string }{
		{"reality", utils.LegReality}, {"tcp", utils.LegReality},
		{"hy2", utils.LegHy2}, {"hysteria2", utils.LegHy2}, {"udp", utils.LegHy2},
		{"auto", utils.LegAuto},
	} {
		if err := runLeg(nil, []string{tc.command}); err != nil {
			t.Fatal(err)
		}
		if got := utils.ReadLeg(); got != tc.want {
			t.Fatalf("%s saved %q, want %q", tc.command, got, tc.want)
		}
	}
}
