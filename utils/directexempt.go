package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The direct-exemption table closes a hole between mole's two halves that had
// been open long enough for nobody to remember opening it.
//
// custom-rules.json can send a domain out the PHYSICAL interface on purpose.
// Three reasons have accumulated: a 46MB `npm publish` PUT that the tunnel's
// Brutal ceiling throttles to ~44KB/s until npm's fetch-timeout kills it;
// Cloudflare challenge pages that never clear for a datacenter IP but pass
// instantly from a residential one (upwork); and CN-adjacent services that are
// simply faster from here. Those rules say "outbound": "direct" and sing-box
// obeys them.
//
// The kill switch, written separately, passes exactly three things on a
// physical interface: the VPS endpoints, local/DHCP infrastructure, Apple's
// 17/8, and Chinese IPv4 space. Everything else is `block drop` — silent, no
// RST. A direct rule pointing at a foreign address therefore routes the
// connection straight into our own firewall, where it dies without a log line
// on either side.
//
// Measured 2026-09-16: every foreign domain carrying "outbound": "direct" —
// npmjs.com, npmjs.org, upwork.com, codebuddy.ai — was completely unreachable,
// while every foreign domain that rode the tunnel was fine. The symptom is
// deceptive: the TUN answers the SYN instantly, so TCP looks connected and it
// is the TLS handshake that hangs until it times out.
//
// So: a table the daemon fills with the addresses those rules actually dial.
// Two properties it must have, both learned the hard way (see ExemptDirectDest):
// the addresses come from what sing-box DIALED, not from what we guessed the
// domain resolves to; and an entry is only ever written for a host that matches
// a rule someone wrote by hand in custom-rules.json.
const directTable = "mole_direct4"

// directExemptPath is the on-disk union of what each direct-ruled host has been
// seen to dial. Re-seeded into the table at every arm so the learning cost is
// paid once per address, not once per `mole up`.
func directExemptPath() string { return filepath.Join(MoleDir(), "direct-exempt.json") }

// DirectDomainSet is the domain half of the "outbound": "direct" rules, in
// sing-box's own two matcher flavours. Built by config, consumed here and by
// the daemon's learner — it is the gate that keeps the table tied to a written
// rule instead of to whatever sing-box happened to route direct.
type DirectDomainSet struct {
	Suffixes []string
	Keywords []string
}

// Match reports whether host is covered by one of the direct rules.
//
// domain_suffix aligns on label boundaries, which is the whole reason this is
// not strings.HasSuffix: sing-box does not match `p47-buy.itunes.apple.com`
// against `buy.itunes.apple.com`, and a check that did would exempt addresses
// for a host the router never sends direct. domain_keyword is a plain
// substring, deliberately — it is the escape hatch for exactly those hosts.
func (d DirectDomainSet) Match(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" {
		return false
	}
	for _, s := range d.Suffixes {
		s = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "."))
		if s == "" {
			continue
		}
		if host == s || strings.HasSuffix(host, "."+s) {
			return true
		}
	}
	for _, k := range d.Keywords {
		k = strings.ToLower(strings.TrimSpace(k))
		if k != "" && strings.Contains(host, k) {
			return true
		}
	}
	return false
}

// Empty says there are no direct domain rules at all, so there is nothing to
// learn and the daemon can skip the whole mechanism.
func (d DirectDomainSet) Empty() bool { return len(d.Suffixes)+len(d.Keywords) == 0 }

var directExemptMu sync.Mutex

type directExemptEntry struct {
	IPs []string  `json:"ips"`
	At  time.Time `json:"at"`
}

type directExemptStore map[string]directExemptEntry

func readDirectExemptStore() directExemptStore {
	data, err := os.ReadFile(directExemptPath())
	if err != nil {
		return directExemptStore{}
	}
	var s directExemptStore
	if err := json.Unmarshal(data, &s); err != nil {
		// A corrupt cache costs the next few connections their warm start, not
		// correctness — the learner refills it. Louder than a silent reset
		// because "npm was slow again this morning" is otherwise unexplainable.
		log.Printf("kill switch: direct exemption cache unreadable (%v) — relearning", err)
		return directExemptStore{}
	}
	if s == nil {
		return directExemptStore{}
	}
	return s
}

