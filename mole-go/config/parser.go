package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/leo/mole/utils"
	"gopkg.in/yaml.v3"
)

// DefaultConfigPath is the default location for mole configuration (~/.mole/config.yaml)
const DefaultConfigPath = "~/.mole/config.yaml"

// Load reads and parses the mole configuration file
func Load(configPath string) (*MoleConfig, error) {
	if configPath == "" {
		configPath = expandPath(DefaultConfigPath)
	} else {
		configPath = expandPath(configPath)
	}

	// Check if file exists
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		// Try to load from environment or use default
		return loadFromEnv()
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg MoleConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Set defaults
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if len(cfg.DNS) == 0 {
		cfg.DNS = []string{"1.1.1.1", "8.8.8.8"}
	}
	if cfg.TUN.MTU == 0 {
		cfg.TUN.MTU = 1500
	}

	return &cfg, nil
}

// loadFromEnv tries to load configuration from environment variables
func loadFromEnv() (*MoleConfig, error) {
	server := os.Getenv("MOLE_SERVER")
	if server == "" {
		return nil, fmt.Errorf("no configuration found. Please create a config file or set MOLE_SERVER environment variable")
	}

	cfg := &MoleConfig{
		Server:   server,
		LogLevel: getEnvOrDefault("MOLE_LOG_LEVEL", "info"),
		TUN: TUNConfig{
			Enabled: true,
			MTU:     1500,
		},
	}

	if dns := os.Getenv("MOLE_DNS"); dns != "" {
		cfg.DNS = strings.Split(dns, ",")
	} else {
		cfg.DNS = []string{"1.1.1.1", "8.8.8.8"}
	}

	return cfg, nil
}

// expandPath expands ~ to home directory
func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, path[2:])
	}
	return path
}

// getEnvOrDefault returns environment variable value or default
func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// parseVLESSURL parses a VLESS URL and extracts configuration
func parseVLESSURL(vlessURL string) (*OutboundConfig, error) {
	u, err := url.Parse(vlessURL)
	if err != nil {
		return nil, fmt.Errorf("invalid VLESS URL: %w", err)
	}

	if u.Scheme != "vless" {
		return nil, fmt.Errorf("unsupported protocol: %s", u.Scheme)
	}

	uuid := u.User.Username()
	host := u.Hostname()
	port, _ := strconv.Atoi(u.Port())
	if port == 0 {
		port = 443
	}

	outbound := &OutboundConfig{
		Type:       "vless",
		Tag:        "proxy",
		Server:     host,
		ServerPort: port,
		UUID:       uuid,
	}

	// Parse query parameters
	query := u.Query()
	if flow := query.Get("flow"); flow != "" {
		outbound.Flow = flow
	}
	if network := query.Get("type"); network != "" {
		outbound.Network = network
	}

	// Parse TLS settings
	if security := query.Get("security"); security == "tls" || security == "reality" {
		outbound.TLS = &TLSConfig{
			Enabled:    true,
			ServerName: query.Get("sni"),
		}
		if insecure := query.Get("allowInsecure"); insecure == "1" {
			outbound.TLS.Insecure = true
		}
	}

	// Parse WebSocket settings
	if outbound.Network == "ws" {
		outbound.Transport = &TransportConfig{
			Type: "ws",
			Path: query.Get("path"),
		}
		if host := query.Get("host"); host != "" {
			outbound.Transport.Headers = map[string]string{
				"Host": host,
			}
		}
	}

	return outbound, nil
}

// ConvertToHiddify converts mole config to Hiddify/sing-box format
func ConvertToHiddify(moleConfig *MoleConfig) (*HiddifyConfig, error) {
	// Parse server URL
	outbound, err := parseVLESSURL(moleConfig.Server)
	if err != nil {
		return nil, fmt.Errorf("failed to parse server URL: %w", err)
	}

	// Build DNS servers
	dnsServers := make([]DNSServer, 0, len(moleConfig.DNS))
	for i, dns := range moleConfig.DNS {
		tag := fmt.Sprintf("dns-%d", i)
		if i == 0 {
			tag = "remote"
		} else if i == 1 {
			tag = "local"
		}
		dnsServers = append(dnsServers, DNSServer{
			Tag:     tag,
			Address: dns,
		})
	}

	hiddifyConfig := &HiddifyConfig{
		Log: LogConfig{
			Level:  moleConfig.LogLevel,
			Output: utils.HiddifyLogPath(),
		},
		DNS: DNSConfig{
			Servers: dnsServers,
		},
		Inbounds: []InboundConfig{
			{
				Type:        "tun",
				Tag:         "tun-in",
				Address:     []string{"172.19.0.1/30", "fdfe:dcba:9876::1/126"},
				MTU:         moleConfig.TUN.MTU,
				AutoRoute:   true,
				StrictRoute: false,
				Stack:       "mixed",
			},
		},
		Outbounds: []OutboundConfig{
			*outbound,
			{Type: "direct", Tag: "direct"},
			{Type: "block", Tag: "block"},
		},
		Route: RouteConfig{
			Rules: []RouteRule{
				{
					GeoIP:    []string{"cn", "private"},
					Outbound: "direct",
				},
				{
					GeoSite:  []string{"cn"},
					Outbound: "direct",
				},
				{
					IPCIDR:   []string{"192.168.0.0/16", "10.0.0.0/8", "172.16.0.0/12", "127.0.0.0/8"},
					Outbound: "direct",
				},
			},
			AutoDetectInterface: true,
			DefaultInterface:    "en0",
			Final:               "proxy",
		},
	}

	return hiddifyConfig, nil
}

// SaveHiddifyConfig saves the Hiddify configuration to a file
func SaveHiddifyConfig(config *HiddifyConfig, path string) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}
