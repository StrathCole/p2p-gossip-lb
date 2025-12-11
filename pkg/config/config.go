package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// ByteSize represents a quantity of bytes parsed from human-readable strings.
type ByteSize int64

// UnmarshalText implements encoding.TextUnmarshaler for Viper compatibility.
func (b *ByteSize) UnmarshalText(text []byte) error {
	value, err := parseByteSize(string(text))
	if err != nil {
		return err
	}
	*b = ByteSize(value)
	return nil
}

func parseByteSize(input string) (int64, error) {
	s := strings.TrimSpace(strings.ToUpper(input))
	if s == "" {
		return 0, fmt.Errorf("empty size string")
	}

	units := []struct {
		suffix string
		factor int64
	}{
		{"TIB", 1 << 40},
		{"GIB", 1 << 30},
		{"MIB", 1 << 20},
		{"KIB", 1 << 10},
		{"TB", 1_000_000_000_000},
		{"GB", 1_000_000_000},
		{"MB", 1_000_000},
		{"KB", 1_000},
	}

	for _, unit := range units {
		if strings.HasSuffix(s, unit.suffix) {
			val, err := parseFloat(strings.TrimSuffix(s, unit.suffix))
			if err != nil {
				return 0, err
			}
			return int64(val * float64(unit.factor)), nil
		}
	}

	val, err := parseFloat(s)
	if err != nil {
		return 0, err
	}
	return int64(val), nil
}

func parseFloat(s string) (float64, error) {
	var f float64
	_, err := fmt.Sscanf(s, "%f", &f)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}
	return f, nil
}

// Config holds the superset of configuration for all components in the mesh.
type Config struct {
	Mesh     MeshConfig     `mapstructure:"mesh"`
	Edge     EdgeConfig     `mapstructure:"edge"`
	Backend  BackendConfig  `mapstructure:"backend"`
	Probe    ProbeConfig    `mapstructure:"probe"`
	Cache    CacheConfig    `mapstructure:"cache"`
	Rate     RateLimit      `mapstructure:"rate_limit"`
	Selector SelectorConfig `mapstructure:"selector"`
	TLS      TLSConfig      `mapstructure:"tls"`
	DNS      DNSConfig      `mapstructure:"dns"`
}

// MeshConfig configures the control plane daemon.
type MeshConfig struct {
	ListenAddrs      []string      `mapstructure:"listen_addrs"`
	Bootstrap        []string      `mapstructure:"bootstrap"`
	AllowlistChains  []string      `mapstructure:"allowlist_chains"`
	DataDir          string        `mapstructure:"data_dir"`
	GossipKey        string        `mapstructure:"gossip_key"`
	HTTPMetrics      string        `mapstructure:"http_metrics"`
	AntiEntropyEvery time.Duration `mapstructure:"anti_entropy_every"`
}

// EdgeConfig configures the mesh proxy edge node.
type EdgeConfig struct {
	ID            string   `mapstructure:"id"`
	ListenHTTP    string   `mapstructure:"listen_http"`
	ListenHTTPS   string   `mapstructure:"listen_https"`
	EnableH3      bool     `mapstructure:"h3"`
	Chains        []string `mapstructure:"chains"`
	Country       string   `mapstructure:"country"`
	Region        string   `mapstructure:"region"`
	AdvertiseIPs  []string `mapstructure:"advertise_ips"`
	NSServing     bool     `mapstructure:"ns_serving"`
	BackendScheme string   `mapstructure:"backend_scheme"` // "http" or "https" (default: "https")
}

// BackendConfig configures the meshagent running alongside a backend node.
type BackendConfig struct {
	ID           string            `mapstructure:"id"`
	ChainID      string            `mapstructure:"chain_id"`
	Host         string            `mapstructure:"host"`
	Caps         map[string]bool   `mapstructure:"caps"`
	Archival     bool              `mapstructure:"archival"`
	Metadata     map[string]string `mapstructure:"metadata"`
	VerifyListen string            `mapstructure:"verify_listen"`
	IdentityKey  string            `mapstructure:"identity_key"`
	OwnerKey     string            `mapstructure:"owner_key"`
	Country      string            `mapstructure:"country"`
	Region       string            `mapstructure:"region"`
	IPs          []string          `mapstructure:"ips"`
	RPCEndpoint  string            `mapstructure:"rpc_endpoint"`
	LCDEndpoint  string            `mapstructure:"lcd_endpoint"`
	PromEndpoint string            `mapstructure:"prom_endpoint"`
}

// ProbeConfig tunes backend probing.
type ProbeConfig struct {
	Interval time.Duration `mapstructure:"interval"`
	Timeout  time.Duration `mapstructure:"timeout"`
}

// CacheConfig controls the height-aware cache on edges.
type CacheConfig struct {
	Enabled      bool     `mapstructure:"enabled"`
	MaxBytes     ByteSize `mapstructure:"max_bytes"`
	MaxItemBytes ByteSize `mapstructure:"max_item_bytes"`
	TTLHeighted  string   `mapstructure:"ttl_heighted"`
}