func writeDirectExemptStore(s directExemptStore) {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	path := directExemptPath()
	if err := os.WriteFile(path, data, 0644); err != nil {
		return
	}
	ChownToMoleDirOwner(path)
}

// SeedDirectExemptions reloads the learned addresses into the PF table and
// prunes anything the rules no longer claim. For `mole up`, where the anchor
// has just been (re)loaded and the table is empty.
//
// Replaying matters because the table is flushed with the anchor: without it
// every address would have to be relearned — at the cost of one dropped
// connection each — on every single start.
func SeedDirectExemptions(set DirectDomainSet) {
	if !KillSwitchLoaded() {
		return
	}
	directExemptMu.Lock()
	kept, ips, _ := pruneDirectExemptStoreLocked(set)
	directExemptMu.Unlock()

	// Flush first so a pruned address is actually gone; -T replace would do it
	// in one call but reports nothing useful when the address list is empty,
	// which is the normal state on a fresh install.
	if _, err := pfctl("-a", pfAnchor, "-t", directTable, "-T", "flush"); err != nil {
		return
	}
	if len(ips) == 0 {
		return
	}
	args := append([]string{"-a", pfAnchor, "-t", directTable, "-T", "add"}, ips...)
	if out, err := pfctl(args...); err != nil {
		log.Printf("kill switch: direct exemptions not restored: %v (%s)", err, strings.TrimSpace(out))
		return
	}
	log.Printf("kill switch: %d direct exemption(s) restored across %d host(s)", len(ips), len(kept))
}

// PruneDirectExemptions closes the holes whose rule is gone, and touches PF
// only when there is one to close. Returns the hosts it dropped.
//
// Separate from SeedDirectExemptions because of who calls it: `mole reload`
// runs unprivileged and advertises "no sudo", so it must not reach for pfctl on
// the ordinary reload where a domain was ADDED — the daemon picks those up by
// watching what gets dialed. Removing a domain is the case that genuinely needs
// the anchor edited, and a sudo prompt there is the honest cost of shutting a
// hole in a fail-closed ruleset. Everything else stays free.
func PruneDirectExemptions(set DirectDomainSet) []string {
	if !KillSwitchLoaded() {
		return nil
	}
	directExemptMu.Lock()
	_, ips, dropped := pruneDirectExemptStoreLocked(set)
	directExemptMu.Unlock()
	if len(dropped) == 0 {
		return nil
	}
	if _, err := pfctl("-a", pfAnchor, "-t", directTable, "-T", "flush"); err != nil {
		log.Printf("kill switch: direct exemptions for %v not withdrawn: %v", dropped, err)
		return nil
	}
	if len(ips) > 0 {
		args := append([]string{"-a", pfAnchor, "-t", directTable, "-T", "add"}, ips...)
		if out, err := pfctl(args...); err != nil {
			log.Printf("kill switch: direct exemptions not restored after prune: %v (%s)", err, strings.TrimSpace(out))
		}
	}
	return dropped
}

// pruneDirectExemptStoreLocked rewrites the store to only the hosts set still
// claims, and reports the surviving addresses and the hosts it dropped.
// Caller holds directExemptMu.
func pruneDirectExemptStoreLocked(set DirectDomainSet) (kept directExemptStore, ips, dropped []string) {
	store := readDirectExemptStore()
	kept = directExemptStore{}
	for host, e := range store {
		if !set.Match(host) {
			dropped = append(dropped, host)
			continue
		}
		kept[host] = e
		ips = append(ips, e.IPs...)
	}
	sort.Strings(dropped)
	if len(dropped) > 0 {
		writeDirectExemptStore(kept)
	}
	return kept, dedupeSorted(ips), dropped
}

