package cmd

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
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

// Ready reports whether the server is fully provisioned (has an IP). Servers
// recorded mid-deploy may exist with InstanceID set but IP still empty until
// `waitForIP` succeeds.
func (s *Server) Ready() bool { return s.IP != "" }

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

var serverReconcileCmd = &cobra.Command{
	Use:   "reconcile",
	Short: "Compare local servers.json against Vultr; show drift",
	RunE:  runReconcile,
}

var serverAdoptCmd = &cobra.Command{
	Use:   "adopt <instance-id>",
	Short: "Add an existing Vultr instance to the local server list",
	Args:  cobra.ExactArgs(1),
	RunE:  runAdopt,
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
	serverCmd.AddCommand(serverReconcileCmd)
	serverCmd.AddCommand(serverAdoptCmd)

	serverDeployCmd.Flags().String("region", "nrt", "Vultr region (nrt, sgp, lax, ...)")
	serverDeployCmd.Flags().String("plan", "vc2-1c-1gb", "Vultr plan")
	serverDeployCmd.Flags().String("name", "", "Server name (auto-generated if empty)")
	serverDeployCmd.Flags().Int("port", 443, "Hysteria2 port")

	serverDestroyCmd.Flags().Bool("force-local", false, "Remove from local list even if the Vultr DELETE fails (use only when the remote instance is verified gone)")

	serverAdoptCmd.Flags().String("name", "", "Local name for the adopted server (defaults to the Vultr label)")
	serverAdoptCmd.Flags().Int("port", 443, "Hysteria2 port the adopted instance listens on")
	serverAdoptCmd.Flags().String("password", "", "Hysteria2 password (required — Vultr does not store this)")
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
		// Instance creation failed; the startup script we just made is now an
		// orphan resource on Vultr. Best-effort cleanup so the user isn't
		// charged for / cluttered with abandoned scripts.
		_, _ = vultrAPI("DELETE", "/startup-scripts/"+scriptID, nil)
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

	// Persist a partial entry IMMEDIATELY so a crash before waitForIP /
	// saveServers below doesn't leak an invisible Vultr instance. The IP is
	// filled in once Vultr assigns one; until then `Ready() == false`.
	srv := Server{
		Name: name, InstanceID: instanceID, Region: region,
		Port: uint16(port), Password: password,
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	servers, _ := loadServers()
	if len(servers) == 0 {
		srv.Active = true
	}
	servers = append(servers, srv)
	if err := saveServers(servers); err != nil {
		fmt.Printf("⚠️  Could not record %s locally: %v\n", instanceID, err)
		fmt.Printf("    The instance IS running on Vultr. Recover it with:\n")
		fmt.Printf("        mole server adopt %s --password %s --port %d\n", instanceID, password, port)
		return err
	}

	ip, ipv6, err := waitForIP(instanceID)
	if err != nil {
		fmt.Printf("⚠️  %v\n", err)
		fmt.Printf("    The instance is recorded locally as %q but has no IP yet.\n", name)
		fmt.Printf("    Run `mole server reconcile` later, or `mole server destroy %s` to remove it.\n", name)
		return err
	}
	fmt.Printf("   IPv4: %s\n", ip)
	if ipv6 != "" {
		fmt.Printf("   IPv6: %s\n", ipv6)
	}

	// Update the entry we already saved with the now-known IP info.
	servers, _ = loadServers()
	for i := range servers {
		if servers[i].InstanceID == instanceID {
			servers[i].IP = ip
			servers[i].IPv6 = ipv6
			srv = servers[i]
			break
		}
	}
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

func runDestroy(cmd *cobra.Command, args []string) error {
	name := args[0]
	forceLocal, _ := cmd.Flags().GetBool("force-local")

	servers, err := loadServers()
	if err != nil {
		return fmt.Errorf("read local servers: %w", err)
	}
	if len(servers) == 0 {
		return fmt.Errorf("no servers")
	}

	idx := indexByName(servers, name)
	if idx < 0 {
		return fmt.Errorf("server %q not found", name)
	}
	target := servers[idx]

	if target.InstanceID != "" {
		_, err := vultrAPI("DELETE", "/instances/"+target.InstanceID, nil)
		switch {
		case err == nil:
			// Remote delete succeeded — fall through and remove local entry.
		case IsVultrNotFound(err):
			// Already gone on Vultr; safe to drop the local entry.
			fmt.Printf("ℹ️  Vultr says %s is already gone — removing local entry\n", target.InstanceID)
		case forceLocal:
			fmt.Printf("⚠️  Vultr DELETE failed: %v\n", err)
			fmt.Printf("    --force-local set; removing local entry anyway. Verify on the Vultr dashboard that %s is really gone.\n", target.InstanceID)
		default:
			return fmt.Errorf("vultr delete failed: %w\n    The local entry was NOT removed. If the instance is really gone on Vultr, retry with --force-local.", err)
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
	if err != nil {
		return fmt.Errorf("read local servers: %w", err)
	}
	if len(servers) == 0 {
		fmt.Println("No servers. Deploy one with `mole server deploy`.")
		return nil
	}
	fmt.Printf("%-3s %-24s %-18s %-6s %-10s %s\n", "", "NAME", "IP", "PORT", "REGION", "STATUS")
	for _, s := range servers {
		marker := "  "
		if s.Active {
			marker = "▸ "
		}
		status := "ready"
		if !s.Ready() {
			status = "provisioning"
		}
		ip := s.IP
		if ip == "" {
			ip = "(pending)"
		}
		fmt.Printf("%s  %-24s %-18s %-6d %-10s %s\n", marker, s.Name, ip, s.Port, s.Region, status)
	}
	return nil
}

// ── reconcile ──────────────────────────────────────────────────

func runReconcile(_ *cobra.Command, _ []string) error {
	local, err := loadServers()
	if err != nil {
		return fmt.Errorf("read local servers: %w", err)
	}
	remote, err := vultrListInstances()
	if err != nil {
		return fmt.Errorf("list vultr instances: %w", err)
	}

	localByID := make(map[string]Server, len(local))
	for _, s := range local {
		if s.InstanceID != "" {
			localByID[s.InstanceID] = s
		}
	}
	remoteByID := make(map[string]vultrInstance, len(remote))
	for _, r := range remote {
		remoteByID[r.ID] = r
	}

	type row struct {
		state   string // "synced" | "remote-only" | "local-only" | "ip-mismatch"
		id, ip  string
		name    string
		region  string
		message string
	}
	var rows []row

	ids := make(map[string]struct{}, len(local)+len(remote))
	for id := range localByID {
		ids[id] = struct{}{}
	}
	for id := range remoteByID {
		ids[id] = struct{}{}
	}
	keys := make([]string, 0, len(ids))
	for id := range ids {
		keys = append(keys, id)
	}
	sort.Strings(keys)

	for _, id := range keys {
		l, lok := localByID[id]
		r, rok := remoteByID[id]
		switch {
		case lok && rok:
			if l.IP == "" {
				rows = append(rows, row{state: "syncing", id: id, ip: r.MainIP, name: l.Name, region: r.Region, message: "local entry was provisioning; remote has IP — run `mole server reconcile --apply` to update (not yet implemented; re-deploy or hand-edit)"})
			} else if l.IP != r.MainIP {
				rows = append(rows, row{state: "drift", id: id, ip: r.MainIP, name: l.Name, region: r.Region, message: fmt.Sprintf("local IP %s ≠ remote IP %s", l.IP, r.MainIP)})
			} else {
				rows = append(rows, row{state: "synced", id: id, ip: r.MainIP, name: l.Name, region: r.Region})
			}
		case rok && !lok:
			rows = append(rows, row{state: "remote-only", id: id, ip: r.MainIP, name: r.Label, region: r.Region, message: fmt.Sprintf("adopt with: mole server adopt %s --password <hy2-password>", id)})
		case lok && !rok:
			rows = append(rows, row{state: "local-only", id: id, ip: l.IP, name: l.Name, region: l.Region, message: fmt.Sprintf("remove with: mole server destroy %s --force-local", l.Name)})
		}
	}

	fmt.Printf("%-12s  %-22s  %-24s  %-18s  %s\n", "STATE", "INSTANCE", "NAME", "IP", "NOTE")
	for _, r := range rows {
		fmt.Printf("%-12s  %-22s  %-24s  %-18s  %s\n", r.state, r.id, r.name, r.ip, r.message)
	}
	if len(rows) == 0 {
		fmt.Println("(no instances on local or Vultr)")
	}
	return nil
}

// ── adopt ─────────────────────────────────────────────────────

func runAdopt(cmd *cobra.Command, args []string) error {
	instanceID := args[0]
	name, _ := cmd.Flags().GetString("name")
	port, _ := cmd.Flags().GetInt("port")
	password, _ := cmd.Flags().GetString("password")

	if password == "" {
		return fmt.Errorf("--password is required: Vultr does not store the Hysteria2 password, you must supply the one used when the instance was created")
	}

	resp, err := vultrAPI("GET", "/instances/"+instanceID, nil)
	if err != nil {
		return fmt.Errorf("fetch vultr instance: %w", err)
	}
	var r struct {
		Instance struct {
			ID     string `json:"id"`
			Label  string `json:"label"`
			Region string `json:"region"`
			MainIP string `json:"main_ip"`
			V6IP   string `json:"v6_main_ip"`
		} `json:"instance"`
	}
	if err := json.Unmarshal(resp, &r); err != nil {
		return err
	}
	if name == "" {
		name = r.Instance.Label
		if name == "" {
			name = "adopted-" + instanceID
		}
	}

	servers, _ := loadServers()
	if indexByName(servers, name) >= 0 {
		return fmt.Errorf("local server %q already exists; pass --name to use a different one", name)
	}
	for _, s := range servers {
		if s.InstanceID == instanceID {
			return fmt.Errorf("instance %s is already adopted as %q", instanceID, s.Name)
		}
	}

	srv := Server{
		Name:       name,
		InstanceID: r.Instance.ID,
		Region:     r.Instance.Region,
		IP:         r.Instance.MainIP,
		IPv6:       r.Instance.V6IP,
		Port:       uint16(port),
		Password:   password,
		CreatedAt:  time.Now().Format(time.RFC3339),
	}
	if len(servers) == 0 {
		srv.Active = true
	}
	servers = append(servers, srv)
	if err := saveServers(servers); err != nil {
		return err
	}
	fmt.Printf("✅ Adopted %s as %q (%s)\n", instanceID, name, srv.IP)
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
		return nil, fmt.Errorf("parse %s: %w (file may be corrupt; do NOT delete it without first backing up — InstanceIDs cannot be recovered. Try `mole server reconcile` after fixing it)", utils.ServersPath(), err)
	}
	return servers, nil
}

// saveServers writes the server list atomically (tmp + rename) so a crash
// mid-write never leaves a half-written or empty file. Losing servers.json
// means losing every InstanceID — the corresponding VPS instances become
// invisible orphans on Vultr.
func saveServers(servers []Server) error {
	data, err := json.MarshalIndent(servers, "", "  ")
	if err != nil {
		return err
	}
	path := utils.ServersPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ActiveServer returns the active server for use by other commands.
func ActiveServer() (*Server, error) {
	servers, err := loadServers()
	if err != nil {
		return nil, err
	}
	for i, s := range servers {
		if s.Active {
			if !servers[i].Ready() {
				return nil, fmt.Errorf("active server %q is still provisioning (no IP yet) — run `mole server reconcile` or wait", servers[i].Name)
			}
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

// VultrError carries the HTTP status from a failed Vultr API call so callers
// can distinguish "instance is gone" (404) from generic transport / 5xx
// failures. Without this distinction, `mole server destroy` cannot safely
// decide whether to drop the local entry.
type VultrError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *VultrError) Error() string {
	return fmt.Sprintf("vultr %s %s: %d %s", e.Method, e.Path, e.Status, e.Body)
}

// IsVultrNotFound reports whether err is a 404 from the Vultr API.
func IsVultrNotFound(err error) bool {
	var ve *VultrError
	return errors.As(err, &ve) && ve.Status == 404
}

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
		return nil, &VultrError{Method: method, Path: path, Status: resp.StatusCode, Body: string(data)}
	}
	return data, nil
}

// vultrInstance is the subset of /instances list fields reconcile needs.
type vultrInstance struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Region string `json:"region"`
	MainIP string `json:"main_ip"`
}

// vultrListInstances fetches every instance on the account. Vultr paginates
// at 100/page; we follow the cursor until exhausted so a user with >100
// instances doesn't get a silently-truncated reconcile.
func vultrListInstances() ([]vultrInstance, error) {
	var all []vultrInstance
	cursor := ""
	for {
		path := "/instances?per_page=100"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		data, err := vultrAPI("GET", path, nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Instances []vultrInstance `json:"instances"`
			Meta      struct {
				Links struct {
					Next string `json:"next"`
				} `json:"links"`
			} `json:"meta"`
		}
		if err := json.Unmarshal(data, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Instances...)
		if page.Meta.Links.Next == "" {
			return all, nil
		}
		cursor = page.Meta.Links.Next
	}
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
