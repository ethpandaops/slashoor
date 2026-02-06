package indexer

// Config holds the configuration for the indexer.
type Config struct {
	// MaxEpochsToKeep is the maximum number of epochs to keep in memory.
	MaxEpochsToKeep uint64 `yaml:"max_epochs_to_keep"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		MaxEpochsToKeep: 54000,
	}
}

// Validate checks the configuration for errors.
func (c *Config) Validate() error {
	return nil
}
