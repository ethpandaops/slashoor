package beacon

import (
	"errors"
	"time"
)

// Config holds the configuration for the beacon client.
type Config struct {
	// Endpoints is a list of beacon node API URLs.
	Endpoints []string `yaml:"endpoints"`
	// Timeout is the timeout for HTTP requests.
	Timeout time.Duration `yaml:"timeout"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		Endpoints: []string{"http://localhost:5052"},
		Timeout:   30 * time.Second,
	}
}

// Validate checks the configuration for errors.
func (c *Config) Validate() error {
	if len(c.Endpoints) == 0 {
		return ErrNoEndpoint
	}

	for _, endpoint := range c.Endpoints {
		if endpoint == "" {
			return errors.New("empty endpoint in endpoints list")
		}
	}

	return nil
}
