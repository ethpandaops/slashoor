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

// attestationKey uniquely identifies attestation data for double vote detection.
type attestationKey struct {
	slot        phase0.Slot
	index       phase0.CommitteeIndex
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
		index:       att.Data.Index,
		targetEpoch: att.Data.Target.Epoch,
	}

	stored := &storedAttestation{
		attestation: att,
		sourceEpoch: att.Data.Source.Epoch,
		targetEpoch: att.Data.Target.Epoch,
		targetRoot:  att.Data.Target.Root,
		signature:   att.Signature,
	}

	s.attestations[key] = append(s.attestations[key], stored)
}

func (s *service) checkDoubleVote(att *beacon.Attestation) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	key := attestationKey{
		slot:        att.Data.Slot,
		index:       att.Data.Index,
		targetEpoch: att.Data.Target.Epoch,
	}

	existing := s.attestations[key]

	for _, stored := range existing {
		if stored.targetRoot != att.Data.Target.Root && stored.signature != att.Signature {
			s.reportCandidate(&SlashingCandidate{
				Type:         ViolationDoubleVote,
				Attestation1: stored.attestation,
				Attestation2: att,
			})
		}
	}
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
	s.log.WithFields(logrus.Fields{
		"type":    candidate.Type.String(),
		"source1": candidate.Attestation1.Data.Source.Epoch,
		"target1": candidate.Attestation1.Data.Target.Epoch,
		"source2": candidate.Attestation2.Data.Source.Epoch,
		"target2": candidate.Attestation2.Data.Target.Epoch,
	}).Warn("slashing candidate detected")

	handlers := s.candidateHandlers

	for _, handler := range handlers {
		handler(candidate)
	}
}
