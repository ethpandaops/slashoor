package dora

import (
	"fmt"
	"time"
)

// Config holds the configuration for the Dora API client. Dora is the durable
// block/duty store: it serves full block bodies (canonical and orphaned) and
// attester committees for historic epochs a beacon node can no longer recompute.
type Config struct {
	// Enabled controls whether the Dora integration is active. When disabled the
	// slasher relies solely on connected beacon nodes (live/recent data only).
	Enabled bool `yaml:"enabled"`
	// URL is the base URL for the Dora API (e.g. https://dora.example.com).
	URL string `yaml:"url"`
	// AuthToken is an optional bearer token for authenticated Dora APIs.
	AuthToken string `yaml:"auth_token"`
	// Timeout is the per-request HTTP timeout.
	Timeout time.Duration `yaml:"timeout"`
	// MaxRetries is the number of retries on 429/5xx responses.
	MaxRetries int `yaml:"max_retries"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		Enabled:    false,
		URL:        "",
		Timeout:    30 * time.Second,
		MaxRetries: 5,
	}
}

// Validate checks the configuration for errors.
func (c *Config) Validate() error {
	if c.Enabled && c.URL == "" {
		return fmt.Errorf("dora URL is required when enabled")
	}
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
	if c.MaxRetries == 0 {
		c.MaxRetries = 5
	}
	return nil
}
