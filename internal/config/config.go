package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type ServerConfig struct {
	URL    string `yaml:"url"`
	APIKey string `yaml:"api_key"`
}

type ParserConfig struct {
	Enabled bool `yaml:"enabled"`
}

type Config struct {
	Server   ServerConfig            `yaml:"server"`
	Interval int                     `yaml:"interval"`
	Lookback int                     `yaml:"lookback"` // hours — only read sessions modified within this window (default: 24)
	Parsers  map[string]ParserConfig `yaml:"parsers"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	// Expand environment variables
	expanded := os.ExpandEnv(string(data))

	cfg := &Config{
		Interval: 60,
		Parsers:  make(map[string]ParserConfig),
	}
	if err := yaml.Unmarshal([]byte(expanded), cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if cfg.Server.URL == "" {
		return nil, fmt.Errorf("server.url is required")
	}
	if cfg.Server.APIKey == "" {
		return nil, fmt.Errorf("server.api_key is required")
	}
	if cfg.Interval < 10 {
		cfg.Interval = 10
	}
	if cfg.Lookback <= 0 {
		cfg.Lookback = 24 // default: only sessions from last 24 hours
	}

	return cfg, nil
}
