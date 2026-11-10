package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/LeonTing1010/mole/core"
	"github.com/LeonTing1010/mole/utils"
	"github.com/spf13/cobra"
)

// Server is one deployed VPS running Hysteria2.
type Server struct {
	DeployID     string            `json:"deploy_id,omitempty"`
	Verification *nodeVerification `json:"verification,omitempty"`
	Name         string            `json:"name"`
	InstanceID   string            `json:"instance_id"`
	Region       string            `json:"region"`
	IP           string            `json:"ip"`
	IPv6         string            `json:"ip_v6,omitempty"`
	Port         uint16            `json:"port"`
	Password     string            `json:"password"`
	CreatedAt    string            `json:"created_at"`
	Active       bool              `json:"active,omitempty"`
	Stopped      bool              `json:"stopped,omitempty"` // true when halted on the server (no compute billing, storage only)
	// Optional per-server tuning. All opt-in: zero values reproduce the previous
	// URI byte-for-byte. Obfs/ObfsPassword turn on hysteria2 salamander
	// obfuscation (must match the server, or the tunnel goes dark). UpMbps/DownMbps
	// pin Brutal congestion control to a measured ceiling instead of the
	// conservative default in core — set them to ~80% of a real speed test, or to
	// -1 to disable Brutal and let hysteria2 use adaptive BBR.
	Obfs         string `json:"obfs,omitempty"`
	ObfsPassword string `json:"obfs_password,omitempty"`
	UpMbps       int    `json:"up_mbps,omitempty"`
	DownMbps     int    `json:"down_mbps,omitempty"`
	// RealityDownMbps paces the node's TCP Brutal sender independently of the
	// hysteria2 ladder. Zero keeps the historical shared-ceiling behavior.
	RealityDownMbps int `json:"reality_down_mbps,omitempty"`

	// TLS identity. TLSCertPEM is the server's leaf certificate, pinned by the
	// client so the tunnel verifies without a domain, a CA, or a Certificate
	// Transparency entry announcing this host to the world. SNI must match a SAN
	// on that certificate — Go dropped Common-Name fallback in 1.15, so a
	// CN-only cert fails verification the moment pinning is switched on.
	//
	// Empty on servers provisioned before pinning existed: those fall back to
	// the old unverified handshake, which `config.Validate` warns about.
	TLSCertPEM string `json:"tls_cert_pem,omitempty"`
	SNI        string `json:"sni,omitempty"`

	// Time-of-day Brutal ceiling. When PeakDownMbps>0, `mole up` emits a
	// peak/off-peak selector and the supervisor flips it on the local clock with
	// no reconnect — UpMbps/DownMbps become the OFF-peak ceiling and these the
	// peak one (set to ~the worst-case peak link speed so Brutal stops flooding).
	// Hours are local 24h; leaving both at 0 uses the default 12:00–02:00 window.
	PeakUpMbps    int `json:"peak_up_mbps,omitempty"`
	PeakDownMbps  int `json:"peak_down_mbps,omitempty"`
	PeakStartHour int `json:"peak_start_hour,omitempty"`
	PeakEndHour   int `json:"peak_end_hour,omitempty"`

	// Port hopping, OFF by default — set these two to turn it on for a server.
	// HopPorts is an inclusive UDP range ("20000-50000") the server also answers
	// on; the client moves between those ports every HopSeconds. Every node mole
	// deploys has the range open and NAT'd already, so switching it on is an
	// edit here and a reconnect, with nothing to do server-side.
	//
	// It is off by default because it is not free, which measurement settled
	// rather than intuition. A/B on the icn node, 2026-08-22, eight sequential
	// 100MB downloads each way, same link, same ceiling (median seconds):
	//
	//	hopping off       17.4s   (~48 Mbps)
	//	hop every 120s    25.0s   (~33 Mbps, -28%)
	//	hop every 30s     ~90s+   (~4 Mbps, -90%)
	//
	// The cost is per hop, so it scales with how often you hop: each new source
	// port is a peer-address change the server's QUIC has to re-validate, and
	// Brutal's send rate restarts from that point. Rotating the source port is
	// also no answer to a five-tuple block that keys on something coarser: on
	// 2026-08-27 four such redials each died after exactly the 30s QUIC idle
	// timeout, and only the REALITY leg was untouched.
	// Reach for Proto instead when UDP itself is what is being singled out.
	HopPorts   string `json:"hop_ports,omitempty"`
	HopSeconds int    `json:"hop_seconds,omitempty"`

	// Proto selects the tunnel protocol. Empty means hysteria2, so every server
	// recorded before the REALITY leg existed keeps behaving exactly as it did.
	//
	// The two legs answer different failure modes on the same path. hysteria2
	// rides UDP/443 and wins on a lossy link, because Brutal holds a fixed send
	// rate straight through loss — but this path repeatedly meets a UDP-specific
	// QoS penalty that no ceiling can outrun, and that no restart clears.
	// VLESS+REALITY rides TCP/443 and looks, byte for byte, like an ordinary TLS
	// 1.3 handshake to RealityDest, which is what makes it worth having when the
	// UDP leg is being singled out.
	Proto string `json:"proto,omitempty"`

	// REALITY parameters. All empty on a hysteria2 server. RealityPubKey is the
	// public half of a keypair whose private half exists only on the VPS;
	// RealityShortID must be one the server accepts; RealityDest is the site
	// whose TLS handshake the server borrows and doubles as the SNI the client
	// sends.
	UUID           string `json:"uuid,omitempty"`
	RealityPubKey  string `json:"reality_public_key,omitempty"`
	RealityShortID string `json:"reality_short_id,omitempty"`
	RealityDest    string `json:"reality_dest,omitempty"`
	// RealityPort is the TCP port the REALITY leg listens on, when it differs
	// from Port. Zero means the same number — which is the normal case, because
	// a dual-leg node runs hysteria2 on UDP/443 and REALITY on TCP/443 and the
	// two never collide.
	RealityPort uint16 `json:"reality_port,omitempty"`
	// RealityMultiplex is an explicit client opt-in after the node's VLESS
	// inbound has multiplex.enabled=true. Absent on older nodes and imports.
	RealityMultiplex bool `json:"reality_multiplex,omitempty"`
}

// Tunnel protocols. protoHysteria2 is also what an empty Proto means.
const (
	protoHysteria2 = "hysteria2"
	protoReality   = "reality"
)

// isReality reports whether this server speaks VLESS+REALITY.
func (s *Server) isReality() bool { return s.Proto == protoReality }

