package config

import (
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/LeonTing1010/mole/core"
	"github.com/LeonTing1010/mole/utils"
)

// ParseOutbound shares URI and TLS-pin validation with the normal client while
// allowing deployment probes to run without a TUN, DNS changes or selectors.
func ParseOutbound(uri string) (*OutboundConfig, error) { return parseServerURL(uri) }

// Build assembles a sing-box config from a server URI (vless://, hy2://).
func Build(serverURI string) (*SingboxConfig, error) {
	return BuildWithStandby(serverURI, "")
}

// BuildWithStandby is Build plus a second, co-hosted transport for the route to
// fall back to.
//
// standbyURI is a vless://…security=reality link, normally pointing at the very
// same box as serverURI: hysteria2 owns UDP/443 and REALITY owns TCP/443, so one
// VPS carries both legs and the failover costs no second server. Empty means the
// historical single-leg layout, byte for byte.
//
// The point of pre-materializing it — rather than rebuilding the config when it
// is needed — is that swapping transports must not restart sing-box. A restart
// rebuilds the TUN, which is a real outage, and it would land in the middle of
// the exact episode the standby leg exists to survive.
func BuildWithStandby(serverURI, standbyURI string) (*SingboxConfig, error) {
	outbound, err := parseServerURL(serverURI)
	if err != nil {
		return nil, err
	}

	// Direct (China) DNS resolver, pinned to AliDNS for best CDN-edge
	// localization (NOT lowest query latency — see PreferredDirectDNS).
	bestDNS := utils.PreferredDirectDNS()

	dnsServers := []DNSServer{
		// Direct DNS - AliDNS, chosen for best CDN-edge localization (not latency)
		{Type: "udp", Server: bestDNS, Tag: "dns-direct"},
		// Remote DNS via proxy. Kept as `final` so non-A/AAAA queries
		// (PTR, TXT, MX, ...) for foreign names don't hit a censored upstream.
		{Type: "tls", Server: "1.1.1.1", Tag: "dns-remote", Detour: "proxy"},
		// FakeIP synthesises 198.18.x.x for A queries so foreign-name resolution
		// never depends on a reachable upstream. The actual IP is recovered from
		// the sniffed SNI/Host when the client connects.
		{Type: "fakeip", Tag: "dns-fake", Inet4Range: "198.18.0.0/15"},
	}

	// Custom rules drive BOTH route and DNS. A domain flagged outbound:direct
	// must resolve to its REAL IP — not a fake-ip — or clients behind a NAT that
	// can't take part in fake-ip reverse-mapping (e.g. an emulator) get a dead
	// 198.18.x.x and hang. So direct domain suffixes are pulled into a dns-direct
	// rule below as well. Single source of truth: custom-rules.json.
	customRules := loadCustomRules()

	// Built-in curated direct rules (e.g. Sunlogin/向日葵) ship with mole as
	// DATA (builtin-rules.json), not as Go literals, so the list of domestic
	// services that should route direct can change without recompiling. Same
	// schema as custom-rules.json; custom rules are inserted AFTER builtin so a
	// user can override any built-in entry.
	builtinRules := loadBuiltinRules()

	// Combined direct-domain sources for DNS resolution (built-in + user).
	directSources := append(append([]RouteRule{}, builtinRules...), customRules...)

	dnsRules := []DNSRule{}
	if outbound.Server != "" && net.ParseIP(outbound.Server) == nil {
		// VPS hostname uses direct DNS to avoid circular dependency
		dnsRules = append(dnsRules, DNSRule{
			Domain: []string{outbound.Server},
			Server: "dns-direct",
		})
	}
	// Reject HTTPS/SVCB. FakeIP cannot synthesise these record types, and a
	// HTTPS RR with `alpn=h3` would otherwise push the browser onto QUIC
	// (rejected by our route) or ECH paths that bypass SNI sniffing.
	dnsRules = append(dnsRules, DNSRule{
		QueryType: []string{"HTTPS", "SVCB"},
		Action:    "reject",
	})
	// Reverse lookups for RFC 6303 locally-served zones are answered here and
	// never leave the machine. Bonjour/DNS-SD browses every interface subnet
	// (en0's 192.168.1.0/24, and utun4's own 172.19.0.0/28), so these fire a few
	// times an hour forever. The upstream resolver can only ever say NXDOMAIN
	// for them — but the query still costs a UDP round trip, and a single lost
	// packet parks the lookup for the full 10s deadline with no retry, stalling
	// Finder's "Shared" list and AirPlay/AirDrop discovery. When the link is
	// sick they arrive in bursts and bury the real errors in the log. Replying
	// NXDOMAIN locally is behaviourally identical and instant.
	dnsRules = append(dnsRules, DNSRule{
		DomainSuffix: localReverseZones(),
		Action:       "reject",
	})
	// Any remaining (public) PTR must never ride the proxy. These don't match
	// the A/AAAA fakeip rule, so without this they fall to `final: dns-remote`
	// (DoT over the tunnel) and each blocks ~20s when the link is congested —
	// the single biggest source of "everything feels frozen".
	dnsRules = append(dnsRules, DNSRule{
		DomainSuffix: []string{".in-addr.arpa", ".ip6.arpa"},
		Server:       "dns-direct",
	})
	// Chinese TLDs use direct DNS so we get real IPs that geoip-cn can route.
	// (Service domains like qq.com are no longer hardcoded here — they live in
	// builtin-rules.json and flow in via directDomainSuffixes, same as oray.)
	dnsRules = append(dnsRules, DNSRule{
		DomainSuffix: []string{".cn", ".com.cn", ".net.cn", ".org.cn", ".gov.cn", ".edu.cn", ".mil.cn", ".中国"},
		Server:       "dns-direct",
	})
	// Proxy-exception domains: a specific host that must ride the tunnel even
	// though its parent suffix is direct (e.g. appstoreconnect.apple.com under
	// apple.com — the CN GSLB hands the domestic path a crippled Kingsoft CDN
	// console, and idmsa/developer.apple.com get their TLS ClientHello reset on
	// direct). These must resolve to fake-ip so the connection carries the
	// DOMAIN to the proxy and is re-resolved abroad; inheriting the parent's
	// dns-direct rule below would instead pin them to the poisoned domestic IP.
	// Emitted before directDomainSuffixes so the more-specific fake-ip rule wins.
	if suffixes := proxyDomainSuffixes(directSources); len(suffixes) > 0 {
		dnsRules = append(dnsRules, DNSRule{
			DomainSuffix: suffixes,
			Server:       "dns-fake",
		})
	}
	// Same exception, matched by substring for hosts no suffix can reach:
	// sing-box's domain_suffix aligns on label boundaries, so the Mac App Store's
	// per-account store partitions (p46-buy/p47-buy.itunes.apple.com — install
	// authorisation, TLS-reset on the direct path) fall through a
	// `buy.itunes.apple.com` suffix. Emitted as its own rule, never merged with
	// the suffix rule above: sing-box ANDs matcher kinds inside a single rule.
	if keywords := proxyDomainKeywords(directSources); len(keywords) > 0 {
		dnsRules = append(dnsRules, DNSRule{
			DomainKeyword: keywords,
			Server:        "dns-fake",
		})
	}
	// Custom direct domains resolve to real IPs too (see note above) so they
	// route via geoip-cn / explicit ip_cidr rules instead of dying on fake-ip.
	if suffixes := directDomainSuffixes(directSources); len(suffixes) > 0 {
		dnsRules = append(dnsRules, DNSRule{
			DomainSuffix: suffixes,
			Server:       "dns-direct",
		})
	}
	// Known-Chinese domains resolve for real, whatever their TLD. The suffix and
	// allowlist rules above only catch .cn and hand-curated names, so domestic
	// sites on .com (baidu, bilibili, xiaohongshu, aliyun, …) used to fall
	// through to fakeip and get routed abroad — 13× slower and burning the
	// cross-border tunnel, and for sites that geo-police their traffic (the
	// jd.com risk_handler incident) outright broken. geoip-cn cannot save them:
	// by the time routing sees a fake IP the real one was never looked up. This
	// has to be decided at the DNS layer, which is what geosite-cn is for.
	//
	// Skipped entirely when the rule-set isn't on disk, so a failed prefetch
	// degrades to the previous suffix+allowlist behaviour rather than a config
	// sing-box rejects at startup.
	geositeCN := geositeCNPath()
	if geositeCN != "" {
		dnsRules = append(dnsRules, DNSRule{
			RuleSet: []string{"geosite-cn"},
			Server:  "dns-direct",
		})
	}
	// Everything else: synthesise a fake IP. The connection itself decides the
	// outbound, not the DNS answer, so a poisoned/blocked upstream can no
	// longer prevent foreign sites from loading.
	dnsRules = append(dnsRules, DNSRule{
		QueryType: []string{"A", "AAAA"},
		Server:    "dns-fake",
	})

	// Base rules that must always come first: sniff, hijack-dns, reject.
	// These are infrastructure; without sniff first, no later domain rule
	// can see the SNI of an encrypted connection.
	//
	// DNS queries to our direct resolvers must bypass hijack-dns so that
	// mole's own CLI commands (status, etc.) can resolve foreign domains
	// without getting fake-IP (198.18.x.x) answers from the TUN DNS.
	// Outbounds are built up here rather than at the struct literal because two
	// things below need to know whether a REALITY leg actually materialized: a
	// route rule naming an outbound that does not exist is a config sing-box
	// refuses to start on, and the Brutal keeper's forwarder is pointless
	// without the leg it paces.
	proxyOutbounds, hy2GateTarget := buildProxyOutbounds(serverURI, standbyURI, outbound)
	hasReality := false
	for _, o := range proxyOutbounds {
		if o.Tag == core.RealityLegTag {
			hasReality = true
			break
		}
	}

	inbounds := []InboundConfig{
		{
			Type:        "tun",
			Tag:         "tun-in",
			Address:     []string{"172.19.0.1/28"},
			MTU:         9000,
			AutoRoute:   true,
			StrictRoute: true,
			Stack:       "gvisor",
		},
		{
			Type:       "direct",
			Tag:        "dns-in",
			Listen:     "172.19.0.1",
			ListenPort: 53,
		},
	}
	if hasReality {
		// A fixed forwarder onto the node's loopback Brutal keeper. Same port
		// both ends, so the address reads the same on either side of the leg.
		inbounds = append(inbounds, InboundConfig{
			Type:            "direct",
			Tag:             core.BrutalInboundTag,
			Listen:          "127.0.0.1",
			ListenPort:      core.BrutalKeeperPort,
			OverrideAddress: "127.0.0.1",
			OverridePort:    core.BrutalKeeperPort,
		})
		// The same destination over the other leg. Its whole purpose is to be
		// identical to the forwarder above in every respect but the leg it is
		// routed over, so the failover gate can time the two and have the
		// difference mean the transport. See core.LegGatePort.
		if hy2GateTarget != "" {
			inbounds = append(inbounds, InboundConfig{
				Type:            "direct",
				Tag:             core.LegGateInboundTag,
				Listen:          "127.0.0.1",
				ListenPort:      core.LegGatePort,
				OverrideAddress: "127.0.0.1",
				OverridePort:    core.BrutalKeeperPort,
			})
			for _, target := range core.LegEgressTargets() {
				for _, probe := range []struct {
					tag  string
					port int
				}{
					{target.RealityTag, target.RealityPort}, {target.Hy2Tag, target.Hy2Port},
				} {
					inbounds = append(inbounds, InboundConfig{
						Type: "direct", Tag: probe.tag, Listen: "127.0.0.1", ListenPort: probe.port,
						OverrideAddress: target.Host, OverridePort: 443,
					})
				}
			}
		}
	}

	baseRules := []RouteRule{
		{Action: "sniff"},
		{Protocol: "dns", IPCIDR: []string{"223.5.5.5/32", "1.1.1.1/32"}, Outbound: "direct"},
		{Protocol: "dns", Action: "hijack-dns"},
		{Protocol: "quic", Action: "reject"},
	}
	// The Brutal keeper's channel, when there is a REALITY leg to pace. It has
	// to sit here, ahead of the systemRules below, because those send
	// 127.0.0.0/8 straight out `direct` — and the whole point of this one is
	// that a loopback address gets resolved on the far side of the leg.
	if hasReality {
		baseRules = append(baseRules, RouteRule{
			Inbound:  []string{core.BrutalInboundTag},
			Outbound: core.RealityLegTag,
		})
		// Pinned to a ladder member rather than to `proxy`, for the same reason
		// the one above is pinned to the REALITY member: a gate that measured
		// whichever leg the selector happens to be on would be timing the leg
		// it is trying to decide about.
		if hy2GateTarget != "" {
			baseRules = append(baseRules, RouteRule{
				Inbound:  []string{core.LegGateInboundTag},
				Outbound: hy2GateTarget,
			})
			for _, target := range core.LegEgressTargets() {
				baseRules = append(baseRules,
					RouteRule{Inbound: []string{target.RealityTag}, Outbound: core.RealityLegTag},
					RouteRule{Inbound: []string{target.Hy2Tag}, Outbound: hy2GateTarget},
				)
			}
		}
	}

	// System rules: private IPs, DNS resolvers, geoip-cn, catch-all.
	// (Curated domestic-service direct rules like Sunlogin/向日葵 live in
	// builtin-rules.json, not here — see loadBuiltinRules.)
	systemRules := []RouteRule{
		// Keep the public DNS resolvers reachable directly so DNS resolution
		// itself never depends on the VPS being up.
		{IPCIDR: []string{"1.1.1.1/32", "223.5.5.5/32"}, Outbound: "direct"},
		{IPCIDR: []string{"192.168.0.0/16", "127.0.0.0/8"}, Outbound: "direct"},
		// Cloud-provider internal ranges. Never send these out the physical
		// NIC — they're not reachable from a home connection and a stale DNS
		// resolution from Vultr/AWS/GCP internal services would otherwise hang
		// for 5s per connection and poison mole's fresh-dial counters. Route
		// through block so sing-box refuses them immediately.
		{IPCIDR: []string{"10.0.0.0/8", "172.16.0.0/12", "169.254.0.0/16"}, Outbound: "block"},
		// SSH/scp/rsync to the VPS itself must NOT route through the tunnel —
		// that would put three layers of TCP (SSH → REALITY → sing-box server
		// → localhost sshd) on every byte and cut upload throughput in half.
		// The VPS Server field is already the IP we dial; if it's a hostname
		// (DNS resolution) we still keep it as a DNS rule above, but the IP
		// form here is what actually bypasses proxy routing.
	}
	if ip := net.ParseIP(outbound.Server); ip != nil {
		systemRules = append(systemRules, RouteRule{
			IPCIDR:   []string{ip.String() + "/32"},
			Outbound: "direct",
		})
	}
	// Also include the standby VPS IP if different (same host in practice, but
	// harmless when equal and correct when they diverge).
	if standbyURI != "" {
		if sb, err := parseServerURL(standbyURI); err == nil && sb.Server != outbound.Server {
			if ip := net.ParseIP(sb.Server); ip != nil {
				systemRules = append(systemRules, RouteRule{
					IPCIDR:   []string{ip.String() + "/32"},
					Outbound: "direct",
				})
			}
		}
	}
	// Remaining system rules...
	systemRules = append(systemRules,
		RouteRule{IPCIDR: []string{"198.18.0.0/15"}, Outbound: "proxy"},
		RouteRule{RuleSet: []string{"geoip-cn"}, Outbound: "direct"},
		RouteRule{IPCIDR: []string{"0.0.0.0/0"}, Outbound: "proxy"},
	)

	// Load custom rules from ~/.mole/custom-rules.json if exists.
	// Custom rules sit between base rules (sniff etc.) and system rules
	// so they can use the sniffed domain while still respecting FakeIP
	// and geoip-cn fallbacks.
	//
	// 域名规则改走 local rule-set（2026-08-04，见 hotrules.go）：纯「一组域名 → 一个出站」
	// 的规则落到 ~/.mole/rule-sets/*.json，sing-box 监听这些文件、改完即时生效，
	// 于是加一个域名不再需要 `mole down && mole up`（那条路会拆 TUN、卸 kill switch、
	// 断掉所有在途连接）。表达不了的规则（ip_cidr/protocol/action/invert）仍原样内联。
	domainRules := append(append([]RouteRule{}, builtinRules...), customRules...)
	hotByOutbound, hotOrder, inlineRules := SplitHotRules(domainRules)
	if err := DetectHotRuleConflicts(hotByOutbound); err != nil {
		return nil, err
	}
	if err := WriteHotRuleSets(hotByOutbound, hotOrder); err != nil {
		return nil, err
	}
	hotDecls, hotRefs := HotRuleSetRefs(hotOrder)

	var routeRules []RouteRule
	routeRules = append(routeRules, baseRules...)
	routeRules = append(routeRules, inlineRules...)
	routeRules = append(routeRules, hotRefs...)
	routeRules = append(routeRules, systemRules...)
	if hasReality && hy2GateTarget != "" {
		proxyOutbounds = append(proxyOutbounds, flowSelectors()...)
		routeRules = flowRoutes(routeRules)
	}

	// Rule-sets are referenced from local files only — never fetched by sing-box
	// itself, which would put a download on the cold-start critical path.
	// utils.EnsureRuleSets prefetches them at `mole up`.
	ruleSets := []RuleSet{
		{
			Type:   "local",
			Tag:    "geoip-cn",
			Format: "binary",
			Path:   filepath.Join(utils.MoleDir(), "geoip-cn.srs"),
		},
	}
	// Declared only when the file exists, matching the DNS rule above: sing-box
	// FATALs on a rule-set whose path is missing, so an absent geosite-cn must
	// leave no trace in the config at all rather than half of one.
	if geositeCN != "" {
		ruleSets = append(ruleSets, RuleSet{
			Type:   "local",
			Tag:    "geosite-cn",
			Format: "binary",
			Path:   geositeCN,
		})
	}
	ruleSets = append(ruleSets, hotDecls...)

	return &SingboxConfig{
		Log:       LogConfig{Level: "warn"},
		DNS:       DNSConfig{Servers: dnsServers, Rules: dnsRules, Final: "dns-remote", Strategy: "ipv4_only"},
		Inbounds:  inbounds,
		Outbounds: append(proxyOutbounds, OutboundConfig{Type: "direct", Tag: "direct"}, OutboundConfig{Type: "block", Tag: "block"}),
		Route: RouteConfig{
			Rules:               routeRules,
			RuleSet:             ruleSets,
			AutoDetectInterface: true,
			Final:               "proxy",
			DefaultDomainResolver: &DefaultDomainResolver{
				Server: "dns-direct",
			},
		},
		Experimental: &ExperimentalConfig{
			ClashAPI:  &ClashAPIConfig{ExternalController: "127.0.0.1:9090"},
			CacheFile: &CacheFileConfig{Enabled: true, Path: filepath.Join(utils.MoleDir(), "sing-box-cache.db"), StoreFakeIP: true},
		},
	}, nil
}

