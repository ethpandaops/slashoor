package indexer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/cockroachdb/pebble"
	"github.com/ethpandaops/go-eth2-client/spec/phase0"
	"github.com/sirupsen/logrus"
)

// schemaVersion is bumped on any incompatible on-disk layout change.
const schemaVersion = 1

// Cursor tracks ingestion progress so a restart resumes rather than rescans.
type Cursor struct {
	// Mode is the configured start mode: "head", "genesis" or "slot".
	Mode string `json:"mode"`
	// StartSlot is the first slot of the configured scan (0 for genesis/head).
	StartSlot uint64 `json:"start_slot"`
	// ProcessedSlot is the highest slot fully processed during backfill; -1
	// before any backfill progress.
	ProcessedSlot int64 `json:"processed_slot"`
	// BackfillComplete is set once backfill has reached head and live
	// following has taken over.
	BackfillComplete bool `json:"backfill_complete"`
}

// Store is the Pebble-backed index.
type Store struct {
	db  *pebble.DB
	cfg Config
	log logrus.FieldLogger
}

// Open opens (or creates) the index at dir. If a database already exists its
// stored config must match cfg, otherwise Open returns an error instructing a
// reset.
func Open(dir string, cfg Config, log logrus.FieldLogger) (*Store, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		return nil, fmt.Errorf("open pebble at %q: %w", dir, err)
	}

	s := &Store{db: db, cfg: cfg, log: log.WithField("package", "indexer")}
	if err := s.guardConfig(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Config returns the active configuration.
func (s *Store) Config() Config { return s.cfg }

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

type storedMeta struct {
	SchemaVersion      int    `json:"schema_version"`
	ChunkSize          uint64 `json:"chunk_size"`
	ValidatorChunkSize uint64 `json:"validator_chunk_size"`
	HistoryLength      uint64 `json:"history_length"`
}

func (s *Store) guardConfig() error {
	raw, err := s.getBytes(metaKey("config"))
	if err != nil {
		return err
	}
	if raw == nil {
		meta := storedMeta{
			SchemaVersion:      schemaVersion,
			ChunkSize:          s.cfg.ChunkSize,
			ValidatorChunkSize: s.cfg.ValidatorChunkSize,
			HistoryLength:      s.cfg.HistoryLength,
		}
		blob, _ := json.Marshal(meta)
		return s.db.Set(metaKey("config"), blob, pebble.Sync)
	}

	var meta storedMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return fmt.Errorf("decode stored config: %w", err)
	}
	if meta.SchemaVersion != schemaVersion {
		return fmt.Errorf("index schema version %d != %d; reset required", meta.SchemaVersion, schemaVersion)
	}
	if meta.ChunkSize != s.cfg.ChunkSize ||
		meta.ValidatorChunkSize != s.cfg.ValidatorChunkSize ||
		meta.HistoryLength != s.cfg.HistoryLength {
		return fmt.Errorf(
			"index config mismatch (stored C=%d K=%d H=%d, configured C=%d K=%d H=%d); reset required",
			meta.ChunkSize, meta.ValidatorChunkSize, meta.HistoryLength,
			s.cfg.ChunkSize, s.cfg.ValidatorChunkSize, s.cfg.HistoryLength)
	}
	return nil
}

// Cursor returns the stored ingestion cursor, or nil if none is set yet.
func (s *Store) Cursor() (*Cursor, error) {
	raw, err := s.getBytes(metaKey("cursor"))
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	var c Cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("decode cursor: %w", err)
	}
	return &c, nil
}

// SetCursor persists the ingestion cursor.
func (s *Store) SetCursor(c *Cursor) error {
	blob, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return s.db.Set(metaKey("cursor"), blob, pebble.Sync)
}

// IsSlashed reports whether a validator is marked slashed in the local mirror.
func (s *Store) IsSlashed(index phase0.ValidatorIndex) (bool, error) {
	raw, err := s.getBytes(slashedKey(index))
	if err != nil {
		return false, err
	}
	return raw != nil, nil
}

// MarkSlashed marks validators as slashed in the local mirror.
func (s *Store) MarkSlashed(indices ...phase0.ValidatorIndex) error {
	if len(indices) == 0 {
		return nil
	}
	batch := s.db.NewBatch()
	defer func() { _ = batch.Close() }()
	for _, idx := range indices {
		if err := batch.Set(slashedKey(idx), []byte{1}, nil); err != nil {
			return err
		}
	}
	return batch.Commit(pebble.Sync)
}

// HasSubmitted reports whether an offense marker exists.
func (s *Store) HasSubmitted(id []byte) (bool, error) {
	raw, err := s.getBytes(submittedKey(id))
	if err != nil {
		return false, err
	}
	return raw != nil, nil
}

// MarkSubmitted records an offense marker so it is not re-submitted.
func (s *Store) MarkSubmitted(id []byte) error {
	return s.db.Set(submittedKey(id), []byte{1}, pebble.Sync)
}

// Prune deletes index data whose target/epoch fell out of the history window
// below keepFromEpoch. It is safe to call repeatedly.
func (s *Store) Prune(keepFromEpoch phase0.Epoch, slotsPerEpoch uint64) error {
	if keepFromEpoch == 0 {
		return nil
	}
	batch := s.db.NewBatch()
	defer func() { _ = batch.Close() }()

	// Epoch-keyed keyspaces: dict and attester groups.
	delEpochRange(batch, prefixDict, keepFromEpoch)
	delEpochRange(batch, prefixGroup, keepFromEpoch)

	// Chunk keyspaces are keyed by epoch-chunk.
	cutoffChunk := s.cfg.EpochChunk(keepFromEpoch)
	delChunkRange(batch, prefixMinChunk, cutoffChunk)
	delChunkRange(batch, prefixMaxChunk, cutoffChunk)

	// Proposer records are keyed by slot.
	cutoffSlot := uint64(keepFromEpoch) * slotsPerEpoch
	delSlotRange(batch, prefixProposer, cutoffSlot)

	return batch.Commit(pebble.Sync)
}

// getBytes reads a key, returning (nil, nil) when absent. The returned slice is
// a copy and safe to retain.
func (s *Store) getBytes(key []byte) ([]byte, error) {
	val, closer, err := s.db.Get(key)
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]byte, len(val))
	copy(out, val)
	closeQuietly(closer)
	return out, nil
}

func closeQuietly(c io.Closer) {
	if c != nil {
		_ = c.Close()
	}
}
