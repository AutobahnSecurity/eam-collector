package config

import (
	"fmt"
	"log"
	"net/url"
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
	u, err := url.Parse(cfg.Server.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid server.url %q: %w", cfg.Server.URL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("server.url must use http:// or https:// scheme, got %q", cfg.Server.URL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("server.url must include a hostname, got %q", cfg.Server.URL)
	}
	if cfg.Server.APIKey == "" {
		return nil, fmt.Errorf("server.api_key is required")
	}
	if cfg.Interval < 10 {
		log.Printf("[config] Warning: interval %d too low, clamped to 10s", cfg.Interval)
		cfg.Interval = 10
	}
	if cfg.Interval > 3600 {
		log.Printf("[config] Warning: interval %d too high, clamped to 3600s", cfg.Interval)
		cfg.Interval = 3600
	}
	if cfg.Lookback <= 0 {
		cfg.Lookback = 24 // default: only sessions from last 24 hours
	}

	return cfg, nil
}