// Default Brutal ceiling and peak window (local 24h) for a server that carries
// no explicit ladder.
//
// A server with no ceiling on file used to be silently catastrophic. Two places
// disagreed about what "unset" meant: parseHysteria2 substituted 20/50, while
// Schedule() reported 0. The config therefore materialised a full ladder whose
// default member was the TOP rung (proxy-bw-50), while the supervisor's
// scheduler stayed asleep and `mole ceiling` refused to run — so nothing could
// ever move it down, and `mole status` reported a healthy tunnel flooding a path
// that sustains 3 Mbps. These constants are now the single source of truth for
// both sides; see ladder().
//
// They are deliberately conservative. Brutal ignores loss by design, so a
// ceiling above the real link floods it into repeated collapse, while one below
// merely leaves throughput on the table. A node whose link has never been
// measured must not start at its ceiling — raise it with `mole ceiling` once the
// path is known.
const (
	defaultUpMbps       = 5
	defaultDownMbps     = 20
	defaultPeakUpMbps   = 2
	defaultPeakDownMbps = 5

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

// ladder resolves this server's effective Brutal ceiling, filling in the
// defaults above for anything unset. Both URI() and Schedule() go through it, so
// the outbound sing-box dials and the ladder the supervisor schedules over can
// never describe different ceilings.
//
// A negative UpMbps/DownMbps is the BBR sentinel and is passed through
// untouched: it means "no declared bandwidth", which is a deliberate choice, not
// an unset field, and substituting a default would silently override it.
func (s *Server) ladder() (up, down, peakUp, peakDown, start, end int) {
	// REALITY has no Brutal ceiling to ladder over: it is TCP, so congestion
	// control belongs to the kernel at both ends. Returning zeros keeps the
	// selector out of the config and leaves the supervisor's clock-driven flip
	// with nothing to flip — rather than laddering over rungs that don't exist.
	if s.isReality() {
		return 0, 0, 0, 0, 0, 0
	}
	if s.UpMbps < 0 || s.DownMbps < 0 {
		return s.UpMbps, s.DownMbps, 0, 0, 0, 0
	}
	up, down = s.UpMbps, s.DownMbps
	if up == 0 {
		up = defaultUpMbps
	}
	if down == 0 {
		down = defaultDownMbps
	}
	peakUp, peakDown = s.PeakUpMbps, s.PeakDownMbps
	if peakDown == 0 {
		peakUp, peakDown = defaultPeakUpMbps, defaultPeakDownMbps
	} else if peakUp == 0 {
		peakUp = defaultPeakUpMbps
	}
	start, end = s.peakWindow()
	return up, down, peakUp, peakDown, start, end
}

// Schedule builds the supervisor's time-of-day bandwidth schedule for this
// server, over the same effective ceiling the outbound is built from.
func (s *Server) Schedule() core.BandwidthSchedule {
	up, down, peakUp, peakDown, start, end := s.ladder()
	return core.BandwidthSchedule{
		OffUp: up, OffDown: down,
		PeakUp: peakUp, PeakDown: peakDown,
		StartHour: start, EndHour: end,
	}
}

// HasAddress reports address assignment, independently of service readiness. Servers
// recorded mid-deploy may exist with InstanceID set but IP still empty until
// `waitForIP` succeeds.
func (s *Server) HasAddress() bool { return s.IP != "" || s.IPv6 != "" }

func (s Server) deployStatus() string {
	if s.Stopped {
		return "stopped"
	}
	if !s.HasAddress() {
		return "provisioning"
	}
	if s.Verification == nil {
		return "unverified"
	}
	return s.Verification.Status
}

// linkHost is the address a share link carries: IPv4 when the node has one,
// IPv6 otherwise, bracketed the way a URL's authority requires.
func (s *Server) linkHost() string {
	host := s.IP
	if host == "" {
		host = s.IPv6
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return host
}

// URI returns the hy2:// URI sing-box consumes. Obfs params are appended only
// when set; the bandwidth ceiling is always emitted, defaults included, so the
// parser never has to invent one of its own.
func (s *Server) URI() string {
	host := s.linkHost()
	if s.isReality() {
		return s.realityURI(host)
	}
	// Identity layer. A pinned certificate replaces the old blanket
	// `insecure=1`, and the SNI travels with it because it has to match a SAN on
	// the pinned cert. The legacy shape (`insecure=1&sni=bing.com`) is kept
	// byte-for-byte for servers with no cert on file, so existing entries in
	// servers.json keep producing the exact URI they always did.
	//
	// bing.com as the default SNI is the thing being retired: claiming to be
	// Bing while addressing a Vultr IP is a one-rule DPI catch, and the
	// self-signed "Bing" cert an active prober gets back is conclusive. New
	// servers get a random, unbranded SAN instead.
	sni := s.SNI
	if sni == "" {
		sni = "bing.com"
	}
	var q string
	if s.TLSCertPEM != "" {
		q = "sni=" + url.QueryEscape(sni) +
			"&pinnedcert=" + base64.RawURLEncoding.EncodeToString([]byte(s.TLSCertPEM))
	} else {
		q = "insecure=1&sni=" + url.QueryEscape(sni)
	}
	if s.Obfs != "" {
		q += "&obfs=" + url.QueryEscape(s.Obfs)
		if s.ObfsPassword != "" {
			q += "&obfs-password=" + url.QueryEscape(s.ObfsPassword)
		}
	}
	// The ceiling is always emitted, defaults included. Leaving it off for an
	// untuned server is what let parseHysteria2 substitute its own, much higher,
	// default — the config and the supervisor then laddered over different
	// numbers and the tunnel ran at the top rung with no way down. An explicit
	// URI costs a few bytes and makes that divergence unrepresentable.
	//
	// The negative sentinel (-1) passes through as-is: the parser reads it as
	// "disable Brutal, use BBR", and ladder() leaves it alone.
	up, down, peakUp, peakDown, start, end := s.ladder()
	q += fmt.Sprintf("&upmbps=%d&downmbps=%d", up, down)
	// Peak profile params drive the time-of-day selector in config.Build.
	if peakDown > 0 {
		q += fmt.Sprintf("&peakdownmbps=%d&peakupmbps=%d&peakstart=%d&peakend=%d",
			peakDown, peakUp, start, end)
	}
	// Port hopping, when the server was set up to answer on a range. Omitted
	// entirely otherwise, so servers without it keep their exact old URI.
	if s.HopPorts != "" {
		q += "&ports=" + url.QueryEscape(s.HopPorts)
		if s.HopSeconds > 0 {
			q += fmt.Sprintf("&hop=%d", s.HopSeconds)
		}
	}
	return fmt.Sprintf("hy2://%s@%s:%d?%s#%s", s.Password, host, s.Port, q, s.Name)
}

// realityURI renders the standard vless:// link for a REALITY node.
//
// The parameter names are the ones every VLESS client already speaks — pbk,
// sid, fp, flow — rather than mole-specific spellings, so the same link works
// in any client and a link from anywhere else works here.
//
// Nothing secret travels in it beyond the uuid: pbk is a public key and sid is
// an identifier the server accepts, neither of which is enough to impersonate
// the server. That is the practical difference from the hysteria2 link, which
// carries the tunnel password and the obfs key.
// hasStandbyLeg reports whether this hysteria2 node ALSO runs the REALITY leg.
//
// It is the dual-leg shape: one VPS, hysteria2 on UDP/443 and VLESS+REALITY on
// TCP/443. A node whose Proto IS reality has no standby — it is already the leg
// that survives a UDP block, and there is nothing to fall back to.
func (s *Server) hasStandbyLeg() bool {
	return !s.isReality() && s.UUID != "" && s.RealityPubKey != ""
}

// StandbyURI renders the vless:// link for the co-hosted REALITY leg, or "" when
// this node has none. It is what config.BuildWithStandby turns into the second
// member of the proxy selector.
func (s *Server) StandbyURI() string {
	if !s.hasStandbyLeg() {
		return ""
	}
	return s.realityURI(s.linkHost())
}

// realityAddr is the REALITY leg's ip:port, or "" when the node has none.
func (s *Server) realityAddr() string {
	if !s.hasStandbyLeg() && !s.isReality() {
		return ""
	}
	host := s.IP
	if host == "" {
		host = s.IPv6
	}
	if host == "" {
		return ""
	}
	port := s.RealityPort
	if port == 0 {
		port = s.Port
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("%s:%d", host, port)
}

func (s *Server) realityURI(host string) string {
	port := s.RealityPort
	if port == 0 {
		port = s.Port
	}
	dest := s.RealityDest
	if dest == "" {
		dest = defaultRealityDest
	}
	q := url.Values{
		"encryption": {"none"},
		"security":   {"reality"},
		"type":       {"tcp"},
		// xtls-rprx-vision splices the inner TLS stream through after the
		// handshake instead of re-encrypting every record inside the tunnel.
		// It is what makes the flow's packet-length profile look like the TLS
		// session it is imitating rather than like a proxy.
		"flow": {"xtls-rprx-vision"},
		// A Chrome ClientHello, forged by uTLS. This is not cosmetic: REALITY
		// authenticates by hiding data in a field of the ClientHello, so the
		// hello has to be one the borrowed site would plausibly receive.
		"fp":  {"chrome"},
		"sni": {dest},
		"pbk": {s.RealityPubKey},
	}
	if s.RealityShortID != "" {
		q.Set("sid", s.RealityShortID)
	}
	if s.RealityMultiplex {
		q.Set("mux", "smux")
	}
	return fmt.Sprintf("vless://%s@%s:%d?%s#%s", s.UUID, host, port, q.Encode(), s.Name)
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

var reconcileDestroyRemote bool

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
	Use:   "import <hysteria2://... | vless://...?security=reality>",
	Short: "Add a server from a hysteria2:// or vless:// (REALITY) URI",
	Args:  cobra.ExactArgs(1),
	RunE:  runImport,
}

func init() {
	serverCmd.AddCommand(serverDeployCmd)
	serverCmd.AddCommand(serverDestroyCmd)
	serverCmd.AddCommand(serverLsCmd)
	serverCmd.AddCommand(serverReconcileCmd)
	serverCmd.AddCommand(serverAdoptCmd)
	serverReconcileCmd.Flags().BoolVar(&reconcileDestroyRemote, "destroy-remote", false,
		"destroy remote-only (orphaned) instances via the Vultr API — no hy2 password needed; the API key authorizes the delete")
	serverCmd.AddCommand(serverStopCmd)
	serverCmd.AddCommand(serverStartCmd)
	serverCmd.AddCommand(serverImportCmd)

	// Seoul, not Tokyo. Measured from a China Mobile Shanghai line on 2026-07-31,
	// all three sampled in the same minute: icn 0% loss / 107ms median, nrt 5.8%
	// loss / 180ms, itm 4.9% / 308ms. Tokyo's transit hauls the traffic through
	// PCCW with a 165ms floor — roughly three times the physical distance — and
	// that sustained loss is what the tunnel kept collapsing on.
	serverDeployCmd.Flags().String("region", "icn", "server region (icn, nrt, sgp, lax, ...)")
	serverDeployCmd.Flags().DurationVar(&deployTimeout, "timeout", nodeVerifyBudget, "maximum wait for address and authenticated protocol verification")
	serverDeployCmd.Flags().BoolVar(&deployNoVerify, "no-verify", false, "do not wait for both legs to answer from outside the tunnel")
	serverDeployCmd.Flags().BoolVar(&deployNoReality, "no-reality", false, "do not install the REALITY standby leg (saves ~100MB RAM; use when hy2 is stable on this path)")
	serverDeployCmd.Flags().String("plan", "vc2-1c-1gb", "server plan")
	serverDeployCmd.Flags().String("name", "", "Server name (auto-generated if empty)")
	serverDeployCmd.Flags().Int("port", 443, "tunnel port")
	serverDeployCmd.Flags().String("proto", protoHysteria2,
		"tunnel protocol: hysteria2 (UDP/443, Brutal congestion control) or reality (VLESS+REALITY over TCP/443)")
	serverDeployCmd.Flags().String("dest", defaultRealityDest,
		"REALITY handshake target — the site whose TLS handshake the node borrows (reality only)")

	serverDestroyCmd.Flags().Bool("force-local", false, "Remove from local list even if the server DELETE fails (use only when the remote instance is verified gone)")

	serverAdoptCmd.Flags().String("name", "", "Local name for the adopted server (defaults to the server's label)")
	serverAdoptCmd.Flags().Int("port", 443, "Hysteria2 port the adopted instance listens on")
	serverAdoptCmd.Flags().String("password", "", "Hysteria2 password (required — the server does not retain this)")

	serverImportCmd.Flags().String("name", "", "Local name (defaults to the URI's #fragment)")
}

// ── deploy ─────────────────────────────────────────────────────

func runDeploy(cmd *cobra.Command, _ []string) error {
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if deployTimeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}

	region, _ := cmd.Flags().GetString("region")
	plan, _ := cmd.Flags().GetString("plan")
	name, _ := cmd.Flags().GetString("name")
	port, _ := cmd.Flags().GetInt("port")
	proto, _ := cmd.Flags().GetString("proto")
	dest, _ := cmd.Flags().GetString("dest")

	switch strings.ToLower(strings.TrimSpace(proto)) {
	case "", protoHysteria2, "hy2":
		proto = protoHysteria2
	case protoReality, "vless":
		proto = protoReality
	default:
		return fmt.Errorf("deploy: unknown --proto %q (want %q or %q)", proto, protoHysteria2, protoReality)
	}
	if name == "" {
		name = fmt.Sprintf("%s-%d", region, time.Now().Unix())
	}

	fmt.Printf("🚀 Deploying %s in %s (%s, %s on port %d)\n", name, region, plan, proto, port)

	if port < 1 || port > 65535 {
		return fmt.Errorf("port must be in 1..65535")
	}
	servers, err := loadServers()
	if err != nil {
		return err
	}
	if indexByName(servers, name) >= 0 {
		return fmt.Errorf("server %q already exists; verify that instance instead", name)
	}
	deployID, err := randomHexString(16)
	if err != nil {
		return err
	}
	srv := Server{Name: name, Region: region, Port: uint16(port), Proto: proto, DeployID: deployID}
	configureDeployCeiling(&srv)
	var script string

	// Identity material is minted here, not on the VPS, for both protocols: the
	// client needs its half in order to connect at all, and anything generated
	// on the far side could only be read back over the very channel it is meant
	// to secure. The secret half does transit the provisioning API — which
	// already mints and carries the tunnel password, so this adds no new trust
	// boundary.
	if proto == protoReality {
		id, err := newRealityIdentity(dest)
		if err != nil {
			return fmt.Errorf("deploy: generate reality identity: %w", err)
		}
		script = realityBootScript(port, srv.DownMbps, id)
		configureDeployedReality(&srv, id)
	} else {
		id, err := newServerIdentity()
		if err != nil {
			return fmt.Errorf("deploy: generate server identity: %w", err)
		}
		password, err := tunnelPassword()
		if err != nil {
			return fmt.Errorf("deploy: generate tunnel password: %w", err)
		}
		script = hy2BootScript(password, port, id)
		srv.Password = password
		srv.TLSCertPEM = id.CertPEM
		srv.SNI = id.SNI
		srv.Obfs = "salamander"
		srv.ObfsPassword = id.ObfsPassword

		// Every hysteria2 node also gets the REALITY leg, on TCP/443 beside it.
		// Not a flag: the failure it answers — UDP/443 throttled per-flow while
		// the link tests clean — is a property of the PATH, so it will be just
		// as true of the node that replaces this one, and with
		// destroy-on-lid-close that is most mornings. A standby leg you have to
		// remember to ask for is one that is missing on the morning it matters.
		//
		// It costs nothing until used: sing-box sits idle on a port the firewall
		// group already opens, and the client's selector only routes to it once
		// the supervisor has measured hy2 carrying nothing.
		//
		// --no-reality skips it to save ~100MB RAM on small plans (0.5gb).
		if !deployNoReality {
			if rid, err := newRealityIdentity(dest); err != nil {
				// Not fatal. A node with one working leg is worth having; a deploy
				// that aborted over the spare would leave none.
				fmt.Printf("⚠️  Standby REALITY leg skipped: %v (the hysteria2 leg is unaffected)\n", err)
			} else {
				script = dualBootScript(password, port, srv.DownMbps, id, rid)
				configureDeployedReality(&srv, rid)
			}
		} else {
			fmt.Println("   🎭 REALITY standby leg skipped (--no-reality)")
		}
	}

	// Provisioning talks to Vultr from this machine, and only from here. The
	// Worker that used to offer a second route could never actually provision:
	// Vultr's API access control allowlists source addresses and a Cloudflare
	// Worker has no stable egress to allowlist, so every call from it came back
	// 403. Keeping it as an alternative meant every deploy could fail in two
	// different ways, one of them silently.
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(script)))
	session, err := newDeploySession(srv, digest)
	if err != nil {
		return err
	}
	defer session.log.Close()
	fmt.Printf("Deployment receipt: %s\n", filepath.Join(session.dir, "receipt.json"))
	return runDeployLifecycle(ctx, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		instanceID, err := deployViaVultr(name, region, plan, script, session, ctx)
		if err != nil {
			return err
		}
		srv.InstanceID = instanceID
		session.record.InstanceID = instanceID
		if err = session.stage("created", nil); err != nil {
			return fmt.Errorf("instance %s created but receipt write failed: %w", instanceID, err)
		}
		_ = stashVultrKey()
		return finishDeploy(ctx, srv, session)
	}, func() error { return session.cleanupInstance(srv) })
}

