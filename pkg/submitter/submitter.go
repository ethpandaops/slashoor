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
	SubmitAttesterSlashing(slashing *beacon.AttesterSlashing)
	SubmitProposerSlashing(slashing *beacon.ProposerSlashing)
}

type service struct {
	cfg    *Config
	log    logrus.FieldLogger
	beacon beacon.Service

	mu                     sync.Mutex
	ctx                    context.Context
	attesterSlashingsCount uint64
	proposerSlashingsCount uint64
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
	s.log.WithFields(logrus.Fields{
		"attester_slashings": s.attesterSlashingsCount,
		"proposer_slashings": s.proposerSlashingsCount,
	}).Info("submitter service stopped")

	return nil
}

// SubmitAttesterSlashing submits an attester slashing to the beacon node.
func (s *service) SubmitAttesterSlashing(slashing *beacon.AttesterSlashing) {
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

	s.attesterSlashingsCount++

	s.log.WithFields(logFields).Info("successfully submitted attester slashing")
}

// SubmitProposerSlashing submits a proposer slashing to the beacon node.
func (s *service) SubmitProposerSlashing(slashing *beacon.ProposerSlashing) {
	if !s.cfg.Enabled {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	logFields := logrus.Fields{
		"slot":           slashing.SignedHeader1.Message.Slot,
		"proposer_index": slashing.SignedHeader1.Message.ProposerIndex,
	}

	if s.cfg.DryRun {
		s.log.WithFields(logFields).Info("dry run: would submit proposer slashing")

		return
	}

	s.log.WithFields(logFields).Info("submitting proposer slashing")

	if err := s.beacon.SubmitProposerSlashing(s.ctx, slashing); err != nil {
		s.log.WithError(err).WithFields(logFields).Error("failed to submit proposer slashing")

		return
	}

	s.proposerSlashingsCount++

	s.log.WithFields(logFields).Info("successfully submitted proposer slashing")
}
