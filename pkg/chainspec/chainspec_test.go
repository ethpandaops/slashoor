package chainspec

import (
	"testing"

	"github.com/ethpandaops/go-eth2-client/spec/phase0"
	"github.com/ethpandaops/go-eth2-client/spec/version"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testSpecMap() map[string]any {
	return map[string]any{
		"SLOTS_PER_EPOCH":        "32",
		"SECONDS_PER_SLOT":       "12",
		"GENESIS_FORK_VERSION":   "0x00000000",
		"ALTAIR_FORK_VERSION":    "0x01000000",
		"ALTAIR_FORK_EPOCH":      "10",
		"BELLATRIX_FORK_VERSION": "0x02000000",
		"BELLATRIX_FORK_EPOCH":   "20",
		"CAPELLA_FORK_VERSION":   "0x03000000",
		"CAPELLA_FORK_EPOCH":     "30",
		"DENEB_FORK_VERSION":     "0x04000000",
		"DENEB_FORK_EPOCH":       "40",
		"ELECTRA_FORK_VERSION":   "0x05000000",
		"ELECTRA_FORK_EPOCH":     "50",
		// Fulu present but unscheduled (far future).
		"FULU_FORK_VERSION": "0x06000000",
		"FULU_FORK_EPOCH":   "18446744073709551615",
	}
}

func TestNewAndSlotMath(t *testing.T) {
	s, err := New(testSpecMap())
	require.NoError(t, err)
	assert.Equal(t, uint64(32), s.SlotsPerEpoch())
	assert.Equal(t, phase0.Epoch(5), s.EpochOfSlot(160))
	assert.Equal(t, phase0.Slot(160), s.EpochStartSlot(5))
}

func TestForkSchedule(t *testing.T) {
	s, err := New(testSpecMap())
	require.NoError(t, err)

	// Fulu is far-future, so it is dropped: phase0..electra remain.
	assert.Len(t, s.Forks(), 6)

	assert.Equal(t, version.DataVersionPhase0, s.DataVersionAtEpoch(0))
	assert.Equal(t, version.DataVersionAltair, s.DataVersionAtEpoch(10))
	assert.Equal(t, version.DataVersionDeneb, s.DataVersionAtEpoch(49))
	assert.Equal(t, version.DataVersionElectra, s.DataVersionAtEpoch(50))
	assert.Equal(t, version.DataVersionElectra, s.DataVersionAtEpoch(1000))

	// By slot.
	assert.Equal(t, version.DataVersionElectra, s.DataVersionAtSlot(50*32))
	assert.Equal(t, version.DataVersionDeneb, s.DataVersionAtSlot(49*32))
}

func TestSubmittableFromEpoch(t *testing.T) {
	s, err := New(testSpecMap())
	require.NoError(t, err)

	// At head epoch 60 (electra active, deneb previous), evidence is submittable
	// down to the deneb fork epoch (40).
	assert.Equal(t, phase0.Epoch(40), s.SubmittableFromEpoch(60))

	// At head epoch 25 (bellatrix active, altair previous) -> altair epoch 10.
	assert.Equal(t, phase0.Epoch(10), s.SubmittableFromEpoch(25))
}

func TestMissingSlotsPerEpoch(t *testing.T) {
	_, err := New(map[string]any{"GENESIS_FORK_VERSION": "0x00000000"})
	require.Error(t, err)
}
