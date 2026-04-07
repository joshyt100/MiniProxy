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

type Config struct {
	Cleartext  CleartextConfig `yaml:"cleartext"`
	ListenAddr string          `yaml:"listen_addr"`
	Upstreams  []string        `yaml:"upstreams"`
	Algo       string          `yaml:"algo"`
	TLS        TLSConfig       `yaml:"tls"`
	Metrics    MetricsConfig   `yaml:"metrics"`
}

func Load(path string) (*Config, error) {
	cfg := &Config{
		Cleartext: CleartextConfig{
			Enabled:    true,
			ListenAddr: ":8080",
		},
		Upstreams: []string{
			"http://localhost:9000",
			"http://localhost:9001",
		},
		Algo: "lc",
		TLS: TLSConfig{
			ListenAddr: ":8443",
		},
		Metrics: MetricsConfig{
			Enabled:    true,
			ListenAddr: ":2112",
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
