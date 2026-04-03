package config

import (
	"go.yaml.in/yaml/v4"
	"os"
)

type TLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert"`
	KeyFile  string `yaml:"key"`
}

type Config struct {
	ListenAddr string    `yaml:"listen_addr"`
	Upstreams  []string  `yaml:"upstreams"`
	Algo       string    `yaml:"algo"`
	TLS        TLSConfig `yaml:"tls"`
}

func Load(path string) (*Config, error) {
	cfg := &Config{
		ListenAddr: ":8080",
		Upstreams: []string{
			"http://localhost:9000",
			"http://localhost:9001",
		},
		Algo: "lc",
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
