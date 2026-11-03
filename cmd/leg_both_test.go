package cmd

import (
	"testing"

	"github.com/LeonTing1010/mole/utils"
)

// Quality/what-x-what: both-mode intent is valid only on a dual-protocol node.
func TestLegBothPersistsOfflineAndRejectsSingleProtocolNodes(t *testing.T) {
	// Why: accepting an undefined second leg would strand flows on bad routes.
	t.Setenv("HOME", t.TempDir())
	srv := Server{Name: "test", Active: true, IP: "127.0.0.1", Port: 443, Password: "test", UUID: "test", RealityPubKey: "test", RealityShortID: "aa"}
	if err := saveServers([]Server{srv}); err != nil {
		t.Fatal(err)
	}
	if err := runLeg(nil, []string{"both"}); err != nil {
		t.Fatal(err)
	}
	if utils.ReadLeg() != utils.LegBoth {
		t.Fatal("dual-mode intent was not saved")
	}
	if err := utils.ClearLeg(); err != nil {
		t.Fatal(err)
	}
	srv.UUID = ""
	srv.RealityPubKey = ""
	if err := saveServers([]Server{srv}); err != nil {
		t.Fatal(err)
	}
	if err := runLeg(nil, []string{"both"}); err == nil {
		t.Fatal("single-protocol node accepted dual traffic")
	}
	if utils.ReadLeg() != utils.LegAuto {
		t.Fatal("rejected command modified the transport pin")
	}
}
