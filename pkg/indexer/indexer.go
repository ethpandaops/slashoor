// Package indexer is the local Pebble-backed detection index and the chunked
// min/max detection engine on top of it. It stores the min/max target arrays,
// per-epoch attestation-data dictionaries and attester reference groups (for
// surround and double-vote detection), proposer block roots (for double-
// proposal detection), a mirror of the on-chain slashed validator set and
// offense-submission markers.
//
// Full attestations are NOT stored here — only compact detection state plus
// locators (block root + attestation index) to re-fetch the full evidence from
// Dora or a beacon node when an offense is found. This keeps the index roughly
// an order of magnitude smaller than the Lighthouse/Prysm equivalents, which
// persist every indexed attestation.
package indexer

import (
	"github.com/ethpandaops/go-eth2-client/spec/phase0"
	"github.com/sirupsen/logrus"
)

// ViolationType is the kind of attester slashing a candidate represents.
type ViolationType int

const (
	// ViolationDoubleVote is two attestations by the same validator with the
	// same target epoch but different data.
	ViolationDoubleVote ViolationType = iota
	// ViolationSurroundVote is one attestation surrounding another (source_1 <
	// source_2 and target_2 < target_1).
	ViolationSurroundVote
)

// String renders the violation type.
func (v ViolationType) String() string {
	switch v {
	case ViolationDoubleVote:
		return "double_vote"
	case ViolationSurroundVote:
		return "surround_vote"
	default:
		return "unknown"
	}
}

// Locator points at a single attestation inside a block so the full evidence
// can be re-fetched from Dora/beacon.
type Locator struct {
	Slot      phase0.Slot
	BlockRoot phase0.Root
	AttIndex  uint16
}

// Vote is a block-included attestation expanded to its attesting validator
// indices, ready to be indexed.
type Vote struct {
	Source phase0.Epoch
	Target phase0.Epoch
	// DataRoot is hash_tree_root(AttestationData); only the first 8 bytes are
	// persisted, so an apparent conflict is always re-verified against the full
	// re-fetched attestations before submission.
	DataRoot phase0.Root
	Indices  []phase0.ValidatorIndex
	Locator  Locator
}

// Proposal is a block header seen for double-proposal detection.
type Proposal struct {
	Slot          phase0.Slot
	ProposerIndex phase0.ValidatorIndex
	BlockRoot     phase0.Root
}

// SlashingCandidate is a detected attester slashing. For a surround violation
// Att1 surrounds Att2. The full attester sets are recovered later by the
// evidence builder; Validator is a representative overlapping index.
type SlashingCandidate struct {
	Type      ViolationType
	Validator phase0.ValidatorIndex
	Att1      Locator
	Att2      Locator
}

// ProposerCandidate is a detected double proposal: two distinct block roots for
// the same (slot, proposer).
type ProposerCandidate struct {
	Slot          phase0.Slot
	ProposerIndex phase0.ValidatorIndex
	Root1         phase0.Root
	Root2         phase0.Root
}

// Detector runs the chunked min/max detection algorithm against an index Txn.
// It is stateless beyond its configuration and safe for concurrent use across
// distinct transactions.
type Detector struct {
	cfg Config
	log logrus.FieldLogger
}

// NewDetector builds a detector for the given index configuration.
func NewDetector(cfg Config, log logrus.FieldLogger) *Detector {
	return &Detector{cfg: cfg, log: log.WithField("package", "indexer")}
}

// windowFloor returns the lowest epoch still inside the detection window at the
// given frontier epoch.
func (d *Detector) windowFloor(currentEpoch phase0.Epoch) phase0.Epoch {
	h := d.cfg.HistoryLength
	if uint64(currentEpoch)+1 <= h {
		return 0
	}
	return phase0.Epoch(uint64(currentEpoch) + 1 - h)
}

// ProcessAttestations indexes a batch of votes and returns any detected attester
// slashing candidates, deduplicated by the conflicting attestation pair.
// currentEpoch is the chronological frontier (the epoch of the batch's slot);
// votes with a target below the detection window are ignored. The caller must
// process batches in ascending slot order and Commit the txn afterwards.
func (d *Detector) ProcessAttestations(t *Txn, votes []Vote, currentEpoch phase0.Epoch) ([]SlashingCandidate, error) {
	floor := d.windowFloor(currentEpoch)
	seen := make(map[candidateKey]struct{})
	var out []SlashingCandidate

	for i := range votes {
		v := &votes[i]
		if v.Source > v.Target || v.Target < floor {
			continue
		}
		cands, err := d.processVote(t, v, currentEpoch, floor)
		if err != nil {
			return nil, err
		}
		for _, c := range cands {
			k := keyOf(c)
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, c)
		}
	}
	return out, nil
}

// ProcessProposal records a block header and returns a candidate if it conflicts
// with a previously recorded header for the same (slot, proposer).
func (d *Detector) ProcessProposal(t *Txn, p Proposal) (*ProposerCandidate, error) {
	existing, ok, err := t.ProposerRoot(p.Slot, p.ProposerIndex)
	if err != nil {
		return nil, err
	}
	if !ok {
		if err := t.SetProposerRoot(p.Slot, p.ProposerIndex, p.BlockRoot); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if existing == p.BlockRoot {
		return nil, nil
	}
	// Keep the first-seen root; report the conflict.
	return &ProposerCandidate{
		Slot:          p.Slot,
		ProposerIndex: p.ProposerIndex,
		Root1:         existing,
		Root2:         p.BlockRoot,
	}, nil
}

// candidateKey deduplicates candidates by conflicting-pair identity.
type candidateKey struct {
	typ   ViolationType
	rootA phase0.Root
	idxA  uint16
	rootB phase0.Root
	idxB  uint16
}

func keyOf(c SlashingCandidate) candidateKey {
	a, b := c.Att1, c.Att2
	// Order-independent so (A,B) and (B,A) collapse to one key.
	if isLess(b, a) {
		a, b = b, a
	}
	return candidateKey{typ: c.Type, rootA: a.BlockRoot, idxA: a.AttIndex, rootB: b.BlockRoot, idxB: b.AttIndex}
}

func isLess(a, b Locator) bool {
	for i := range 32 {
		if a.BlockRoot[i] != b.BlockRoot[i] {
			return a.BlockRoot[i] < b.BlockRoot[i]
		}
	}
	return a.AttIndex < b.AttIndex
}