// finishDeploy records a freshly created instance locally, waits for its IP and
// reports the result.
//
// srv arrives carrying everything protocol-specific the caller minted — the
// identity the boot script was rendered from, and nothing the far end did not
// receive. That ordering matters: recording a pin, an obfs key or a REALITY
// public key the server never installed doesn't leave the tunnel merely
// unverified, it blackholes it, on a box reachable no other way.
// Only fresh templates explicitly install server multiplex support. Imports
// retain their existing opt-in; deployment verification qualifies real delivery.
func configureDeployedReality(srv *Server, id *realityIdentity) {
	srv.UUID, srv.RealityPubKey = id.UUID, id.PublicKey
	srv.RealityShortID, srv.RealityDest = id.ShortID, id.Dest
	srv.RealityMultiplex = true
}

func configureDeployCeiling(srv *Server) {
	if srv.isReality() {
		return
	}
	srv.UpMbps, srv.DownMbps = core.DefaultUpMbps, core.DefaultDownMbps
	srv.PeakUpMbps, srv.PeakDownMbps = core.DefaultPeakUpMbps, core.DefaultPeakDownMbps
	srv.PeakStartHour, srv.PeakEndHour = defaultPeakStartHour, defaultPeakEndHour
	if l, ok := recallLadder(srv.Region); ok {
		srv.UpMbps, srv.DownMbps = l.UpMbps, l.DownMbps
		srv.PeakUpMbps, srv.PeakDownMbps = l.PeakUpMbps, l.PeakDownMbps
		srv.RealityDownMbps = l.RealityDownMbps
		srv.PeakStartHour, srv.PeakEndHour = l.PeakStartHour, l.PeakEndHour
	}
}

