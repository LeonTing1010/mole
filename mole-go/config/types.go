package config

// MoleConfig represents the user-friendly mole configuration
type MoleConfig struct {
	Server   string   `yaml:"server" json:"server"`
	DNS      []string `yaml:"dns" json:"dns"`
	LogLevel string   `yaml:"log_level" json:"log_level"`
	TUN      TUNConfig `yaml:"tun" json:"tun"`
}

// TUNConfig represents TUN interface settings
type TUNConfig struct {
	Enabled     bool   `yaml:"enabled" json:"enabled"`
	InterfaceName string `yaml:"interface_name,omitempty" json:"interface_name,omitempty"`
	MTU         int    `yaml:"mtu,omitempty" json:"mtu,omitempty"`
}

// HiddifyConfig represents the Hiddify/sing-box configuration format
type HiddifyConfig struct {
	Log          LogConfig          `json:"log"`
	DNS          DNSConfig          `json:"dns"`
	Inbounds     []InboundConfig    `json:"inbounds"`
	Outbounds    []OutboundConfig   `json:"outbounds"`
	Route        RouteConfig        `json:"route"`
}

// LogConfig represents logging configuration
type LogConfig struct {
	Level  string `json:"level"`
	Output string `json:"output,omitempty"`
}

// DNSConfig represents DNS configuration
type DNSConfig struct {
	Servers []DNSServer `json:"servers"`
	Rules   []DNSRule   `json:"rules,omitempty"`
}

// DNSServer represents a DNS server
type DNSServer struct {
	Tag     string `json:"tag"`
	Address string `json:"address"`
}

// DNSRule represents a DNS routing rule
type DNSRule struct {
	DomainSuffix []string `json:"domain_suffix,omitempty"`
	Server       string   `json:"server"`
}

// InboundConfig represents inbound connection configuration
type InboundConfig struct {
	Type       string   `json:"type"`
	Tag        string   `json:"tag"`
	Address    []string `json:"address,omitempty"`
	MTU        int      `json:"mtu,omitempty"`
	AutoRoute  bool     `json:"auto_route,omitempty"`
	StrictRoute bool    `json:"strict_route,omitempty"`
	Stack      string   `json:"stack,omitempty"`
}

// OutboundConfig represents outbound connection configuration
type OutboundConfig struct {
	Type        string                 `json:"type"`
	Tag         string                 `json:"tag"`
	Server      string                 `json:"server,omitempty"`
	ServerPort  int                    `json:"server_port,omitempty"`
	UUID        string                 `json:"uuid,omitempty"`
	Flow        string                 `json:"flow,omitempty"`
	Network     string                 `json:"network,omitempty"`
	TLS         *TLSConfig             `json:"tls,omitempty"`
	Transport   *TransportConfig       `json:"transport,omitempty"`
}

// TLSConfig represents TLS configuration
type TLSConfig struct {
	Enabled     bool   `json:"enabled"`
	ServerName  string `json:"server_name,omitempty"`
	Insecure    bool   `json:"insecure,omitempty"`
}

// TransportConfig represents transport layer configuration
type TransportConfig struct {
	Type    string            `json:"type"`
	Path    string            `json:"path,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// RouteConfig represents routing configuration
type RouteConfig struct {
	Rules             []RouteRule `json:"rules"`
	AutoDetectInterface bool      `json:"auto_detect_interface"`
	DefaultInterface    string    `json:"default_interface,omitempty"`
	Final               string    `json:"final"`
}

// RouteRule represents a routing rule
type RouteRule struct {
	GeoIP    []string `json:"geoip,omitempty"`
	GeoSite  []string `json:"geosite,omitempty"`
	IPCIDR   []string `json:"ip_cidr,omitempty"`
	Outbound string   `json:"outbound"`
}
