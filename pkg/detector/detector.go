package detector

import (
	"context"
	"fmt"
	"sync"

	"github.com/sirupsen/logrus"

	"github.com/slashoor/slashoor/pkg/beacon"
	"github.com/slashoor/slashoor/pkg/indexer"
)

// SlashingHandler is called when a slashing proof is ready.
type SlashingHandler func(*beacon.AttesterSlashing)

// Service defines the interface for the detector.
type Service interface {
	Start(ctx context.Context) error
	Stop() error
	HandleViolation(violation *indexer.SlashingViolation)
	OnSlashing(handler SlashingHandler)
}

type service struct {
	cfg *Config
	log logrus.FieldLogger

	mu               sync.RWMutex
	slashingHandlers []SlashingHandler
	slashingsCreated uint64
}

// New creates a new detector service.
func New(cfg *Config, log logrus.FieldLogger) Service {
	return &service{
		cfg:              cfg,
		log:              log.WithField("package", "detector"),
		slashingHandlers: make([]SlashingHandler, 0, 4),
	}
}

// Start initializes the detector service.
func (s *service) Start(ctx context.Context) error {
	if err := s.cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	s.log.WithField("enabled", s.cfg.Enabled).Info("detector service started")

	return nil
}

// Stop shuts down the detector service.
func (s *service) Stop() error {
	s.log.WithField("slashings_created", s.slashingsCreated).Info("detector service stopped")

	return nil
}

// OnSlashing registers a handler for slashing proofs.
func (s *service) OnSlashing(handler SlashingHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.slashingHandlers = append(s.slashingHandlers, handler)
}

// HandleViolation processes a slashing violation and creates a proof.
func (s *service) HandleViolation(violation *indexer.SlashingViolation) {
	if !s.cfg.Enabled {
		return
	}

	slashing := s.createSlashingProof(violation)
	if slashing == nil {
		return
	}

	s.mu.Lock()
	s.slashingsCreated++
	s.mu.Unlock()

	s.log.WithFields(logrus.Fields{
		"type":          violation.Type.String(),
		"validator_idx": violation.ValidatorIdx,
	}).Info("created slashing proof")

	s.notifyHandlers(slashing)
}

func (s *service) createSlashingProof(
	violation *indexer.SlashingViolation,
) *beacon.AttesterSlashing {
	att1 := violation.Attestation1
	att2 := violation.Attestation2

	if att1 == nil || att2 == nil {
		s.log.Warn("cannot create slashing proof: missing attestation data")

		return nil
	}

	slashing := &beacon.AttesterSlashing{
		Attestation1: att1,
		Attestation2: att2,
	}

	if !s.validateSlashing(slashing) {
		return nil
	}

	return slashing
}

func (s *service) validateSlashing(slashing *beacon.AttesterSlashing) bool {
	att1 := slashing.Attestation1
	att2 := slashing.Attestation2

	hasOverlap := false

	for _, idx1 := range att1.AttestingIndices {
		for _, idx2 := range att2.AttestingIndices {
			if idx1 == idx2 {
				hasOverlap = true

				break
			}
		}

		if hasOverlap {
			break
		}
	}

	if !hasOverlap {
		s.log.Debug("no overlapping validators in slashing proof")

		return false
	}

	if isDoubleVote(att1, att2) || isSurroundVote(att1, att2) || isSurroundVote(att2, att1) {
		return true
	}

	s.log.Debug("slashing proof does not contain valid violation")

	return false
}

func isDoubleVote(att1, att2 *beacon.IndexedAttestation) bool {
	return att1.Data.Target.Epoch == att2.Data.Target.Epoch &&
		att1.Data.Target.Root != att2.Data.Target.Root
}

func isSurroundVote(att1, att2 *beacon.IndexedAttestation) bool {
	return att1.Data.Source.Epoch < att2.Data.Source.Epoch &&
		att2.Data.Target.Epoch < att1.Data.Target.Epoch
}

func (s *service) notifyHandlers(slashing *beacon.AttesterSlashing) {
	s.mu.RLock()
	handlers := s.slashingHandlers
	s.mu.RUnlock()

	for _, handler := range handlers {
		handler(slashing)
	}
}
