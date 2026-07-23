package utils

import (
	"strings"
	"testing"
)

func TestFilterIPv4CIDRs(t *testing.T) {
	body := `1.0.1.0/24
# comment
223.5.5.0/24

::/0
2400:3200::/32
not-a-cidr
1.2.3.4`
	got := filterIPv4CIDRs(body)
	want := []string{"1.0.1.0/24", "223.5.5.0/24"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestEmbeddedCNCIDRsIsUsable(t *testing.T) {
	valid := filterIPv4CIDRs(embeddedCNCIDRs)
	if len(valid) < cnCIDRMinEntries {
		t.Fatalf("embedded CN list has only %d valid CIDRs (want ≥%d) — the fallback would arm a kill-everything switch", len(valid), cnCIDRMinEntries)
	}
}

func TestBuildPFRulesShape(t *testing.T) {
	rules := buildPFRules("45.32.28.60")

	// The table must be declared before any rule references it.
	if !strings.HasPrefix(strings.SplitN(rules, "\n", 3)[1], "table <mole_cn4>") {
		t.Fatalf("table declaration not on line 2:\n%s", rules)
	}
	// utun/lo must never be mentioned — tunnel traffic is out of scope.
	if strings.Contains(rules, "utun") || strings.Contains(rules, "lo0") {
		t.Fatalf("rules must not touch utun/lo0:\n%s", rules)
	}
	for _, ifc := range physicalInterfaces() {
		// Per interface: the VPS pass must come before the inet block, and a
		// block must exist for both address families.
		vps := strings.Index(rules, "pass out quick on "+ifc+" to 45.32.28.60")
		cn := strings.Index(rules, "pass out quick on "+ifc+" inet to <mole_cn4>")
		blk4 := strings.Index(rules, "block out quick on "+ifc+" inet all")
		blk6 := strings.Index(rules, "block out quick on "+ifc+" inet6 all")
		if vps < 0 || cn < 0 || blk4 < 0 || blk6 < 0 {
			t.Fatalf("missing rule for %s:\n%s", ifc, rules)
		}
		if !(vps < blk4 && cn < blk4) {
			t.Fatalf("pass rules must precede the block for %s (quick = first match wins):\n%s", ifc, rules)
		}
	}
}