// RateLimit configures token buckets per client.
type RateLimit struct {
	PerIPRPS int `mapstructure:"per_ip_rps"`
	Burst    int `mapstructure:"burst"`
}

// SelectorConfig exposes tuning for the load selection algorithm.
type SelectorConfig struct {
	DeltaHeight int64   `mapstructure:"delta_height"`
	SoftmaxT    float64 `mapstructure:"softmax_T"`
}

// TLSConfig configures public certificate management.
type TLSConfig struct {
	ACMEEmail string   `mapstructure:"acme_email"`
	Domains   []string `mapstructure:"domains"`
}

// DNSConfig configures the authoritative DNS service.
type DNSConfig struct {
	Zone       string        `mapstructure:"zone"`
	NSLabels   []string      `mapstructure:"ns_labels"`
	TTLNSA     int           `mapstructure:"ttl_ns_a"`
	TTLService int           `mapstructure:"ttl_service"`
	DNSSEC     bool          `mapstructure:"dnssec"`
	Listen     string        `mapstructure:"listen"`
	Chains     []string      `mapstructure:"chains"`
	MaxAnswers int           `mapstructure:"max_answers"`
	EnableECS  bool          `mapstructure:"ecs"`
	RRLQPS     float64       `mapstructure:"rrl_qps"`
	RRLBurst   int           `mapstructure:"rrl_burst"`
	RRLWindow  time.Duration `mapstructure:"rrl_window"`

	// Geo-based DNS settings
	GeoEnabled   bool    `mapstructure:"geo_enabled"`    // Enable geo-based DNS responses
	GeoCityDB    string  `mapstructure:"geo_city_db"`    // Path to MaxMind GeoIP2/GeoLite2-City database
	GeoASNDB     string  `mapstructure:"geo_asn_db"`     // Path to MaxMind GeoIP2/GeoLite2-ASN database (optional)
	GeoWeight    float64 `mapstructure:"geo_weight"`     // Weight for geo scoring (0.0-1.0, default: 0.5)
	GeoReloadSec int     `mapstructure:"geo_reload_sec"` // Reload geo databases every N seconds (0 = no reload)
}

// Load reads configuration from disk using Viper with sensible defaults.
func Load(path string) (Config, error) {
	var cfg Config

	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("yaml")
	if err := v.ReadInConfig(); err != nil {
		return cfg, fmt.Errorf("config: read %s: %w", path, err)
	}
	v.AutomaticEnv()

	if err := v.Unmarshal(&cfg); err != nil {
		return cfg, fmt.Errorf("config: decode %s: %w", path, err)
	}

	if cfg.Probe.Interval == 0 {
		cfg.Probe.Interval = 2 * time.Second
	}
	if cfg.Probe.Timeout == 0 {
		cfg.Probe.Timeout = 2 * time.Second
	}
	if cfg.Mesh.AntiEntropyEvery == 0 {
		cfg.Mesh.AntiEntropyEvery = 10 * time.Second
	}
	if cfg.Rate.PerIPRPS == 0 {
		cfg.Rate.PerIPRPS = 100
	}
	if cfg.Rate.Burst == 0 {
		cfg.Rate.Burst = 200
	}
	if cfg.Selector.DeltaHeight == 0 {
		cfg.Selector.DeltaHeight = 2
	}
	if cfg.Selector.SoftmaxT == 0 {
		cfg.Selector.SoftmaxT = 0.7
	}
	if cfg.Cache.TTLHeighted == "" {
		cfg.Cache.TTLHeighted = "infinite"
	}
	if cfg.DNS.Listen == "" {
		cfg.DNS.Listen = ":53"
	}
	if cfg.DNS.TTLNSA == 0 {
		cfg.DNS.TTLNSA = 30
	}
	if cfg.DNS.TTLService == 0 {
		cfg.DNS.TTLService = 20
	}
	if cfg.DNS.MaxAnswers == 0 {
		cfg.DNS.MaxAnswers = 4
	}
	if cfg.DNS.RRLQPS < 0 {
		return cfg, fmt.Errorf("config: dns rrl qps cannot be negative")
	}
	if cfg.DNS.RRLBurst < 0 {
		return cfg, fmt.Errorf("config: dns rrl burst cannot be negative")
	}
	if cfg.DNS.RRLWindow < 0 {
		return cfg, fmt.Errorf("config: dns rrl window cannot be negative")
	}
	if cfg.DNS.RRLQPS > 0 && cfg.DNS.RRLBurst == 0 {
		cfg.DNS.RRLBurst = int(cfg.DNS.RRLQPS * 2)
		if cfg.DNS.RRLBurst == 0 {
			cfg.DNS.RRLBurst = 1
		}
	}
	if cfg.DNS.RRLWindow == 0 {
		cfg.DNS.RRLWindow = 5 * time.Minute
	}
	return cfg, nil
}
