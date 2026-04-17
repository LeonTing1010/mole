package cmd

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/LeonTing1010/mole/utils"
	"github.com/spf13/cobra"
)

// Server is one deployed VPS running Hysteria2.
type Server struct {
	Name       string `json:"name"`
	InstanceID string `json:"instance_id"`
	Region     string `json:"region"`
	IP         string `json:"ip"`
	IPv6       string `json:"ip_v6,omitempty"`
	Port       uint16 `json:"port"`
	Password   string `json:"password"`
	CreatedAt  string `json:"created_at"`
	Active     bool   `json:"active,omitempty"`
}

// URI returns the hy2:// URI sing-box consumes.
func (s *Server) URI() string {
	host := s.IP
	if strings.Contains(host, ":") && host[0] != '[' {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("hy2://%s@%s:%d?insecure=1&sni=bing.com#%s",
		s.Password, host, s.Port, s.Name)
}

// ── Commands ──────────────────────────────────────────────────

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Manage VPS servers",
}

var serverDeployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Deploy a new VPS with Hysteria2",
	RunE:  runDeploy,
}

var serverDestroyCmd = &cobra.Command{
	Use:   "destroy <name>",
	Short: "Destroy a VPS",
	Args:  cobra.ExactArgs(1),
	RunE:  runDestroy,
}

var serverLsCmd = &cobra.Command{
	Use:     "ls",
	Aliases: []string{"list"},
	Short:   "List deployed VPS",
	RunE:    runLs,
}

var useCmd = &cobra.Command{
	Use:   "use <name>",
	Short: "Set the active VPS",
	Args:  cobra.ExactArgs(1),
	RunE:  runUse,
}

func init() {
	serverCmd.AddCommand(serverDeployCmd)
	serverCmd.AddCommand(serverDestroyCmd)
	serverCmd.AddCommand(serverLsCmd)

	serverDeployCmd.Flags().String("region", "nrt", "Vultr region (nrt, sgp, lax, ...)")
	serverDeployCmd.Flags().String("plan", "vc2-1c-1gb", "Vultr plan")
	serverDeployCmd.Flags().String("name", "", "Server name (auto-generated if empty)")
	serverDeployCmd.Flags().Int("port", 443, "Hysteria2 port")
}

// ── deploy ─────────────────────────────────────────────────────

func runDeploy(cmd *cobra.Command, _ []string) error {
	region, _ := cmd.Flags().GetString("region")
	plan, _ := cmd.Flags().GetString("plan")
	name, _ := cmd.Flags().GetString("name")
	port, _ := cmd.Flags().GetInt("port")

	if name == "" {
		name = fmt.Sprintf("%s-%d", region, time.Now().Unix())
	}

	password := randomHex(8)

	fmt.Printf("🚀 Deploying %s in %s (%s, port %d)\n", name, region, plan, port)

	scriptID, err := vultrCreateStartupScript(name, port, password)
	if err != nil {
		return fmt.Errorf("create startup script: %w", err)
	}

	createResp, err := vultrAPI("POST", "/instances", map[string]any{
		"region":      region,
		"plan":        plan,
		"os_id":       "2136", // Ubuntu 22.04 LTS
		"label":       name,
		"script_id":   scriptID,
		"enable_ipv6": true,
		"backups":     "disabled",
	})
	if err != nil {
		return fmt.Errorf("create instance: %w", err)
	}

	var created struct {
		Instance struct {
			ID string `json:"id"`
		} `json:"instance"`
	}
	if err := json.Unmarshal(createResp, &created); err != nil {
		return err
	}
	instanceID := created.Instance.ID
	fmt.Printf("✅ Instance %s created — waiting for IP...\n", instanceID)

	ip, ipv6, err := waitForIP(instanceID)
	if err != nil {
		return err
	}
	fmt.Printf("   IPv4: %s\n", ip)
	if ipv6 != "" {
		fmt.Printf("   IPv6: %s\n", ipv6)
	}

	srv := Server{
		Name: name, InstanceID: instanceID, Region: region,
		IP: ip, IPv6: ipv6,
		Port: uint16(port), Password: password,
		CreatedAt: time.Now().Format(time.RFC3339),
	}

	servers, _ := loadServers()
	// First-deployed server becomes active automatically.
	if len(servers) == 0 {
		srv.Active = true
	}
	servers = append(servers, srv)
	if err := saveServers(servers); err != nil {
		return err
	}

	fmt.Printf("\n🔗 %s\n", srv.URI())
	fmt.Println("\n⏳ Hysteria2 boot script is installing in the background (~2 min).")
	if srv.Active {
		fmt.Println("   This is now the active server — run `mole up` once it's ready.")
	} else {
		fmt.Printf("   Switch to it with `mole use %s`.\n", name)
	}
	return nil
}

func waitForIP(instanceID string) (string, string, error) {
	for i := 0; i < 30; i++ {
		resp, err := vultrAPI("GET", "/instances/"+instanceID, nil)
		if err == nil {
			var r struct {
				Instance struct {
					MainIP string `json:"main_ip"`
					V6IP   string `json:"v6_main_ip"`
				} `json:"instance"`
			}
			if json.Unmarshal(resp, &r) == nil {
				if ip := r.Instance.MainIP; ip != "" && ip != "0.0.0.0" {
					return ip, r.Instance.V6IP, nil
				}
			}
		}
		fmt.Printf("   waiting... %d/30\n", i+1)
		time.Sleep(2 * time.Second)
	}
	return "", "", fmt.Errorf("instance %s did not get an IP within 60s", instanceID)
}

// ── destroy ────────────────────────────────────────────────────

