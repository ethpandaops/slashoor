// Package detector turns detection candidates into submittable slashing
// evidence. On a candidate it re-fetches the full attestations/blocks from Dora
// or a beacon node (the local index keeps only locators), re-verifies the
// slashable condition against the spec, expands the attester sets, applies the
// slashed_any and fork-domain filters, and builds the fork-correct
// AttesterSlashing / ProposerSlashing container.
package detector

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/ethpandaops/go-eth2-client/spec/all"
	"github.com/ethpandaops/go-eth2-client/spec/phase0"
	"github.com/ethpandaops/go-eth2-client/spec/version"
	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/slashoor/pkg/beacon"
	"github.com/ethpandaops/slashoor/pkg/chainspec"
	"github.com/ethpandaops/slashoor/pkg/dora"
	"github.com/ethpandaops/slashoor/pkg/duties"
	"github.com/ethpandaops/slashoor/pkg/indexer"
)

// AttesterEvidence is a built attester slashing plus its classification.
type AttesterEvidence struct {
	Slashing *all.AttesterSlashing
	// Offenders is the intersection of both attesting sets that is not yet
	// slashed (the validators this slashing would actually punish).
	Offenders []phase0.ValidatorIndex
	// Submittable is false when the evidence cannot be included on chain
	// (fork-domain expiry or an all-slashed intersection); such evidence is
	// still reported for visibility.
	Submittable bool
	Reason      string
	OffenseID   [32]byte
}

// ProposerEvidence is a built proposer slashing plus its classification.
type ProposerEvidence struct {
	Slashing    *phase0.ProposerSlashing
	Submittable bool
	Reason      string
	OffenseID   [32]byte
}

// Service builds slashing evidence from detection candidates.
type Service struct {
	cfg    *Config
	spec   *chainspec.Spec
	beacon beacon.Service
	dora   dora.Client // may be nil
	duties *duties.Resolver
	store  *indexer.Store
	log    logrus.FieldLogger
}

// New builds an evidence service.
func New(cfg *Config, spec *chainspec.Spec, beaconSvc beacon.Service, doraClient dora.Client, dutiesResolver *duties.Resolver, store *indexer.Store, log logrus.FieldLogger) *Service {
	return &Service{
		cfg:    cfg,
		spec:   spec,
		beacon: beaconSvc,
		dora:   doraClient,
		duties: dutiesResolver,
		store:  store,
		log:    log.WithField("package", "detector"),
	}
}

// BuildAttester constructs attester slashing evidence for a candidate. It
// returns (nil, nil) when the evidence can no longer be built (e.g. the block
// was pruned from Dora) or when re-verification shows the pair is not actually
// slashable (e.g. a data-root prefix collision).
func (s *Service) BuildAttester(ctx context.Context, cand indexer.SlashingCandidate, headEpoch phase0.Epoch) (*AttesterEvidence, error) {
	att1, err := s.fetchAttestation(ctx, cand.Att1)
	if err != nil {
		return nil, err
	}
	att2, err := s.fetchAttestation(ctx, cand.Att2)
	if err != nil {
		return nil, err
	}
	if att1 == nil || att2 == nil {
		s.log.WithField("validator", cand.Validator).Warn("attester evidence unavailable (block pruned?)")
		return nil, nil
	}

	// Re-verify and order so attestation_1 surrounds attestation_2 (or, for a
	// double vote, keep either order).
	ordered1, ordered2, ok := orderSlashablePair(att1, att2)
	if !ok {
		s.log.WithField("validator", cand.Validator).Debug("candidate not slashable on re-verification, dropping")
		return nil, nil
	}

	indices1, err := s.duties.AttestingIndices(ctx, ordered1)
	if err != nil {
		return nil, fmt.Errorf("expand attestation 1: %w", err)
	}
	indices2, err := s.duties.AttestingIndices(ctx, ordered2)
	if err != nil {
		return nil, fmt.Errorf("expand attestation 2: %w", err)
	}
	intersection := intersect(indices1, indices2)
	if len(intersection) == 0 {
		s.log.WithField("validator", cand.Validator).Debug("empty attester intersection, dropping")
		return nil, nil
	}

	slashing := &all.AttesterSlashing{
		Version:      newerVersion(ordered1.Version, ordered2.Version),
		Attestation1: indexedAttestation(ordered1, indices1),
		Attestation2: indexedAttestation(ordered2, indices2),
	}

	ev := &AttesterEvidence{
		Slashing:  slashing,
		OffenseID: attesterOffenseID(ordered1.Data, ordered2.Data),
	}

	// Fork-domain: evidence for a target epoch below the previous fork can no
	// longer be BLS-verified on chain.
	minTarget := minEpoch(ordered1.Data.Target.Epoch, ordered2.Data.Target.Epoch)
	if minTarget < s.spec.SubmittableFromEpoch(headEpoch) {
		ev.Reason = "evidence predates the previous fork; not submittable"
		return ev, nil
	}

	// slashed_any: at least one intersection member must still be slashable.
	offenders := s.filterSlashable(ctx, intersection, headEpoch)
	ev.Offenders = offenders
	if len(offenders) == 0 {
		ev.Reason = "all overlapping validators already slashed"
		return ev, nil
	}

	ev.Submittable = true
	return ev, nil
}

