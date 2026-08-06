package detector

import (
	"context"
	"testing"

	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/sirupsen/logrus"

	"github.com/slashoor/slashoor/pkg/beacon"
	"github.com/slashoor/slashoor/pkg/indexer"
)

// stubBeacon implements beacon.Service, resolving attesting validators from a
// fixed map keyed by the attestation's first signature byte.
type stubBeacon struct {
	beacon.Service
	validatorsBySig map[byte][]phase0.ValidatorIndex
}

func (s *stubBeacon) ResolveAttestingValidators(
	_ context.Context,
	att *beacon.Attestation,
) ([]phase0.ValidatorIndex, error) {
	return s.validatorsBySig[att.Signature[0]], nil
}

func makeCandidate(version1, version2 string) *indexer.SlashingCandidate {
	att := func(sigByte byte, version string) *beacon.Attestation {
		var sig phase0.BLSSignature

		sig[0] = sigByte

		return &beacon.Attestation{
			Data: &beacon.AttestationData{
				Slot:   100,
				Source: &beacon.Checkpoint{Epoch: 2},
				Target: &beacon.Checkpoint{Epoch: 3},
			},
			Signature: sig,
			Version:   version,
		}
	}

	return &indexer.SlashingCandidate{
		Type:         indexer.ViolationDoubleVote,
		Attestation1: att(0x01, version1),
		Attestation2: att(0x02, version2),
	}
}

func newTestDetector(t *testing.T, stub *stubBeacon) (Service, *[]*beacon.AttesterSlashing) {
	t.Helper()

	log := logrus.New()
	log.SetLevel(logrus.PanicLevel)

	svc := New(&Config{Enabled: true}, stub, log)

	slashings := &[]*beacon.AttesterSlashing{}

	svc.OnSlashing(func(sl *beacon.AttesterSlashing) {
		*slashings = append(*slashings, sl)
	})

	return svc, slashings
}

func TestSlashingCarriesFullAttestingSets(t *testing.T) {
	stub := &stubBeacon{validatorsBySig: map[byte][]phase0.ValidatorIndex{
		0x01: {1, 2, 3},
		0x02: {2, 3, 4, 5},
	}}

	svc, slashings := newTestDetector(t, stub)

	svc.HandleCandidate(context.Background(), makeCandidate("electra", "electra"))

	if len(*slashings) != 1 {
		t.Fatalf("got %d slashings, want 1", len(*slashings))
	}

	sl := (*slashings)[0]

	// Each half must carry its own full attesting set: the aggregate
	// signature covers every participant, so submitting the intersection
	// would fail signature verification on the node.
	if len(sl.Attestation1.AttestingIndices) != 3 {
		t.Fatalf("attestation1 indices = %v, want full set of 3", sl.Attestation1.AttestingIndices)
	}

	if len(sl.Attestation2.AttestingIndices) != 4 {
		t.Fatalf("attestation2 indices = %v, want full set of 4", sl.Attestation2.AttestingIndices)
	}

	if sl.Version != "electra" {
		t.Fatalf("version = %q, want electra", sl.Version)
	}
}

func TestNoSlashingWithoutOverlap(t *testing.T) {
	stub := &stubBeacon{validatorsBySig: map[byte][]phase0.ValidatorIndex{
		0x01: {1, 2},
		0x02: {3, 4},
	}}

	svc, slashings := newTestDetector(t, stub)

	svc.HandleCandidate(context.Background(), makeCandidate("electra", "electra"))

	if len(*slashings) != 0 {
		t.Fatalf("got %d slashings, want 0 for disjoint validator sets", len(*slashings))
	}
}

func TestSlashingVersionUsesNewerFork(t *testing.T) {
	stub := &stubBeacon{validatorsBySig: map[byte][]phase0.ValidatorIndex{
		0x01: {1},
		0x02: {1},
	}}

	svc, slashings := newTestDetector(t, stub)

	svc.HandleCandidate(context.Background(), makeCandidate("electra", "gloas"))

	if len(*slashings) != 1 {
		t.Fatalf("got %d slashings, want 1", len(*slashings))
	}

	if (*slashings)[0].Version != "gloas" {
		t.Fatalf("version = %q, want gloas", (*slashings)[0].Version)
	}
}
