package detector

import (
	"context"
	"fmt"
	"sync"

	"github.com/attestantio/go-eth2-client/spec/phase0"
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
	HandleCandidate(ctx context.Context, candidate *indexer.SlashingCandidate)
	OnSlashing(handler SlashingHandler)
}

type service struct {
	cfg    *Config
	log    logrus.FieldLogger
	beacon beacon.Service

	mu               sync.RWMutex
	slashingHandlers []SlashingHandler
	slashingsCreated uint64
}

// New creates a new detector service.
func New(cfg *Config, beacon beacon.Service, log logrus.FieldLogger) Service {
	return &service{
		cfg:              cfg,
		log:              log.WithField("package", "detector"),
		beacon:           beacon,
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

// HandleCandidate processes a slashing candidate and creates a proof if valid.
func (s *service) HandleCandidate(ctx context.Context, candidate *indexer.SlashingCandidate) {
	if !s.cfg.Enabled {
		return
	}

	validators1, err := s.beacon.ResolveAttestingValidators(ctx, candidate.Attestation1)
	if err != nil {
		s.log.WithError(err).Debug("failed to resolve validators for attestation 1")

		return
	}

	validators2, err := s.beacon.ResolveAttestingValidators(ctx, candidate.Attestation2)
	if err != nil {
		s.log.WithError(err).Debug("failed to resolve validators for attestation 2")

		return
	}

	overlap := findOverlappingValidators(validators1, validators2)
	if len(overlap) == 0 {
		s.log.Debug("no overlapping validators in slashing candidate")

		return
	}

	slashing := &beacon.AttesterSlashing{
		Attestation1: &beacon.IndexedAttestation{
			AttestingIndices: overlap,
			Data:             candidate.Attestation1.Data,
			Signature:        candidate.Attestation1.Signature,
		},
		Attestation2: &beacon.IndexedAttestation{
			AttestingIndices: overlap,
			Data:             candidate.Attestation2.Data,
			Signature:        candidate.Attestation2.Signature,
		},
	}

	s.mu.Lock()
	s.slashingsCreated++
	s.mu.Unlock()

	s.log.WithFields(logrus.Fields{
		"type":       candidate.Type.String(),
		"validators": len(overlap),
	}).Info("created slashing proof")

	s.notifyHandlers(slashing)
}

func findOverlappingValidators(v1, v2 []phase0.ValidatorIndex) []phase0.ValidatorIndex {
	set := make(map[phase0.ValidatorIndex]struct{}, len(v1))
	for _, v := range v1 {
		set[v] = struct{}{}
	}

	overlap := make([]phase0.ValidatorIndex, 0)

	for _, v := range v2 {
		if _, exists := set[v]; exists {
			overlap = append(overlap, v)
		}
	}

	return overlap
}

func (s *service) notifyHandlers(slashing *beacon.AttesterSlashing) {
	s.mu.RLock()
	handlers := s.slashingHandlers
	s.mu.RUnlock()

	for _, handler := range handlers {
		handler(slashing)
	}
}
