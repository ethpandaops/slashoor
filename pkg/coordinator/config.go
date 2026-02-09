package coordinator

import (
	"fmt"

	"github.com/slashoor/slashoor/pkg/beacon"
	"github.com/slashoor/slashoor/pkg/detector"
	"github.com/slashoor/slashoor/pkg/dora"
	"github.com/slashoor/slashoor/pkg/indexer"
	"github.com/slashoor/slashoor/pkg/proposer"
	"github.com/slashoor/slashoor/pkg/submitter"
)

// Config holds the top-level configuration for the slashoor.
type Config struct {
	Beacon    *beacon.Config    `yaml:"beacon"`
	Indexer   *indexer.Config   `yaml:"indexer"`
	Detector  *detector.Config  `yaml:"detector"`
	Proposer  *proposer.Config  `yaml:"proposer"`
	Submitter *submitter.Config `yaml:"submitter"`
	Dora      *dora.Config      `yaml:"dora"`

	// StartSlot, if set via --start-slot flag, rescans from this slot to head on startup.
	// Useful for rescanning historical attestations.
	StartSlot uint64 `yaml:"-"`

	// StartSlotEnabled indicates --start-slot was explicitly set.
	StartSlotEnabled bool `yaml:"-"`

	// BackfillSlots is the number of historical slots to fetch on startup.
	// Only used if StartSlot is 0.
	// Default is 64 slots (about 2 epochs).
	BackfillSlots uint64 `yaml:"backfill_slots"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		Beacon:        beacon.DefaultConfig(),
		Indexer:       indexer.DefaultConfig(),
		Detector:      detector.DefaultConfig(),
		Proposer:      proposer.DefaultConfig(),
		Submitter:     submitter.DefaultConfig(),
		Dora:          dora.DefaultConfig(),
		BackfillSlots: 64,
	}
}

// Validate checks the configuration for errors.
func (c *Config) Validate() error {
	if c.Beacon == nil {
		c.Beacon = beacon.DefaultConfig()
	}

	if err := c.Beacon.Validate(); err != nil {
		return fmt.Errorf("beacon config: %w", err)
	}

	if c.Indexer == nil {
		c.Indexer = indexer.DefaultConfig()
	}

	if err := c.Indexer.Validate(); err != nil {
		return fmt.Errorf("indexer config: %w", err)
	}

	if c.Detector == nil {
		c.Detector = detector.DefaultConfig()
	}

	if err := c.Detector.Validate(); err != nil {
		return fmt.Errorf("detector config: %w", err)
	}

	if c.Proposer == nil {
		c.Proposer = proposer.DefaultConfig()
	}

	if err := c.Proposer.Validate(); err != nil {
		return fmt.Errorf("proposer config: %w", err)
	}

	if c.Submitter == nil {
		c.Submitter = submitter.DefaultConfig()
	}

	if err := c.Submitter.Validate(); err != nil {
		return fmt.Errorf("submitter config: %w", err)
	}

	if c.Dora == nil {
		c.Dora = dora.DefaultConfig()
	}

	if err := c.Dora.Validate(); err != nil {
		return fmt.Errorf("dora config: %w", err)
	}

	if c.BackfillSlots == 0 {
		c.BackfillSlots = 64
	}

	return nil
}
