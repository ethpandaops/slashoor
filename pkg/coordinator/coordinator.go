package coordinator

import (
	"context"
	"fmt"
	"sync"

	"github.com/sirupsen/logrus"

	"github.com/slashoor/slashoor/pkg/beacon"
	"github.com/slashoor/slashoor/pkg/detector"
	"github.com/slashoor/slashoor/pkg/indexer"
	"github.com/slashoor/slashoor/pkg/submitter"
)

// Service defines the interface for the coordinator.
type Service interface {
	Start(ctx context.Context) error
	Stop() error
}

type service struct {
	cfg *Config
	log logrus.FieldLogger

	beacon    beacon.Service
	indexer   indexer.Service
	detector  detector.Service
	submitter submitter.Service

	wg     sync.WaitGroup
	cancel context.CancelFunc
}

// New creates a new coordinator service.
func New(cfg *Config, log logrus.FieldLogger) (Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	coordLog := log.WithField("package", "coordinator")

	beaconSvc := beacon.New(cfg.Beacon, log)
	indexerSvc := indexer.New(cfg.Indexer, log)
	detectorSvc := detector.New(cfg.Detector, log)
	submitterSvc := submitter.New(cfg.Submitter, beaconSvc, log)

	return &service{
		cfg:       cfg,
		log:       coordLog,
		beacon:    beaconSvc,
		indexer:   indexerSvc,
		detector:  detectorSvc,
		submitter: submitterSvc,
	}, nil
}

// Start initializes and starts all services.
func (s *service) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	if err := s.beacon.Start(ctx); err != nil {
		return fmt.Errorf("failed to start beacon service: %w", err)
	}

	if err := s.indexer.Start(ctx); err != nil {
		return fmt.Errorf("failed to start indexer service: %w", err)
	}

	if err := s.detector.Start(ctx); err != nil {
		return fmt.Errorf("failed to start detector service: %w", err)
	}

	if err := s.submitter.Start(ctx); err != nil {
		return fmt.Errorf("failed to start submitter service: %w", err)
	}

	s.wireServices()

	if err := s.subscribeToAttestations(ctx); err != nil {
		return fmt.Errorf("failed to subscribe to attestations: %w", err)
	}

	s.log.Info("coordinator started")

	return nil
}

// Stop shuts down all services.
func (s *service) Stop() error {
	s.log.Info("stopping coordinator")

	if s.cancel != nil {
		s.cancel()
	}

	s.wg.Wait()

	var errs []error

	if err := s.submitter.Stop(); err != nil {
		errs = append(errs, fmt.Errorf("submitter: %w", err))
	}

	if err := s.detector.Stop(); err != nil {
		errs = append(errs, fmt.Errorf("detector: %w", err))
	}

	if err := s.indexer.Stop(); err != nil {
		errs = append(errs, fmt.Errorf("indexer: %w", err))
	}

	if err := s.beacon.Stop(); err != nil {
		errs = append(errs, fmt.Errorf("beacon: %w", err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("errors during shutdown: %v", errs)
	}

	s.log.Info("coordinator stopped")

	return nil
}

func (s *service) wireServices() {
	s.indexer.OnViolation(func(violation *indexer.SlashingViolation) {
		s.detector.HandleViolation(violation)
	})

	s.detector.OnSlashing(func(slashing *beacon.AttesterSlashing) {
		s.submitter.Submit(slashing)
	})
}

func (s *service) subscribeToAttestations(ctx context.Context) error {
	s.wg.Add(1)

	go func() {
		defer s.wg.Done()

		if err := s.beacon.SubscribeToAttestations(ctx, func(att *beacon.IndexedAttestation) {
			s.indexer.ProcessAttestation(att)
		}); err != nil {
			s.log.WithError(err).Error("attestation subscription failed")
		}
	}()

	return nil
}
