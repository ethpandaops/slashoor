package submitter

// Config holds the configuration for the submitter.
type Config struct {
	// Enabled controls whether slashing submission is active.
	Enabled bool `yaml:"enabled"`
	// DryRun logs slashings without submitting them.
	DryRun bool `yaml:"dry_run"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		Enabled: true,
		DryRun:  false,
	}
}

// Validate checks the configuration for errors.
func (c *Config) Validate() error {
	return nil
}