// Save writes a sing-box config to disk.
func Save(cfg *SingboxConfig, path string) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if cfg.Experimental != nil && cfg.Experimental.CacheFile != nil {
		cache := cfg.Experimental.CacheFile
		if cache.Enabled && cache.Path != "" {
			// The engine otherwise creates a world-readable DNS metadata DB.
			// Do not truncate a live or previously persisted mapping table.
			f, err := os.OpenFile(cache.Path, os.O_CREATE|os.O_WRONLY, 0600)
			if err != nil {
				return fmt.Errorf("prepare private DNS cache: %w", err)
			}
			err = f.Chmod(0600)
			if closeErr := f.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return fmt.Errorf("secure DNS cache: %w", err)
			}
		}
	}
	return os.WriteFile(path, data, 0644)
}

// buildProxyOutbounds returns the proxy outbound(s) for the route to target.
//
// Whenever a Brutal ceiling is in play, it returns a `proxy` selector over a
// LADDER of otherwise-identical hy2 outbounds differing only in their declared
// bandwidth — `proxy-bw-2`, `proxy-bw-5`, … The ladder exists because sing-box
// bakes up_mbps/down_mbps in at config load and the Clash API cannot mutate them;
// pre-materializing each ceiling is the only way to change one without restarting
// sing-box (which would rebuild the TUN = a real outage). Two things ride it:
// the supervisor's clock-driven peak/off-peak flip, and a manual `mole ceiling`
// pin. Both are one Clash API call.
//
// The selector's default member is whichever ceiling applies at build time, so
// the tunnel comes up correct even before the supervisor's first tick. Members
// are lazily dialed, so unselected rungs cost nothing at runtime.
//
// Falls back to a single outbound tagged `proxy` — the historical layout — for
// non-hy2 protocols and for BBR mode (down_mbps<=0), where there is no ceiling
// to ladder. The route always targets `proxy` either way.
// The second return is the ladder member the failover gate measures hy2 on —
// empty when there is no hy2 member to measure. With two legs it is a separate
// selector so the probe can measure the exact rung the scheduler would select.
func buildProxyOutbounds(serverURI, standbyURI string, outbound *OutboundConfig) ([]OutboundConfig, string) {
	// standby is the REALITY member, or nil when there is no second leg. Parsed
	// up front so a malformed standby URI degrades to the single-leg layout
	// instead of taking the whole tunnel down with it — the primary is what the
	// user is trying to bring up.
	var standby *OutboundConfig
	if standbyURI != "" {
		if o, err := parseServerURL(standbyURI); err == nil && o.Type == "vless" {
			o.Tag = core.RealityLegTag
			standby = o
		} else {
			log.Printf("⚠️  standby leg ignored — %q is not a usable vless://…security=reality URI", standbyURI)
		}
	}
	single := func() ([]OutboundConfig, string) {
		outbound.Tag = core.ProxySelectorTag
		// No selector, so nothing to fail over to and nothing for the gate.
		return []OutboundConfig{*outbound}, ""
	}
	// A selector is only worth building when there is something to switch
	// between. With a standby leg there always is, even on a server with no
	// ladder at all, so that path builds a two-member selector rather than
	// falling back to the bare outbound the route would otherwise target.
	selectOver := func(members []string, rungs []OutboundConfig, def string) []OutboundConfig {
		out := make([]OutboundConfig, 1, len(rungs)+2)
		out = append(out, rungs...)
		if standby != nil {
			if outbound.Type == "hysteria2" {
				out = append(out, OutboundConfig{
					Type: "selector", Tag: core.Hy2ProbeSelectorTag,
					Outbounds: append([]string(nil), members...), Default: members[len(members)-1],
				})
			}
			members = append(members, standby.Tag)
			out = append(out, *standby)
		}
		out[0] = OutboundConfig{
			Type: "selector", Tag: core.ProxySelectorTag,
			Outbounds: members, Default: def,
		}
		return out
	}
	if outbound.Type != "hysteria2" {
		if standby == nil {
			return single()
		}
		primary := *outbound
		primary.Tag = "proxy-primary"
		// Not hy2, so the gate has no ladder to measure against REALITY.
		return selectOver([]string{primary.Tag}, []OutboundConfig{primary}, standby.Tag), ""
	}
	u, err := url.Parse(serverURI)
	if err != nil {
		return single()
	}
	q := u.Query()
	peakDown, _ := strconv.Atoi(q.Get("peakdownmbps"))
	peakUp, _ := strconv.Atoi(q.Get("peakupmbps"))
	start, _ := strconv.Atoi(q.Get("peakstart"))
	end, _ := strconv.Atoi(q.Get("peakend"))

	sched := core.BandwidthSchedule{
		OffUp: outbound.UpMbps, OffDown: outbound.DownMbps,
		PeakUp: peakUp, PeakDown: peakDown,
		StartHour: start, EndHour: end,
	}
	rungs := sched.Rungs()
	if len(rungs) < 2 {
		// No ceiling configured, or the ceiling is so low the ladder collapses to
		// one rung — nothing on the ladder to switch between, so keep the simpler
		// shape unless the standby leg gives the selector a reason to exist.
		if standby == nil {
			return single()
		}
		primary := *outbound
		primary.Tag = core.BandwidthRungTag(outbound.DownMbps)
		return selectOver([]string{primary.Tag}, []OutboundConfig{primary}, standby.Tag), core.Hy2ProbeSelectorTag
	}

	members := make([]string, 0, len(rungs))
	ladder := make([]OutboundConfig, 0, len(rungs))
	for _, down := range rungs {
		rung := *outbound
		rung.Tag = core.BandwidthRungTag(down)
		rung.UpMbps = sched.RungUp(down)
		rung.DownMbps = down
		members = append(members, rung.Tag)
		ladder = append(ladder, rung)
	}
	// With a REALITY leg present it is the default: it is the primary, and a
	// tunnel that came up on the ladder would run its first minute — until the
	// supervisor's first tick — on the leg measured failing ten times a day.
	gate := members[len(members)-1]
	if standby != nil {
		return selectOver(members, ladder, standby.Tag), core.Hy2ProbeSelectorTag
	}
	return selectOver(members, ladder, sched.Member(time.Now())), gate
}

