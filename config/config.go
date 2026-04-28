package config

import (
	"os"

	"go.yaml.in/yaml/v4"
)

// TLSConfig controls HTTPS listening. If Enabled is false the TLS server is not started.
type TLSConfig struct {
	Enabled    bool   `yaml:"enabled"`
	ListenAddr string `yaml:"listen_addr"`
	CertFile   string `yaml:"cert"`
	KeyFile    string `yaml:"key"`
}

// MetricsConfig controls the Prometheus metrics endpoint.
type MetricsConfig struct {
	Enabled    bool   `yaml:"enabled"`
	ListenAddr string `yaml:"listen_addr"`
}

// CleartextConfig controls the plain HTTP listener.
type CleartextConfig struct {
	Enabled    bool   `yaml:"enabled"`
	ListenAddr string `yaml:"listen_addr"`
}

// RateLimitConfig controls request rate limiting. When PerIP is true each
// client IP gets its own token bucket; otherwise a single global bucket is used.
type RateLimitConfig struct {
	Enabled bool    `yaml:"enabled"`
	RPS     float64 `yaml:"rps"`   // sustained requests per second
	Burst   int     `yaml:"burst"` // maximum burst above RPS
	PerIP   bool    `yaml:"per_ip"`
}

// LoggingConfig sets the log verbosity and output format.
// Level is one of debug, info, warn, error. Format is json or text.
type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// HealthConfig controls active upstream health checking. When Enabled is false
// upstreams are assumed healthy unless passive failures say otherwise.
// PassiveCooldownSecs sets how long a failed upstream is excluded from rotation.
type HealthConfig struct {
	Enabled             bool   `yaml:"enabled"`
	Path                string `yaml:"path"`
	IntervalSeconds     int    `yaml:"interval_seconds"`
	TimeoutSeconds      int    `yaml:"timeout_seconds"`
	PassiveCooldownSecs int    `yaml:"passive_cooldown_seconds"`
}

// Config is the top-level configuration for the proxy, populated from a YAML
// file. All fields have defaults applied by Load before unmarshalling, so
// missing keys fall back to sensible values rather than zero values.
type Config struct {
	Cleartext  CleartextConfig `yaml:"cleartext"`
	ListenAddr string          `yaml:"listen_addr"`
	Upstreams  []string        `yaml:"upstreams"`
	Algo       string          `yaml:"algo"`
	TLS        TLSConfig       `yaml:"tls"`
	Metrics    MetricsConfig   `yaml:"metrics"`
	RateLimit  RateLimitConfig `yaml:"rate_limit"`
	Logger     LoggingConfig   `yaml:"logging"`
	Health     HealthConfig    `yaml:"health"`
}

// Load reads and parses the YAML file at path into a Config. Defaults are
// applied before unmarshalling so partial files are valid. If the file does
// not exist the defaults are returned as-is without an error.
func Load(path string) (*Config, error) {
	cfg := &Config{
		Cleartext: CleartextConfig{
			Enabled:    true,
			ListenAddr: ":8080",
		},
		Algo: "lc",
		TLS: TLSConfig{
			ListenAddr: ":8443",
		},
		Metrics: MetricsConfig{
			Enabled:    true,
			ListenAddr: ":2112",
		},
		Logger:    LoggingConfig{Level: "info", Format: "text"},
		RateLimit: RateLimitConfig{Enabled: false},
		Health: HealthConfig{
			Enabled:             false,
			Path:                "/",
			IntervalSeconds:     5,
			TimeoutSeconds:      2,
			PassiveCooldownSecs: 10,
		},
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, err
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}
