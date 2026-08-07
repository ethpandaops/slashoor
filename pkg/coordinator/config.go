package coordinator

import (
	"fmt"

	"github.com/ethpandaops/slashoor/pkg/beacon"
	"github.com/ethpandaops/slashoor/pkg/dora"
	"github.com/ethpandaops/slashoor/pkg/indexer"
	"github.com/ethpandaops/slashoor/pkg/submitter"
)

// StartMode selects where indexing begins.
type StartMode string

const (
	// StartHead begins live-following at the current head with no historic scan.
	StartHead StartMode = "head"
	// StartGenesis scans forward from genesis, then follows head.
	StartGenesis StartMode = "genesis"
	// StartSlot scans forward from a configured slot, then follows head.
	StartSlot StartMode = "slot"
)

// StartConfig configures the indexing start point.
type StartConfig struct {
	Mode StartMode `yaml:"mode"`
	Slot uint64    `yaml:"slot"`
}

// DetectionConfig toggles the two detection paths.
type DetectionConfig struct {
	Attester bool `yaml:"attester"`
	Proposer bool `yaml:"proposer"`
}

// Config is the top-level slashoor configuration.
type Config struct {
	// DataDir is the directory for the local Pebble index.
	DataDir string `yaml:"data_dir"`

	Beacon    *beacon.Config    `yaml:"beacon"`
	Dora      *dora.Config      `yaml:"dora"`
	Indexer   indexer.Config    `yaml:"indexer"`
	Submitter *submitter.Config `yaml:"submitter"`
	Detection DetectionConfig   `yaml:"detection"`
	Start     StartConfig       `yaml:"start"`

	// BackfillWindowSlots bounds how many slots are loaded per backfill page.
	BackfillWindowSlots uint64 `yaml:"backfill_window_slots"`

	// ResetToSlot, set via --reset-to-slot, wipes the index and restarts the
	// scan from this slot (0 = genesis). Negative means no reset.
	ResetToSlot  int64 `yaml:"-"`
	ResetEnabled bool  `yaml:"-"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		DataDir:             "./slashoor-db",
		Beacon:              beacon.DefaultConfig(),
		Dora:                dora.DefaultConfig(),
		Indexer:             indexer.DefaultConfig(),
		Submitter:           submitter.DefaultConfig(),
		Detection:           DetectionConfig{Attester: true, Proposer: true},
		Start:               StartConfig{Mode: StartHead},
		BackfillWindowSlots: 8192,
		ResetToSlot:         -1,
	}
}

// Validate normalizes and checks the configuration.
func (c *Config) Validate() error {
	if c.DataDir == "" {
		c.DataDir = "./slashoor-db"
	}
	if c.Beacon == nil {
		c.Beacon = beacon.DefaultConfig()
	}
	if err := c.Beacon.Validate(); err != nil {
		return fmt.Errorf("beacon config: %w", err)
	}
	if c.Dora == nil {
		c.Dora = dora.DefaultConfig()
	}
	if err := c.Dora.Validate(); err != nil {
		return fmt.Errorf("dora config: %w", err)
	}
	if c.Indexer.ChunkSize == 0 {
		c.Indexer = indexer.DefaultConfig()
	}
	if err := c.Indexer.Validate(); err != nil {
		return fmt.Errorf("indexer config: %w", err)
	}
	if c.Submitter == nil {
		c.Submitter = submitter.DefaultConfig()
	}
	if err := c.Submitter.Validate(); err != nil {
		return fmt.Errorf("submitter config: %w", err)
	}
	if c.Start.Mode == "" {
		c.Start.Mode = StartHead
	}
	switch c.Start.Mode {
	case StartHead, StartGenesis, StartSlot:
	default:
		return fmt.Errorf("invalid start mode %q (want head|genesis|slot)", c.Start.Mode)
	}
	if c.BackfillWindowSlots == 0 {
		c.BackfillWindowSlots = 8192
	}
	return nil
}
