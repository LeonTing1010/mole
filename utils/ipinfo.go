package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// directResolver is a DNS resolver that queries 223.5.5.5 (Alibaba DNS)
// directly, bypassing the system DNS which is taken over to the TUN gateway
// (172.19.0.1) when mole is running. The route config sends 223.5.5.5/32
// through the "direct" outbound, so DNS queries don't go through the proxy.
var directResolver = &net.Resolver{
	PreferGo: true,
	Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		d := net.Dialer{Timeout: 5 * time.Second}
		// Use the caller's network type (udp/tcp) so DNS-over-TCP
		// fallback works for large responses. Always connect to the
		// direct DNS server regardless of what address Go's resolver
		// read from the (taken-over) system DNS config.
		return d.DialContext(ctx, network, "223.5.5.5:53")
	},
}

// newDirectHTTPClient returns an HTTP client that resolves domains via
// directResolver instead of the system DNS. Use this for requests that MUST
// bypass the proxy regardless of VPN state — e.g. fetching boot rule-sets at
// cold start, before sing-box exists.
func newDirectHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:  timeout,
				Resolver: directResolver,
			}).DialContext,
			IdleConnTimeout:       10 * time.Second,
			ResponseHeaderTimeout: timeout,
		},
	}
}

// newProxyHTTPClient returns an HTTP client that uses the system resolver.
// While mole is running the system DNS is the TUN gateway, so the domain is
// resolved to a FakeIP, the connection is routed through the proxy outbound,
// and sing-box recovers the real domain and resolves it at the proxy egress.
//
// This is the correct client for exit-IP / geolocation checks: they must
// reflect the proxy's egress. Resolving a (possibly censored) foreign domain
// like ipinfo.io via a China resolver and then connecting from a foreign
// egress is incoherent — a poisoned answer makes the TLS handshake land on the
// wrong host and fail certificate verification.
func newProxyHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			IdleConnTimeout:       10 * time.Second,
			ResponseHeaderTimeout: timeout,
		},
	}
}

// vpnRunning reports whether a live mole daemon is currently running — i.e.
// whether DNS has been taken over and traffic is flowing through the TUN.
func vpnRunning() bool {
	pid, err := ReadPID()
	return err == nil && IsRunning(pid)
}

// ipLookupClient picks the right HTTP client for an IP-geolocation request:
// through the proxy (resolve at egress) when the VPN is up, or direct when it
// isn't (e.g. during `mole up`, before sing-box has started and there is no
// proxy to route through).
func ipLookupClient(timeout time.Duration) *http.Client {
	if vpnRunning() {
		return newProxyHTTPClient(timeout)
	}
	return newDirectHTTPClient(timeout)
}

// IPInfo represents geolocation information for an IP address
type IPInfo struct {
	IP      string `json:"ip"`
	City    string `json:"city"`
	Region  string `json:"region"`
	Country string `json:"country"`
	Org     string `json:"org"`
}

// GetIPInfo retrieves geolocation information for a server address
func GetIPInfo(server string) (IPInfo, error) {
	// Parse URI if it looks like one
	if strings.Contains(server, "://") {
		u, err := url.Parse(server)
		if err == nil && u.Host != "" {
			server = u.Host
		}
	}

	// Extract IP from server address (host:port)
	host, _, err := net.SplitHostPort(server)
	if err != nil {
		host = server
	}

	// Check if it's already an IP address
	ip := net.ParseIP(host)
	if ip == nil {
		// It's a domain — resolve it via directResolver to avoid
		// fake-IP (198.18.x.x) answers from the taken-over system DNS.
		ips, err := directResolver.LookupIPAddr(context.Background(), host)
		if err != nil || len(ips) == 0 {
			return IPInfo{}, fmt.Errorf("failed to resolve %s: %w", host, err)
		}
		ip = ips[0].IP
	}

	ipStr := ip.String()

	// Try multiple IP geolocation services
	info, err := getIPInfoFromService(ipStr)
	if err != nil {
		// Return basic info if geolocation fails
		return IPInfo{
			IP: ipStr,
		}, nil
	}

	return info, nil
}

