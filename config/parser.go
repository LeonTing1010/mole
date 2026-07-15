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

	// Only use local geoip-cn rule-set to avoid download failures on startup
	ruleSets := []RuleSet{
		{
			Type:   "local",
			Tag:    "geoip-cn",
			Format: "binary",
			Path:   filepath.Join(utils.MoleDir(), "geoip-cn.srs"),
		},
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
// With no peak profile it returns a single outbound tagged `proxy` — identical
// to the historical layout. When the hy2 URI carries peak bandwidth params
// (?peakdownmbps=…), it returns a `proxy` selector over two otherwise-identical
// hy2 outbounds that differ only in their Brutal ceiling: `proxy-offpeak` and
// `proxy-peak`. The supervisor flips the selector by the clock at runtime, so a
// single fixed ceiling never has to fit both peak and off-peak. The selector's
// default member is whichever profile is current at build time, so the tunnel
// comes up on the right ceiling even before the supervisor's first tick.
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
	if peakDown <= 0 {
		return single()
	}
	peakUp, _ := strconv.Atoi(q.Get("peakupmbps"))
	start, _ := strconv.Atoi(q.Get("peakstart"))
	end, _ := strconv.Atoi(q.Get("peakend"))

	offpeak := *outbound
	offpeak.Tag = core.ProxyOffpeakTag
	peak := *outbound
	peak.Tag = core.ProxyPeakTag
	peak.UpMbps = peakUp
	peak.DownMbps = peakDown

	sched := core.BandwidthSchedule{
		OffUp: outbound.UpMbps, OffDown: outbound.DownMbps,
		PeakUp: peakUp, PeakDown: peakDown,
		StartHour: start, EndHour: end,
	}
	selector := OutboundConfig{
		Type:      "selector",
		Tag:       core.ProxySelectorTag,
		Outbounds: []string{core.ProxyOffpeakTag, core.ProxyPeakTag},
		Default:   sched.Member(time.Now()),
	}
	return []OutboundConfig{selector, offpeak, peak}
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

// Validate checks that the config doesn't reference missing rule-sets.
func Validate(cfg *SingboxConfig) error {
	// Check for remote rule-sets (not allowed - should use local only)
	for _, rs := range cfg.Route.RuleSet {
		if rs.Type == "remote" {
			return fmt.Errorf("config references remote rule-set %s - should use local only", rs.Tag)
		}
		if rs.Type == "local" && rs.Path != "" {
			if _, err := os.Stat(rs.Path); os.IsNotExist(err) {
				return fmt.Errorf("rule-set %s references non-existent file: %s", rs.Tag, rs.Path)
			}
		}
	}

	// Check DNS rules don't use rule_set (we use domain_suffix instead)
	for _, rule := range cfg.DNS.Rules {
		if len(rule.RuleSet) > 0 {
			return fmt.Errorf("DNS rule references rule_set %v - should use domain_suffix", rule.RuleSet)
		}
	}

	return nil
}
