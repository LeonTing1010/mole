package utils

import (
	"os"
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
	rules := buildPFRules([]string{"45.32.28.60"})

	// Tables must be declared before any rule references them.
	lines := strings.Split(rules, "\n")
	if !strings.HasPrefix(lines[1], "table <mole_cn4>") {
		t.Fatalf("CN table declaration not on line 2:\n%s", rules)
	}
	if !strings.HasPrefix(lines[2], "table <"+apiTable+">") {
		t.Fatalf("API table declaration not on line 3:\n%s", rules)
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

// A standby node must be reachable too. Passing only the active endpoint makes
// every direct probe to another server fail at our own firewall, which reads
// exactly like a dead node — and makes failing over to one impossible to
// verify beforehand.
func TestPFRulesPassEveryKnownServer(t *testing.T) {
	rules := buildPFRules([]string{"45.32.28.60", "139.180.199.247"})
	for _, ip := range []string{"45.32.28.60", "139.180.199.247"} {
		if !strings.Contains(rules, "to "+ip+"\n") {
			t.Errorf("no pass rule for %s:\n%s", ip, rules)
		}
	}
	// Blanket blocks must still be present, or the change would have opened
	// the very leak the anchor exists to close.
	if !strings.Contains(rules, "block out quick") {
		t.Error("block rules disappeared")
	}
}

// The management endpoint must be reachable off-tunnel on every physical
// interface, ahead of the block.
//
// This is the rule whose absence cost a server. On 2026-08-05 the lid closed
// during a UDP blackout: the DELETE to api.vultr.com had only the dead tunnel
// to travel down, returned EOF, and the instance billed for the rest of the
// day. A destroy has to work at exactly the moment the tunnel does not, which
// means the control plane needs a path the data plane's failure cannot take
// away.
func TestPFRulesExemptTheManagementAPI(t *testing.T) {
	rules := buildPFRules([]string{"45.32.28.60"})
	for _, ifc := range physicalInterfaces() {
		api := strings.Index(rules, "pass out quick on "+ifc+" inet to <"+apiTable+">")
		blk := strings.Index(rules, "block out quick on "+ifc+" inet all")
		if api < 0 {
			t.Fatalf("no management-API exemption for %s:\n%s", ifc, rules)
		}
		if api > blk {
			t.Fatalf("API exemption for %s comes after the block, so it never matches:\n%s", ifc, rules)
		}
	}
}

// The exemption is one address, not a subnet. Every IP in this table is
// reachable off-tunnel by anything on the host, so it stays as small as the
// job allows: passing Vultr's /19 would let any traffic to any Vultr-hosted
// site leak straight past the kill switch.
func TestAPIExemptionIsNotABlanketSubnet(t *testing.T) {
	rules := buildPFRules([]string{"45.32.28.60"})
	decl := ""
	for _, l := range strings.Split(rules, "\n") {
		if strings.HasPrefix(l, "table <"+apiTable+">") {
			decl = l
		}
	}
	if decl == "" {
		t.Fatal("API table never declared")
	}
	// `persist` and nothing else: the table is filled at runtime by
	// AllowDirectHost from a live lookup, never with a hardcoded range.
	if strings.Contains(decl, "/") {
		t.Errorf("API table declares a CIDR, which would exempt more than the endpoint: %q", decl)
	}
	if !strings.Contains(decl, "persist") {
		t.Errorf("API table must persist, or it vanishes when empty and the pass rule fails to load: %q", decl)
	}
}

// Empty entries would render a syntactically invalid `pass out quick on en0 to`
// and pfctl would reject the whole anchor — arming nothing at all.
func TestPFRulesSkipEmptyAddresses(t *testing.T) {
	rules := buildPFRules([]string{"", "45.32.28.60", "  "})
	if strings.Contains(rules, "to \n") || strings.Contains(rules, "to   \n") {
		t.Errorf("empty address emitted a malformed rule:\n%s", rules)
	}
}

// The arm-time seed must run after the token file exists, because
// AllowDirectHost refuses to do anything while KillSwitchLoaded() is false and
// that is exactly what the token file answers.
//
// This is a source-order test rather than a behavioural one because the
// behaviour needs root, a real PF anchor, and a network — and because the bug
// it guards against was invisible in all three. The seed sat above `pfctl -E`
// for as long as the guard existed, returned nil without adding anything, and
// left mole_api4 empty on every fresh arm. Nothing failed; the kill switch just
// quietly blocked the management call that a closing lid depends on, and it
// only surfaced when someone read the table on a live host.
func TestKillSwitchSeedsTheExemptionAfterTheToken(t *testing.T) {
	src, err := os.ReadFile("killswitch.go")
	if err != nil {
		t.Fatalf("read killswitch.go: %v", err)
	}
	body := string(src)

	start := strings.Index(body, "func LoadKillSwitch(")
	if start < 0 {
		t.Fatal("LoadKillSwitch not found")
	}
	fn := body[start:]
	if end := strings.Index(fn, "\n}\n"); end > 0 {
		fn = fn[:end]
	}

	token := strings.Index(fn, "os.WriteFile(pfTokenPath()")
	seed := strings.Index(fn, "AllowDirectHost(APIHost)")
	enable := strings.Index(fn, `pfctl("-E")`)
	if token < 0 || seed < 0 || enable < 0 {
		t.Fatalf("LoadKillSwitch no longer has the three steps this pins (token=%d seed=%d enable=%d)", token, seed, enable)
	}
	if seed < enable {
		t.Error("the exemption is seeded before `pfctl -E`; PF is not enabled yet")
	}
	if seed < token {
		t.Error("the exemption is seeded before the token file is written, so AllowDirectHost's " +
			"KillSwitchLoaded() guard is still false and the seed silently does nothing")
	}
}

// Apple's range must be passed, and passed BEFORE the block — otherwise
// routing Apple's background services direct just moves the stall from the
// tunnel to our own firewall, and the symptomsd feedback loop that the direct
// routing exists to break fires exactly as before. See appleRange.
func TestPFRulesPassAppleRange(t *testing.T) {
	rules := buildPFRules([]string{"45.32.28.60"})
	for _, ifc := range physicalInterfaces() {
		apple := strings.Index(rules, "pass out quick on "+ifc+" inet to "+appleRange)
		blk4 := strings.Index(rules, "block out quick on "+ifc+" inet all")
		if apple < 0 {
			t.Fatalf("no Apple pass rule for %s:\n%s", ifc, rules)
		}
		if apple > blk4 {
			t.Fatalf("Apple pass must precede the block for %s (quick = first match wins):\n%s", ifc, rules)
		}
	}
	// The hole is exactly one /8 and nothing wider: a rule that passed all of
	// inet would silently retire the kill switch.
	if strings.Contains(rules, "pass out quick on en0 inet all") {
		t.Error("the Apple exception must not widen into a blanket inet pass")
	}
}
