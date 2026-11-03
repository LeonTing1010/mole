package config

// SingboxConfig is the JSON layout sing-box consumes.
type SingboxConfig struct {
	Log          LogConfig           `json:"log"`
	DNS          DNSConfig           `json:"dns"`
	Inbounds     []InboundConfig     `json:"inbounds"`
	Outbounds    []OutboundConfig    `json:"outbounds"`
	Route        RouteConfig         `json:"route"`
	Experimental *ExperimentalConfig `json:"experimental,omitempty"`
}

// ExperimentalConfig exposes the Clash-compatible API so mole can
// flip a selector outbound at runtime.
type ExperimentalConfig struct {
	ClashAPI  *ClashAPIConfig  `json:"clash_api,omitempty"`
	CacheFile *CacheFileConfig `json:"cache_file,omitempty"`
}

type CacheFileConfig struct {
	Enabled     bool   `json:"enabled"`
	Path        string `json:"path"`
	StoreFakeIP bool   `json:"store_fakeip"`
}

type ClashAPIConfig struct {
	ExternalController string `json:"external_controller"`
	Secret             string `json:"secret,omitempty"`
}

type LogConfig struct {
	Level     string `json:"level"`
	Output    string `json:"output,omitempty"`
	Timestamp bool   `json:"timestamp,omitempty"`
}

type DNSConfig struct {
	Servers          []DNSServer `json:"servers"`
	Rules            []DNSRule   `json:"rules,omitempty"`
	Final            string      `json:"final,omitempty"`
	Strategy         string      `json:"strategy,omitempty"`
	IndependentCache bool        `json:"independent_cache,omitempty"`
}

type DNSServer struct {
	Type       string `json:"type"`
	Server     string `json:"server,omitempty"`
	Tag        string `json:"tag"`
	Detour     string `json:"detour,omitempty"`
	Inet4Range string `json:"inet4_range,omitempty"`
}

type DNSRule struct {
	Domain        []string `json:"domain,omitempty"`
	DomainSuffix  []string `json:"domain_suffix,omitempty"`
	DomainKeyword []string `json:"domain_keyword,omitempty"`
	RuleSet       []string `json:"rule_set,omitempty"`
	QueryType     []string `json:"query_type,omitempty"`
	Server        string   `json:"server,omitempty"`
	Action        string   `json:"action,omitempty"`
}

type InboundConfig struct {
	Type        string   `json:"type"`
	Tag         string   `json:"tag"`
	Address     []string `json:"address,omitempty"`
	MTU         int      `json:"mtu,omitempty"`
	AutoRoute   bool     `json:"auto_route,omitempty"`
	StrictRoute bool     `json:"strict_route,omitempty"`
	Stack       string   `json:"stack,omitempty"`
	Listen      string   `json:"listen,omitempty"`
	ListenPort  int      `json:"listen_port,omitempty"`
	// OverrideAddress/OverridePort turn a `direct` inbound into a fixed
	// forwarder: everything accepted on Listen:ListenPort is sent to
	// OverrideAddress:OverridePort through whatever outbound the route picks.
	// That is how the client reaches the node's loopback-only Brutal keeper —
	// a destination of 127.0.0.1 is meaningless to name from here, because the
	// TUN never captures loopback, so the address is written on the node's side
	// of the leg instead. A fixed forwarder rather than a socks/mixed inbound
	// on purpose: it can reach exactly one address, so no other local process
	// can borrow it as a general proxy into the tunnel.
	OverrideAddress string `json:"override_address,omitempty"`
	OverridePort    int    `json:"override_port,omitempty"`
}

type OutboundConfig struct {
	Type       string `json:"type"`
	Tag        string `json:"tag"`
	Server     string `json:"server,omitempty"`
	ServerPort int    `json:"server_port,omitempty"`
	UUID       string `json:"uuid,omitempty"`
	Password   string `json:"password,omitempty"`
	UpMbps     int    `json:"up_mbps,omitempty"`
	DownMbps   int    `json:"down_mbps,omitempty"`
	// ServerPorts/HopInterval turn on hysteria2 port hopping. sing-quic keeps
	// ONE QUIC session and swaps the UDP socket under it every HopInterval, so
	// hopping is invisible to everything above — no reconnect, no dropped
	// streams. Format is sing-box's: ["20000:50000"], colon-separated, and a Go
	// duration string. Both must be omitted together; a range the server does
	// not answer on blackholes the tunnel on the first hop.
	ServerPorts []string         `json:"server_ports,omitempty"`
	HopInterval string           `json:"hop_interval,omitempty"`
	Obfs        *Hy2Obfs         `json:"obfs,omitempty"`
	Flow        string           `json:"flow,omitempty"`
	Network     string           `json:"network,omitempty"`
	Version     string           `json:"version,omitempty"`
	Outbounds   []string         `json:"outbounds,omitempty"`
	Default     string           `json:"default,omitempty"`
	TLS         *TLSConfig       `json:"tls,omitempty"`
	Transport   *TransportConfig `json:"transport,omitempty"`
	// QUIC tuning — applied to every QUIC-based outbound (HY2, VLESS+REALITY
	// with flow=xtls-rprx-vision, TUIC). Smaller initial packets raise the
	// chance a handshake gets through a lossy path on the first try; disabling
	// PMTUD is safe because we know the VPS MTU and a failed ICMP probe would
	// just make things worse.
	InitialPacketSize int  `json:"initial_packet_size,omitempty"`
	DisablePathMTU    bool `json:"disable_path_mtu_discovery,omitempty"`
	// TCPFastOpen sends the first payload inside the SYN, removing one RTT from
	// every new connection. The node already runs net.ipv4.tcp_fastopen=3 (see
	// cmd's boot script); without this flag the client never asks for it.
	TCPFastOpen bool             `json:"tcp_fast_open,omitempty"`
	Multiplex   *MultiplexConfig `json:"multiplex,omitempty"`
}

