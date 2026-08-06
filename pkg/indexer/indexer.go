package indexer

import (
	"context"
	"fmt"
	"sync"

	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/sirupsen/logrus"

	"github.com/slashoor/slashoor/pkg/beacon"
)

// SlashingCandidate represents a potential slashing between two attestations.
type SlashingCandidate struct {
	Type         ViolationType
	Attestation1 *beacon.Attestation
	Attestation2 *beacon.Attestation
}

// ViolationType represents the type of slashing violation.
type ViolationType int

const (
	ViolationDoubleVote ViolationType = iota
	ViolationSurroundVote
)

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

// CandidateHandler is called when a slashing candidate is detected.
type CandidateHandler func(*SlashingCandidate)

// Service defines the interface for the indexer.
type Service interface {
	Start(ctx context.Context) error
	Stop() error
	ProcessAttestation(att *beacon.Attestation)
	OnCandidate(handler CandidateHandler)
}

// attestationKey buckets attestations for double vote detection. A validator
// has one committee assignment per epoch, so both halves of an on-chain double
// vote share the same slot; the committee index is deliberately NOT part of
// the key: post-Electra data.index no longer identifies the committee (it is
// 0, and from Gloas on a payload-availability bit), so attestations that
// differ only in index must still land in the same bucket to be compared.
type attestationKey struct {
	slot        phase0.Slot
	targetEpoch phase0.Epoch
}

// storedAttestation holds attestation data for slashing detection.
type storedAttestation struct {
	attestation *beacon.Attestation
	sourceEpoch phase0.Epoch
	targetEpoch phase0.Epoch
	targetRoot  phase0.Root
	signature   phase0.BLSSignature
}

type service struct {
	cfg *Config
	log logrus.FieldLogger

	mu           sync.RWMutex
	attestations map[attestationKey][]*storedAttestation
	epochBounds  *EpochBounds
	maxTarget    phase0.Epoch

	candidateHandlers []CandidateHandler
	processedCount    uint64
}

// New creates a new indexer service.
func New(cfg *Config, log logrus.FieldLogger) Service {
	return &service{
		cfg:               cfg,
		log:               log.WithField("package", "indexer"),
		attestations:      make(map[attestationKey][]*storedAttestation, 4096),
		epochBounds:       NewEpochBounds(),
		candidateHandlers: make([]CandidateHandler, 0, 4),
	}
}

// Start initializes the indexer service.
func (s *service) Start(ctx context.Context) error {
	if err := s.cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	s.log.Info("indexer service started")

	return nil
}

// Stop shuts down the indexer service.
func (s *service) Stop() error {
	s.log.WithField("processed_attestations", s.processedCount).Info("indexer service stopped")

	return nil
}

// OnCandidate registers a handler for slashing candidates.
func (s *service) OnCandidate(handler CandidateHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.candidateHandlers = append(s.candidateHandlers, handler)
}

// ProcessAttestation processes an attestation and checks for slashing violations.
func (s *service) ProcessAttestation(att *beacon.Attestation) {
	s.mu.Lock()
	s.processedCount++
	s.mu.Unlock()

	sourceEpoch := att.Data.Source.Epoch
	targetEpoch := att.Data.Target.Epoch

	s.checkDoubleVote(att)

	if s.epochBounds.CheckSurroundVote(sourceEpoch, targetEpoch) {
		s.checkSurroundViolations(att)
	}

	if s.epochBounds.CheckSurroundedVote(sourceEpoch, targetEpoch) {
		s.checkSurroundedViolations(att)
	}

	s.epochBounds.Update(sourceEpoch, targetEpoch)
	s.storeAttestation(att)
}

