package detector

// Config holds the configuration for the detector.
type Config struct {
	// Enabled controls whether slashing detection is active.
	Enabled bool `yaml:"enabled"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		Enabled: true,
	}
}

// Validate checks the configuration for errors.
func (c *Config) Validate() error {
	return nil
}