func parseServerURL(serverURL string) (*OutboundConfig, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, fmt.Errorf("invalid server URL: %w", err)
	}
	switch u.Scheme {
	case "vless":
		return parseVLESS(u)
	case "hy2", "hysteria2":
		return parseHysteria2(u)
	}
	return nil, fmt.Errorf("unsupported protocol: %s", u.Scheme)
}

// defaultRealityDest is the handshake target assumed when a vless:// link omits
// ?sni=. It must stay in step with cmd's constant of the same name; the two are
// separate because config must not import cmd.
const defaultRealityDest = "www.apple.com"

func parseVLESS(u *url.URL) (*OutboundConfig, error) {
	port, _ := strconv.Atoi(u.Port())
	if port == 0 {
		port = 443
	}
	o := &OutboundConfig{
		Type: "vless", Tag: "proxy",
		Server: u.Hostname(), ServerPort: port,
		UUID: u.User.Username(),
	}
	// TCP Fast Open sends the first payload inside the SYN, removing one RTT
	// from every new connection. The node already runs net.ipv4.tcp_fastopen=3
	// (see cmd's boot script); without this flag the client never asks for it.
	//
	// A/B tested 2026-10-14 (spec.md D4) on the hypothesis that a data-carrying
	// SYN is what access networks drop when they shape :443. It is not: with
	// TFO off a QoS window still dropped new connections at the same rate
	// (130 "dial tcp VPS:443: i/o timeout" in 30min vs 65 in 14min with it on).
	// The shaper discards plain SYNs too, so the RTT this saves is kept.
	//
	// Multiplex stays opt-in: older nodes did not explicitly enable it on the
	// inbound, and reject requests via the deprecated global mux handler.
	// Vision itself supports smux when BOTH ends enable it (verified with
	// sing-box 1.14.3 on 2026-10-20). A small native session pool reuses warm
	// TCP/REALITY connections instead of exposing every new flow to SYN loss.
	o.TCPFastOpen = true
	q := u.Query()
	if protocol := q.Get("mux"); protocol != "" {
		if protocol != "smux" {
			return nil, fmt.Errorf("unsupported VLESS multiplex protocol %q (want smux)", protocol)
		}
		// One active stream can open another session: the 4/4 pool helped
		// fresh flows but constrained eight-flow downloads on the measured
		// path. 8/1 retains idle-session reuse without that concurrency clamp.
		o.Multiplex = &MultiplexConfig{Enabled: true, Protocol: protocol, MaxConnections: 8, MinStreams: 1}
	}
	if flow := q.Get("flow"); flow != "" {
		o.Flow = flow
	}
	if network := q.Get("type"); network != "" && network != "tcp" {
		// sing-box treats an empty network as TCP; spelling it out would be
		// rejected for outbounds that don't take the field.
		o.Network = network
	}
	if sec := q.Get("security"); sec == "tls" || sec == "reality" {
		o.TLS = &TLSConfig{Enabled: true, ServerName: q.Get("sni"), Insecure: q.Get("allowInsecure") == "1"}
	}
	if q.Get("security") == "reality" && o.TLS != nil {
		// REALITY replaces certificate trust outright: the certificate the
		// client sees is the borrowed site's real one, and the proof that the
		// peer is the intended server is the ?pbk= keypair, not the chain. So
		// insecure must be off — it would mean nothing here anyway — and the
		// pin the hysteria2 leg needs has no counterpart.
		o.TLS.Insecure = false
		o.TLS.Reality = &RealityConfig{
			Enabled:   true,
			PublicKey: q.Get("pbk"),
			ShortID:   q.Get("sid"),
		}
		// A forged browser hello is mandatory, not decorative: REALITY hides its
		// authentication inside the ClientHello, and Go's native hello is one no
		// browser produces. Default to Chrome, which is what the borrowed sites
		// see most of.
		fp := q.Get("fp")
		if fp == "" {
			fp = "chrome"
		}
		o.TLS.UTLS = &UTLSConfig{Enabled: true, Fingerprint: fp}
		// The handshake is a TLS 1.3 one to the borrowed site, so the SNI is
		// that site's name and there is no sensible fallback if the link omits
		// it — an empty server_name would send no SNI at all and the borrowed
		// server would answer with a default vhost the client then rejects.
		if o.TLS.ServerName == "" {
			o.TLS.ServerName = defaultRealityDest
		}
	}
	if o.Network == "ws" {
		o.Transport = &TransportConfig{Type: "ws", Path: q.Get("path")}
		if h := q.Get("host"); h != "" {
			o.Transport.Headers = map[string]string{"Host": h}
		}
	}
	return o, nil
}

