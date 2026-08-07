package indexer

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/cockroachdb/pebble"
	"github.com/ethpandaops/go-eth2-client/spec/phase0"
)

// neutralMin is the min-chunk sentinel (target = +infinity: no constraint).
const neutralMin = uint16(0xFFFF)

// neutralMax is the max-chunk sentinel (target = index epoch: no constraint).
const neutralMax = uint16(0x0000)

// Txn is a batched read-modify-write context over the index. Chunks, attester
// groups and dictionaries are held decoded in an in-memory overlay and flushed
// to a single Pebble batch on Commit, so repeated updates within one processing
// batch avoid re-encoding. Proposer records are written through the (indexed)
// batch immediately for read-your-writes. A Txn is not safe for concurrent use.
type Txn struct {
	s *Store
	b *pebble.Batch

	minChunks  map[uint64][]uint16
	maxChunks  map[uint64][]uint16
	minDirty   map[uint64]bool
	maxDirty   map[uint64]bool
	groups     map[uint64][]uint16
	groupDirty map[uint64]bool
	dicts      map[uint32]*EpochDict
	dictDirty  map[uint32]bool
}

// NewTxn starts a new batched transaction.
func (s *Store) NewTxn() *Txn {
	return &Txn{
		s:          s,
		b:          s.db.NewIndexedBatch(),
		minChunks:  make(map[uint64][]uint16),
		maxChunks:  make(map[uint64][]uint16),
		minDirty:   make(map[uint64]bool),
		maxDirty:   make(map[uint64]bool),
		groups:     make(map[uint64][]uint16),
		groupDirty: make(map[uint64]bool),
		dicts:      make(map[uint32]*EpochDict),
		dictDirty:  make(map[uint32]bool),
	}
}

// Close discards the transaction without committing. Safe to call after Commit.
func (t *Txn) Close() {
	if t.b != nil {
		_ = t.b.Close()
		t.b = nil
	}
}

func chunkCacheKey(epochChunk, valChunk uint32) uint64 {
	return uint64(epochChunk)<<32 | uint64(valChunk)
}

func groupCacheKey(epoch phase0.Epoch, valChunk uint32) uint64 {
	return uint64(uint32(epoch))<<32 | uint64(valChunk)
}

// MinChunk returns the decoded min-target chunk, loading it from disk (or
// neutral-filling a new one) on first access. The returned slice is owned by
// the Txn; mutate it in place and call DirtyMinChunk to persist.
func (t *Txn) MinChunk(epochChunk, valChunk uint32) ([]uint16, error) {
	return t.chunk(t.minChunks, prefixMinChunk, neutralMin, epochChunk, valChunk)
}

// MaxChunk mirrors MinChunk for the max-target array.
func (t *Txn) MaxChunk(epochChunk, valChunk uint32) ([]uint16, error) {
	return t.chunk(t.maxChunks, prefixMaxChunk, neutralMax, epochChunk, valChunk)
}

func (t *Txn) chunk(cache map[uint64][]uint16, prefix byte, neutral uint16, epochChunk, valChunk uint32) ([]uint16, error) {
	ck := chunkCacheKey(epochChunk, valChunk)
	if c, ok := cache[ck]; ok {
		return c, nil
	}
	raw, err := t.s.getBytes(chunkKey(prefix, epochChunk, valChunk))
	if err != nil {
		return nil, err
	}
	var cells []uint16
	if raw == nil {
		cells = make([]uint16, t.s.cfg.cellsPerChunk())
		if neutral != 0 {
			for i := range cells {
				cells[i] = neutral
			}
		}
	} else {
		cells, err = decodeChunk(raw, t.s.cfg.cellsPerChunk())
		if err != nil {
			return nil, err
		}
	}
	cache[ck] = cells
	return cells, nil
}

// DirtyMinChunk marks a min chunk for flush at Commit.
func (t *Txn) DirtyMinChunk(epochChunk, valChunk uint32) {
	t.minDirty[chunkCacheKey(epochChunk, valChunk)] = true
}

// DirtyMaxChunk marks a max chunk for flush at Commit.
func (t *Txn) DirtyMaxChunk(epochChunk, valChunk uint32) {
	t.maxDirty[chunkCacheKey(epochChunk, valChunk)] = true
}

// Group returns the K-length attester ref slice for (target epoch, validator
// chunk), zero-filled if absent. Mutate in place and call DirtyGroup.
func (t *Txn) Group(epoch phase0.Epoch, valChunk uint32) ([]uint16, error) {
	gk := groupCacheKey(epoch, valChunk)
	if g, ok := t.groups[gk]; ok {
		return g, nil
	}
	raw, err := t.s.getBytes(groupKey(epoch, valChunk))
	if err != nil {
		return nil, err
	}
	var refs []uint16
	if raw == nil {
		refs = make([]uint16, t.s.cfg.ValidatorChunkSize)
	} else {
		refs, err = decodeGroup(raw, int(t.s.cfg.ValidatorChunkSize))
		if err != nil {
			return nil, err
		}
	}
	t.groups[gk] = refs
	return refs, nil
}

