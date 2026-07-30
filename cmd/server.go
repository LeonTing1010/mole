package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LeonTing1010/mole/core"
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
	Stopped    bool   `json:"stopped,omitempty"` // true when halted on the server (no compute billing, storage only)
	// Optional per-server tuning. All opt-in: zero values reproduce the previous
	// URI byte-for-byte. Obfs/ObfsPassword turn on hysteria2 salamander
	// obfuscation (must match the server, or the tunnel goes dark). UpMbps/DownMbps
	// pin Brutal congestion control to a measured ceiling instead of the built-in
	// 20/50 default — set them to ~80% of a real speed test, or to -1 to disable
	// Brutal and let hysteria2 use adaptive BBR (best for time-varying links).
	Obfs         string `json:"obfs,omitempty"`
	ObfsPassword string `json:"obfs_password,omitempty"`
	UpMbps       int    `json:"up_mbps,omitempty"`
	DownMbps     int    `json:"down_mbps,omitempty"`

	// Time-of-day Brutal ceiling. When PeakDownMbps>0, `mole up` emits a
	// peak/off-peak selector and the supervisor flips it on the local clock with
	// no reconnect — UpMbps/DownMbps become the OFF-peak ceiling and these the
	// peak one (set to ~the worst-case peak link speed so Brutal stops flooding).
	// Hours are local 24h; leaving both at 0 uses the default 12:00–02:00 window.
	PeakUpMbps    int `json:"peak_up_mbps,omitempty"`
	PeakDownMbps  int `json:"peak_down_mbps,omitempty"`
	PeakStartHour int `json:"peak_start_hour,omitempty"`
	PeakEndHour   int `json:"peak_end_hour,omitempty"`
}

// Default peak window (local 24h) when a server sets a peak ceiling but no hours.
const (
	defaultPeakStartHour = 12
	defaultPeakEndHour   = 2
)

// peakWindow resolves the peak window, applying the default when start==end.
func (s *Server) peakWindow() (start, end int) {
	if s.PeakStartHour == s.PeakEndHour {
		return defaultPeakStartHour, defaultPeakEndHour
	}
	return s.PeakStartHour, s.PeakEndHour
}

// Schedule builds the supervisor's time-of-day bandwidth schedule for this
// server. Enabled() is false (inert) unless PeakDownMbps>0.
func (s *Server) Schedule() core.BandwidthSchedule {
	start, end := s.peakWindow()
	return core.BandwidthSchedule{
		OffUp: s.UpMbps, OffDown: s.DownMbps,
		PeakUp: s.PeakUpMbps, PeakDown: s.PeakDownMbps,
		StartHour: start, EndHour: end,
	}
}

// Ready reports whether the server is fully provisioned (has an IP). Servers
// recorded mid-deploy may exist with InstanceID set but IP still empty until
// `waitForIP` succeeds.
func (s *Server) Ready() bool { return s.IP != "" }

