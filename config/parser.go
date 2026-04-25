package config

import (
	"encoding/json"
	"fmt"
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

	dnsServers := []DNSServer{
		{Type: "tls", Server: "1.1.1.1", Tag: "dns-remote", Detour: "proxy"},
		{Type: "udp", Server: "223.5.5.5", Tag: "dns-direct"},
	}

	dnsRules := []DNSRule{}
	if outbound.Server != "" && net.ParseIP(outbound.Server) == nil {
		dnsRules = append(dnsRules, DNSRule{
			Domain: []string{outbound.Server},
			Server: "dns-direct",
		})
	}
	// Non-CN domains resolve through the proxy so GFW-poisoned A records
	// never reach the client.
	dnsRules = append(dnsRules, DNSRule{
		RuleSet: []string{"geosite-geolocation-!cn"},
		Server:  "dns-remote",
	})
	// Chinese domains use direct DNS for better performance
	dnsRules = append(dnsRules, DNSRule{
		RuleSet: []string{"geosite-cn"},
		Server:  "dns-direct",
	})

	routeRules := []RouteRule{
		{Action: "sniff"},
		{Protocol: "dns", Action: "hijack-dns"},
		{Protocol: "quic", Action: "reject"},
		// Keep DNS resolvers reachable even when VPS is down so that
		// blocked requests fail fast with ERR_CONNECTION_REFUSED instead of DNS timeouts.
		{IPCIDR: []string{"1.1.1.1/32", "223.5.5.5/32"}, Outbound: "direct"},
		{IPCIDR: []string{"192.168.0.0/16", "10.0.0.0/8", "172.16.0.0/12", "127.0.0.0/8"}, Outbound: "direct"},
		// Non-CN by domain, then non-CN by IP as a fallback for IP-literal
		// connections that never surface a hostname.
		{RuleSet: []string{"geosite-geolocation-!cn"}, Outbound: "auto"},
		{RuleSet: []string{"geoip-cn"}, Invert: true, Outbound: "auto"},
	}

	ruleSets := []RuleSet{
		buildRuleSet("geosite-geolocation-!cn", "https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/geosite-geolocation-!cn.srs"),
		buildRuleSet("geosite-cn", "https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/geosite-cn.srs"),
		buildRuleSet("geoip-cn", "https://raw.githubusercontent.com/SagerNet/sing-geoip/rule-set/geoip-cn.srs"),
	}

	return &SingboxConfig{
		Log: LogConfig{Level: "info"},
		DNS: DNSConfig{Servers: dnsServers, Rules: dnsRules, Final: "dns-remote", Strategy: "ipv4_only"},
		Inbounds: []InboundConfig{{
			Type: "tun", Tag: "tun-in",
			Address:     []string{"172.19.0.1/28"},
			MTU:         9000,
			AutoRoute:   true,
			StrictRoute: true,
			Stack:       "gvisor",
		}},
		Outbounds: []OutboundConfig{
			*outbound,
			{Type: "direct", Tag: "direct"},
			// "block" is a black-hole socks outbound pointing at an unroutable
			// local port — connections fail immediately, giving us fail-closed
			// behavior via the selector below when the VPS is unreachable.
			{Type: "socks", Tag: "block", Server: "127.0.0.1", ServerPort: 1, Version: "5"},
			// "auto" is flipped between "proxy" and "block" at runtime through
			// the Clash API based on VPS health.
			{Type: "selector", Tag: "auto", Outbounds: []string{"proxy", "block"}, Default: "proxy"},
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

func buildRuleSet(tag, url string) RuleSet {
	localPath := filepath.Join(utils.MoleDir(), tag+".srs")
	if _, err := os.Stat(localPath); err == nil {
		return RuleSet{Type: "local", Tag: tag, Format: "binary", Path: localPath}
	}
	return RuleSet{
		Type:           "remote",
		Tag:            tag,
		Format:         "binary",
		URL:            url,
		DownloadDetour: "proxy",
		UpdateInterval: "168h",
	}
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
