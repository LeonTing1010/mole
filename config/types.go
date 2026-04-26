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
	ClashAPI *ClashAPIConfig `json:"clash_api,omitempty"`
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
	Servers  []DNSServer `json:"servers"`
	Rules    []DNSRule   `json:"rules,omitempty"`
	Final    string      `json:"final,omitempty"`
	Strategy string      `json:"strategy,omitempty"`
}

type DNSServer struct {
	Type   string `json:"type"`
	Server string `json:"server,omitempty"`
	Tag    string `json:"tag"`
	Detour string `json:"detour,omitempty"`
}

type DNSRule struct {
	Domain  []string `json:"domain,omitempty"`
	RuleSet []string `json:"rule_set,omitempty"`
	Server  string   `json:"server"`
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
}

type OutboundConfig struct {
	Type       string           `json:"type"`
	Tag        string           `json:"tag"`
	Server     string           `json:"server,omitempty"`
	ServerPort int              `json:"server_port,omitempty"`
	UUID       string           `json:"uuid,omitempty"`
	Password   string           `json:"password,omitempty"`
	Flow       string           `json:"flow,omitempty"`
	Network    string           `json:"network,omitempty"`
	Version    string           `json:"version,omitempty"`
	Outbounds  []string         `json:"outbounds,omitempty"`
	Default    string           `json:"default,omitempty"`
	TLS        *TLSConfig       `json:"tls,omitempty"`
	Transport  *TransportConfig `json:"transport,omitempty"`
}

type TLSConfig struct {
	Enabled    bool   `json:"enabled"`
	ServerName string `json:"server_name,omitempty"`
	Insecure   bool   `json:"insecure,omitempty"`
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
	Action   string   `json:"action,omitempty"`
	Protocol string   `json:"protocol,omitempty"`
	IPCIDR   []string `json:"ip_cidr,omitempty"`
	RuleSet  []string `json:"rule_set,omitempty"`
	Invert   bool     `json:"invert,omitempty"`
	Outbound string   `json:"outbound,omitempty"`
}