// GetMyIPInfo returns geolocation for the caller's own public IP (i.e. the
// current egress — useful for confirming VPN exit).
//
// Routes through the proxy (via the system DNS → FakeIP → proxy path) so the
// reported IP is the proxy's egress and ipinfo.io is resolved at that egress
// rather than via a China resolver that may hand back a poisoned answer.
func GetMyIPInfo() (IPInfo, error) {
	client := ipLookupClient(15 * time.Second)
	resp, err := client.Get("https://ipinfo.io/json")
	if err != nil {
		return IPInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return IPInfo{}, fmt.Errorf("ipinfo.io returned status %d", resp.StatusCode)
	}
	var r struct {
		IP, City, Region, Country, Org string
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return IPInfo{}, err
	}
	return IPInfo{IP: r.IP, City: r.City, Region: r.Region, Country: r.Country, Org: r.Org}, nil
}

// getIPInfoFromService queries IP geolocation services
func getIPInfoFromService(ip string) (IPInfo, error) {
	// Service 1: ipinfo.io (no auth required for basic info)
	info, err := queryIPInfoIO(ip)
	if err == nil {
		return info, nil
	}

	// Service 2: ip-api.com (free, no auth)
	info, err = queryIPAPI(ip)
	if err == nil {
		return info, nil
	}

	return IPInfo{}, fmt.Errorf("all geolocation services failed")
}

// queryIPInfoIO queries ipinfo.io
func queryIPInfoIO(ip string) (IPInfo, error) {
	url := fmt.Sprintf("https://ipinfo.io/%s/json", ip)
	client := ipLookupClient(10 * time.Second)
	resp, err := client.Get(url)
	if err != nil {
		return IPInfo{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return IPInfo{}, fmt.Errorf("ipinfo.io returned status %d", resp.StatusCode)
	}

	var result struct {
		IP      string `json:"ip"`
		City    string `json:"city"`
		Region  string `json:"region"`
		Country string `json:"country"`
		Org     string `json:"org"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return IPInfo{}, err
	}

	return IPInfo{
		IP:      result.IP,
		City:    result.City,
		Region:  result.Region,
		Country: result.Country,
		Org:     result.Org,
	}, nil
}

// queryIPAPI queries ip-api.com
func queryIPAPI(ip string) (IPInfo, error) {
	url := fmt.Sprintf("http://ip-api.com/json/%s", ip)
	client := ipLookupClient(10 * time.Second)
	resp, err := client.Get(url)
	if err != nil {
		return IPInfo{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return IPInfo{}, fmt.Errorf("ip-api.com returned status %d", resp.StatusCode)
	}

	var result struct {
		Status  string `json:"status"`
		Query   string `json:"query"`
		City    string `json:"city"`
		Region  string `json:"regionName"`
		Country string `json:"country"`
		ISP     string `json:"isp"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return IPInfo{}, err
	}

	if result.Status != "success" {
		return IPInfo{}, fmt.Errorf("ip-api.com query failed")
	}

	return IPInfo{
		IP:      result.Query,
		City:    result.City,
		Region:  result.Region,
		Country: result.Country,
		Org:     result.ISP,
	}, nil
}

// IsPrivateIP checks if an IP address is private
func IsPrivateIP(ip string) bool {
	parsedIP := net.ParseIP(ip)
	if parsedIP == nil {
		return false
	}

	privateRanges := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",
		"169.254.0.0/16",
		"::1/128",
		"fc00::/7",
	}

	for _, cidr := range privateRanges {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		if ipNet.Contains(parsedIP) {
			return true
		}
	}

	return false
}

// FormatIPInfo formats IPInfo for display
func FormatIPInfo(info IPInfo) string {
	parts := []string{}

	if info.Country != "" {
		parts = append(parts, info.Country)
	}
	if info.City != "" {
		parts = append(parts, info.City)
	}
	if info.Region != "" && info.Region != info.City {
		parts = append(parts, info.Region)
	}

	if len(parts) == 0 {
		return info.IP
	}

	return fmt.Sprintf("%s (%s)", info.IP, strings.Join(parts, ", "))
}