func runDestroy(_ *cobra.Command, args []string) error {
	name := args[0]
	servers, err := loadServers()
	if err != nil || len(servers) == 0 {
		return fmt.Errorf("no servers")
	}

	idx := indexByName(servers, name)
	if idx < 0 {
		return fmt.Errorf("server %q not found", name)
	}
	target := servers[idx]

	if target.InstanceID != "" {
		if _, err := vultrAPI("DELETE", "/instances/"+target.InstanceID, nil); err != nil {
			fmt.Printf("⚠️  Vultr delete failed: %v (removing from local list anyway)\n", err)
		}
	}

	servers = append(servers[:idx], servers[idx+1:]...)
	// If the destroyed server was active, promote the first remaining.
	if target.Active && len(servers) > 0 {
		servers[0].Active = true
	}
	if err := saveServers(servers); err != nil {
		return err
	}
	fmt.Printf("🗑️  %s destroyed\n", name)
	return nil
}

// ── ls ─────────────────────────────────────────────────────────

func runLs(_ *cobra.Command, _ []string) error {
	servers, err := loadServers()
	if err != nil || len(servers) == 0 {
		fmt.Println("No servers. Deploy one with `mole server deploy`.")
		return nil
	}
	fmt.Printf("%-3s %-24s %-18s %-6s %s\n", "", "NAME", "IP", "PORT", "REGION")
	for _, s := range servers {
		marker := "  "
		if s.Active {
			marker = "▸ "
		}
		fmt.Printf("%s  %-24s %-18s %-6d %s\n", marker, s.Name, s.IP, s.Port, s.Region)
	}
	return nil
}

// ── use ────────────────────────────────────────────────────────

func runUse(_ *cobra.Command, args []string) error {
	name := args[0]
	servers, err := loadServers()
	if err != nil || len(servers) == 0 {
		return fmt.Errorf("no servers")
	}
	idx := indexByName(servers, name)
	if idx < 0 {
		return fmt.Errorf("server %q not found", name)
	}
	for i := range servers {
		servers[i].Active = (i == idx)
	}
	if err := saveServers(servers); err != nil {
		return err
	}
	fmt.Printf("✅ Active server: %s (%s)\n", servers[idx].Name, servers[idx].IP)
	fmt.Println("   Run `mole up` to connect.")
	return nil
}

// ── storage ────────────────────────────────────────────────────

func loadServers() ([]Server, error) {
	data, err := os.ReadFile(utils.ServersPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var servers []Server
	if err := json.Unmarshal(data, &servers); err != nil {
		return nil, err
	}
	return servers, nil
}

func saveServers(servers []Server) error {
	data, err := json.MarshalIndent(servers, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(utils.ServersPath(), data, 0644)
}

// ActiveServer returns the active server for use by other commands.
func ActiveServer() (*Server, error) {
	servers, err := loadServers()
	if err != nil {
		return nil, err
	}
	for i, s := range servers {
		if s.Active {
			return &servers[i], nil
		}
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("no servers deployed — run `mole server deploy`")
	}
	return nil, fmt.Errorf("no active server — pick one with `mole use <name>`")
}

func indexByName(servers []Server, name string) int {
	for i, s := range servers {
		if s.Name == name {
			return i
		}
	}
	return -1
}

// ── Vultr API ─────────────────────────────────────────────────

const vultrBase = "https://api.vultr.com/v2"

func vultrAPIKey() (string, error) {
	if k := strings.TrimSpace(os.Getenv("VULTR_API_KEY")); k != "" {
		return k, nil
	}
	return "", fmt.Errorf("VULTR_API_KEY environment variable is not set")
}

func vultrAPI(method, path string, body any) ([]byte, error) {
	key, err := vultrAPIKey()
	if err != nil {
		return nil, err
	}
	var reqBody io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reqBody = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, vultrBase+path, reqBody)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("vultr %s %s: %d %s", method, path, resp.StatusCode, string(data))
	}
	return data, nil
}

func vultrCreateStartupScript(name string, port int, password string) (string, error) {
	script := hysteria2BootScript(port, password)
	resp, err := vultrAPI("POST", "/startup-scripts", map[string]any{
		"name":   name,
		"type":   "boot",
		"script": base64.StdEncoding.EncodeToString([]byte(script)),
	})
	if err != nil {
		return "", err
	}
	var r struct {
		StartupScript struct {
			ID string `json:"id"`
		} `json:"startup_script"`
	}
	if err := json.Unmarshal(resp, &r); err != nil {
		return "", err
	}
	return r.StartupScript.ID, nil
}

func hysteria2BootScript(port int, password string) string {
	return fmt.Sprintf(`#!/bin/bash
set -ex
exec > /var/log/hysteria-install.log 2>&1

apt update && apt install -y curl openssl iptables
curl -fsSL https://get.hy2.sh/ | bash
mkdir -p /etc/hysteria
openssl ecparam -genkey -name prime256v1 -out /etc/hysteria/server.key
openssl req -new -x509 -key /etc/hysteria/server.key -out /etc/hysteria/server.crt -subj /CN=bing.com -days 36500
chown hysteria:hysteria /etc/hysteria/server.{key,crt} 2>/dev/null || true

cat > /etc/hysteria/config.yaml <<EOF
listen: :%d
tls:
  cert: /etc/hysteria/server.crt
  key: /etc/hysteria/server.key
auth:
  type: password
  password: %s
masquerade:
  type: proxy
  proxy:
    url: https://bing.com
    rewriteHost: true
EOF

iptables -I INPUT -p udp --dport %d -j ACCEPT 2>/dev/null || true
iptables -I INPUT -p tcp --dport %d -j ACCEPT 2>/dev/null || true

systemctl enable hysteria
systemctl restart hysteria
`, port, password, port, port)
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}
