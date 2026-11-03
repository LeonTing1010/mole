package utils

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The kill switch is a PF anchor that makes "foreign traffic must ride the
// tunnel" a property of the HOST, not of the sing-box process. sing-box's
// auto_route only protects while sing-box is alive: the moment it dies, its
// routes vanish and every foreign connection silently falls back to the
// physical interface — traffic doesn't stop, it LEAKS. The anchor outlives
// the process: physical-interface egress is blocked except for the VPS
// endpoint, Chinese IPv4 space, and local infrastructure, so a dead sing-box
// means foreign = blocked (fail-closed) while domestic direct keeps working.
//
// The anchor lives under com.apple/* because the stock /etc/pf.conf already
// evaluates that wildcard — rules load into it without editing any system
// file. Rules are scoped to physical interfaces (en*/bridge*), so utun
// traffic is never touched.
//
// Deliberate consequence: if the daemon is SIGKILL'd, nothing unloads the
// anchor and foreign stays blocked until the next `mole up`/`mole down`.
// That is the feature — the alternative is leaking until someone notices.
const pfAnchor = "com.apple/mole"

// apiTable holds the management-plane addresses that may be reached DIRECTLY,
// off the tunnel, while the kill switch is armed.
//
// It exists because the two halves of mole disagree about api.vultr.com. To
// the data plane it is ordinary foreign traffic and belongs in the tunnel; to
// the control plane it is the one address that must stay reachable when the
// tunnel is the thing that is broken — a destroy has to work precisely when
// the path is dark. Before this table there was no such exemption, so the
// DELETE had only the dead tunnel to travel down.
//
// A table rather than literal rules so the daemon can add an address with
// `pfctl -T add` at the moment it needs one, without rebuilding and reloading
// the anchor mid-sleep. The leak surface is one /32 of Vultr's API endpoint,
// reachable by anything on the host — a fair price for a destroy that fires.
const apiTable = "mole_api4"

// APIHost is the management endpoint the kill switch exempts. Kept next to the
// table so the two can never name different services.
const APIHost = "api.vultr.com"

// cnCIDRURL is the canonical China IPv4 CIDR list (APNIC-derived, one CIDR
// per line). Same lineage of data as the geoip-cn rule-set sing-box routes
// by, so the PF table and sing-box agree on what "China" means.
const cnCIDRURL = "https://raw.githubusercontent.com/17mon/china_ip_list/master/china_ip_list.txt"

// cnCIDRMaxAge mirrors geosite-cn's refresh cadence. CN address allocations
// move slowly; weekly is plenty.
const cnCIDRMaxAge = 7 * 24 * time.Hour

// cnCIDRMinEntries rejects a fetched list that is implausibly small. A
// truncated download must never become the PF table: the table is what keeps
// domestic traffic FLOWING while everything else is blocked, so a bad table
// converts "kill switch" into "kill everything".
const cnCIDRMinEntries = 1000

//go:embed cn-cidr.txt
var embeddedCNCIDRs string

// CNCIDRPath is the on-disk China IPv4 list the PF table loads from.
func CNCIDRPath() string { return filepath.Join(MoleDir(), "cn-cidr.txt") }

func pfRulesPath() string { return filepath.Join(MoleDir(), "pf-killswitch.conf") }

// pfTokenPath stores the pfctl -E reference token so `mole down` can release
// our enable reference (-X) instead of force-disabling PF for everyone.
func pfTokenPath() string { return filepath.Join(MoleDir(), "pf-token") }

// EnsureCNCIDRs makes sure a valid China CIDR list exists on disk, mirroring
// EnsureRuleSets: fetch when missing or stale, validate before replacing,
// fall back to the cached then the embedded copy. Never blocks startup.
func EnsureCNCIDRs() {
	path := CNCIDRPath()
	if st, err := os.Stat(path); err == nil && st.Size() > 0 && time.Since(st.ModTime()) < cnCIDRMaxAge {
		return
	}
	if err := fetchCNCIDRs(path); err != nil {
		if st, statErr := os.Stat(path); statErr == nil && st.Size() > 0 {
			fmt.Printf("⚠️  CN CIDR list refresh failed: %v (using cached copy)\n", err)
			return
		}
		fmt.Printf("⚠️  CN CIDR list fetch failed: %v (using embedded copy)\n", err)
		_ = os.WriteFile(path, []byte(embeddedCNCIDRs), 0644)
	}
}