func parseHysteria2(u *url.URL) (*OutboundConfig, error) {
	host := strings.Trim(u.Hostname(), "[]")
	port, _ := strconv.Atoi(u.Port())
	if port == 0 {
		port = 443
	}
	o := &OutboundConfig{
		Type: "hysteria2", Tag: "proxy",
		Server: host, ServerPort: port,
		Password: u.User.Username(),
	}
	q := u.Query()
	if sni := q.Get("sni"); sni != "" {
		o.TLS = &TLSConfig{Enabled: true, ServerName: sni}
	}
	if q.Get("insecure") == "1" {
		if o.TLS == nil {
			o.TLS = &TLSConfig{Enabled: true}
		}
		o.TLS.Insecure = true
	}

	// Hysteria2 congestion control: declaring up/down bandwidth switches the
	// client from loss-sensitive BBR to Brutal, which holds a fixed send rate
	// straight through packet loss. Without it a lossy international path
	// collapses throughput to tens of KB/s. These are a ceiling, not a target —
	// keep them at or below the real link speed, because Brutal floods the path
	// (and gets slower) if set too high. Defaults suit a ~100Mbit line; override
	// per-server with ?upmbps=&downmbps= in the URI.
	//
	// A negative value (?upmbps=-1) disables Brutal and falls back to the adaptive
	// BBR-style control. That's the right call when the link speed swings by time
	// of day (e.g. a cross-border path that does 30Mbit off-peak but 2.5Mbit at
	// night) — no single fixed Brutal rate fits, and a too-high one floods the
	// path into repeated collapse-to-zero. Leaving up/down at 0 omits the fields
	// from the outbound, so sing-box uses BBR.
	// This fallback is shared with cmd's Server.ladder() through core. It used to
	// be a local 20/50 while the server list resolved the same "unset" to 0, and
	// that disagreement is what let a freshly deployed node ladder from a ceiling
	// the supervisor never knew about — pinned to the top rung with nothing able
	// to lower it.
	upMbps, downMbps := core.DefaultUpMbps, core.DefaultDownMbps
	up, _ := strconv.Atoi(q.Get("upmbps"))
	down, _ := strconv.Atoi(q.Get("downmbps"))
	switch {
	case up < 0 || down < 0:
		upMbps, downMbps = 0, 0 // disable Brutal → BBR
	default:
		if up > 0 {
			upMbps = up
		}
		if down > 0 {
			downMbps = down
		}
	}
	o.UpMbps = upMbps
	o.DownMbps = downMbps

	// Conservative UDP packet defaults. These do not configure the VPS NIC MTU
	// or the Hysteria server's packet size. Compare PMTUD on this path before
	// changing either client setting; the server has no top-level mtu option.
	o.InitialPacketSize = 1200
	o.DisablePathMTU = true

	// Salamander obfuscation. Raw QUIC/443 to a foreign IP is easily
	// fingerprinted and throttled by the GFW; obfs wraps it so the path stays
	// stable at peak hours. Inert unless the URI carries ?obfs= AND the server
	// is configured with the same type/password. Enable per-server via
	// ?obfs=salamander&obfs-password=xxx in the URI.
	if obfsType := q.Get("obfs"); obfsType != "" {
		o.Obfs = &Hy2Obfs{Type: obfsType, Password: q.Get("obfs-password")}
	}

	// Port hopping. ?ports=20000-50000 is the server-facing spelling (it matches
	// hysteria's own `listen:` range syntax and the firewall rule); sing-box
	// wants colons, so translate rather than making the operator remember two
	// formats for one number.
	//
	// Silently ignoring a malformed range would be the worst outcome — the
	// tunnel would look configured for hopping and quietly never hop — so an
	// unparseable value simply leaves hopping off, which is the safe shape and
	// is what Validate can then be asked about.
	if ports := q.Get("ports"); ports != "" {
		if lo, hi, ok := parsePortRange(ports); ok {
			o.ServerPorts = []string{fmt.Sprintf("%d:%d", lo, hi)}
			hop := 30
			if n, err := strconv.Atoi(q.Get("hop")); err == nil && n >= 5 {
				hop = n // hysteria refuses anything under 5s
			}
			o.HopInterval = fmt.Sprintf("%ds", hop)
		}
	}

	// Pinned server certificate, base64 of the leaf PEM. Deliberately parsed
	// last so it can overrule ?insecure=1: a pin is strictly stronger than any
	// verification the flag could have skipped, and a URI carrying both is a
	// stale link being upgraded, not a request to keep trusting anything.
	if pin := q.Get("pinnedcert"); pin != "" {
		if pem := decodeB64(pin); pem != "" {
			if o.TLS == nil {
				o.TLS = &TLSConfig{Enabled: true}
			}
			o.TLS.Certificate = strings.Split(pem, "\n")
			o.TLS.Insecure = false
		}
	}

	return o, nil
}

