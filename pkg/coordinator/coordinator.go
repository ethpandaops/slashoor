package coordinator

import (
	"context"
	"fmt"
	"sync"

	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/sirupsen/logrus"

	"github.com/slashoor/slashoor/pkg/beacon"
	"github.com/slashoor/slashoor/pkg/detector"
	"github.com/slashoor/slashoor/pkg/indexer"
	"github.com/slashoor/slashoor/pkg/proposer"
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
	proposer  proposer.Service
	submitter submitter.Service

	wg     sync.WaitGroup
	ctx    context.Context
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
	detectorSvc := detector.New(cfg.Detector, beaconSvc, log)
	proposerSvc := proposer.New(cfg.Proposer, beaconSvc, log)
	submitterSvc := submitter.New(cfg.Submitter, beaconSvc, log)

	return &service{
		cfg:       cfg,
		log:       coordLog,
		beacon:    beaconSvc,
		indexer:   indexerSvc,
		detector:  detectorSvc,
		proposer:  proposerSvc,
		submitter: submitterSvc,
	}, nil
}

// Start initializes and starts all services.
func (s *service) Start(ctx context.Context) error {
	s.ctx, s.cancel = context.WithCancel(ctx)

	if err := s.beacon.Start(s.ctx); err != nil {
		return fmt.Errorf("failed to start beacon service: %w", err)
	}

	if err := s.indexer.Start(s.ctx); err != nil {
		return fmt.Errorf("failed to start indexer service: %w", err)
	}

	if err := s.detector.Start(s.ctx); err != nil {
		return fmt.Errorf("failed to start detector service: %w", err)
	}

	if err := s.proposer.Start(s.ctx); err != nil {
		return fmt.Errorf("failed to start proposer service: %w", err)
	}

	if err := s.submitter.Start(s.ctx); err != nil {
		return fmt.Errorf("failed to start submitter service: %w", err)
	}

	s.wireServices()

	if err := s.backfillAttestations(); err != nil {
		s.log.WithError(err).Warn("failed to backfill attestations")
	}

	if err := s.subscribeToHeads(); err != nil {
		return fmt.Errorf("failed to subscribe to heads: %w", err)
	}

	if err := s.subscribeToBlocks(); err != nil {
		return fmt.Errorf("failed to subscribe to blocks: %w", err)
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

	if err := s.proposer.Stop(); err != nil {
		errs = append(errs, fmt.Errorf("proposer: %w", err))
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
	s.indexer.OnCandidate(func(candidate *indexer.SlashingCandidate) {
		s.detector.HandleCandidate(s.ctx, candidate)
	})

	s.detector.OnSlashing(func(slashing *beacon.AttesterSlashing) {
		s.submitter.SubmitAttesterSlashing(slashing)
	})

	s.proposer.OnSlashing(func(slashing *beacon.ProposerSlashing) {
		s.submitter.SubmitProposerSlashing(slashing)
	})
}

func (s *service) subscribeToHeads() error {
	s.wg.Add(1)

	go func() {
		defer s.wg.Done()

		if err := s.beacon.SubscribeToHeads(s.ctx, s.handleHead); err != nil {
			if s.ctx.Err() == nil {
				s.log.WithError(err).Error("head subscription failed")
			}
		}
	}()

	return nil
}

func (s *service) subscribeToBlocks() error {
	s.wg.Add(1)

	go func() {
		defer s.wg.Done()

		if err := s.beacon.SubscribeToBlocks(s.ctx, s.handleBlock); err != nil {
			if s.ctx.Err() == nil {
				s.log.WithError(err).Error("block subscription failed")
			}
		}
	}()

	return nil
}

func (s *service) handleBlock(event *beacon.BlockEvent) {
	// SSE block events don't include proposer_index, fetch it from header
	if event.ProposerIndex == 0 {
		header, err := s.beacon.GetSignedBlockHeader(s.ctx, event.Block)
		if err != nil {
			s.log.WithError(err).WithField("block", fmt.Sprintf("0x%x", event.Block[:8])).
				Debug("failed to get block header")

			return
		}

		if header != nil {
			event.ProposerIndex = header.Message.ProposerIndex
		}
	}

	s.log.WithFields(logrus.Fields{
		"slot":     event.Slot,
		"proposer": event.ProposerIndex,
		"block":    fmt.Sprintf("0x%x", event.Block[:8]),
	}).Debug("new block received")

	s.proposer.HandleBlock(s.ctx, event)
}

func (s *service) handleHead(event *beacon.HeadEvent) {
	s.log.WithField("slot", event.Slot).Debug("new head received")

	attestations, err := s.beacon.GetBlockAttestations(s.ctx, event.Slot)
	if err != nil {
		s.log.WithError(err).WithField("slot", event.Slot).Warn("failed to get block attestations")

		return
	}

	s.log.WithFields(logrus.Fields{
		"slot":         event.Slot,
		"attestations": len(attestations),
	}).Debug("processing block attestations")

	for _, att := range attestations {
		s.indexer.ProcessAttestation(att)
	}
}

func (s *service) backfillAttestations() error {
	finality, err := s.beacon.GetFinality(s.ctx)
	if err != nil {
		return fmt.Errorf("failed to get finality: %w", err)
	}

	var startSlot, endSlot uint64

	if s.cfg.StartSlotEnabled {
		startSlot = s.cfg.StartSlot
		endSlot = uint64(finality.HeadSlot)
	} else if s.cfg.BackfillSlots > 0 {
		endSlot = uint64(finality.HeadSlot)

		if endSlot > s.cfg.BackfillSlots {
			startSlot = endSlot - s.cfg.BackfillSlots
		}
	} else {
		return nil
	}

	if startSlot >= endSlot {
		return nil
	}

	totalSlots := endSlot - startSlot
	s.log.WithFields(logrus.Fields{
		"start_slot":  startSlot,
		"end_slot":    endSlot,
		"total_slots": totalSlots,
	}).Info("backfilling attestations")

	var (
		totalAttestations uint64
		processedSlots    uint64
	)

	for slot := startSlot; slot <= endSlot; slot++ {
		if s.ctx.Err() != nil {
			return s.ctx.Err()
		}

		attestations, err := s.beacon.GetBlockAttestations(s.ctx, phase0.Slot(slot))
		if err != nil {
			s.log.WithError(err).WithField("slot", slot).Debug("failed to get block attestations")

			continue
		}

		for _, att := range attestations {
			s.indexer.ProcessAttestation(att)
			totalAttestations++
		}

		processedSlots++

		if processedSlots%100 == 0 {
			s.log.WithFields(logrus.Fields{
				"slot":               slot,
				"progress":           fmt.Sprintf("%.1f%%", float64(processedSlots)/float64(totalSlots)*100),
				"total_attestations": totalAttestations,
			}).Info("backfill progress")
		}
	}

	s.log.WithFields(logrus.Fields{
		"slots_processed":    processedSlots,
		"total_attestations": totalAttestations,
	}).Info("backfill complete")

	return nil
}