func finishDeploy(parent context.Context, srv Server, session *deploySession) error {
	srv.CreatedAt = time.Now().Format(time.RFC3339)
	if err := appendServer(srv); err != nil {
		_ = session.stage("local_record_failed", err)
		return fmt.Errorf("instance %s exists; recovery identity is in %s: %w", srv.InstanceID, session.dir, err)
	}
	ctx, cancel := context.WithTimeout(parent, deployTimeout)
	defer cancel()
	if err := session.stage("waiting_for_address", nil); err != nil {
		return err
	}
	ip, ipv6, err := waitForIP(ctx, srv.InstanceID)
	if err != nil {
		_ = session.stage("provisioning", err)
		return fmt.Errorf("instance %s retained and potentially billing; retry mole server verify %s; receipt %s: %w", srv.InstanceID, srv.Name, session.dir, err)
	}
	if err = changeServer(srv.InstanceID, srv.Name, func(current *Server) { current.IP = ip; current.IPv6 = ipv6; srv = *current }); err != nil {
		return err
	}
	fmt.Fprintf(session.writer(), "Address assigned: %s; waiting for server installation.\n", serverHost(srv))
	if deployNoVerify {
		return session.stage("unverified", nil)
	}
	if err = session.stage("verifying", nil); err != nil {
		return err
	}
	result, verifyErr := verifyNode(ctx, srv, session.writer())
	session.record.Verification = &result
	storeErr := changeServer(srv.InstanceID, srv.Name, func(s *Server) { s.Verification = &result })
	recordErr := session.stage(result.Status, verifyErr)
	if err = errors.Join(verifyErr, storeErr, recordErr); err != nil {
		return fmt.Errorf("instance %s retained and potentially billing; verify with mole server verify %s or destroy with mole server destroy %s; receipt %s: %w", srv.InstanceID, srv.Name, srv.Name, session.dir, err)
	}
	fmt.Fprintln(session.writer(), "Requested tunnels ready. Optional Brutal capability is checked separately by the client.")
	return nil
}

var (
	deployTimeout   time.Duration
	deployNoVerify  bool
	deployNoReality bool
)

