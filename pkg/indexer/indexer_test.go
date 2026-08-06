package indexer

import (
	"testing"

	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/sirupsen/logrus"

	"github.com/slashoor/slashoor/pkg/beacon"
)

func newTestIndexer(t *testing.T, cfg *Config) (*service, *[]*SlashingCandidate) {
	t.Helper()

	if cfg == nil {
		cfg = DefaultConfig()
	}

	log := logrus.New()
	log.SetLevel(logrus.PanicLevel)

	svc, ok := New(cfg, log).(*service)
	if !ok {
		t.Fatal("New did not return *service")
	}

	candidates := &[]*SlashingCandidate{}

	svc.OnCandidate(func(c *SlashingCandidate) {
		*candidates = append(*candidates, c)
	})

	return svc, candidates
}

func makeAttestation(
	slot phase0.Slot,
	index phase0.CommitteeIndex,
	sourceEpoch, targetEpoch phase0.Epoch,
	headRoot, targetRoot byte,
	sigByte byte,
) *beacon.Attestation {
	var (
		head, target phase0.Root
		sig          phase0.BLSSignature
	)

	head[0] = headRoot
	target[0] = targetRoot
	sig[0] = sigByte

	return &beacon.Attestation{
		AggregationBits: []byte{0xff},
		Data: &beacon.AttestationData{
			Slot:            slot,
			Index:           index,
			BeaconBlockRoot: head,
			Source:          &beacon.Checkpoint{Epoch: sourceEpoch},
			Target:          &beacon.Checkpoint{Epoch: targetEpoch, Root: target},
		},
		Signature: sig,
	}
}

func TestDoubleVoteDifferingTargetRoot(t *testing.T) {
	s, candidates := newTestIndexer(t, nil)

	s.ProcessAttestation(makeAttestation(64, 0, 1, 2, 0xaa, 0x01, 0x01))
	s.ProcessAttestation(makeAttestation(64, 0, 1, 2, 0xaa, 0x02, 0x02))

	if len(*candidates) != 1 {
		t.Fatalf("got %d candidates, want 1", len(*candidates))
	}

	if (*candidates)[0].Type != ViolationDoubleVote {
		t.Fatalf("got %v, want double vote", (*candidates)[0].Type)
	}
}

func TestDoubleVoteDifferingHeadRootOnly(t *testing.T) {
	// Same target root but rival head roots is still differing attestation
	// data with the same target epoch, i.e. a slashable double vote.
	s, candidates := newTestIndexer(t, nil)

	s.ProcessAttestation(makeAttestation(64, 0, 1, 2, 0xaa, 0x01, 0x01))
	s.ProcessAttestation(makeAttestation(64, 0, 1, 2, 0xbb, 0x01, 0x02))

	if len(*candidates) != 1 {
		t.Fatalf("got %d candidates, want 1", len(*candidates))
	}
}

func TestDoubleVoteDifferingPayloadIndex(t *testing.T) {
	// Gloas repurposes data.index as a payload-availability bit; attestations
	// differing only in index conflict and must be compared, not bucketed apart.
	s, candidates := newTestIndexer(t, nil)

	s.ProcessAttestation(makeAttestation(64, 0, 1, 2, 0xaa, 0x01, 0x01))
	s.ProcessAttestation(makeAttestation(64, 1, 1, 2, 0xaa, 0x01, 0x02))

	if len(*candidates) != 1 {
		t.Fatalf("got %d candidates, want 1", len(*candidates))
	}
}

func TestNoCandidateForSameDataDifferentAggregate(t *testing.T) {
	s, candidates := newTestIndexer(t, nil)

	// Same data, different aggregate signatures: never slashable.
	s.ProcessAttestation(makeAttestation(64, 0, 1, 2, 0xaa, 0x01, 0x01))
	s.ProcessAttestation(makeAttestation(64, 0, 1, 2, 0xaa, 0x01, 0x02))

	if len(*candidates) != 0 {
		t.Fatalf("got %d candidates, want 0", len(*candidates))
	}
}

func TestReprocessingSameAttestationIsDeduplicated(t *testing.T) {
	s, candidates := newTestIndexer(t, nil)

	att := makeAttestation(64, 0, 1, 2, 0xaa, 0x01, 0x01)
	s.ProcessAttestation(att)
	s.ProcessAttestation(att)

	s.mu.RLock()
	stored := len(s.attestations[attestationKey{slot: 64, targetEpoch: 2}])
	s.mu.RUnlock()

	if stored != 1 {
		t.Fatalf("stored %d copies, want 1", stored)
	}

	// A later conflicting attestation must yield exactly one candidate, not
	// one per stored duplicate.
	s.ProcessAttestation(makeAttestation(64, 0, 1, 2, 0xaa, 0x02, 0x02))

	if len(*candidates) != 1 {
		t.Fatalf("got %d candidates, want 1", len(*candidates))
	}
}