func fetchCNCIDRs(dst string) error {
	client := newDirectHTTPClient(30 * time.Second)
	resp, err := client.Get(cnCIDRURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	valid := filterIPv4CIDRs(string(body))
	if len(valid) < cnCIDRMinEntries {
		return fmt.Errorf("only %d valid CIDRs (want ≥%d) — refusing truncated list", len(valid), cnCIDRMinEntries)
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(valid, "\n")+"\n"), 0644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// filterIPv4CIDRs keeps only lines that parse as IPv4 CIDRs. The PF table is
// IPv4-only by design: IPv6 egress on physical interfaces is blocked outright
// (link-local excepted), which is also mole's AAAA-leak posture.
func filterIPv4CIDRs(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ip, _, err := net.ParseCIDR(line)
		if err != nil || ip.To4() == nil {
			continue
		}
		out = append(out, line)
	}
	return out
}

// physicalInterfaces returns every present en*/bridge* interface. Down
// interfaces are included on purpose: an idle USB NIC that comes up later
// must already be covered. Interfaces that APPEAR after load (rare) are not
// covered until the next `mole up`.
func physicalInterfaces() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return []string{"en0"}
	}
	var out []string
	for _, ifc := range ifaces {
		if strings.HasPrefix(ifc.Name, "en") || strings.HasPrefix(ifc.Name, "bridge") {
			out = append(out, ifc.Name)
		}
	}
	if len(out) == 0 {
		out = []string{"en0"}
	}
	return out
}

// appleRange is Apple's own IPv4 allocation, and the one deliberate hole in an
// otherwise fail-closed ruleset.
//
// It is here because of a feedback loop that cost more downtime than the GFW
// did. Apple's background services — apsd, cloudd, accountsd, AddressBook
// sync — were routed through the tunnel, so every tunnel hiccup hung THEIR
// connections; macOS's symptomsd watches exactly those, calls the interface
// stalled, and answers by releasing the DHCP lease and re-associating Wi-Fi.
// That took the uplink out from under every hy2 session (which then died with
// `can't assign requested address`) and turned a 10–20 second wobble into a
// 60–190 second outage. Measured 2026-08-22: five such events, the three
// longest outages of the day among them.
//
// Routing those services direct is what breaks the loop, and this rule is what
// makes "direct" actually work — without it sing-box sends them to the physical
// interface and PF drops them, which is a stall too, just a quieter one.
//
// The security trade is small and deliberate. 17.0.0.0/8 is Apple's own
// registered space, the traffic is already authenticated to Apple by the
// device's identity, and this is the same path every Mac in the country uses
// when no VPN is running at all. What it does concede: if sing-box dies, Apple
// services keep talking from the real IP instead of failing closed.
const appleRange = "17.0.0.0/8"

// literalPassRanges are the CIDRs every physical interface passes outright:
// local and link-local infrastructure, plus Apple's own allocation.
//
// One slice, two readers — buildPFRules emits them and alreadyPassed tests
// against them. They were two hand-kept lists for about an hour, which was long
// enough to see how that ends: the exemption learner would have gone on adding
// addresses PF was already passing, and every drift between the lists would
// have shown up as a table that quietly disagreed with the rules.
var literalPassRanges = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"169.254.0.0/16",
	"100.64.0.0/10",
	"224.0.0.0/4",
	"255.255.255.255/32",
	appleRange,
}