// waitForIP polls until Vultr assigns the instance an address.
//
// The old bound was 60s, which Vultr routinely exceeds — and exceeding it was
// treated as a deploy failure that told the operator to go run `mole server
// reconcile --apply` by hand. Nothing had actually failed: the instance existed,
// its id was already recorded, and the only missing piece was a value that
// arrives on its own. Wait long enough to cover the normal case, and let
// reconcileAddresses pick up the rest without anyone being asked to.
func waitForIP(parent context.Context, instanceID string) (string, string, error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	started := time.Now()
	for attempt := 1; ctx.Err() == nil; attempt++ {
		resp, err := serverAPIContext(ctx, apiBudget, "GET", "/instances/"+instanceID, nil)
		if IsServerNotFound(err) {
			return "", "", err
		}
		if err == nil {
			if ip, v6 := parseInstanceAddress(resp); ip != "" {
				return ip, v6, nil
			}
		}
		if attempt%5 == 0 {
			fmt.Printf("   still waiting for an address (%ds)...\n", int(time.Since(started).Seconds()))
		}
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
	return "", "", fmt.Errorf("instance %s address unverified: %w", instanceID, ctx.Err())
}

// parseInstanceAddress pulls the addresses out of a single-instance GET,
// returning empty strings while Vultr has not assigned one yet.
//
// The envelope is the whole point. Vultr wraps this response in
// {"instance": {...}}, the same way the list endpoint wraps its array in
// {"instances": [...]} — and reading main_ip off the top level, as this used
// to, finds nothing no matter how long it waits. Every deploy burned the full
// five minutes and then reported no address for a machine that had been
// reachable for four of them, while `mole server reconcile` — which reads the
// list endpoint, correctly unwrapped — filled it in on the first try.
func parseInstanceAddress(resp []byte) (ip, ipv6 string) {
	var r struct {
		Instance struct {
			MainIP string `json:"main_ip"`
			V6IP   string `json:"v6_main_ip"`
		} `json:"instance"`
	}
	if json.Unmarshal(resp, &r) != nil {
		return "", ""
	}
	// Vultr reports 0.0.0.0 for "allocated, not assigned yet", which is not an
	// address anyone can connect to.
	if r.Instance.MainIP == "" || r.Instance.MainIP == "0.0.0.0" {
		return "", ""
	}
	return r.Instance.MainIP, r.Instance.V6IP
}

// reconcileAddresses fills in the address of every local entry that has an
// instance id but no IP, by asking Vultr. It is the deterministic half of
// `mole server reconcile` — an entry recorded mid-provision is missing a value
// that only one source can supply, and there is no judgement involved in
// copying it over. Anything ambiguous (an entry whose IP disagrees with the
// remote one, an instance with no local entry) is deliberately left alone for
// `mole server reconcile` to report.
//
// Returns how many entries it filled. A failure to reach Vultr is not fatal to
// callers that were only trying to self-heal, so they may ignore the error.
func reconcileAddresses() (int, error) {
	local, err := loadServers()
	if err != nil {
		return 0, err
	}
	needed := false
	for _, s := range local {
		if s.InstanceID != "" && s.IP == "" && s.IPv6 == "" {
			needed = true
			break
		}
	}
	if !needed {
		return 0, nil
	}

	remote, err := serverListInstances()
	if err != nil {
		return 0, err
	}

	filled := 0
	if err := updateServers(func(current []Server) ([]Server, error) { filled = fillAddresses(current, remote); return current, nil }); err != nil {
		return 0, err
	}

	return filled, nil
}

// ── destroy ────────────────────────────────────────────────────

func runDestroy(cmd *cobra.Command, args []string) error {
	forceLocal, _ := cmd.Flags().GetBool("force-local")
	return destroyServerByName(args[0], forceLocal)
}

// destroyServerByName deletes one instance and drops its local entry.
//
// Split out of runDestroy so the daemon's idle-destroy can reach exactly the
// same code: a second implementation would be a second set of rules for when
// the local entry may be removed, and the whole point of those rules is that a
// failed delete must not leave mole believing a billing instance is gone.
func destroyServerByName(name string, forceLocal bool) error {
	return destroyServerWithin(name, forceLocal, apiBudget)
}

// destroyServerWithin is destroyServerByName with the management call's budget
// spelled out. The daemon sets it: the window it has at a lid close and the one
// it has on a dark wake differ by an order of magnitude, and spending 20
// seconds inside a 2-second wake just means being frozen mid-request and
// waking up to a dead socket.
func destroyServerWithin(name string, forceLocal bool, budget time.Duration) error {
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
		_, err := serverAPIWithin(budget, "DELETE", "/instances/"+target.InstanceID, nil)
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
			// A transport failure leaves the DELETE's outcome unknown: Vultr may
			// have processed the request and lost the reply on the way back.
			// Concluding "failed, still billing" from that alone invents a bill
			// that may not exist — on 2026-11-03 a lid close destroyed the
			// instance, the reply never arrived, and servers.json kept a phantom
			// the daemon then dialled all evening with no host at the address.
			// Ask the account which answer is true; only an account that
			// answers can settle it, and only an absent instance counts as gone.
			if gone, ok := instanceGoneRemotely(target.InstanceID); ok && gone {
				fmt.Printf("ℹ️  The DELETE answered with an error (%v), but %s is no longer on the account — treating it as destroyed\n", err, target.InstanceID)
			} else {
				return fmt.Errorf("server delete failed: %w\n    The local entry was NOT removed. If the instance is really gone on the server, retry with --force-local.", err)
			}
		}
	}

	// Save the ceiling before the entry carrying it disappears. This is the last
	// moment it exists: the next deploy in this region would otherwise start
	// from the defaults, however much measuring went into these numbers.
	rememberLadder(target)

	if err := removeRecordedServer(target.InstanceID, target.Name); err != nil {
		return err
	}

	// If this is the server a failed lid-close destroy left billing, the debt is
	// now paid and the record has to go — it is what `mole status` nags from,
	// and a nag about a server that no longer exists teaches people to ignore
	// the next one. Any other name leaves it alone.
	if debt := utils.ReadLidDebt(); debt != nil && debt.Server == name {
		_ = utils.ClearLidDebt()
	}
	fmt.Printf("🗑️  %s destroyed\n", name)
	return nil
}

// destroyActiveServerWithin destroys whichever server is active at the moment of
// the call. The daemon's destroy hook goes through here rather than closing over
// the name it started with, because between `mole up` and a lid close the answer
// changes: a first lid close destroys the server, and the name captured hours
// earlier now refers to nothing.
//
// When there is no local entry left, the two possibilities are opposite and
// indistinguishable from here — everything really is gone, or the local entry
// was lost while the instance kept billing (the trap
// `mole server reconcile --destroy-remote` exists for). Guessing is not an
// option in either direction: guess "gone" and a real orphan bills in silence,
// guess "billing" and `mole status` invents a debt nobody owes, which is what it
// did all afternoon on 2026-08-15. So ask the provider, and only report nothing
// to destroy when the account is genuinely empty.
// listInstancesForDestroy is the account read destroyActiveServerWithin relies
// on, held in a var so a test can answer for the account — including refusing to
// answer, which is the branch that must never be mistaken for "empty".
var listInstancesForDestroy = serverListInstancesWithin

// deleteVerifyBudget is the window given to the account read that settles an
// ambiguous DELETE. Short on purpose: it only runs after a delete has already
// failed, and the caller may be a machine about to sleep on a lid close.
const deleteVerifyBudget = 4 * time.Second

