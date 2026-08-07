package indexer

import (
	"testing"

	"github.com/ethpandaops/go-eth2-client/spec/phase0"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), DefaultConfig(), logrus.New())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestConfigValidate(t *testing.T) {
	assert.NoError(t, DefaultConfig().Validate())
	assert.Error(t, Config{ChunkSize: 0, ValidatorChunkSize: 256, HistoryLength: 4096}.Validate())
	assert.Error(t, Config{ChunkSize: 16, ValidatorChunkSize: 256, HistoryLength: 4095}.Validate(), "not multiple of chunk size")
	assert.Error(t, Config{ChunkSize: 16, ValidatorChunkSize: 256, HistoryLength: MaxHistoryLength + 16}.Validate())
}

func TestConfigGuardMismatch(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, DefaultConfig(), logrus.New())
	require.NoError(t, err)
	require.NoError(t, s.Close())

	// Reopen with same config: ok.
	s2, err := Open(dir, DefaultConfig(), logrus.New())
	require.NoError(t, err)
	require.NoError(t, s2.Close())

	// Reopen with different history length: rejected.
	bad := DefaultConfig()
	bad.HistoryLength = 8192
	_, err = Open(dir, bad, logrus.New())
	require.Error(t, err)
}

func TestCursorRoundtrip(t *testing.T) {
	s := testStore(t)
	c, err := s.Cursor()
	require.NoError(t, err)
	assert.Nil(t, c)

	want := &Cursor{Mode: "genesis", StartSlot: 0, ProcessedSlot: 12345, BackfillComplete: false}
	require.NoError(t, s.SetCursor(want))
	got, err := s.Cursor()
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestSlashedSet(t *testing.T) {
	s := testStore(t)
	ok, err := s.IsSlashed(42)
	require.NoError(t, err)
	assert.False(t, ok)

	require.NoError(t, s.MarkSlashed(42, 43))
	for _, idx := range []phase0.ValidatorIndex{42, 43} {
		ok, err := s.IsSlashed(idx)
		require.NoError(t, err)
		assert.True(t, ok)
	}
	ok, err = s.IsSlashed(44)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestSubmittedMarkers(t *testing.T) {
	s := testStore(t)
	id := []byte("offense-1")
	ok, err := s.HasSubmitted(id)
	require.NoError(t, err)
	assert.False(t, ok)
	require.NoError(t, s.MarkSubmitted(id))
	ok, err = s.HasSubmitted(id)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestCellIndex(t *testing.T) {
	c := DefaultConfig()
	// validator 0, epoch 0 -> cell 0
	assert.Equal(t, 0, c.CellIndex(0, 0))
	// validator 0, epoch 1 -> cell 1 (epoch offset)
	assert.Equal(t, 1, c.CellIndex(0, 1))
	// validator 1, epoch 0 -> cell C
	assert.Equal(t, int(c.ChunkSize), c.CellIndex(1, 0))
	// wraps within chunk
	assert.Equal(t, 0, c.CellIndex(phase0.ValidatorIndex(c.ValidatorChunkSize), c.EpochStartWrap()))
}

// EpochStartWrap is a tiny helper for the test above (epoch multiple of C).
func (c Config) EpochStartWrap() phase0.Epoch { return phase0.Epoch(c.ChunkSize) }

func TestChunkRoundtrip(t *testing.T) {
	s := testStore(t)
	txn := s.NewTxn()
	defer txn.Close()

	min, err := txn.MinChunk(3, 7)
	require.NoError(t, err)
	assert.Len(t, min, DefaultConfig().cellsPerChunk())
	for _, c := range min {
		assert.Equal(t, neutralMin, c, "min chunk neutral-filled")
	}
	min[5] = 42
	txn.DirtyMinChunk(3, 7)

	max, err := txn.MaxChunk(3, 7)
	require.NoError(t, err)
	assert.Equal(t, neutralMax, max[0], "max chunk zero-filled")
	max[9] = 100
	txn.DirtyMaxChunk(3, 7)

	require.NoError(t, txn.Commit(true))

	// New txn sees persisted values.
	txn2 := s.NewTxn()
	defer txn2.Close()
	min2, err := txn2.MinChunk(3, 7)
	require.NoError(t, err)
	assert.Equal(t, uint16(42), min2[5])
	max2, err := txn2.MaxChunk(3, 7)
	require.NoError(t, err)
	assert.Equal(t, uint16(100), max2[9])
}

func TestDictInternAndGroup(t *testing.T) {
	s := testStore(t)
	txn := s.NewTxn()
	defer txn.Close()

	epoch := phase0.Epoch(10)
	d, err := txn.Dict(epoch)
	require.NoError(t, err)

	e1 := DictEntry{DataRoot8: [8]byte{1}, Slot: 320, BlockRoot: phase0.Root{0xaa}, AttIndex: 0}
	e2 := DictEntry{DataRoot8: [8]byte{2}, Slot: 321, BlockRoot: phase0.Root{0xbb}, AttIndex: 1}
	ref1 := d.Intern(e1)
	ref2 := d.Intern(e2)
	assert.Equal(t, uint16(1), ref1)
	assert.Equal(t, uint16(2), ref2)
	assert.Equal(t, ref1, d.Intern(e1), "intern is idempotent by data root")
	txn.DirtyDict(epoch)

	refs, err := txn.Group(epoch, 0)
	require.NoError(t, err)
	refs[100] = ref1
	refs[101] = ref2
	txn.DirtyGroup(epoch, 0)

	require.NoError(t, txn.Commit(true))

	txn2 := s.NewTxn()
	defer txn2.Close()
	d2, err := txn2.Dict(epoch)
	require.NoError(t, err)
	require.Len(t, d2.Entries, 2)
	got, ok := d2.Get(ref1)
	require.True(t, ok)
	assert.Equal(t, e1, got)

	refs2, err := txn2.Group(epoch, 0)
	require.NoError(t, err)
	assert.Equal(t, ref1, refs2[100])
	assert.Equal(t, ref2, refs2[101])
	assert.Equal(t, uint16(0), refs2[0])
}

func TestProposerRecords(t *testing.T) {
	s := testStore(t)
	txn := s.NewTxn()
	defer txn.Close()

	_, ok, err := txn.ProposerRoot(64, 5)
	require.NoError(t, err)
	assert.False(t, ok)

	rootA := phase0.Root{0x11}
	require.NoError(t, txn.SetProposerRoot(64, 5, rootA))
	got, ok, err := txn.ProposerRoot(64, 5)
	require.NoError(t, err)
	require.True(t, ok, "read-your-writes within a txn")
	assert.Equal(t, rootA, got)
}

func TestPruneBoundaries(t *testing.T) {
	s := testStore(t)
	cfg := DefaultConfig()

	// Write dict entries at epochs 10 and 5000, chunks accordingly.
	write := func(epoch phase0.Epoch) {
		txn := s.NewTxn()
		d, err := txn.Dict(epoch)
		require.NoError(t, err)
		d.Intern(DictEntry{DataRoot8: [8]byte{byte(epoch)}, Slot: cfg.EpochStartSlot(epoch)})
		txn.DirtyDict(epoch)
		refs, err := txn.Group(epoch, 0)
		require.NoError(t, err)
		refs[0] = 1
		txn.DirtyGroup(epoch, 0)
		min, err := txn.MinChunk(cfg.EpochChunk(epoch), 0)
		require.NoError(t, err)
		min[0] = 1
		txn.DirtyMinChunk(cfg.EpochChunk(epoch), 0)
		require.NoError(t, txn.Commit(true))
	}
	write(10)
	write(5000)

	// Keep from epoch 4096: epoch 10 data should go, epoch 5000 should stay.
	require.NoError(t, s.Prune(4096, cfg.SlotsPerEpochForTest()))

	txn := s.NewTxn()
	defer txn.Close()
	dOld, err := txn.Dict(10)
	require.NoError(t, err)
	assert.Len(t, dOld.Entries, 0, "old epoch dict pruned")
	dNew, err := txn.Dict(5000)
	require.NoError(t, err)
	assert.Len(t, dNew.Entries, 1, "recent epoch dict retained")
}

// SlotsPerEpochForTest exposes a fixed slots-per-epoch for prune tests.
func (Config) SlotsPerEpochForTest() uint64 { return 32 }

// EpochStartSlot mirrors chainspec math for tests that need slot boundaries.
func (c Config) EpochStartSlot(epoch phase0.Epoch) phase0.Slot {
	return phase0.Slot(uint64(epoch) * 32)
}
