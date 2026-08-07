package detector

import (
	"testing"

	"github.com/ethpandaops/go-eth2-client/spec/all"
	"github.com/ethpandaops/go-eth2-client/spec/phase0"
	"github.com/ethpandaops/go-eth2-client/spec/version"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func attData(source, target phase0.Epoch, blockRoot byte) *phase0.AttestationData {
	return &phase0.AttestationData{
		Slot:            phase0.Slot(target) * 32,
		Index:           0,
		BeaconBlockRoot: phase0.Root{blockRoot},
		Source:          &phase0.Checkpoint{Epoch: source},
		Target:          &phase0.Checkpoint{Epoch: target},
	}
}

func att(source, target phase0.Epoch, blockRoot byte) *all.Attestation {
	return &all.Attestation{Version: version.DataVersionElectra, Data: attData(source, target, blockRoot)}
}

func TestOrderSlashablePairDoubleVote(t *testing.T) {
	a := att(3, 5, 0xaa)
	b := att(3, 5, 0xbb) // same target, different block root -> double vote
	a1, a2, ok := orderSlashablePair(a, b)
	require.True(t, ok)
	assert.Equal(t, a, a1)
	assert.Equal(t, b, a2)
}

func TestOrderSlashablePairSurround(t *testing.T) {
	inner := att(4, 5, 0x01)
	outer := att(3, 6, 0x02) // outer surrounds inner
	// Pass in reversed order; ordering must put the surrounder first.
	a1, a2, ok := orderSlashablePair(inner, outer)
	require.True(t, ok)
	assert.Equal(t, outer, a1, "surrounder is attestation_1")
	assert.Equal(t, inner, a2)
}

func TestOrderSlashablePairNotSlashable(t *testing.T) {
	a := att(3, 5, 0x01)
	b := att(6, 8, 0x02) // disjoint, not slashable
	_, _, ok := orderSlashablePair(a, b)
	assert.False(t, ok)
}

func TestIntersect(t *testing.T) {
	a := []phase0.ValidatorIndex{1, 3, 5, 7, 9}
	b := []phase0.ValidatorIndex{2, 3, 5, 8, 9}
	assert.Equal(t, []phase0.ValidatorIndex{3, 5, 9}, intersect(a, b))
	assert.Empty(t, intersect([]phase0.ValidatorIndex{1, 2}, []phase0.ValidatorIndex{3, 4}))
}

func TestOffenseIDOrderIndependent(t *testing.T) {
	d1 := attData(3, 5, 0xaa)
	d2 := attData(3, 5, 0xbb)
	assert.Equal(t, attesterOffenseID(d1, d2), attesterOffenseID(d2, d1))

	r1 := phase0.Root{0x11}
	r2 := phase0.Root{0x22}
	assert.Equal(t, proposerOffenseID(r1, r2), proposerOffenseID(r2, r1))
	assert.NotEqual(t, attesterOffenseID(d1, d2), proposerOffenseID(r1, r2))
}

func TestNewerVersion(t *testing.T) {
	assert.Equal(t, version.DataVersionElectra, newerVersion(version.DataVersionDeneb, version.DataVersionElectra))
	assert.Equal(t, version.DataVersionElectra, newerVersion(version.DataVersionElectra, version.DataVersionDeneb))
}