// instanceGoneRemotely answers the one question that decides whether an
// ambiguous DELETE deleted anything: is this instance still on the account?
// ok is false when the account could not be read, which must never be mistaken
// for "gone" — the same rule destroyActiveServerWithin follows.
func instanceGoneRemotely(id string) (gone, ok bool) {
	if id == "" {
		return false, false
	}
	remote, err := listInstancesForDestroy(deleteVerifyBudget)
	if err != nil {
		return false, false
	}
	for _, r := range remote {
		if r.ID == id {
			return false, true
		}
	}
	return true, true
}

func destroyActiveServerWithin(budget time.Duration) error {
	servers, err := loadServers()
	if err != nil {
		return fmt.Errorf("read local servers: %w", err)
	}
	for _, s := range servers {
		if s.Active {
			return destroyServerWithin(s.Name, false, budget)
		}
	}
	if len(servers) > 0 {
		// Entries but none marked active: picking one is a guess with a bill
		// attached either way, so name the state instead of acting on it.
		return fmt.Errorf("%d server(s) deployed but none is active — destroy it with `mole server destroy <name>`", len(servers))
	}

	remote, err := listInstancesForDestroy(budget)
	if err != nil {
		return fmt.Errorf("no local server to destroy, and checking the account for an orphan failed: %w", err)
	}
	if len(remote) == 0 {
		return core.ErrNothingToDestroy
	}
	ids := make([]string, 0, len(remote))
	for _, r := range remote {
		ids = append(ids, fmt.Sprintf("%s (%s)", r.Label, r.ID))
	}
	return fmt.Errorf("no local server entry, but %d instance(s) are still on the account: %s — destroy with `mole server reconcile --destroy-remote`",
		len(remote), strings.Join(ids, ", "))
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
		status := s.deployStatus()
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
	// Fill in missing addresses first, unconditionally. This used to sit behind
	// --apply, which made the one repair in here that needs no judgement wait on
	// somebody remembering a flag. Ambiguous states below are still only
	// reported, never acted on.
	if filled, err := reconcileAddresses(); err != nil {
		return fmt.Errorf("record missing addresses: %w", err)
	} else if filled > 0 {
		fmt.Printf("✔ recorded %d missing address(es)\n\n", filled)
	}

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
		v6      string
		name    string
		region  string
		message string
	}
	var rows []row
	// remoteOnly holds the instance IDs that exist on Vultr but not in
	// servers.json — true orphans. They can be deleted with the Vultr API key
	// alone (no hy2 password), so they are the one drift class reconcile can
	// safely act on; everything else needs a judgement or a secret.
	var remoteOnly []string

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
				// Reached only when Vultr itself still has no address for the
				// instance — the fill above already handled every case where it did.
				rows = append(rows, row{state: "syncing", id: id, ip: r.MainIP, v6: r.V6MainIP, name: l.Name, region: r.Region,
					message: "still provisioning; no address on either side yet"})
			} else if l.IP != r.MainIP {
				rows = append(rows, row{state: "drift", id: id, ip: r.MainIP, name: l.Name, region: r.Region, message: fmt.Sprintf("local IP %s ≠ remote IP %s", l.IP, r.MainIP)})
			} else {
				rows = append(rows, row{state: "synced", id: id, ip: r.MainIP, name: l.Name, region: r.Region})
			}
		case rok && !lok:
			rows = append(rows, row{state: "remote-only", id: id, ip: r.MainIP, name: r.Label, region: r.Region, message: fmt.Sprintf("orphan — adopt: `mole server adopt %s --password <hy2>`, or destroy: `mole server reconcile --destroy-remote`", id)})
			remoteOnly = append(remoteOnly, id)
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

	// --destroy-remote acts on the one drift class that needs no judgement and
	// no secret: an instance Vultr still bills for but mole has no record of.
	// The API key authorizes the DELETE, so it works even when the hy2 password
	// is long gone — which is exactly the trap the lid-close destroy fell into
	// when its local entry vanished but the instance kept billing.
	if reconcileDestroyRemote && len(remoteOnly) > 0 {
		fmt.Println()
		for _, id := range remoteOnly {
			if _, err := serverAPI("DELETE", "/instances/"+id, nil); err != nil {
				if IsServerNotFound(err) {
					fmt.Printf("🗑️  %s already gone on the server\n", id)
					continue
				}
				fmt.Printf("⚠️  %s NOT destroyed: %v\n", id, err)
				continue
			}
			fmt.Printf("🗑️  %s destroyed (orphan removed)\n", id)
			// This is the route out for the worst version of the lid-close
			// failure: the destroy failed, the local entry vanished, and the
			// instance kept billing with nothing pointing at it. If the orphan
			// just deleted is the one the debt names, the debt is paid.
			if debt := utils.ReadLidDebt(); debt != nil && debt.Server != "" && debt.Server == remoteByID[id].Label {
				_ = utils.ClearLidDebt()
			}
		}
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
	if err := appendServer(srv); err != nil {
		return err
	}
	fmt.Printf("✅ Adopted %s as %q (%s)\n", instanceID, name, srv.IP)
	return nil
}

// runImport adds a server from a hysteria2:// URI. This is the pure-client
// path: paste the link, connect, no control plane involved. It is deliberately
// the only import form — a subscription URL would mean re-fetching a node
// descriptor over the network on every connect, which is one more thing that
// can be unreachable, stale, or lying at exactly the moment the tunnel is
// already failing.
func runImport(cmd *cobra.Command, args []string) error {
	name, _ := cmd.Flags().GetString("name")

	srv, err := serverFromURI(args[0])
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
	for _, existing := range servers {
		if serverHost(existing) == serverHost(*srv) && existing.Port == srv.Port {
			return fmt.Errorf("a server at %s:%d already exists as %q", serverHost(*srv), srv.Port, existing.Name)
		}
	}
	if len(servers) == 0 {
		srv.Active = true
	}
	srv.CreatedAt = time.Now().Format(time.RFC3339)
	if err := appendServer(*srv); err != nil {
		return err
	}
	fmt.Printf("✅ Imported %q (%s:%d)\n", srv.Name, serverHost(*srv), srv.Port)
	return nil
}

