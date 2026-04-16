package utils

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

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
	// Extract IP from server address (host:port)
	host, _, err := net.SplitHostPort(server)
	if err != nil {
		host = server
	}

	// Check if it's already an IP address
	ip := net.ParseIP(host)
	if ip == nil {
		// It's a domain, resolve it
		ips, err := net.LookupIP(host)
		if err != nil || len(ips) == 0 {
			return IPInfo{}, fmt.Errorf("failed to resolve %s: %w", host, err)
		}
		ip = ips[0]
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
	
	client := &http.Client{
		Timeout: 10 * time.Second,
	}

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
	
	client := &http.Client{
		Timeout: 10 * time.Second,
	}

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