func TestSurroundVoteDetection(t *testing.T) {
	s, candidates := newTestIndexer(t, nil)

	// Stored (source 5, target 6), then (source 4, target 7) surrounds it.
	s.ProcessAttestation(makeAttestation(192, 0, 5, 6, 0xaa, 0x01, 0x01))
	s.ProcessAttestation(makeAttestation(224, 0, 4, 7, 0xbb, 0x02, 0x02))

	if len(*candidates) != 1 {
		t.Fatalf("got %d candidates, want 1", len(*candidates))
	}

	if (*candidates)[0].Type != ViolationSurroundVote {
		t.Fatalf("got %v, want surround vote", (*candidates)[0].Type)
	}
}

func TestSurroundedVoteDetection(t *testing.T) {
	s, candidates := newTestIndexer(t, nil)

	// Stored (source 4, target 7), then (source 5, target 6) is surrounded by it.
	s.ProcessAttestation(makeAttestation(224, 0, 4, 7, 0xbb, 0x01, 0x01))
	s.ProcessAttestation(makeAttestation(192, 0, 5, 6, 0xaa, 0x02, 0x02))

	if len(*candidates) != 1 {
		t.Fatalf("got %d candidates, want 1", len(*candidates))
	}

	if (*candidates)[0].Type != ViolationSurroundVote {
		t.Fatalf("got %v, want surround vote", (*candidates)[0].Type)
	}
}

func TestNoCandidateForHonestChain(t *testing.T) {
	s, candidates := newTestIndexer(t, nil)

	// A normal sequence of honest attestations across epochs.
	for epoch := phase0.Epoch(1); epoch <= 10; epoch++ {
		slot := phase0.Slot(epoch * 32)
		s.ProcessAttestation(makeAttestation(slot, 0, epoch-1, epoch, byte(epoch), byte(epoch), byte(epoch)))
	}

	if len(*candidates) != 0 {
		t.Fatalf("got %d candidates, want 0", len(*candidates))
	}
}

func TestPruningRespectsMaxEpochsToKeep(t *testing.T) {
	s, _ := newTestIndexer(t, &Config{MaxEpochsToKeep: 4})

	for epoch := phase0.Epoch(1); epoch <= 10; epoch++ {
		slot := phase0.Slot(epoch * 32)
		s.ProcessAttestation(makeAttestation(slot, 0, epoch-1, epoch, byte(epoch), byte(epoch), byte(epoch)))
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	for key := range s.attestations {
		if key.targetEpoch < 6 {
			t.Fatalf("attestation with target epoch %d should have been pruned", key.targetEpoch)
		}
	}

	if len(s.attestations) == 0 {
		t.Fatal("recent attestations should have been retained")
	}
}

func TestEpochBounds(t *testing.T) {
	b := NewEpochBounds()

	b.Update(5, 6)
	b.Update(8, 9)

	// m(i): min target for source > i.
	if got := b.GetMinTarget(4); got != 6 {
		t.Fatalf("GetMinTarget(4) = %d, want 6", got)
	}

	if got := b.GetMinTarget(7); got != 9 {
		t.Fatalf("GetMinTarget(7) = %d, want 9", got)
	}

	if got := b.GetMinTarget(8); got != maxEpochValue {
		t.Fatalf("GetMinTarget(8) = %d, want none", got)
	}

	// M(i): max target for source < i.
	if got := b.GetMaxTarget(9); got != 9 {
		t.Fatalf("GetMaxTarget(9) = %d, want 9", got)
	}

	if got := b.GetMaxTarget(6); got != 6 {
		t.Fatalf("GetMaxTarget(6) = %d, want 6", got)
	}

	if got := b.GetMaxTarget(5); got != minEpochValue {
		t.Fatalf("GetMaxTarget(5) = %d, want none", got)
	}

	// Memoized answers must be invalidated by an improving update.
	b.Update(6, 5)

	if got := b.GetMinTarget(4); got != 5 {
		t.Fatalf("GetMinTarget(4) after update = %d, want 5", got)
	}

	// Surround gates: stored (5,6); attestation (4,7) surrounds it.
	if !b.CheckSurroundVote(4, 7) {
		t.Fatal("CheckSurroundVote(4,7) should pass with stored (5,6)")
	}

	// Attestation (5,6) is surrounded by a stored (4,7).
	b.Update(4, 7)

	if !b.CheckSurroundedVote(5, 6) {
		t.Fatal("CheckSurroundedVote(5,6) should pass with stored (4,7)")
	}
}
