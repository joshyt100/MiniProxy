package config

import (
	"go.yaml.in/yaml/v4"
	"os"
)

type TLSConfig struct {
	Enabled    bool   `yaml:"enabled"`
	ListenAddr string `yaml:"listen_addr"`
	CertFile   string `yaml:"cert"`
	KeyFile    string `yaml:"key"`
}

// add Metrics
type MetricsConfig struct {
	Enabled    bool   `yaml:"enabled"`
	ListenAddr string `yaml:"listen_addr"`
}

type CleartextConfig struct {
	Enabled    bool   `yaml:"enabled"`
	ListenAddr string `yaml:"listen_addr"`
}

type RateLimitConfig struct {
	Enabled bool    `yaml:"enabled"`
	RPS     float64 `yaml:"rps"`   // request per second
	Burst   int     `yaml:"burst"` // max burst size (burst refers to the max requests a client can make instantly before the rate limiter kicks in)

	PerIP bool `yaml:"per_ip"` // true -> per client IP, false -> global
}

type LoggingConfig struct {
	Level  string `yaml:"level"`  // debug, info, warn error
	Format string `yaml:"format"` // json, text
}

type Config struct {
	Cleartext  CleartextConfig `yaml:"cleartext"`
	ListenAddr string          `yaml:"listen_addr"`
	Upstreams  []string        `yaml:"upstreams"`
	Algo       string          `yaml:"algo"`
	TLS        TLSConfig       `yaml:"tls"`
	Metrics    MetricsConfig   `yaml:"metrics"`
	RateLimit  RateLimitConfig `yaml:"rate_limit"`
	Logger     LoggingConfig   `yaml:"logging"`
}

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
		Logger: LoggingConfig{Level: "info", Format: "text"},

		RateLimit: RateLimitConfig{Enabled: false}, // disable rate limiting for default settings (add note to docs)
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
