package submitter

import (
	"context"
	"fmt"
	"sync"

	"github.com/sirupsen/logrus"

	"github.com/slashoor/slashoor/pkg/beacon"
)

// Service defines the interface for the submitter.
type Service interface {
	Start(ctx context.Context) error
	Stop() error
	Submit(slashing *beacon.AttesterSlashing)
}

type service struct {
	cfg    *Config
	log    logrus.FieldLogger
	beacon beacon.Service

	mu             sync.Mutex
	ctx            context.Context
	submittedCount uint64
}

// New creates a new submitter service.
func New(cfg *Config, beacon beacon.Service, log logrus.FieldLogger) Service {
	return &service{
		cfg:    cfg,
		log:    log.WithField("package", "submitter"),
		beacon: beacon,
	}
}

// Start initializes the submitter service.
func (s *service) Start(ctx context.Context) error {
	if err := s.cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	s.ctx = ctx

	s.log.WithFields(logrus.Fields{
		"enabled": s.cfg.Enabled,
		"dry_run": s.cfg.DryRun,
	}).Info("submitter service started")

	return nil
}

// Stop shuts down the submitter service.
func (s *service) Stop() error {
	s.log.WithField("submitted_count", s.submittedCount).Info("submitter service stopped")

	return nil
}

// Submit submits an attester slashing to the beacon node.
func (s *service) Submit(slashing *beacon.AttesterSlashing) {
	if !s.cfg.Enabled {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	logFields := logrus.Fields{
		"att1_source": slashing.Attestation1.Data.Source.Epoch,
		"att1_target": slashing.Attestation1.Data.Target.Epoch,
		"att2_source": slashing.Attestation2.Data.Source.Epoch,
		"att2_target": slashing.Attestation2.Data.Target.Epoch,
	}

	if s.cfg.DryRun {
		s.log.WithFields(logFields).Info("dry run: would submit attester slashing")

		return
	}

	s.log.WithFields(logFields).Info("submitting attester slashing")

	if err := s.beacon.SubmitAttesterSlashing(s.ctx, slashing); err != nil {
		s.log.WithError(err).WithFields(logFields).Error("failed to submit attester slashing")

		return
	}

	s.submittedCount++

	s.log.WithFields(logFields).Info("successfully submitted attester slashing")
}