// buildPFRules renders the anchor ruleset. Per physical interface, first
// match wins (quick): VPS endpoints (the tunnel itself + the health probe),
// local/broadcast infrastructure, DHCP, Apple's own range, Chinese IPv4 space,
// IPv6 link-local — then block everything else, both families. utun/lo are
// never mentioned, so tunnel-bound traffic is untouched.
//
// Every KNOWN server is passed, not just the active one. Passing only the
// active endpoint silently breaks any direct contact with a second node: a
// health probe to a candidate server leaves the machine bound to a physical
// interface, matches no pass rule, and is dropped here — indistinguishable,
// from the caller's side, from a server that is down or a path that is dark.
// Being able to reach a node you are not currently using is a prerequisite for
// ever failing over to one.
func buildPFRules(vpsIPs []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# generated by mole — do not edit; unloaded by `mole down`\n")
	fmt.Fprintf(&b, "table <mole_cn4> persist file \"%s\"\n", CNCIDRPath())
	fmt.Fprintf(&b, "table <%s> persist\n", apiTable)
	fmt.Fprintf(&b, "table <%s> persist\n", directTable)
	for _, ifc := range physicalInterfaces() {
		fmt.Fprintf(&b, "pass out quick on %s inet to <%s>\n", ifc, apiTable)
		// The data-plane counterpart: addresses that custom-rules.json routes
		// direct on purpose. Empty until the daemon learns one, so a host that
		// has never used a direct rule carries no extra hole. See directexempt.go.
		fmt.Fprintf(&b, "pass out quick on %s inet to <%s>\n", ifc, directTable)
		for _, ip := range vpsIPs {
			if ip = strings.TrimSpace(ip); ip != "" {
				fmt.Fprintf(&b, "pass out quick on %s to %s\n", ifc, ip)
			}
		}
		fmt.Fprintf(&b, "pass out quick on %s inet to { %s }\n", ifc, strings.Join(literalPassRanges, ", "))
		fmt.Fprintf(&b, "pass out quick on %s proto udp from any port 68 to any port 67\n", ifc)
		fmt.Fprintf(&b, "pass out quick on %s inet to <mole_cn4>\n", ifc)
		fmt.Fprintf(&b, "pass out quick on %s inet6 to { fe80::/10, ff02::/16 }\n", ifc)
		fmt.Fprintf(&b, "block out quick on %s inet all\n", ifc)
		// IPv6 is refused, not dropped. The tunnel and every DNS answer are
		// IPv4-only, but apps with their own resolver still dial IPv6 — WeChat's
		// HTTPDNS hands out Tencent v6 CDN addresses — and a silent drop leaves
		// each of those in SYN_SENT until it times out before the app falls back
		// to v4. Measured 2026-09-15: 203/203 WeChat v6 connects hung while
		// every v4 one completed, which is what made 公众号 and 视频号 slow to
		// open. A reset keeps the block fail-closed and makes the fallback instant.
		fmt.Fprintf(&b, "block return out quick on %s inet6 all\n", ifc)
	}
	return b.String()
}

// LoadKillSwitch writes the ruleset, loads it into the anchor, and takes a
// PF enable reference. Requires root (the daemon has it).
func LoadKillSwitch(vpsIPs ...string) error {
	if _, err := os.Stat(CNCIDRPath()); err != nil {
		// EnsureCNCIDRs runs in the parent, but the daemon must not depend on
		// that having succeeded — fall back to the embedded snapshot.
		if err := os.WriteFile(CNCIDRPath(), []byte(embeddedCNCIDRs), 0644); err != nil {
			return fmt.Errorf("write embedded CN list: %w", err)
		}
	}
	rules := buildPFRules(vpsIPs)
	if err := os.WriteFile(pfRulesPath(), []byte(rules), 0644); err != nil {
		return fmt.Errorf("write rules: %w", err)
	}
	if out, err := pfctl("-a", pfAnchor, "-f", pfRulesPath()); err != nil {
		return fmt.Errorf("load anchor: %v (%s)", err, strings.TrimSpace(out))
	}
	out, err := pfctl("-E")
	if err != nil {
		_, _ = pfctl("-a", pfAnchor, "-F", "all")
		return fmt.Errorf("enable pf: %v (%s)", err, strings.TrimSpace(out))
	}
	// pfctl -E reports a reference token on stderr; keep it so unload can
	// release exactly our reference. A missing token is survivable — unload
	// then just flushes the anchor and leaves PF enabled (rules are gone, so
	// nothing is blocked).
	if m := regexp.MustCompile(`Token\s*:\s*(\d+)`).FindStringSubmatch(out); m != nil {
		_ = os.WriteFile(pfTokenPath(), []byte(m[1]), 0644)
	}
	// Seed the exemption now, while DNS is still the system's and the answer is
	// cheap. AllowDirectHost is called again before each management request, so
	// a stale seed costs nothing — but arming with it already populated means
	// the very first destroy does not depend on a lookup succeeding at the worst
	// possible moment.
	//
	// After the token, not before, and that ordering is the whole point: this
	// call used to sit above `pfctl -E`, where AllowDirectHost's own
	// KillSwitchLoaded guard — which is the presence of the token file written
	// two lines up — was still false. The seed returned nil without doing
	// anything, on every arm, since the guard was introduced. The symptom was
	// invisible from here and only showed up as an empty mole_api4 table on a
	// live host: our own kill switch dropping the management call that a closing
	// lid depends on.
	if err := AllowDirectHost(APIHost); err != nil {
		log.Printf("kill switch: %s not exempted yet (%v) — retried on demand", APIHost, err)
	}
	return nil
}

