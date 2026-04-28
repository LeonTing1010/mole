package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DNSCache stores the fastest DNS server and its timestamp.
type DNSCache struct {
	Server    string    `json:"server"`
	LatencyMs int       `json:"latency_ms"`
	TestedAt  time.Time `json:"tested_at"`
}

const dnsCacheFile = "dns-cache.json"
const dnsCacheTTL = 24 * time.Hour // Re-test every 24 hours

// DNSCandidates is the list of DNS servers to test.
// These are major public DNS servers in China.
var DNSCandidates = []string{
	"223.5.5.5:53",     // Alibaba DNS
	"119.29.29.29:53",  // Tencent DNSPod
	"114.114.114.114:53", // 114DNS
	"180.76.76.76:53",  // Baidu DNS
	"1.2.4.8:53",       // CNNIC SDNS
}

// TestDomain is used to test DNS resolution speed.
const TestDomain = "www.baidu.com"

// GetCachedDNS returns the cached fastest DNS, or empty if expired/missing.
func GetCachedDNS() string {
	cache, err := loadDNSCache()
	if err != nil {
		return ""
	}
	if time.Since(cache.TestedAt) > dnsCacheTTL {
		return "" // Expired
	}
	return cache.Server
}

// SaveDNSCache saves the fastest DNS to cache.
func SaveDNSCache(server string, latencyMs int) error {
	cache := DNSCache{
		Server:    server,
		LatencyMs: latencyMs,
		TestedAt:  time.Now(),
	}
	return saveDNSCache(cache)
}

func loadDNSCache() (*DNSCache, error) {
	path := filepath.Join(MoleDir(), dnsCacheFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cache DNSCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, err
	}
	return &cache, nil
}

func saveDNSCache(cache DNSCache) error {
	path := filepath.Join(MoleDir(), dnsCacheFile)
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// TestDNSLatency tests a DNS server by resolving a domain.
// Returns latency in milliseconds, or -1 if failed.
func TestDNSLatency(server string) int {
	// Remove port if present for display
	host := server
	if idx := strings.LastIndex(server, ":"); idx != -1 {
		host = server[:idx]
	}

	// Set timeout
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{
				Timeout: 3 * time.Second,
			}
			return d.DialContext(ctx, network, server)
		},
	}

	start := time.Now()
	_, err := resolver.LookupIPAddr(context.Background(), TestDomain)
	elapsed := time.Since(start)

	if err != nil {
		fmt.Printf("  DNS %s: failed (%v)\n", host, err)
		return -1
	}

	latency := int(elapsed.Milliseconds())
	fmt.Printf("  DNS %s: %dms\n", host, latency)
	return latency
}

// FindFastestDNS tests all candidates and returns the fastest one.
func FindFastestDNS() (string, int, error) {
	fmt.Println("🔍 Testing DNS servers...")

	var fastest string
	var minLatency int = -1

	for _, server := range DNSCandidates {
		latency := TestDNSLatency(server)
		if latency < 0 {
			continue // Failed
		}
		if minLatency == -1 || latency < minLatency {
			minLatency = latency
			fastest = server
		}
	}

	if fastest == "" {
		// Fallback to default
		return "223.5.5.5:53", 0, fmt.Errorf("all DNS servers failed, using default")
	}

	// Remove port for config
	if idx := strings.LastIndex(fastest, ":"); idx != -1 {
		fastest = fastest[:idx]
	}

	fmt.Printf("✅ Selected fastest DNS: %s (%dms)\n", fastest, minLatency)
	return fastest, minLatency, nil
}

// GetBestDNS returns the best DNS server (cached or newly tested).
func GetBestDNS() string {
	// Try cache first
	if cached := GetCachedDNS(); cached != "" {
		fmt.Printf("📌 Using cached DNS: %s\n", cached)
		return cached
	}

	// Test and find fastest
	server, latency, err := FindFastestDNS()
	if err != nil {
		fmt.Printf("⚠️  %v\n", err)
	}

	// Save to cache
	if err := SaveDNSCache(server, latency); err != nil {
		fmt.Printf("⚠️  Failed to save DNS cache: %v\n", err)
	}

	return server
}