// BuildProposer constructs proposer slashing evidence for a double proposal.
func (s *Service) BuildProposer(ctx context.Context, cand indexer.ProposerCandidate, headEpoch phase0.Epoch) (*ProposerEvidence, error) {
	block1, err := s.fetchBlock(ctx, cand.Root1, cand.Slot)
	if err != nil {
		return nil, err
	}
	block2, err := s.fetchBlock(ctx, cand.Root2, cand.Slot)
	if err != nil {
		return nil, err
	}
	if block1 == nil || block2 == nil {
		s.log.WithField("proposer", cand.ProposerIndex).Warn("proposer evidence unavailable (block pruned?)")
		return nil, nil
	}

	header1, err := s.blockHeader(block1)
	if err != nil {
		return nil, err
	}
	header2, err := s.blockHeader(block2)
	if err != nil {
		return nil, err
	}
	if *header1.Message == *header2.Message {
		s.log.WithField("proposer", cand.ProposerIndex).Debug("identical headers on re-verification, dropping")
		return nil, nil
	}

	ev := &ProposerEvidence{
		Slashing:  &phase0.ProposerSlashing{SignedHeader1: header1, SignedHeader2: header2},
		OffenseID: proposerOffenseID(cand.Root1, cand.Root2),
	}

	// A proposer slashing is signed under DOMAIN_BEACON_PROPOSER at the slot's
	// epoch; the same fork-domain expiry applies.
	slotEpoch := s.spec.EpochOfSlot(cand.Slot)
	if slotEpoch < s.spec.SubmittableFromEpoch(headEpoch) {
		ev.Reason = "evidence predates the previous fork; not submittable"
		return ev, nil
	}
	if slashable := s.filterSlashable(ctx, []phase0.ValidatorIndex{cand.ProposerIndex}, headEpoch); len(slashable) == 0 {
		ev.Reason = "proposer already slashed"
		return ev, nil
	}

	ev.Submittable = true
	return ev, nil
}

// fetchAttestation re-fetches a block and returns the attestation at the
// locator's index.
func (s *Service) fetchAttestation(ctx context.Context, loc indexer.Locator) (*all.Attestation, error) {
	block, err := s.fetchBlock(ctx, loc.BlockRoot, loc.Slot)
	if err != nil || block == nil {
		return nil, err
	}
	if block.Message == nil || block.Message.Body == nil {
		return nil, nil
	}
	atts := block.Message.Body.Attestations
	if int(loc.AttIndex) >= len(atts) {
		return nil, fmt.Errorf("attestation index %d out of range (%d) in block 0x%x", loc.AttIndex, len(atts), loc.BlockRoot)
	}
	return atts[loc.AttIndex], nil
}

// fetchBlock tries the beacon node first (fast for canonical blocks) and falls
// back to Dora, which serves orphaned blocks and history the beacon node pruned.
func (s *Service) fetchBlock(ctx context.Context, root phase0.Root, slot phase0.Slot) (*all.SignedBeaconBlock, error) {
	block, err := s.beacon.BlockByRoot(ctx, root)
	if err == nil && block != nil {
		return block, nil
	}
	if err != nil {
		s.log.WithError(err).WithField("root", fmt.Sprintf("0x%x", root)).Debug("beacon block fetch failed, trying dora")
	}
	if s.dora != nil {
		return s.dora.BlockByRoot(ctx, root, slot)
	}
	return block, err
}

// blockHeader derives a signed header from a block, using fork-aware hashing for
// the body root.
func (s *Service) blockHeader(block *all.SignedBeaconBlock) (*phase0.SignedBeaconBlockHeader, error) {
	if block.Message == nil {
		return nil, fmt.Errorf("block has no message")
	}
	bodyRoot, err := s.spec.DynSSZ().HashTreeRoot(block.Message.Body)
	if err != nil {
		return nil, fmt.Errorf("hash block body: %w", err)
	}
	return &phase0.SignedBeaconBlockHeader{
		Message: &phase0.BeaconBlockHeader{
			Slot:          block.Message.Slot,
			ProposerIndex: block.Message.ProposerIndex,
			ParentRoot:    block.Message.ParentRoot,
			StateRoot:     block.Message.StateRoot,
			BodyRoot:      phase0.Root(bodyRoot),
		},
		Signature: block.Signature,
	}, nil
}

