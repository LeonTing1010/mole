package utils

import (
	"os"
	"testing"
)

func TestTunedCeilingDoesNotReplaceManualSettingOrFollowAnotherNode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	initial := CeilingSetting{Down: 600, Tune: true}
	if err := WriteCeilingSetting(initial); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(CeilingPath()); err != nil || string(data) != "600\n" {
		t.Fatalf("startup tuning must leave a legacy-compatible pin: %q (%v)", data, err)
	}
	if setting := ReadCeilingSetting(); setting != initial || ReadCeiling() != 600 {
		t.Fatalf("saved setting = %+v; want %+v", setting, initial)
	}
	if err := WriteCeiling(200); err != nil {
		t.Fatal(err)
	}
	if err := WriteTunedCeiling("old-node", 600, 300); err != nil {
		t.Fatal(err)
	}
	if setting := ReadCeilingSetting(); setting != (CeilingSetting{Down: 200}) {
		t.Fatalf("a late result overwrote the manual setting: %+v", setting)
	}
	if ReadTunedCeiling("old-node", 600) != 300 || ReadTunedCeiling("new-node", 600) != 0 || ReadTunedCeiling("old-node", 200) != 0 {
		t.Fatal("a benchmark result must apply only to its node and seed")
	}
}
