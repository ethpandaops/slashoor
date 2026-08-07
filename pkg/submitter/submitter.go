// Package submitter submits (or, in dry-run, logs) slashing evidence to the
// beacon nodes. It deduplicates by offense id against the persistent index so a
// restart never re-submits, and classifies report-only evidence (fork-expired
// or already-slashed) without touching the network.
package submitter

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/slashoor/pkg/beacon"
	"github.com/ethpandaops/slashoor/pkg/detector"
	"github.com/ethpandaops/slashoor/pkg/indexer"
)

// Stats are submission counters.
type Stats struct {
	Submitted  uint64
	DryRun     uint64
	ReportOnly uint64
	Duplicates uint64
	Failed     uint64
}

// Service submits slashing evidence.
type Service interface {
	Start(ctx context.Context) error
	Stop() error
	SubmitAttester(ctx context.Context, ev *detector.AttesterEvidence) error
	SubmitProposer(ctx context.Context, ev *detector.ProposerEvidence) error
	Stats() Stats
}

type service struct {
	cfg    *Config
	beacon beacon.Service
	store  *indexer.Store
	log    logrus.FieldLogger

	mu    sync.Mutex // serializes submissions
	stats struct {
		submitted, dryRun, reportOnly, duplicates, failed atomic.Uint64
	}
}

// New builds a submitter.
func New(cfg *Config, beaconSvc beacon.Service, store *indexer.Store, log logrus.FieldLogger) Service {
	return &service{
		cfg:    cfg,
		beacon: beaconSvc,
		store:  store,
		log:    log.WithField("package", "submitter"),
	}
}

func (s *service) Start(context.Context) error { return nil }
func (s *service) Stop() error                 { return nil }

// SubmitAttester submits an attester slashing, or logs a report-only offense.
func (s *service) SubmitAttester(ctx context.Context, ev *detector.AttesterEvidence) error {
	if ev == nil || ev.Slashing == nil {
		return nil
	}
	log := s.log.WithFields(logrus.Fields{
		"type":      "attester",
		"offenders": len(ev.Offenders),
		"version":   ev.Slashing.Version.String(),
	})
	return s.submit(ev.OffenseID, ev.Submittable, ev.Reason, log, func() error {
		return s.beacon.SubmitAttesterSlashing(ctx, ev.Slashing)
	})
}

// SubmitProposer submits a proposer slashing, or logs a report-only offense.
func (s *service) SubmitProposer(ctx context.Context, ev *detector.ProposerEvidence) error {
	if ev == nil || ev.Slashing == nil {
		return nil
	}
	proposer := uint64(0)
	if ev.Slashing.SignedHeader1 != nil && ev.Slashing.SignedHeader1.Message != nil {
		proposer = uint64(ev.Slashing.SignedHeader1.Message.ProposerIndex)
	}
	log := s.log.WithFields(logrus.Fields{"type": "proposer", "proposer": proposer})
	return s.submit(ev.OffenseID, ev.Submittable, ev.Reason, log, func() error {
		return s.beacon.SubmitProposerSlashing(ctx, ev.Slashing)
	})
}

// submit implements the shared dedup / dry-run / report-only / broadcast flow.
func (s *service) submit(offenseID [32]byte, submittable bool, reason string, log logrus.FieldLogger, broadcast func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	dup, err := s.store.HasSubmitted(offenseID[:])
	if err != nil {
		return err
	}
	if dup {
		s.stats.duplicates.Add(1)
		return nil
	}

	if !submittable {
		log.WithField("reason", reason).Info("slashing detected but not submittable (report-only)")
		s.stats.reportOnly.Add(1)
		return s.store.MarkSubmitted(offenseID[:])
	}

	if !s.cfg.Enabled || s.cfg.DryRun {
		log.WithField("dry_run", true).Warn("slashing detected (submission disabled/dry-run)")
		s.stats.dryRun.Add(1)
		return s.store.MarkSubmitted(offenseID[:])
	}

	if err := broadcast(); err != nil {
		s.stats.failed.Add(1)
		log.WithError(err).Error("slashing submission failed")
		return err // not marked: retried on the next detection
	}

	log.Warn("slashing submitted")
	s.stats.submitted.Add(1)
	return s.store.MarkSubmitted(offenseID[:])
}

// Stats returns a snapshot of submission counters.
func (s *service) Stats() Stats {
	return Stats{
		Submitted:  s.stats.submitted.Load(),
		DryRun:     s.stats.dryRun.Load(),
		ReportOnly: s.stats.reportOnly.Load(),
		Duplicates: s.stats.duplicates.Load(),
		Failed:     s.stats.failed.Load(),
	}
}
