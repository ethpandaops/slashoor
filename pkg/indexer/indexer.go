package indexer

import (
	"context"
	"fmt"
	"sync"

	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/sirupsen/logrus"

	"github.com/slashoor/slashoor/pkg/beacon"
)

// SlashingViolation represents a detected slashing violation.
type SlashingViolation struct {
	Type         ViolationType
	ValidatorIdx phase0.ValidatorIndex
	Attestation1 *beacon.IndexedAttestation
	Attestation2 *beacon.IndexedAttestation
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

// ViolationHandler is called when a slashing violation is detected.
type ViolationHandler func(*SlashingViolation)

// Service defines the interface for the indexer.
type Service interface {
	Start(ctx context.Context) error
	Stop() error
	ProcessAttestation(att *beacon.IndexedAttestation)
	OnViolation(handler ViolationHandler)
}

type service struct {
	cfg            *Config
	log            logrus.FieldLogger
	epochBounds    *EpochBounds
	validatorStore *ValidatorStore

	mu                sync.RWMutex
	violationHandlers []ViolationHandler
	processedCount    uint64
}

// New creates a new indexer service.
func New(cfg *Config, log logrus.FieldLogger) Service {
	return &service{
		cfg:               cfg,
		log:               log.WithField("package", "indexer"),
		epochBounds:       NewEpochBounds(),
		validatorStore:    NewValidatorStore(),
		violationHandlers: make([]ViolationHandler, 0, 4),
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

// OnViolation registers a handler for slashing violations.
func (s *service) OnViolation(handler ViolationHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.violationHandlers = append(s.violationHandlers, handler)
}

// ProcessAttestation processes an attestation and checks for slashing violations.
func (s *service) ProcessAttestation(att *beacon.IndexedAttestation) {
	s.mu.Lock()
	s.processedCount++
	s.mu.Unlock()

	sourceEpoch := att.Data.Source.Epoch
	targetEpoch := att.Data.Target.Epoch

	if s.epochBounds.CheckSurroundVote(sourceEpoch, targetEpoch) {
		s.checkSurroundViolations(att)
	}

	if s.epochBounds.CheckSurroundedVote(sourceEpoch, targetEpoch) {
		s.checkSurroundedViolations(att)
	}

	s.checkDoubleVotes(att)

	s.epochBounds.Update(sourceEpoch, targetEpoch)
	s.validatorStore.Add(att)
}

func (s *service) checkDoubleVotes(att *beacon.IndexedAttestation) {
	for _, validatorIdx := range att.AttestingIndices {
		existing := s.validatorStore.FindDoubleVote(
			validatorIdx,
			att.Data.Target.Epoch,
			att.Data.Target.Root,
		)
		if existing != nil {
			s.reportViolation(&SlashingViolation{
				Type:         ViolationDoubleVote,
				ValidatorIdx: validatorIdx,
				Attestation1: existing.Attestation,
				Attestation2: att,
			})
		}
	}
}

func (s *service) checkSurroundViolations(att *beacon.IndexedAttestation) {
	for _, validatorIdx := range att.AttestingIndices {
		existing := s.validatorStore.FindSurroundedVote(
			validatorIdx,
			att.Data.Source.Epoch,
			att.Data.Target.Epoch,
		)
		if existing != nil {
			s.reportViolation(&SlashingViolation{
				Type:         ViolationSurroundVote,
				ValidatorIdx: validatorIdx,
				Attestation1: existing.Attestation,
				Attestation2: att,
			})
		}
	}
}

func (s *service) checkSurroundedViolations(att *beacon.IndexedAttestation) {
	for _, validatorIdx := range att.AttestingIndices {
		existing := s.validatorStore.FindSurroundingVote(
			validatorIdx,
			att.Data.Source.Epoch,
			att.Data.Target.Epoch,
		)
		if existing != nil {
			s.reportViolation(&SlashingViolation{
				Type:         ViolationSurroundVote,
				ValidatorIdx: validatorIdx,
				Attestation1: att,
				Attestation2: existing.Attestation,
			})
		}
	}
}

func (s *service) reportViolation(violation *SlashingViolation) {
	s.log.WithFields(logrus.Fields{
		"type":          violation.Type.String(),
		"validator_idx": violation.ValidatorIdx,
		"source1":       violation.Attestation1.Data.Source.Epoch,
		"target1":       violation.Attestation1.Data.Target.Epoch,
		"source2":       violation.Attestation2.Data.Source.Epoch,
		"target2":       violation.Attestation2.Data.Target.Epoch,
	}).Warn("slashing violation detected")

	s.mu.RLock()
	handlers := s.violationHandlers
	s.mu.RUnlock()

	for _, handler := range handlers {
		handler(violation)
	}
}