// MultiplexConfig uses sing-box's native stream/session pool. Enable only when
// the node explicitly accepts multiplex; legacy global mux rejects requests.
type MultiplexConfig struct {
	Enabled        bool   `json:"enabled"`
	Protocol       string `json:"protocol"`
	MaxConnections int    `json:"max_connections"`
	MinStreams     int    `json:"min_streams"`
}

// Hy2Obfs enables hysteria2 salamander obfuscation. Without it the raw QUIC
// flow on UDP/443 is trivially fingerprinted and QoS-throttled by the GFW,
// which shows up as bursts of "no recent network activity" idle-timeouts and
// collapsing throughput at peak hours. Must match the server's obfs settings.
type Hy2Obfs struct {
	Type     string `json:"type"`
	Password string `json:"password,omitempty"`
}

type TLSConfig struct {
	Enabled    bool   `json:"enabled"`
	ServerName string `json:"server_name,omitempty"`
	Insecure   bool   `json:"insecure,omitempty"`
	// Certificate pins the exact server certificate, as PEM lines. With it set,
	// Insecure MUST stay false: the pin replaces CA trust entirely, so the tunnel
	// verifies without a domain, an ACME cert, or an entry in a Certificate
	// Transparency log. Without a pin, hysteria2's password is handed to an
	// unverified peer — anyone able to steer the UDP flow completes TLS with a
	// cert of their choosing and harvests both password and plaintext.
	Certificate []string `json:"certificate,omitempty"`

	// UTLS forges the ClientHello of a real browser. REALITY requires it: the
	// authentication data is hidden inside a ClientHello field, so the hello has
	// to be one the borrowed site would plausibly receive. Go's own TLS stack
	// has a fingerprint nothing else on the internet produces.
	UTLS *UTLSConfig `json:"utls,omitempty"`

	// Reality turns the handshake into a borrowed one. The server completes TLS
	// against Reality's target on the client's behalf, so what a prober sees is
	// that site's real certificate — nothing self-signed, nothing to pin, and
	// nothing that contradicts the destination's ASN.
	Reality *RealityConfig `json:"reality,omitempty"`
}

type UTLSConfig struct {
	Enabled     bool   `json:"enabled"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// RealityConfig is the client half of REALITY. PublicKey's private half never
// leaves the VPS, and ShortID is one of the ids that server accepts — neither is
// a secret in the sense the hysteria2 password is, which is why a vless:// link
// can be shared where a hy2:// one cannot.
type RealityConfig struct {
	Enabled   bool   `json:"enabled"`
	PublicKey string `json:"public_key,omitempty"`
	ShortID   string `json:"short_id,omitempty"`
}

type TransportConfig struct {
	Type    string            `json:"type"`
	Path    string            `json:"path,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

type RouteConfig struct {
	Rules                 []RouteRule            `json:"rules"`
	RuleSet               []RuleSet              `json:"rule_set,omitempty"`
	AutoDetectInterface   bool                   `json:"auto_detect_interface"`
	Final                 string                 `json:"final"`
	DefaultDomainResolver *DefaultDomainResolver `json:"default_domain_resolver,omitempty"`
}

type RuleSet struct {
	Type           string `json:"type"`
	Tag            string `json:"tag"`
	Format         string `json:"format,omitempty"`
	URL            string `json:"url,omitempty"`
	Path           string `json:"path,omitempty"`
	DownloadDetour string `json:"download_detour,omitempty"`
	UpdateInterval string `json:"update_interval,omitempty"`
}

type DefaultDomainResolver struct {
	Server string `json:"server"`
	Detour string `json:"detour,omitempty"`
}

type RouteRule struct {
	SourcePortRange []string `json:"source_port_range,omitempty"`
	Action          string   `json:"action,omitempty"`
	Protocol        string   `json:"protocol,omitempty"`
	IPCIDR          []string `json:"ip_cidr,omitempty"`
	DomainSuffix    []string `json:"domain_suffix,omitempty"`
	// DomainKeyword matches a substring anywhere in the domain, with no regard
	// for label boundaries — the escape hatch for hosts a suffix cannot reach.
	// sing-box's domain_suffix aligns on dots, so `buy.itunes.apple.com` does
	// NOT match `p47-buy.itunes.apple.com` (Apple's per-account store
	// partitions). Keep a rule to keyword-only or suffix-only: sing-box ANDs
	// matcher kinds within one rule, so mixing them matches neither alone.
	DomainKeyword []string `json:"domain_keyword,omitempty"`
	RuleSet       []string `json:"rule_set,omitempty"`
	// Inbound pins a rule to traffic that arrived on one named inbound,
	// regardless of where it is going. Used to bind the Brutal keeper's
	// forwarder to the REALITY leg specifically: the rate has to travel over
	// the very leg it describes, whichever leg the selector is carrying.
	Inbound  []string `json:"inbound,omitempty"`
	Invert   bool     `json:"invert,omitempty"`
	Outbound string   `json:"outbound,omitempty"`
}