// ExemptDirectDest opens the physical interface to one address that a direct
// rule sent there, and to every sibling that address's host currently resolves
// to.
//
// host is checked against the rules by the caller; ip is what sing-box actually
// dialed. Both halves are load-bearing:
//
// Pre-resolving the domain and exempting the answer is NOT enough, which is why
// this takes a dialed address at all. These are anycast CDN names: asking
// 223.5.5.5 for npmjs.org returns twelve addresses out of a pool that rotates,
// and on 2026-09-16 the address sing-box dialed for registry.npmjs.org —
// 104.16.0.35 — was in none of the twelve a lookup one second earlier had
// returned. A table built from lookups alone would have kept missing it.
//
// Resolving anyway and adding the whole pool is what keeps the cost to one
// stalled connection instead of one per address: the next rotation onto a
// sibling is already exempted. Combined with the on-disk union, a host goes
// cold exactly once, ever.
func ExemptDirectDest(host, ip string) error {
	if !KillSwitchLoaded() {
		return nil
	}
	if net.ParseIP(ip) == nil || net.ParseIP(ip).To4() == nil {
		return nil // the table and the block rules it opens are both IPv4
	}
	if alreadyPassed(ip) {
		return nil
	}

	candidates := []string{ip}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Failure is fine and common (the lookup rides the same dark link that is
	// making this call happen). The dialed address alone still unblocks the
	// retry; the siblings get picked up the next time one of them is dialed.
	if pool, err := ResolveDirect(ctx, host); err == nil {
		candidates = append(candidates, pool...)
	}

	var add []string
	for _, c := range dedupeSorted(candidates) {
		if !alreadyPassed(c) {
			add = append(add, c)
		}
	}
	if len(add) == 0 {
		return nil
	}
	args := append([]string{"-a", pfAnchor, "-t", directTable, "-T", "add"}, add...)
	if out, err := pfctl(args...); err != nil {
		return fmt.Errorf("exempt %s %v: %v (%s)", host, add, err, strings.TrimSpace(out))
	}

	directExemptMu.Lock()
	store := readDirectExemptStore()
	store[host] = directExemptEntry{IPs: dedupeSorted(append(store[host].IPs, add...)), At: time.Now()}
	writeDirectExemptStore(store)
	directExemptMu.Unlock()

	// One line per newly-opened address, on purpose. This is the only record
	// that a hole was punched in a fail-closed ruleset, and the rule that
	// justified it lives in a file nobody reads at 3am.
	log.Printf("kill switch: %s dialed %s direct — exempting %v", host, ip, add)
	return nil
}

// alreadyPassed reports whether the armed ruleset would already let ip out of a
// physical interface, so the learner does not grow the table with addresses
// that never needed it — which, for the CN domains that make up most of the
// direct rules, is all of them.
//
// The CN and direct memberships are asked of PF itself rather than re-derived
// from cn-cidr.txt. The table loaded in the kernel is the thing that decides
// whether a packet leaves; a second reading of the file it was built from is
// only a guess about that, and the two drift the moment a refresh lands without
// a re-arm.
func alreadyPassed(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return true // not an address we can exempt; treat as handled
	}
	if parsed.IsLoopback() || parsed.IsPrivate() || parsed.IsLinkLocalUnicast() ||
		parsed.IsLinkLocalMulticast() || parsed.IsMulticast() || parsed.IsUnspecified() {
		return true
	}
	for _, cidr := range literalPassRanges {
		if _, n, err := net.ParseCIDR(cidr); err == nil && n.Contains(parsed) {
			return true
		}
	}
	for _, table := range []string{"mole_cn4", directTable, apiTable} {
		if pfTableHas(table, ip) {
			return true
		}
	}
	return false
}

// pfTableHas asks the kernel whether a table contains an address. pfctl exits 0
// on a match and 2 on a miss; anything else (most often 255 — the anchor is not
// loaded) is not an answer, and reporting "absent" there is the safe reading:
// the caller's next step is to add the address, which is a no-op if it is
// already there and a failed pfctl if the anchor is truly gone.
func pfTableHas(table, ip string) bool {
	cmd := pfctlCmd("-a", pfAnchor, "-t", table, "-T", "test", ip)
	if err := cmd.Run(); err == nil {
		return true
	}
	return false
}

func dedupeSorted(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
