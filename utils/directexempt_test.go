package utils

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The bug this whole file exists for: a rule that routes a foreign domain
// direct is routed straight into the kill switch's own block, silently. The
// table must therefore be declared and passed BEFORE the block, on every
// physical interface — a table that exists but is never consulted reproduces
// the outage exactly.
func TestPFRulesPassDirectTable(t *testing.T) {
	rules := buildPFRules([]string{"45.32.28.60"})
	if !strings.Contains(rules, "table <"+directTable+"> persist") {
		t.Fatalf("%s is never declared:\n%s", directTable, rules)
	}
	for _, ifc := range physicalInterfaces() {
		pass := strings.Index(rules, "pass out quick on "+ifc+" inet to <"+directTable+">")
		blk := strings.Index(rules, "block out quick on "+ifc+" inet all")
		if pass < 0 {
			t.Fatalf("no %s pass rule for %s:\n%s", directTable, ifc, rules)
		}
		if pass > blk {
			t.Fatalf("%s pass must precede the block for %s (quick = first match wins):\n%s", directTable, ifc, rules)
		}
	}
}

// literalPassRanges is shared by the rule builder and alreadyPassed; if the
// builder stops emitting one of them the two disagree about what is already
// open, and the learner starts adding addresses PF passes anyway.
func TestPFRulesEmitEveryLiteralPassRange(t *testing.T) {
	rules := buildPFRules(nil)
	for _, cidr := range literalPassRanges {
		if !strings.Contains(rules, cidr) {
			t.Errorf("literalPassRanges has %s but the ruleset never passes it", cidr)
		}
	}
}

// domain_suffix aligns on label boundaries in sing-box. A matcher that used a
// plain string suffix would exempt addresses for hosts the router never sends
// direct — the table would drift wider than the rules that justify it.
func TestDirectDomainSetMatch(t *testing.T) {
	set := DirectDomainSet{
		Suffixes: []string{"npmjs.com", ".upwork.com"},
		Keywords: []string{"-buy.itunes"},
	}
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"npmjs.com", true},
		{"www.npmjs.com", true},
		{"WWW.NPMJS.COM", true},
		{"www.npmjs.com.", true}, // trailing root dot
		{"upwork.com", true},     // leading dot in the rule is cosmetic
		{"assets-static.upwork.com", true},
		{"p47-buy.itunes.apple.com", true}, // keyword is a bare substring
		{"evilnpmjs.com", false},           // not on a label boundary
		{"npmjs.com.attacker.net", false},  // suffix, not prefix
		{"registry.npmjs.org", false},      // .org is a different rule
		{"", false},
	} {
		if got := set.Match(tc.host); got != tc.want {
			t.Errorf("Match(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
	if !(DirectDomainSet{}).Empty() {
		t.Error("an empty set must report Empty so the daemon can skip the learner")
	}
}

// Pruning is what makes deleting a domain from custom-rules.json actually shut
// the hole it opened. Before this the only thing that ever removed an exemption
// was a reboot.
func TestPruneDirectExemptStoreDropsUnclaimedHosts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, ".mole"), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := directExemptStore{
		"registry.npmjs.org": {IPs: []string{"104.16.0.35", "104.16.1.34"}},
		"www.upwork.com":     {IPs: []string{"104.18.128.226"}},
	}
	data, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(directExemptPath(), data, 0o644); err != nil {
		t.Fatal(err)
	}

	kept, ips, dropped := pruneDirectExemptStoreLocked(DirectDomainSet{Suffixes: []string{"npmjs.org"}})
	if len(dropped) != 1 || dropped[0] != "www.upwork.com" {
		t.Fatalf("dropped = %v, want [www.upwork.com]", dropped)
	}
	if _, ok := kept["registry.npmjs.org"]; !ok || len(kept) != 1 {
		t.Fatalf("kept = %v, want only registry.npmjs.org", kept)
	}
	if strings.Join(ips, ",") != "104.16.0.35,104.16.1.34" {
		t.Fatalf("ips = %v, want the npm pair, sorted and deduped", ips)
	}

	// The prune must survive a restart, or the next arm replays the hole.
	var onDisk directExemptStore
	raw, err := os.ReadFile(directExemptPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if _, ok := onDisk["www.upwork.com"]; ok {
		t.Error("the dropped host is still in the store; the next seed would re-open it")
	}
}

// Addresses the ruleset already passes must not be added: most direct rules are
// CN domains, and a table that accumulated those would grow without bound while
// saying nothing about what is actually exempted.
func TestAlreadyPassedCoversLiteralRanges(t *testing.T) {
	for _, ip := range []string{"192.168.1.34", "10.0.0.1", "169.254.1.1", "127.0.0.1", "17.253.144.10"} {
		if !alreadyPassed(ip) {
			t.Errorf("alreadyPassed(%s) = false; the ruleset passes it already", ip)
		}
	}
}