// realityServerFromURI is the inverse of Server.realityURI(). It accepts any
// standard vless:// REALITY link, not only ones mole emitted, which is what
// makes a node provisioned by hand or by another tool usable here.
//
// It refuses a link that is not REALITY. Plain VLESS over TLS to a bare IP has
// no certificate that verifies and no borrowed handshake to hide behind — mole
// would be importing an outbound it has no way to authenticate.
func realityServerFromURI(u *url.URL) (*Server, error) {
	q := u.Query()
	if mux := q.Get("mux"); mux != "" && mux != "smux" {
		return nil, fmt.Errorf("unsupported VLESS multiplex protocol %q (want smux)", mux)
	}
	if q.Get("security") != "reality" {
		return nil, fmt.Errorf("vless link is not REALITY (security=%q): mole imports vless://…?security=reality links only", q.Get("security"))
	}
	uuid := u.User.Username()
	if uuid == "" {
		return nil, fmt.Errorf("URI missing uuid")
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("URI missing host")
	}
	pbk := q.Get("pbk")
	if pbk == "" {
		return nil, fmt.Errorf("URI missing ?pbk= (the server's REALITY public key)")
	}
	port, _ := strconv.Atoi(u.Port())
	if port == 0 {
		port = 443
	}
	srv := &Server{
		Proto:            protoReality,
		UUID:             uuid,
		Port:             uint16(port),
		RealityPubKey:    pbk,
		RealityShortID:   q.Get("sid"),
		RealityDest:      q.Get("sni"),
		RealityMultiplex: q.Get("mux") == "smux",
	}
	if srv.RealityDest == "" {
		srv.RealityDest = defaultRealityDest
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		srv.IPv6 = host
	} else {
		srv.IP = host
	}
	if frag := u.Fragment; frag != "" {
		srv.Name = frag
	}
	return srv, nil
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
// It contacts nothing: the URI carries everything.
func serverFromURI(raw string) (*Server, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid URI: %w", err)
	}
	switch u.Scheme {
	case "hy2", "hysteria2":
	case "vless":
		return realityServerFromURI(u)
	default:
		return nil, fmt.Errorf("unsupported scheme %q: import supports hysteria2:// and vless:// links only", u.Scheme)
	}
	password := u.User.Username()
	if password == "" {
		return nil, fmt.Errorf("URI missing password")
	}
	// url.Hostname()/Port() correctly strip and split bracketed IPv6
	// (e.g. [2606:…]:8443); Server.URI() always emits IPv6 bracketed, so the
	// link Server.URI() emits parses back cleanly here.
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
	if v := q.Get("sni"); v != "" {
		srv.SNI = v
	}
	if v := q.Get("pinnedcert"); v != "" {
		if b, err := base64.RawURLEncoding.DecodeString(v); err == nil {
			srv.TLSCertPEM = string(b)
		} else if b, err := base64.StdEncoding.DecodeString(v); err == nil {
			srv.TLSCertPEM = string(b)
		}
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

	if err := changeServer(target.InstanceID, target.Name, func(s *Server) { s.Stopped = true; s.Verification = nil }); err != nil {
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

	if err := changeServer(target.InstanceID, target.Name, func(s *Server) { s.Stopped = false; s.Verification = nil }); err != nil {
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
	if err := updateServers(func(current []Server) ([]Server, error) {
		selected := indexByName(current, name)
		if selected < 0 {
			return nil, fmt.Errorf("server no longer exists")
		}
		for i := range current {
			current[i].Active = i == selected
		}
		return current, nil
	}); err != nil {
		return err
	}

	fmt.Printf("✅ Active server: %s (%s)\n", servers[idx].Name, servers[idx].IP)
	fmt.Println("   Run `mole up` to connect.")
	return nil
}

// ── storage ────────────────────────────────────────────────────

func loadServers() ([]Server, error) { return readServersAt(utils.ServersPath()) }

// Full replacement is reserved for test fixtures. Production mutations use
// updateServers / changeServer so stale snapshots cannot overwrite peers.
func saveServers(servers []Server) error {
	return updateServers(func([]Server) ([]Server, error) { return servers, nil })
}

// The two states `mole up` repairs on its own. They are sentinels rather than
// message text because the recovery keys off them: matching on a substring
// means any reword of the message silently turns the automatic repair back into
// a manual one, with nothing failing to say so.
var (
	// ErrServerStopped: the instance is halted on Vultr and needs starting.
	ErrServerStopped = errors.New("active server is stopped")
	// ErrServerProvisioning: the entry exists but its address has not arrived.
	ErrServerProvisioning = errors.New("active server is still provisioning")
)

// fillAddresses copies each remote address onto the local entry that is missing
// one, matching on instance id, and reports how many it filled. Pure so the
// matching rules can be tested without a Vultr account: an entry that already
// has an address is never touched (that would be the "drift" case, which needs a
// human), and a placeholder 0.0.0.0 is not an address.
func fillAddresses(local []Server, remote []serverInstance) int {
	byID := make(map[string]serverInstance, len(remote))
	for _, r := range remote {
		byID[r.ID] = r
	}
	filled := 0
	for i := range local {
		if local[i].InstanceID == "" || local[i].IP != "" {
			continue
		}
		r, ok := byID[local[i].InstanceID]
		if !ok || r.MainIP == "" || r.MainIP == "0.0.0.0" {
			continue
		}
		local[i].IP = r.MainIP
		local[i].IPv6 = r.V6MainIP
		filled++
	}
	return filled
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
				return nil, fmt.Errorf("%w: %q", ErrServerStopped, servers[i].Name)
			}
			if !servers[i].HasAddress() {
				return nil, fmt.Errorf("%w: %q has no address yet", ErrServerProvisioning, servers[i].Name)
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

// knownServerIPs returns every address mole may need to reach directly: the
// active tunnel endpoint plus every standby node recorded locally. The active
// server comes first so its pass rule is matched first.
func knownServerIPs() []string {
	servers, _ := loadServers()
	var active, others []string
	for _, s := range servers {
		for _, ip := range []string{s.IP, s.IPv6} {
			if ip == "" {
				continue
			}
			if s.Active {
				active = append(active, ip)
			} else {
				others = append(others, ip)
			}
		}
	}
	return append(active, others...)
}

// ── Vultr management API ───────────────────────────────────────

// vultrBase is the only management endpoint mole talks to. Var (not const) so
// tests can point it at a local httptest server.
var vultrBase = "https://api.vultr.com/v2"

type ServerError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *ServerError) Error() string {
	return fmt.Sprintf("server %s %s: %d %s", e.Method, e.Path, e.Status, e.Body)
}

// IsServerNotFound reports whether err is a 404 from the Vultr API.
func IsServerNotFound(err error) bool {
	var se *ServerError
	return errors.As(err, &se) && se.Status == 404
}

// vultrKey returns the account API key every management call authenticates
// with. It is root-scoped on the Vultr account, which is precisely why it now
// lives only here and on this machine: it used to also sit as a secret inside an
// internet-facing Cloudflare Worker, where any flaw in an unauthenticated route
// reached it.
// The environment wins: it is where the key lives for every interactive call,
// and a stale file must never quietly outrank the key the operator just
// exported. The file is the fallback for the one caller that cannot have an
// environment — the sudo-launched daemon doing idle-destroy. See
// utils.VultrKeyPath.
func vultrKey() (string, error) {
	if k := strings.TrimSpace(os.Getenv("VULTR_API_KEY")); k != "" {
		return k, nil
	}
	if b, err := os.ReadFile(utils.VultrKeyPath()); err == nil {
		if k := strings.TrimSpace(string(b)); k != "" {
			return k, nil
		}
	}
	return "", fmt.Errorf("VULTR_API_KEY environment variable is not set")
}

// vultrKeyAvailable reports whether a destroy could authenticate right now.
// Used to disarm destroy-on-lid-close up front instead of discovering the problem
// at the moment the lid closes, when nothing is watching.
func vultrKeyAvailable() bool {
	_, err := vultrKey()
	return err == nil
}

// stashVultrKey copies the key out of the environment to where the daemon can
// read it. Called from `mole up` before the sudo re-exec, which is the last
// moment the environment still exists.
func stashVultrKey() error {
	k := strings.TrimSpace(os.Getenv("VULTR_API_KEY"))
	if k == "" {
		return fmt.Errorf("VULTR_API_KEY is not set (the daemon deletes the server on its own when this Mac sleeps, and sudo does not carry the environment into it)")
	}
	p := utils.VultrKeyPath()
	// Remove first: WriteFile does not narrow the mode of a file that already
	// exists, so a previously world-readable copy would stay that way.
	_ = os.Remove(p)
	if err := os.WriteFile(p, []byte(k+"\n"), 0600); err != nil {
		return fmt.Errorf("stash the Vultr key for the daemon: %w", err)
	}
	return nil
}

// serverAPI performs one management-plane request straight against the Vultr
// API.
//
// There used to be a control plane in front of this — a Worker that proxied the
// same calls, with a transport-error fallback to Vultr, a rule for which
// failures were authoritative, and a rule for which operations were safe to
// retry down the second path. All of that existed to reconcile two ways of
// reaching one API, and every one of those rules was a place to be wrong: a base
// URL that never resolved once turned Cloudflare's HTML 404 into "your instance
// is gone" and abandoned a VPS that had just been created. One path cannot
// disagree with itself.
func serverAPI(method, path string, body any) ([]byte, error) {
	return serverAPIWithin(apiBudget, method, path, body)
}

// apiBudget is how long a management call may take end to end, retries
// included. It is not a timeout picked for comfort — it is sized to the
// shortest window mole ever has to make one of these calls in.
//
// A closing lid gives roughly 30 seconds before macOS sleeps regardless of
// what anyone is doing. The old code allowed 60 for a single attempt, so a slow
// path could not have finished even in principle; the machine would have slept
// mid-request. Sized under the window instead, with attempts short enough that
// a dead path is abandoned rather than waited on.
const apiBudget = 20 * time.Second

// apiAttempt bounds one try. Measured direct: connect 0.3s, TLS 0.6s, total
// ~0.9s to api.vultr.com. Six seconds is an enormous multiple of that, which
// is the point — anything slower is a path problem, and the remedy for a path
// problem is the next attempt, not more patience on this one.
//
// 2026-10-07: was 6s, same as retryBudget — the retry loop could only run ONE
// attempt before the deadline, so a single dark-wake path timeout killed the
// destroy and then reconcileLidDebt cancelled the debt on the next lid open
// (which also happened twice today: 13:59 and 19:12, both on battery dark
// wakes). 2s fits 3+ attempts inside retryBudget's 6s window, and the
// bound/routed alternation at server.go:1729 now actually alternates instead
// of always hitting bound. A bound-to-Vultr-API call that cannot finish in
// 2s is a path problem worth retrying, not one worth waiting through.
const apiAttempt = 2 * time.Second

// serverAPIWithin performs one management-plane request against the Vultr API,
// retrying transport failures until budget is spent.
//
// Two paths, tried in this order:
//
//   - Bound: socket pinned to the physical interface, name resolved by
//     223.5.5.5. Independent of the tunnel, which is what makes it correct for
//     the caller that matters most — destroy-on-lid-close runs when the lid
//     shuts, and the lid tends to shut on an evening the path already went
//     dark. It needs the PF exemption, refreshed here on every call.
//   - Routed: whatever the routing table says, which while mole is up means
//     through the tunnel. Kept as a fallback for the network where direct
//     egress to the API is the thing that is blocked.
//
// Only transport errors are retried. An HTTP status is the server's answer and
// repeating the request cannot improve it — and for DELETE specifically, a 404
// on a retry is the truthful "already gone" that IsServerNotFound reads.
//
// POST is never retried, and that asymmetry is deliberate. A transport error
// does not say whether the server acted: a create that succeeded and then lost
// its response looks exactly like one that never arrived, and the retry that
// helps a DELETE would leave a second instance running and billing with no
// local record of it. Better to fail loudly and let a human look.
func serverAPIWithin(budget time.Duration, method, path string, body any) ([]byte, error) {
	return serverAPIContext(context.Background(), budget, method, path, body)
}

func serverAPIContext(parent context.Context, budget time.Duration, method, path string, body any) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()

	key, err := vultrKey()
	if err != nil {
		return nil, err
	}
	auth := "Bearer " + key
	deadline, _ := ctx.Deadline()

	if err := utils.AllowDirectHostContext(ctx, utils.APIHost); err != nil {
		// Not fatal: the routed path may still work, and on a host where the
		// kill switch is not loaded there was nothing to exempt.
		log.Printf("management API: direct exemption unavailable: %v", err)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !idempotent(method) {
		budget = time.Until(deadline)
		client, cerr := utils.NewBoundHTTPClient(budget)
		if cerr != nil {
			client = &http.Client{Timeout: budget}
		}
		return callAPIContext(ctx, client, vultrBase, auth, method, path, body)
	}

	var lastErr error
	for attempt := 0; ctx.Err() == nil; attempt++ {
		timeout := apiAttempt
		if remaining := time.Until(deadline); remaining < timeout {
			timeout = remaining
		}

		var client *http.Client
		// Alternate: bound first, then routed, so a budget of three attempts
		// tries direct twice and the tunnel once rather than burning the whole
		// window on one path that is already answering with EOF.
		if attempt%2 == 0 {
			client, err = utils.NewBoundHTTPClient(timeout)
			if err != nil {
				client = &http.Client{Timeout: timeout}
			}
		} else {
			client = &http.Client{Timeout: timeout}
		}

		data, err := callAPIContext(ctx, client, vultrBase, auth, method, path, body)
		if err == nil {
			return data, nil
		}
		var se *ServerError
		if errors.As(err, &se) {
			return nil, err
		}
		lastErr = err
	}
	return nil, fmt.Errorf("management call expired: %w", errors.Join(ctx.Err(), lastErr))
}

// idempotent reports whether repeating method can only be answered the same
// way or more truthfully — the precondition for retrying it.
func idempotent(method string) bool {
	switch method {
	case "GET", "HEAD", "PUT", "DELETE":
		return true
	}
	return false
}

// callAPI performs one management-plane request against base+path and returns
// the response body, or a *ServerError for HTTP >= 400, or a raw transport
// error (EOF / timeout / DNS) for connection-level failures.
func callAPI(client *http.Client, base, auth string, method, path string, body any) ([]byte, error) {
	return callAPIContext(context.Background(), client, base, auth, method, path, body)
}
func callAPIContext(ctx context.Context, client *http.Client, base, auth, method, path string, body any) ([]byte, error) {

	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &ServerError{Method: method, Path: path, Status: resp.StatusCode, Body: string(data)}
	}
	return data, nil
}

// serverInstance is the subset of /api/instances list fields reconcile needs.
type serverInstance struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Region   string `json:"region"`
	MainIP   string `json:"main_ip"`
	V6MainIP string `json:"v6_main_ip"`
}

// serverListInstances fetches every instance on the Vultr account.
func serverListInstances() ([]serverInstance, error) {
	return serverListInstancesWithin(apiBudget)
}

// serverListInstancesWithin is serverListInstances bounded by the caller's
// window — the daemon asks this inside a closing lid, where the budget is the
// difference between an answer and being frozen mid-request.
func serverListInstancesWithin(budget time.Duration) ([]serverInstance, error) {
	data, err := serverAPIWithin(budget, "GET", "/instances?per_page=100", nil)
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
