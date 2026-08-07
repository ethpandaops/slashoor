package indexer

import (
	"errors"
	"fmt"

	"github.com/ethpandaops/go-eth2-client/spec/phase0"
)

// Config parameterizes the chunked min/max target arrays that back surround and
// double-vote detection. It must stay constant for the life of a database; a
// change requires a reset. These are the same parameters Lighthouse and Prysm
// converged on.
type Config struct {
	// ChunkSize is the number of epochs per chunk (C).
	ChunkSize uint64 `yaml:"chunk_size"`
	// ValidatorChunkSize is the number of validators per chunk (K).
	ValidatorChunkSize uint64 `yaml:"validator_chunk_size"`
	// HistoryLength is the sliding detection window in epochs (H). Must be a
	// multiple of ChunkSize and <= MaxHistoryLength.
	HistoryLength uint64 `yaml:"history_length"`
}

// DefaultConfig returns the industry-standard parameters (Lighthouse/Prysm):
// C=16 epochs, K=256 validators, H=4096 epochs (~18 days).
func DefaultConfig() Config {
	return Config{ChunkSize: 16, ValidatorChunkSize: 256, HistoryLength: 4096}
}

// MaxHistoryLength is the largest supported window. Distances are uint16, so
// targets must stay within 2^16 epochs of their array index.
const MaxHistoryLength = 65536

// Validate checks the configuration invariants.
func (c Config) Validate() error {
	if c.ChunkSize == 0 || c.ValidatorChunkSize == 0 || c.HistoryLength == 0 {
		return errors.New("chunk size, validator chunk size and history length must be non-zero")
	}
	if c.HistoryLength%c.ChunkSize != 0 {
		return fmt.Errorf("history length %d must be a multiple of chunk size %d", c.HistoryLength, c.ChunkSize)
	}
	if c.HistoryLength > MaxHistoryLength {
		return fmt.Errorf("history length %d exceeds maximum %d", c.HistoryLength, MaxHistoryLength)
	}
	return nil
}

// cellsPerChunk is C*K.
func (c Config) cellsPerChunk() int {
	return int(c.ChunkSize * c.ValidatorChunkSize)
}

// EpochChunk returns the epoch-chunk index for an epoch.
func (c Config) EpochChunk(epoch phase0.Epoch) uint32 {
	return uint32(uint64(epoch) / c.ChunkSize)
}

// ValidatorChunk returns the validator-chunk index for a validator.
func (c Config) ValidatorChunk(index phase0.ValidatorIndex) uint32 {
	return uint32(uint64(index) / c.ValidatorChunkSize)
}

// CellIndex returns the flat cell offset for (validator, epoch) within a chunk.
func (c Config) CellIndex(index phase0.ValidatorIndex, epoch phase0.Epoch) int {
	valOff := uint64(index) % c.ValidatorChunkSize
	epochOff := uint64(epoch) % c.ChunkSize
	return int(valOff*c.ChunkSize + epochOff)
}
