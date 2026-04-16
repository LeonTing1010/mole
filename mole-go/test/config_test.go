package main

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/leo/mole/config"
)

func TestConfigParsing(t *testing.T) {
	// Create a test config file
	testConfig := `
server: "vless://550e8400-e29b-41d4-a716-446655440000@example.com:443?security=tls&sni=example.com&flow=xtls-rprx-vision"
dns:
  - "1.1.1.1"
  - "8.8.8.8"
log_level: "info"
tun:
  enabled: true
  mtu: 1500
`
	tmpFile, err := os.CreateTemp("", "mole-config-*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(testConfig); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}
	tmpFile.Close()

	// Test loading config
	cfg, err := config.Load(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// Verify config values
	if cfg.Server == "" {
		t.Error("Server should not be empty")
	}
	if len(cfg.DNS) != 2 {
		t.Errorf("Expected 2 DNS servers, got %d", len(cfg.DNS))
	}
	if cfg.LogLevel != "info" {
		t.Errorf("Expected log level 'info', got '%s'", cfg.LogLevel)
	}
	if !cfg.TUN.Enabled {
		t.Error("TUN should be enabled")
	}
	if cfg.TUN.MTU != 1500 {
		t.Errorf("Expected MTU 1500, got %d", cfg.TUN.MTU)
	}

	fmt.Println("✅ Config parsing test passed")
}

func TestHiddifyConfigConversion(t *testing.T) {
	// Create a test config
	testConfig := `
server: "vless://550e8400-e29b-41d4-a716-446655440000@example.com:443?security=tls&sni=example.com&flow=xtls-rprx-vision"
dns:
  - "1.1.1.1"
  - "8.8.8.8"
log_level: "info"
tun:
  enabled: true
  mtu: 1500
`
	tmpFile, err := os.CreateTemp("", "mole-config-*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(testConfig); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}
	tmpFile.Close()

	// Load and convert
	cfg, err := config.Load(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	hiddifyCfg, err := config.ConvertToHiddify(cfg)
	if err != nil {
		t.Fatalf("Failed to convert config: %v", err)
	}

	// Verify Hiddify config structure
	if hiddifyCfg.Log.Level != "info" {
		t.Errorf("Expected log level 'info', got '%s'", hiddifyCfg.Log.Level)
	}
	if len(hiddifyCfg.Inbounds) == 0 {
		t.Error("Should have at least one inbound")
	}
	if len(hiddifyCfg.Outbounds) == 0 {
		t.Error("Should have at least one outbound")
	}

	// Verify TUN inbound
	tunInbound := hiddifyCfg.Inbounds[0]
	if tunInbound.Type != "tun" {
		t.Errorf("Expected inbound type 'tun', got '%s'", tunInbound.Type)
	}
	if tunInbound.MTU != 1500 {
		t.Errorf("Expected MTU 1500, got %d", tunInbound.MTU)
	}

	// Verify outbound
	outbound := hiddifyCfg.Outbounds[0]
	if outbound.Type != "vless" {
		t.Errorf("Expected outbound type 'vless', got '%s'", outbound.Type)
	}
	if outbound.Server != "example.com" {
		t.Errorf("Expected server 'example.com', got '%s'", outbound.Server)
	}
	if outbound.ServerPort != 443 {
		t.Errorf("Expected port 443, got %d", outbound.ServerPort)
	}

	// Verify route rules
	if len(hiddifyCfg.Route.Rules) == 0 {
		t.Error("Should have route rules")
	}

	// Test JSON serialization
	jsonData, err := json.MarshalIndent(hiddifyCfg, "", "  ")
	if err != nil {
		t.Fatalf("Failed to marshal Hiddify config: %v", err)
	}

	if len(jsonData) == 0 {
		t.Error("JSON output should not be empty")
	}

	fmt.Println("✅ Hiddify config conversion test passed")
	fmt.Printf("Generated config size: %d bytes\n", len(jsonData))
}

func TestVLESSURLParsing(t *testing.T) {
	testCases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{
			name:    "Valid VLESS URL",
			url:     "vless://550e8400-e29b-41d4-a716-446655440000@example.com:443?security=tls&sni=example.com&flow=xtls-rprx-vision",
			wantErr: false,
		},
		{
			name:    "VLESS URL without port",
			url:     "vless://550e8400-e29b-41d4-a716-446655440000@example.com",
			wantErr: false,
		},
		{
			name:    "Invalid URL",
			url:     "not-a-valid-url",
			wantErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.MoleConfig{
				Server:   tc.url,
				DNS:      []string{"1.1.1.1"},
				LogLevel: "info",
			}

			_, err := config.ConvertToHiddify(cfg)
			if tc.wantErr && err == nil {
				t.Errorf("Expected error for URL: %s", tc.url)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
		})
	}

	fmt.Println("✅ VLESS URL parsing test passed")
}

func main() {
	// Run tests
	t := &testing.T{}

	fmt.Println("🧪 Running mole configuration tests...")
	fmt.Println()

	TestConfigParsing(t)
	TestHiddifyConfigConversion(t)
	TestVLESSURLParsing(t)

	fmt.Println()
	fmt.Println("✅ All tests passed!")
}