// decodeB64 accepts the URL-safe unpadded form Server.URI emits and the padded
// standard form a hand-pasted link may carry. Returns "" when the payload isn't
// decodable, so a corrupt pin degrades to "no pin" — which Validate then warns
// about loudly rather than failing on, since servers provisioned before pinning
// existed have no cert to offer and must keep working.
func decodeB64(s string) string {
	for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) > 0 {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

// geositeCNPath returns the on-disk geosite-cn rule-set, or "" when it isn't
// usable. Callers must emit NOTHING geosite-related when it returns "": sing-box
// treats a missing rule-set path as fatal, so referencing one we don't have
// would turn a best-effort prefetch failure into a tunnel that won't start.
//
// This is the guard that makes re-adding geosite-cn safe after it was dropped in
// 2e9a278 "to avoid download failures" — the capability is back, the failure
// mode isn't. Size is checked, not just existence, because downloadRuleSet
// writes to a .tmp and renames, but an interrupted earlier version could leave a
// zero-byte file behind.
func geositeCNPath() string {
	path := filepath.Join(utils.MoleDir(), "geosite-cn.srs")
	if st, err := os.Stat(path); err != nil || st.Size() == 0 {
		return ""
	}
	return path
}

//go:embed builtin-rules.json
var builtinRulesFS embed.FS

// builtinRulesCachePath is where utils.EnsureBuiltinRules caches the fetched
// list (~/.mole/builtin-rules.json).
func builtinRulesCachePath() string {
	return filepath.Join(utils.MoleDir(), "builtin-rules.json")
}

// loadBuiltinRules reads the curated, version-stable direct-domain list that
// ships with mole (e.g. Sunlogin/向日葵, Tencent/WeChat). Keeping these out of
// parser.go means adding/removing a "domestic service that should route direct"
// is a data edit, not a recompile. Schema is identical to custom-rules.json.
//
// Precedence: the startup prefetch (utils.EnsureBuiltinRules) caches the latest
// list from the source repo to ~/.mole/builtin-rules.json; we prefer that cached
// copy so upstream updates land without recompiling. If it's missing or invalid
// we fall back to the embedded copy shipped in the binary, so a fetch failure
// never breaks startup.
func loadBuiltinRules() []RouteRule {
	if data, err := os.ReadFile(builtinRulesCachePath()); err == nil {
		var rules []RouteRule
		if err := json.Unmarshal(data, &rules); err == nil {
			return rules
		}
		log.Printf("⚠️  cached builtin-rules.json invalid, falling back to embedded copy")
	}
	data, err := builtinRulesFS.ReadFile("builtin-rules.json")
	if err != nil {
		log.Printf("⚠️  failed to read embedded builtin-rules.json: %v", err)
		return nil
	}
	var rules []RouteRule
	if err := json.Unmarshal(data, &rules); err != nil {
		log.Printf("⚠️  failed to parse embedded builtin-rules.json: %v", err)
		return nil
	}
	return rules
}

// loadCustomRules reads user-defined routing rules from ~/.mole/custom-rules.json.
// The file format: [{"domain_suffix": ["example.com"], "outbound": "direct"}, ...]
func loadCustomRules() []RouteRule {
	path := utils.CustomRulesPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var rules []RouteRule
	if err := json.Unmarshal(data, &rules); err != nil {
		log.Printf("⚠️  failed to parse custom rules from %s: %v", path, err)
		return nil
	}
	return rules
}

// localReverseZones lists the reverse-DNS zones RFC 6303 says a resolver must
// answer itself: private IPv4 space, link-local, loopback, and the IPv6 ULA and
// link-local ranges. Every suffix keeps its leading dot so matching stays on a
// label boundary — ".10.in-addr.arpa" must not swallow "110.in-addr.arpa",
// which is public address space.
func localReverseZones() []string {
	zones := []string{
		".10.in-addr.arpa",      // 10.0.0.0/8
		".168.192.in-addr.arpa", // 192.168.0.0/16
		".254.169.in-addr.arpa", // 169.254.0.0/16 link-local
		".127.in-addr.arpa",     // loopback
		".0.in-addr.arpa",       // "this" network
		".d.f.ip6.arpa",         // fd00::/8 unique-local
		".8.e.f.ip6.arpa",       // fe80::/10 link-local
		".9.e.f.ip6.arpa",
		".a.e.f.ip6.arpa",
		".b.e.f.ip6.arpa",
	}
	for i := 16; i <= 31; i++ { // 172.16.0.0/12
		zones = append(zones, fmt.Sprintf(".%d.172.in-addr.arpa", i))
	}
	return zones
}

// directDomainSuffixes collects every domain_suffix from custom rules whose
// outbound is "direct", de-duplicated. These resolve via dns-direct (real IPs)
// so direct routing works for NAT'd clients that can't use fake-ip.
func directDomainSuffixes(rules []RouteRule) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rules {
		if r.Outbound != "direct" {
			continue
		}
		for _, s := range r.DomainSuffix {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}

// proxyDomainSuffixes collects domain suffixes from rules explicitly routed to
// the proxy. These are the mirror of directDomainSuffixes: a more-specific host
// (e.g. appstoreconnect.apple.com) that must ride the tunnel despite living
// under a direct parent suffix. They resolve to fake-ip so the connection
// carries the domain to the proxy for re-resolution abroad — see the dns-fake
// rule in Build, emitted before the dns-direct suffix rule so this wins.
func proxyDomainSuffixes(rules []RouteRule) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rules {
		if r.Outbound != "proxy" {
			continue
		}
		for _, s := range r.DomainSuffix {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}

// proxyDomainKeywords is proxyDomainSuffixes' substring twin: keywords from
// rules explicitly routed to the proxy, for hosts whose blocked label sits in
// the middle of the name (pNN-buy.itunes.apple.com) and so cannot be expressed
// as a dot-aligned suffix.
func proxyDomainKeywords(rules []RouteRule) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rules {
		if r.Outbound != "proxy" {
			continue
		}
		for _, k := range r.DomainKeyword {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	return out
}

// Validate checks that every rule-set referenced by a route or DNS rule is a
// declared, local, present file — never fetched by sing-box on the cold-start
// critical path.
//
// DNS rules MAY reference rule_set (e.g. geosite-cn routes known-Chinese
// domains to dns-direct): that is what enables real-IP resolution for domestic
// sites on .com TLDs, which no hand-curated domain_suffix list can cover. What
// must hold is that the referenced rule-set is declared and on disk, so a
// missing file degrades gracefully rather than becoming a fatal sing-box
// startup error (sing-box treats a dangling rule-set as fatal).
func Validate(cfg *SingboxConfig) error {
	// Declared rule-sets must be local and present on disk.
	declared := map[string]bool{}
	for _, rs := range cfg.Route.RuleSet {
		if rs.Type == "remote" {
			return fmt.Errorf("config references remote rule-set %s - should use local only", rs.Tag)
		}
		if rs.Type == "local" && rs.Path != "" {
			st, err := os.Stat(rs.Path)
			if err != nil || st.Size() == 0 {
				return fmt.Errorf("rule-set %s references non-existent or empty file: %s", rs.Tag, rs.Path)
			}
		}
		declared[rs.Tag] = true
	}

	// Every rule_set a DNS rule references must be a declared rule-set.
	for _, rule := range cfg.DNS.Rules {
		for _, rs := range rule.RuleSet {
			if !declared[rs] {
				return fmt.Errorf("DNS rule references undeclared rule-set %s", rs)
			}
		}
	}

	// Every rule_set a route rule references must be a declared rule-set.
	for _, rule := range cfg.Route.Rules {
		for _, rs := range rule.RuleSet {
			if !declared[rs] {
				return fmt.Errorf("route rule references undeclared rule-set %s", rs)
			}
		}
	}

	// An unverified proxy TLS session is a live MITM hole, not a style issue:
	// hysteria2 authenticates with a password sent *inside* that session, so a
	// peer we never verified walks away with the password and the plaintext.
	// This warns instead of failing because servers provisioned before pinning
	// existed genuinely have no cert to pin, and refusing to start would strand
	// the user offline. Re-provision to clear it (`mole server deploy`).
	if insecureProxyTLS(cfg) {
		log.Printf("⚠️  proxy TLS is unverified (no pinned certificate) — the tunnel password is exposed to any on-path attacker; re-provision with `mole server deploy` to pin it")
	}

	return nil
}

// insecureProxyTLS reports whether any proxy outbound skips verification
// without a pin standing in for it.
func insecureProxyTLS(cfg *SingboxConfig) bool {
	for _, o := range cfg.Outbounds {
		if o.TLS != nil && o.TLS.Insecure && len(o.TLS.Certificate) == 0 {
			return true
		}
	}
	return false
}

// parsePortRange reads "20000-50000" (and the colon form sing-box uses) into an
// inclusive pair. Returns ok=false for anything that isn't a sane UDP range, so
// a typo turns port hopping off rather than pointing the tunnel at ports the
// server never agreed to answer on.
func parsePortRange(s string) (lo, hi int, ok bool) {
	sep := strings.IndexAny(s, "-:")
	if sep <= 0 {
		return 0, 0, false
	}
	lo, err1 := strconv.Atoi(strings.TrimSpace(s[:sep]))
	hi, err2 := strconv.Atoi(strings.TrimSpace(s[sep+1:]))
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	if lo < 1 || hi > 65535 || lo >= hi {
		return 0, 0, false
	}
	return lo, hi, true
}