// AllowDirectHost resolves host outside the tunnel and adds every address to
// the exemption table, so a socket bound to a physical interface can reach it
// while the kill switch is armed.
//
// Idempotent and cheap (`pfctl -T add` on an address already present is a
// no-op), which is what lets every management call do this first instead of
// trusting a seed taken hours earlier — an endpoint that moved between arming
// and a closing lid would otherwise be blocked by our own rules at the one
// moment nobody is watching.
//
// A no-op when the kill switch is not loaded: without the anchor there is
// nothing blocking the address in the first place.
func AllowDirectHost(host string) error {
	return AllowDirectHostContext(context.Background(), host)
}

func AllowDirectHostContext(parent context.Context, host string) error {
	if !KillSwitchLoaded() {
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	// A resolution failure used to abandon the exemption entirely, which is the
	// worst of both worlds: the bound client below may still have a cached
	// address to dial, and our own kill switch would then be the thing dropping
	// it. Exempt whatever we know about and report the staleness rather than
	// leaving the table empty.
	ips, live, err := ResolveDirectCached(ctx, host)
	if len(ips) == 0 {
		return err
	}
	args := append([]string{"-a", pfAnchor, "-t", apiTable, "-T", "add"}, ips...)
	cmd := pfctlCmd(args...)
	bounded := exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	if out, perr := bounded.CombinedOutput(); perr != nil {
		return fmt.Errorf("exempt %v: %v (%s)", ips, perr, strings.TrimSpace(string(out)))
	}
	if !live {
		// Worth a line — the exemption is in place, but it was built from an
		// address nobody has confirmed lately, so a later "connection refused"
		// has a suspect. Not an error: the table is exempted either way, and
		// returning one would make every caller's log say the opposite.
		log.Printf("kill switch: exempted %s from cache %v (live lookup failed: %v)", host, ips, err)
	}
	return nil
}

// UnloadKillSwitch flushes the anchor and releases our PF enable reference.
// Safe to call when nothing is loaded; uses sudo when invoked unprivileged
// (same posture as RestoreDNS).
func UnloadKillSwitch() {
	_, _ = pfctl("-a", pfAnchor, "-F", "all")
	if data, err := os.ReadFile(pfTokenPath()); err == nil {
		if token := strings.TrimSpace(string(data)); token != "" {
			_, _ = pfctl("-X", token)
		}
		_ = os.Remove(pfTokenPath())
	}
}

// KillSwitchLoaded reports whether this machine currently has our anchor
// armed. Token-file based so unprivileged `mole status` can answer it.
func KillSwitchLoaded() bool {
	_, err := os.Stat(pfTokenPath())
	return err == nil
}

func pfctl(args ...string) (string, error) {
	out, err := pfctlCmd(args...).CombinedOutput()
	return string(out), err
}

// pfctlCmd builds the invocation without running it, for the callers that need
// the exit status rather than the output — `pfctl -T test` answers in the
// status code and prints the same "0/1 addresses match." either way.
func pfctlCmd(args ...string) *exec.Cmd {
	if os.Geteuid() == 0 {
		return exec.Command("pfctl", args...)
	}
	return exec.Command("sudo", append([]string{"pfctl"}, args...)...)
}