// filterSlashable returns the subset of indices that is still slashable
// (is_slashable_validator), preferring authoritative live status and falling
// back to the local slashed mirror.
func (s *Service) filterSlashable(ctx context.Context, indices []phase0.ValidatorIndex, headEpoch phase0.Epoch) []phase0.ValidatorIndex {
	statuses := s.statuses(ctx, indices)
	var out []phase0.ValidatorIndex
	for _, idx := range indices {
		if st, ok := statuses[idx]; ok {
			if !st.Slashed && st.ActivationEpoch <= headEpoch && headEpoch < st.WithdrawableEpoch {
				out = append(out, idx)
			}
			continue
		}
		// Fall back to the local mirror.
		if slashed, err := s.store.IsSlashed(idx); err == nil && !slashed {
			out = append(out, idx)
		}
	}
	return out
}

// statuses fetches validator status, preferring Dora's bulk endpoint.
func (s *Service) statuses(ctx context.Context, indices []phase0.ValidatorIndex) map[phase0.ValidatorIndex]beacon.ValidatorStatus {
	if s.dora != nil {
		if st, err := s.dora.ValidatorsStatus(ctx, indices); err == nil && len(st) > 0 {
			out := make(map[phase0.ValidatorIndex]beacon.ValidatorStatus, len(st))
			for k, v := range st {
				out[k] = beacon.ValidatorStatus{
					Index:             v.Index,
					Slashed:           v.Slashed,
					Status:            v.Status,
					ActivationEpoch:   v.ActivationEpoch,
					ExitEpoch:         v.ExitEpoch,
					WithdrawableEpoch: v.WithdrawableEpoch,
				}
			}
			return out
		}
	}
	if st, err := s.beacon.ValidatorsStatus(ctx, indices); err == nil {
		return st
	}
	return nil
}

// --- helpers ---

// orderSlashablePair re-verifies the pair and orders it so that (for a surround)
// the first attestation surrounds the second. Returns ok=false if the pair is
// not slashable.
func orderSlashablePair(a, b *all.Attestation) (att1, att2 *all.Attestation, ok bool) {
	if a.Data == nil || b.Data == nil {
		return nil, nil, false
	}
	ra, err1 := a.Data.HashTreeRoot()
	rb, err2 := b.Data.HashTreeRoot()
	if err1 != nil || err2 != nil {
		return nil, nil, false
	}
	// Double vote: same target epoch, different data.
	if ra != rb && a.Data.Target.Epoch == b.Data.Target.Epoch {
		return a, b, true
	}
	// Surround: a surrounds b.
	if a.Data.Source.Epoch < b.Data.Source.Epoch && b.Data.Target.Epoch < a.Data.Target.Epoch {
		return a, b, true
	}
	// Surround: b surrounds a.
	if b.Data.Source.Epoch < a.Data.Source.Epoch && a.Data.Target.Epoch < b.Data.Target.Epoch {
		return b, a, true
	}
	return nil, nil, false
}

func indexedAttestation(att *all.Attestation, indices []phase0.ValidatorIndex) *all.IndexedAttestation {
	idx := make([]uint64, len(indices))
	for i, v := range indices {
		idx[i] = uint64(v)
	}
	return &all.IndexedAttestation{
		Version:          att.Version,
		AttestingIndices: idx,
		Data:             att.Data,
		Signature:        att.Signature,
	}
}

func intersect(a, b []phase0.ValidatorIndex) []phase0.ValidatorIndex {
	var out []phase0.ValidatorIndex
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, a[i])
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return out
}

func newerVersion(a, b version.DataVersion) version.DataVersion {
	if a > b {
		return a
	}
	return b
}

func minEpoch(a, b phase0.Epoch) phase0.Epoch {
	if a < b {
		return a
	}
	return b
}

func attesterOffenseID(d1, d2 *phase0.AttestationData) [32]byte {
	r1, _ := d1.HashTreeRoot()
	r2, _ := d2.HashTreeRoot()
	return orderedHash(0x01, r1, r2)
}

func proposerOffenseID(r1, r2 phase0.Root) [32]byte {
	return orderedHash(0x02, r1, r2)
}

func orderedHash(kind byte, a, b [32]byte) [32]byte {
	lo, hi := a, b
	if bytesGreater(lo, hi) {
		lo, hi = hi, lo
	}
	h := sha256.New()
	h.Write([]byte{kind})
	h.Write(lo[:])
	h.Write(hi[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func bytesGreater(a, b [32]byte) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}