func (s *service) storeAttestation(att *beacon.Attestation) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := attestationKey{
		slot:        att.Data.Slot,
		targetEpoch: att.Data.Target.Epoch,
	}

	// Head events and backfill overlap, so the same aggregate can be
	// processed more than once; storing it again would make every future
	// conflict report duplicate candidates.
	for _, stored := range s.attestations[key] {
		if stored.signature == att.Signature && attestationDataEqual(stored.attestation.Data, att.Data) {
			return
		}
	}

	s.attestations[key] = append(s.attestations[key], &storedAttestation{
		attestation: att,
		sourceEpoch: att.Data.Source.Epoch,
		targetEpoch: att.Data.Target.Epoch,
		targetRoot:  att.Data.Target.Root,
		signature:   att.Signature,
	})

	if att.Data.Target.Epoch > s.maxTarget {
		s.maxTarget = att.Data.Target.Epoch
		s.pruneLocked()
	}
}

// pruneLocked drops attestations outside the retention window. Callers must
// hold s.mu.
func (s *service) pruneLocked() {
	if s.cfg.MaxEpochsToKeep == 0 || s.maxTarget < phase0.Epoch(s.cfg.MaxEpochsToKeep) {
		return
	}

	cutoff := s.maxTarget - phase0.Epoch(s.cfg.MaxEpochsToKeep)

	for key := range s.attestations {
		if key.targetEpoch < cutoff {
			delete(s.attestations, key)
		}
	}
}

func (s *service) checkDoubleVote(att *beacon.Attestation) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	key := attestationKey{
		slot:        att.Data.Slot,
		targetEpoch: att.Data.Target.Epoch,
	}

	// Two attestations with the same target epoch and ANY difference in their
	// data are a double vote (is_slashable_attestation_data) — not just a
	// differing target root: rival head roots or a differing payload bit
	// (Gloas data.index) conflict too. Same data in different aggregates is
	// never slashable, however the signatures differ.
	for _, stored := range s.attestations[key] {
		if !attestationDataEqual(stored.attestation.Data, att.Data) {
			s.reportCandidate(&SlashingCandidate{
				Type:         ViolationDoubleVote,
				Attestation1: stored.attestation,
				Attestation2: att,
			})
		}
	}
}

func attestationDataEqual(a, b *beacon.AttestationData) bool {
	return a.Slot == b.Slot &&
		a.Index == b.Index &&
		a.BeaconBlockRoot == b.BeaconBlockRoot &&
		a.Source.Epoch == b.Source.Epoch &&
		a.Source.Root == b.Source.Root &&
		a.Target.Epoch == b.Target.Epoch &&
		a.Target.Root == b.Target.Root
}

func (s *service) checkSurroundViolations(att *beacon.Attestation) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, storedList := range s.attestations {
		for _, stored := range storedList {
			if att.Data.Source.Epoch < stored.sourceEpoch && stored.targetEpoch < att.Data.Target.Epoch {
				s.reportCandidate(&SlashingCandidate{
					Type:         ViolationSurroundVote,
					Attestation1: att,
					Attestation2: stored.attestation,
				})
			}
		}
	}
}

func (s *service) checkSurroundedViolations(att *beacon.Attestation) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, storedList := range s.attestations {
		for _, stored := range storedList {
			if stored.sourceEpoch < att.Data.Source.Epoch && att.Data.Target.Epoch < stored.targetEpoch {
				s.reportCandidate(&SlashingCandidate{
					Type:         ViolationSurroundVote,
					Attestation1: stored.attestation,
					Attestation2: att,
				})
			}
		}
	}
}

func (s *service) reportCandidate(candidate *SlashingCandidate) {
	// Debug, not Warn: rival aggregates around a reorg produce candidates
	// constantly; only the detector's validator-overlap check decides whether
	// one is an actual equivocation worth shouting about.
	s.log.WithFields(logrus.Fields{
		"type":    candidate.Type.String(),
		"slot1":   candidate.Attestation1.Data.Slot,
		"slot2":   candidate.Attestation2.Data.Slot,
		"source1": candidate.Attestation1.Data.Source.Epoch,
		"target1": candidate.Attestation1.Data.Target.Epoch,
		"source2": candidate.Attestation2.Data.Source.Epoch,
		"target2": candidate.Attestation2.Data.Target.Epoch,
	}).Debug("slashing candidate detected")

	handlers := s.candidateHandlers

	for _, handler := range handlers {
		handler(candidate)
	}
}