// DirtyGroup marks an attester group for flush at Commit.
func (t *Txn) DirtyGroup(epoch phase0.Epoch, valChunk uint32) {
	t.groupDirty[groupCacheKey(epoch, valChunk)] = true
}

// Dict returns the epoch's attestation-data dictionary, empty if absent.
// Mutate via Intern and call DirtyDict.
func (t *Txn) Dict(epoch phase0.Epoch) (*EpochDict, error) {
	e32 := uint32(epoch)
	if d, ok := t.dicts[e32]; ok {
		return d, nil
	}
	raw, err := t.s.getBytes(dictKey(epoch))
	if err != nil {
		return nil, err
	}
	var d *EpochDict
	if raw == nil {
		d = &EpochDict{}
	} else {
		d, err = decodeDict(raw)
		if err != nil {
			return nil, err
		}
	}
	t.dicts[e32] = d
	return d, nil
}

// DirtyDict marks an epoch dictionary for flush at Commit.
func (t *Txn) DirtyDict(epoch phase0.Epoch) {
	t.dictDirty[uint32(epoch)] = true
}

// ProposerRoot returns the recorded block root for (slot, proposer), if any.
func (t *Txn) ProposerRoot(slot phase0.Slot, proposer phase0.ValidatorIndex) (phase0.Root, bool, error) {
	val, closer, err := t.b.Get(proposerKey(slot, proposer))
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return phase0.Root{}, false, nil
		}
		return phase0.Root{}, false, err
	}
	var root phase0.Root
	copy(root[:], val)
	closeQuietly(closer)
	return root, true, nil
}

// SetProposerRoot records the block root for (slot, proposer).
func (t *Txn) SetProposerRoot(slot phase0.Slot, proposer phase0.ValidatorIndex, root phase0.Root) error {
	return t.b.Set(proposerKey(slot, proposer), root[:], nil)
}

// Commit flushes the overlay to the batch and commits it. When sync is false the
// commit is not fsync'd; crash safety then relies on the ingestion cursor being
// persisted only after a successful Commit.
func (t *Txn) Commit(sync bool) error {
	for ck := range t.minDirty {
		ec, vc := uint32(ck>>32), uint32(ck)
		if err := t.b.Set(chunkKey(prefixMinChunk, ec, vc), encodeChunk(t.minChunks[ck]), nil); err != nil {
			return err
		}
	}
	for ck := range t.maxDirty {
		ec, vc := uint32(ck>>32), uint32(ck)
		if err := t.b.Set(chunkKey(prefixMaxChunk, ec, vc), encodeChunk(t.maxChunks[ck]), nil); err != nil {
			return err
		}
	}
	for gk := range t.groupDirty {
		epoch, vc := phase0.Epoch(gk>>32), uint32(gk)
		if err := t.b.Set(groupKey(epoch, vc), encodeGroup(t.groups[gk]), nil); err != nil {
			return err
		}
	}
	for e32 := range t.dictDirty {
		epoch := phase0.Epoch(e32)
		if err := t.b.Set(dictKey(epoch), encodeDict(t.dicts[e32]), nil); err != nil {
			return err
		}
	}

	opts := pebble.NoSync
	if sync {
		opts = pebble.Sync
	}
	if err := t.b.Commit(opts); err != nil {
		return fmt.Errorf("commit index batch: %w", err)
	}
	return nil
}

// --- range-delete helpers used by Store.Prune ---

func delEpochRange(batch *pebble.Batch, prefix byte, keepFromEpoch phase0.Epoch) {
	start := []byte{prefix, 0, 0, 0, 0}
	end := make([]byte, 5)
	end[0] = prefix
	binary.BigEndian.PutUint32(end[1:5], uint32(keepFromEpoch))
	_ = batch.DeleteRange(start, end, nil)
}

func delChunkRange(batch *pebble.Batch, prefix byte, cutoffChunk uint32) {
	start := []byte{prefix, 0, 0, 0, 0}
	end := make([]byte, 5)
	end[0] = prefix
	binary.BigEndian.PutUint32(end[1:5], cutoffChunk)
	_ = batch.DeleteRange(start, end, nil)
}

func delSlotRange(batch *pebble.Batch, prefix byte, cutoffSlot uint64) {
	start := make([]byte, 9)
	start[0] = prefix
	end := make([]byte, 9)
	end[0] = prefix
	binary.BigEndian.PutUint64(end[1:9], cutoffSlot)
	_ = batch.DeleteRange(start, end, nil)
}
