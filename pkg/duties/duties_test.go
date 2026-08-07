package duties

import (
	"testing"

	bitfield "github.com/OffchainLabs/go-bitfield"
	"github.com/ethpandaops/go-eth2-client/spec/all"
	"github.com/ethpandaops/go-eth2-client/spec/phase0"
	"github.com/ethpandaops/go-eth2-client/spec/version"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExpandAttestationPhase0(t *testing.T) {
	committee := []phase0.ValidatorIndex{10, 11, 12, 13}
	slotCommittees := [][]phase0.ValidatorIndex{committee}

	aggBits := bitfield.NewBitlist(4)
	aggBits.SetBitAt(0, true)
	aggBits.SetBitAt(2, true)

	att := &all.Attestation{
		Version:         version.DataVersionDeneb,
		AggregationBits: aggBits,
		Data:            &phase0.AttestationData{Slot: 100, Index: 0},
	}

	got, err := ExpandAttestation(att, slotCommittees, false)
	require.NoError(t, err)
	assert.Equal(t, []phase0.ValidatorIndex{10, 12}, got)
}

func TestExpandAttestationElectra(t *testing.T) {
	// Two committees this slot; the attestation aggregates committees 0 and 1.
	committee0 := []phase0.ValidatorIndex{10, 11}
	committee1 := []phase0.ValidatorIndex{20, 21, 22}
	slotCommittees := [][]phase0.ValidatorIndex{committee0, committee1}

	// aggregation bits are concatenated: [c0.0 c0.1 | c1.0 c1.1 c1.2] = 5 bits.
	aggBits := bitfield.NewBitlist(5)
	aggBits.SetBitAt(0, true) // committee0[0] = 10
	aggBits.SetBitAt(2, true) // committee1[0] = 20
	aggBits.SetBitAt(4, true) // committee1[2] = 22

	committeeBits := bitfield.NewBitvector64()
	committeeBits.SetBitAt(0, true)
	committeeBits.SetBitAt(1, true)

	att := &all.Attestation{
		Version:         version.DataVersionElectra,
		AggregationBits: aggBits,
		CommitteeBits:   committeeBits,
		Data:            &phase0.AttestationData{Slot: 100, Index: 0},
	}

	got, err := ExpandAttestation(att, slotCommittees, true)
	require.NoError(t, err)
	assert.Equal(t, []phase0.ValidatorIndex{10, 20, 22}, got)
}

func TestExpandAttestationOutOfRange(t *testing.T) {
	slotCommittees := [][]phase0.ValidatorIndex{{1, 2}}
	aggBits := bitfield.NewBitlist(2)
	att := &all.Attestation{
		Version:         version.DataVersionDeneb,
		AggregationBits: aggBits,
		Data:            &phase0.AttestationData{Slot: 100, Index: 5},
	}
	_, err := ExpandAttestation(att, slotCommittees, false)
	require.Error(t, err)
}
