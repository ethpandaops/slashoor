package dora

import "fmt"

// Config holds the configuration for the Dora API client.
type Config struct {
	// Enabled controls whether Dora historical scanning is active.
	Enabled bool `yaml:"enabled"`

	// URL is the base URL for the Dora API (e.g., https://dora.example.com).
	URL string `yaml:"url"`

	// ScanOnStartup controls whether to scan for historical proposer slashings on startup.
	ScanOnStartup bool `yaml:"scan_on_startup"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		Enabled:       false,
		URL:           "",
		ScanOnStartup: true,
	}
}

// Validate checks the configuration for errors.
func (c *Config) Validate() error {
	if c.Enabled && c.URL == "" {
		return fmt.Errorf("dora URL is required when enabled")
	}

	return nil
}