// URI returns the hy2:// URI sing-box consumes. Optional obfs/bandwidth query
// params are appended only when set, so an untuned server yields the exact same
// URI as before.
func (s *Server) URI() string {
	host := s.IP
	if host == "" {
		host = s.IPv6
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	q := "insecure=1&sni=bing.com"
	if s.Obfs != "" {
		q += "&obfs=" + url.QueryEscape(s.Obfs)
		if s.ObfsPassword != "" {
			q += "&obfs-password=" + url.QueryEscape(s.ObfsPassword)
		}
	}
	// Non-zero passes through, including a negative sentinel (-1) that the parser
	// reads as "disable Brutal, use BBR". Zero stays absent so an untuned server
	// yields the byte-for-byte same URI as before.
	if s.UpMbps != 0 {
		q += fmt.Sprintf("&upmbps=%d", s.UpMbps)
	}
	if s.DownMbps != 0 {
		q += fmt.Sprintf("&downmbps=%d", s.DownMbps)
	}
	// Peak profile params drive the time-of-day selector in config.Build. Emitted
	// only when a peak ceiling is set, so an untuned server's URI is unchanged.
	if s.PeakDownMbps > 0 {
		start, end := s.peakWindow()
		q += fmt.Sprintf("&peakdownmbps=%d", s.PeakDownMbps)
		if s.PeakUpMbps > 0 {
			q += fmt.Sprintf("&peakupmbps=%d", s.PeakUpMbps)
		}
		q += fmt.Sprintf("&peakstart=%d&peakend=%d", start, end)
	}
	return fmt.Sprintf("hy2://%s@%s:%d?%s#%s", s.Password, host, s.Port, q, s.Name)
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
	Short: "Compare local servers.json against the server; show drift",
	RunE:  runReconcile,
}

var serverAdoptCmd = &cobra.Command{
	Use:   "adopt <instance-id>",
	Short: "Add an existing server instance to the local server list",
	Args:  cobra.ExactArgs(1),
	RunE:  runAdopt,
}

var serverStopCmd = &cobra.Command{
	Use:   "stop <name>",
	Short: "Halt a VPS (stops compute billing, keeps storage)",
	Args:  cobra.ExactArgs(1),
	RunE:  runStop,
}

var serverStartCmd = &cobra.Command{
	Use:   "start <name>",
	Short: "Start a halted VPS",
	Args:  cobra.ExactArgs(1),
	RunE:  runStart,
}

var useCmd = &cobra.Command{
	Use:   "use <name>",
	Short: "Set the active VPS",
	Args:  cobra.ExactArgs(1),
	RunE:  runUse,
}

var serverImportCmd = &cobra.Command{
	Use:   "import <hysteria2://...>",
	Short: "Add a server from a subscription URI (no mole-server required)",
	Args:  cobra.ExactArgs(1),
	RunE:  runImport,
}

func init() {
	serverCmd.AddCommand(serverDeployCmd)
	serverCmd.AddCommand(serverDestroyCmd)
	serverCmd.AddCommand(serverLsCmd)
	serverCmd.AddCommand(serverReconcileCmd)
	serverCmd.AddCommand(serverAdoptCmd)
	serverCmd.AddCommand(serverStopCmd)
	serverCmd.AddCommand(serverStartCmd)
	serverCmd.AddCommand(serverImportCmd)

	serverDeployCmd.Flags().String("region", "nrt", "server region (nrt, sgp, lax, ...)")
	serverDeployCmd.Flags().String("plan", "vc2-1c-1gb", "server plan")
	serverDeployCmd.Flags().String("name", "", "Server name (auto-generated if empty)")
	serverDeployCmd.Flags().Int("port", 443, "Hysteria2 port")

	serverDestroyCmd.Flags().Bool("force-local", false, "Remove from local list even if the server DELETE fails (use only when the remote instance is verified gone)")

	serverAdoptCmd.Flags().String("name", "", "Local name for the adopted server (defaults to the server's label)")
	serverAdoptCmd.Flags().Int("port", 443, "Hysteria2 port the adopted instance listens on")
	serverAdoptCmd.Flags().String("password", "", "Hysteria2 password (required — the server does not retain this)")

	serverImportCmd.Flags().String("name", "", "Local name (defaults to the URI's #fragment)")
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

	fmt.Printf("🚀 Deploying %s in %s (%s, port %d)\n", name, region, plan, port)

	resp, err := serverAPI("POST", "/instances", map[string]any{
		"region":      region,
		"plan":        plan,
		"label":       name,
		"port":        port,
		"enable_ipv6": true,
	})
	if err != nil {
		return fmt.Errorf("deploy: %w", err)
	}

	var created struct {
		Instance struct {
			ID       string `json:"id"`
			Password string `json:"password"`
		} `json:"instance"`
	}
	if err := json.Unmarshal(resp, &created); err != nil {
		return err
	}
	instanceID := created.Instance.ID
	password := created.Instance.Password
	fmt.Printf("✅ Instance %s created — waiting for IP...\n", instanceID)

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
		fmt.Printf("    The instance IS running. Recover it with:\n")
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
		resp, err := serverAPI("GET", "/instances/"+instanceID, nil)
		if err == nil {
			var r struct {
				MainIP string `json:"main_ip"`
				V6IP   string `json:"v6_main_ip"`
			}
			if json.Unmarshal(resp, &r) == nil {
				if ip := r.MainIP; ip != "" && ip != "0.0.0.0" {
					return ip, r.V6IP, nil
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
		_, err := serverAPI("DELETE", "/instances/"+target.InstanceID, nil)
		switch {
		case err == nil:
			// Remote delete succeeded — fall through and remove local entry.
		case IsServerNotFound(err):
			// Already gone; safe to drop the local entry.
			fmt.Printf("ℹ️  Server says %s is already gone — removing local entry\n", target.InstanceID)
		case forceLocal:
			fmt.Printf("⚠️  Server DELETE failed: %v\n", err)
			fmt.Printf("    --force-local set; removing local entry anyway. Verify the remote instance is really gone before forcing removal.\n")
		default:
			return fmt.Errorf("server delete failed: %w\n    The local entry was NOT removed. If the instance is really gone on the server, retry with --force-local.", err)
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
		if s.Stopped {
			status = "stopped"
		} else if !s.Ready() {
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
	remote, err := serverListInstances()
	if err != nil {
		return fmt.Errorf("list server instances: %w", err)
	}

	localByID := make(map[string]Server, len(local))
	for _, s := range local {
		if s.InstanceID != "" {
			localByID[s.InstanceID] = s
		}
	}
	remoteByID := make(map[string]serverInstance, len(remote))
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
		fmt.Println("(no instances on local or server)")
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
		return fmt.Errorf("--password is required: the server does not retain the Hysteria2 password, you must supply the one used when the instance was created")
	}

	resp, err := serverAPI("GET", "/instances/"+instanceID, nil)
	if err != nil {
		return fmt.Errorf("fetch instance: %w", err)
	}
	var r struct {
		ID     string `json:"id"`
		Label  string `json:"label"`
		Region string `json:"region"`
		MainIP string `json:"main_ip"`
		V6IP   string `json:"v6_main_ip"`
	}
	if err := json.Unmarshal(resp, &r); err != nil {
		return err
	}
	if name == "" {
		name = r.Label
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
		InstanceID: r.ID,
		Region:     r.Region,
		IP:         r.MainIP,
		IPv6:       r.V6IP,
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

// runImport adds a server from a subscription URI without contacting
// mole-server — the inverse of the link mole-server (or any hy2 endpoint) hands
// out. This is the path that lets mole act as a pure client: paste the URI,
// connect, no control plane required.
func runImport(cmd *cobra.Command, args []string) error {
	uri := args[0]
	name, _ := cmd.Flags().GetString("name")

	srv, err := serverFromURI(uri)
	if err != nil {
		return err
	}
	if name != "" {
		srv.Name = name
	}
	if srv.Name == "" {
		srv.Name = serverHost(*srv)
		if srv.Name == "" {
			srv.Name = "imported"
		}
	}

	servers, _ := loadServers()
	if indexByName(servers, srv.Name) >= 0 {
		return fmt.Errorf("local server %q already exists; pass --name to use a different one", srv.Name)
	}
	for _, s := range servers {
		if serverHost(s) == serverHost(*srv) && s.Port == srv.Port {
			return fmt.Errorf("a server at %s:%d already exists as %q", serverHost(*srv), srv.Port, s.Name)
		}
	}
	if len(servers) == 0 {
		srv.Active = true
	}
	srv.CreatedAt = time.Now().Format(time.RFC3339)
	servers = append(servers, *srv)
	if err := saveServers(servers); err != nil {
		return err
	}
	fmt.Printf("✅ Imported %q (%s:%d)\n", srv.Name, serverHost(*srv), srv.Port)
	return nil
}

// serverHost returns the effective connect address (IPv4 preferred, else IPv6)
// so dedupe and display don't trip over the IP/IPv6 split in the struct.
func serverHost(s Server) string {
	if s.IP != "" {
		return s.IP
	}
	return s.IPv6
}

// serverFromURI is the inverse of Server.URI(): it parses a hysteria2
// subscription link into a Server entry, round-tripping every field Server.URI
// emits (password, host, port, obfs, bandwidth ceilings, peak window, name).
// It never talks to mole-server.
func serverFromURI(raw string) (*Server, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid URI: %w", err)
	}
	if u.Scheme != "hy2" && u.Scheme != "hysteria2" {
		return nil, fmt.Errorf("unsupported scheme %q: import supports hysteria2:// links only", u.Scheme)
	}
	password := u.User.Username()
	if password == "" {
		return nil, fmt.Errorf("URI missing password")
	}
	// url.Hostname()/Port() correctly strip and split bracketed IPv6
	// (e.g. [2606:…]:8443); Server.URI() always emits IPv6 bracketed, so the
	// link mole-server hands out parses back cleanly here.
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("URI missing host")
	}
	port, _ := strconv.Atoi(u.Port())
	if port == 0 {
		port = 443
	}
	srv := &Server{
		Password: password,
		Port:     uint16(port),
	}
	// Preserve the address family so the connect path binds the right one and
	// Server.URI() round-trips byte-for-byte.
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		srv.IPv6 = host
	} else {
		srv.IP = host
	}
	q := u.Query()
	if v := q.Get("obfs"); v != "" {
		srv.Obfs = v
		srv.ObfsPassword = q.Get("obfs-password")
	}
	if v := q.Get("upmbps"); v != "" {
		srv.UpMbps, _ = strconv.Atoi(v)
	}
	if v := q.Get("downmbps"); v != "" {
		srv.DownMbps, _ = strconv.Atoi(v)
	}
	if v := q.Get("peakdownmbps"); v != "" {
		srv.PeakDownMbps, _ = strconv.Atoi(v)
	}
	if v := q.Get("peakupmbps"); v != "" {
		srv.PeakUpMbps, _ = strconv.Atoi(v)
	}
	if v := q.Get("peakstart"); v != "" {
		srv.PeakStartHour, _ = strconv.Atoi(v)
	}
	if v := q.Get("peakend"); v != "" {
		srv.PeakEndHour, _ = strconv.Atoi(v)
	}
	if frag := u.Fragment; frag != "" {
		srv.Name = frag
	}
	return srv, nil
}

// ── stop ─────────────────────────────────────────────────────

func runStop(_ *cobra.Command, args []string) error {
	name := args[0]
	servers, err := loadServers()
	if err != nil {
		return fmt.Errorf("read local servers: %w", err)
	}
	idx := indexByName(servers, name)
	if idx < 0 {
		return fmt.Errorf("server %q not found", name)
	}
	target := servers[idx]

	if target.Stopped {
		fmt.Printf("ℹ️  %s is already stopped\n", name)
		return nil
	}

	if target.InstanceID == "" {
		return fmt.Errorf("server %q has no instance ID", name)
	}

	fmt.Printf("⏸️  Halting %s...\n", name)
	_, err = serverAPI("POST", "/instances/"+target.InstanceID+"/halt", nil)
	if err != nil {
		return fmt.Errorf("halt instance: %w", err)
	}

	servers[idx].Stopped = true
	if err := saveServers(servers); err != nil {
		return err
	}
	fmt.Printf("✅ %s halted (storage preserved, ~$0.5/mo)\n", name)
	fmt.Println("   Resume with: mole server start " + name)
	return nil
}

// ── start ────────────────────────────────────────────────────

func runStart(_ *cobra.Command, args []string) error {
	name := args[0]
	servers, err := loadServers()
	if err != nil {
		return fmt.Errorf("read local servers: %w", err)
	}
	idx := indexByName(servers, name)
	if idx < 0 {
		return fmt.Errorf("server %q not found", name)
	}
	target := servers[idx]

	if !target.Stopped {
		fmt.Printf("ℹ️  %s is already running\n", name)
		return nil
	}

	if target.InstanceID == "" {
		return fmt.Errorf("server %q has no instance ID", name)
	}

	fmt.Printf("▶️  Starting %s...\n", name)
	_, err = serverAPI("POST", "/instances/"+target.InstanceID+"/start", nil)
	if err != nil {
		return fmt.Errorf("start instance: %w", err)
	}

	// Wait for the instance to be running
	fmt.Println("   Waiting for instance to be ready...")
	for i := 0; i < 30; i++ {
		resp, err := serverAPI("GET", "/instances/"+target.InstanceID, nil)
		if err == nil {
			var r struct {
				Status string `json:"status"`
			}
			if json.Unmarshal(resp, &r) == nil && r.Status == "active" {
				break
			}
		}
		time.Sleep(2 * time.Second)
	}

	servers[idx].Stopped = false
	if err := saveServers(servers); err != nil {
		return err
	}
	fmt.Printf("✅ %s started\n", name)
	fmt.Println("   Run `mole up` to connect.")
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
// invisible orphans on the server.
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
			if servers[i].Stopped {
				return nil, fmt.Errorf("active server %q is stopped — run `mole server start %s` to resume", servers[i].Name, servers[i].Name)
			}
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

// ── mole-server API ────────────────────────────────────────────

const moleServerBase = "https://mole.taprun.dev/api"

// ServerError carries the HTTP status from a failed mole-server API call so
// callers can distinguish "instance is gone" (404) from generic transport /
// 5xx failures. Without this distinction, `mole server destroy` cannot safely
// decide whether to drop the local entry.
type ServerError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *ServerError) Error() string {
	return fmt.Sprintf("server %s %s: %d %s", e.Method, e.Path, e.Status, e.Body)
}

// IsServerNotFound reports whether err is a 404 from the mole-server API.
func IsServerNotFound(err error) bool {
	var se *ServerError
	return errors.As(err, &se) && se.Status == 404
}

func moleServerToken() (string, error) {
	if k := strings.TrimSpace(os.Getenv("MOLE_SERVER_TOKEN")); k != "" {
		return k, nil
	}
	return "", fmt.Errorf("MOLE_SERVER_TOKEN environment variable is not set")
}

func serverAPI(method, path string, body any) ([]byte, error) {
	token, err := moleServerToken()
	if err != nil {
		return nil, err
	}
	var reqBody io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reqBody = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, moleServerBase+path, reqBody)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, &ServerError{Method: method, Path: path, Status: resp.StatusCode, Body: string(data)}
	}
	return data, nil
}

// serverInstance is the subset of /api/instances list fields reconcile needs.
type serverInstance struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Region string `json:"region"`
	MainIP string `json:"main_ip"`
}

// serverListInstances fetches all instances via mole-server.
func serverListInstances() ([]serverInstance, error) {
	data, err := serverAPI("GET", "/instances", nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Instances []serverInstance `json:"instances"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return resp.Instances, nil
}
