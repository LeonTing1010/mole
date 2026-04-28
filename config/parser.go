package config

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/LeonTing1010/mole/utils"
)

// Build assembles a sing-box config from a server URI (vless://, hy2://).
func Build(serverURI string) (*SingboxConfig, error) {
	outbound, err := parseServerURL(serverURI)
	if err != nil {
		return nil, err
	}

	// Get best DNS server (cached or test to find fastest)
	bestDNS := utils.GetBestDNS()

	dnsServers := []DNSServer{
		// Direct DNS - auto-selected for best performance
		{Type: "udp", Server: bestDNS, Tag: "dns-direct"},
		// Remote DNS via proxy for foreign domains (avoids DNS poisoning)
		{Type: "tls", Server: "1.1.1.1", Tag: "dns-remote", Detour: "proxy"},
	}

	dnsRules := []DNSRule{}
	if outbound.Server != "" && net.ParseIP(outbound.Server) == nil {
		// VPS hostname uses direct DNS to avoid circular dependency
		dnsRules = append(dnsRules, DNSRule{
			Domain: []string{outbound.Server},
			Server: "dns-direct",
		})
	}
	// Chinese domains use direct DNS
	dnsRules = append(dnsRules, DNSRule{
		DomainSuffix: []string{".cn", ".com.cn", ".net.cn", ".org.cn", ".gov.cn", ".edu.cn", ".mil.cn", ".中国", ".qq.com"},
		Server:       "dns-direct",
	})

	routeRules := []RouteRule{
		{Action: "sniff"},
		{Protocol: "dns", Action: "hijack-dns"},
		{Protocol: "quic", Action: "reject"},
		// Keep DNS resolvers reachable even when VPS is down so that
		// blocked requests fail fast with ERR_CONNECTION_REFUSED instead of DNS timeouts.
		{IPCIDR: []string{"1.1.1.1/32", "223.5.5.5/32"}, Outbound: "direct"},
		{IPCIDR: []string{"192.168.0.0/16", "10.0.0.0/8", "172.16.0.0/12", "127.0.0.0/8"}, Outbound: "direct"},
		// Sunlogin (向日葵) remote desktop - direct connection for domestic servers
		{DomainSuffix: []string{"oray.com", "orayimg.com", "oray.net"}, Outbound: "direct"},
		// China IP ranges go direct
		{RuleSet: []string{"geoip-cn"}, Outbound: "direct"},
		// Everything else goes through proxy
		{IPCIDR: []string{"0.0.0.0/0"}, Outbound: "proxy"},
	}

	// Load custom rules from ~/.mole/custom-rules.json if exists
	if customRules := loadCustomRules(); len(customRules) > 0 {
		routeRules = append(customRules, routeRules...)
	}

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
		Log: LogConfig{Level: "info"},
		DNS: DNSConfig{Servers: dnsServers, Rules: dnsRules, Final: "dns-remote", Strategy: "ipv4_only"},
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
		Outbounds: []OutboundConfig{
			*outbound,
			{Type: "direct", Tag: "direct"},
		},
		Route: RouteConfig{
			Rules:               routeRules,
			RuleSet:             ruleSets,
			AutoDetectInterface: true,
			Final:               "direct",
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
	return o, nil
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
