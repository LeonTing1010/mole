package config

import (
	"encoding/json"
	"embed"
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

// Build assembles a sing-box config from a server URI (vless://, hy2://).
func Build(serverURI string) (*SingboxConfig, error) {
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
	// Reverse-DNS (PTR) lookups must never ride the proxy. mDNS/Bonjour floods
	// *.in-addr.arpa for LAN addresses (192.168/172.16/10.x); these don't match
	// the A/AAAA fakeip rule, so without this they fall to `final: dns-remote`
	// (DoT over the tunnel) and each blocks ~20s when the link is congested —
	// the single biggest source of "everything feels frozen". Direct resolver
	// answers real PTRs fast and returns a quick NXDOMAIN for private ones.
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
	baseRules := []RouteRule{
		{Action: "sniff"},
		{Protocol: "dns", IPCIDR: []string{"223.5.5.5/32", "1.1.1.1/32"}, Outbound: "direct"},
		{Protocol: "dns", Action: "hijack-dns"},
		{Protocol: "quic", Action: "reject"},
	}

	// System rules: private IPs, DNS resolvers, geoip-cn, catch-all.
	// (Curated domestic-service direct rules like Sunlogin/向日葵 live in
	// builtin-rules.json, not here — see loadBuiltinRules.)
	systemRules := []RouteRule{
		// Keep the public DNS resolvers reachable directly so DNS resolution
		// itself never depends on the VPS being up.
		{IPCIDR: []string{"1.1.1.1/32", "223.5.5.5/32"}, Outbound: "direct"},
		{IPCIDR: []string{"192.168.0.0/16", "10.0.0.0/8", "172.16.0.0/12", "127.0.0.0/8"}, Outbound: "direct"},
		// FakeIP range is foreign-traffic only by construction; send through proxy.
		// Must come before geoip-cn so the sniffed SNI doesn't accidentally hit
		// a CN-routed rule via some other heuristic.
		{IPCIDR: []string{"198.18.0.0/15"}, Outbound: "proxy"},
		// China IP ranges go direct
		{RuleSet: []string{"geoip-cn"}, Outbound: "direct"},
		// Everything else goes through proxy
		{IPCIDR: []string{"0.0.0.0/0"}, Outbound: "proxy"},
	}

	// Load custom rules from ~/.mole/custom-rules.json if exists.
	// Custom rules sit between base rules (sniff etc.) and system rules
	// so they can use the sniffed domain while still respecting FakeIP
	// and geoip-cn fallbacks.
	var routeRules []RouteRule
	routeRules = append(routeRules, baseRules...)
	routeRules = append(routeRules, builtinRules...)
	routeRules = append(routeRules, customRules...)
	routeRules = append(routeRules, systemRules...)

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

	return &SingboxConfig{
		Log: LogConfig{Level: "warn"},
		DNS: DNSConfig{Servers: dnsServers, Rules: dnsRules, Final: "dns-remote", Strategy: "ipv4_only", IndependentCache: true},
		Inbounds: []InboundConfig{
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
		},
		Outbounds: append(buildProxyOutbounds(serverURI, outbound),
			OutboundConfig{Type: "direct", Tag: "direct"}),
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
			ClashAPI: &ClashAPIConfig{ExternalController: "127.0.0.1:9090"},
		},
	}, nil
}

// Save writes a sing-box config to disk.
func Save(cfg *SingboxConfig, path string) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
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
func buildProxyOutbounds(serverURI string, outbound *OutboundConfig) []OutboundConfig {
	single := func() []OutboundConfig {
		outbound.Tag = core.ProxySelectorTag
		return []OutboundConfig{*outbound}
	}
	if outbound.Type != "hysteria2" {
		return single()
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
		// one rung — nothing to switch between, so keep the simpler shape.
		return single()
	}

	members := make([]string, 0, len(rungs))
	out := make([]OutboundConfig, 1, len(rungs)+1)
	for _, down := range rungs {
		rung := *outbound
		rung.Tag = core.BandwidthRungTag(down)
		rung.UpMbps = sched.RungUp(down)
		rung.DownMbps = down
		members = append(members, rung.Tag)
		out = append(out, rung)
	}
	out[0] = OutboundConfig{
		Type:      "selector",
		Tag:       core.ProxySelectorTag,
		Outbounds: members,
		Default:   sched.Member(time.Now()),
	}
	return out
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
	q := u.Query()
	if flow := q.Get("flow"); flow != "" {
		o.Flow = flow
	}
	if network := q.Get("type"); network != "" {
		o.Network = network
	}
	if sec := q.Get("security"); sec == "tls" || sec == "reality" {
		o.TLS = &TLSConfig{Enabled: true, ServerName: q.Get("sni"), Insecure: q.Get("allowInsecure") == "1"}
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
	upMbps, downMbps := 20, 50
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

	// Salamander obfuscation. Raw QUIC/443 to a foreign IP is easily
	// fingerprinted and throttled by the GFW; obfs wraps it so the path stays
	// stable at peak hours. Inert unless the URI carries ?obfs= AND the server
	// is configured with the same type/password. Enable per-server via
	// ?obfs=salamander&obfs-password=xxx in the URI.
	if obfsType := q.Get("obfs"); obfsType != "" {
		o.Obfs = &Hy2Obfs{Type: obfsType, Password: q.Get("obfs-password")}
	}

	return o, nil
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

	return nil
}
