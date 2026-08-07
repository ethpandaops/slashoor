// Package coordinator wires the slasher together and drives its lifecycle: it
// fetches the chain spec, opens the index, runs the historic backfill sweep
// (via Dora) and the live head follower (via SSE), and routes detection
// candidates through evidence building to submission.
package coordinator

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethpandaops/go-eth2-client/spec/all"
	"github.com/ethpandaops/go-eth2-client/spec/phase0"
	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/slashoor/pkg/beacon"
	"github.com/ethpandaops/slashoor/pkg/chainspec"
	"github.com/ethpandaops/slashoor/pkg/detector"
	"github.com/ethpandaops/slashoor/pkg/dora"
	"github.com/ethpandaops/slashoor/pkg/duties"
	"github.com/ethpandaops/slashoor/pkg/indexer"
	"github.com/ethpandaops/slashoor/pkg/ingest"
	"github.com/ethpandaops/slashoor/pkg/submitter"
)

// Coordinator owns and drives all services.
type Coordinator struct {
	cfg *Config
	log logrus.FieldLogger

	beacon beacon.Service
	dora   dora.Client // nil when disabled

	spec      *chainspec.Spec
	store     *indexer.Store
	duties    *duties.Resolver
	ingest    *ingest.Extractor
	detect    *indexer.Detector
	evidence  *detector.Service
	submitter submitter.Service

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// frontier is the highest epoch processed so far, driving the detection
	// window and pruning.
	frontier   atomic.Uint64
	lastPruned phase0.Epoch
	procMu     sync.Mutex // serializes block processing
}

// New builds a coordinator from configuration.
func New(cfg *Config, log logrus.FieldLogger) (*Coordinator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	c := &Coordinator{
		cfg:    cfg,
		log:    log.WithField("package", "coordinator"),
		beacon: beacon.New(cfg.Beacon, log),
	}
	if cfg.Dora.Enabled {
		c.dora = dora.New(cfg.Dora, log)
	}
	return c, nil
}

// Start brings up every service and launches backfill and live following.
func (c *Coordinator) Start(ctx context.Context) error {
	c.ctx, c.cancel = context.WithCancel(ctx)

	if err := c.beacon.Start(c.ctx); err != nil {
		return fmt.Errorf("start beacon: %w", err)
	}

	spec, err := c.loadSpec(c.ctx)
	if err != nil {
		return fmt.Errorf("load chain spec: %w", err)
	}
	c.spec = spec
	if c.dora != nil {
		c.dora.SetSpec(spec)
	}
	c.log.WithFields(logrus.Fields{
		"slots_per_epoch": spec.SlotsPerEpoch(),
		"forks":           len(spec.Forks()),
	}).Info("chain spec loaded")

	if err := c.openStore(c.ctx); err != nil {
		return err
	}

	c.duties = duties.New(c.beacon, c.dora, spec, c.log)
	c.ingest = ingest.New(spec, c.duties, c.log)
	c.detect = indexer.NewDetector(c.store.Config(), c.log)
	c.evidence = detector.New(&detector.Config{Enabled: c.cfg.Detection.Attester}, spec, c.beacon, c.dora, c.duties, c.store, c.log)
	c.submitter = submitter.New(c.cfg.Submitter, c.beacon, c.store, c.log)
	if err := c.submitter.Start(c.ctx); err != nil {
		return fmt.Errorf("start submitter: %w", err)
	}

	c.wg.Add(1)
	go c.run()
	return nil
}

// Stop tears everything down.
func (c *Coordinator) Stop() error {
	if c.cancel != nil {
		c.cancel()
	}
	c.wg.Wait()
	_ = c.submitter.Stop()
	_ = c.beacon.Stop()
	if c.store != nil {
		if err := c.store.Close(); err != nil {
			return err
		}
	}
	stats := c.submitter.Stats()
	c.log.WithFields(logrus.Fields{
		"submitted":   stats.Submitted,
		"dry_run":     stats.DryRun,
		"report_only": stats.ReportOnly,
		"duplicates":  stats.Duplicates,
		"failed":      stats.Failed,
	}).Info("slashoor stopped")
	return nil
}

// loadSpec fetches and parses the chain spec, retrying until the context ends.
func (c *Coordinator) loadSpec(ctx context.Context) (*chainspec.Spec, error) {
	for {
		raw, err := c.beacon.Spec(ctx)
		if err == nil {
			return chainspec.New(raw)
		}
		c.log.WithError(err).Warn("waiting for a beacon node to fetch the spec")
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// run performs backfill (if configured) then live following.
func (c *Coordinator) run() {
	defer c.wg.Done()

	c.wg.Add(1)
	go c.pruneLoop()

	cursor, err := c.store.Cursor()
	if err != nil {
		c.log.WithError(err).Error("read cursor")
		return
	}

	if !cursor.BackfillComplete {
		if err := c.backfill(cursor); err != nil {
			if c.ctx.Err() != nil {
				return
			}
			c.log.WithError(err).Error("backfill failed")
		}
	}

	c.follow()
}

// openStore opens (or, on reset, recreates) the index and reconciles the stored
// cursor with the configured start mode.
func (c *Coordinator) openStore(ctx context.Context) error {
	if c.cfg.ResetEnabled {
		c.log.WithField("dir", c.cfg.DataDir).Warn("resetting index (data will be wiped)")
		if err := os.RemoveAll(c.cfg.DataDir); err != nil {
			return fmt.Errorf("reset index: %w", err)
		}
	}

	store, err := indexer.Open(c.cfg.DataDir, c.cfg.Indexer, c.log)
	if err != nil {
		return fmt.Errorf("open index (a config change may require --reset-to-slot): %w", err)
	}
	c.store = store

	desiredStart, mode := c.desiredStart(ctx)

	cursor, err := store.Cursor()
	if err != nil {
		return err
	}
	if cursor == nil {
		cursor = c.newCursor(desiredStart, mode)
		if err := store.SetCursor(cursor); err != nil {
			return err
		}
	} else if !c.cfg.ResetEnabled && uint64(desiredStart) < cursor.StartSlot {
		return fmt.Errorf(
			"configured start slot %d is earlier than the stored start %d; detection state would be incomplete. Pass --reset-to-slot %d to rebuild",
			desiredStart, cursor.StartSlot, desiredStart)
	}
	c.frontier.Store(uint64(c.spec.EpochOfSlot(phase0.Slot(cursor.StartSlot))))
	if cursor.ProcessedSlot >= 0 {
		c.frontier.Store(uint64(c.spec.EpochOfSlot(phase0.Slot(cursor.ProcessedSlot))))
	}
	return nil
}

// desiredStart resolves the configured start slot, honoring a reset override.
func (c *Coordinator) desiredStart(ctx context.Context) (phase0.Slot, string) {
	if c.cfg.ResetEnabled {
		if c.cfg.ResetToSlot <= 0 {
			return 0, string(StartGenesis)
		}
		return phase0.Slot(c.cfg.ResetToSlot), string(StartSlot)
	}
	switch c.cfg.Start.Mode {
	case StartGenesis:
		return 0, string(StartGenesis)
	case StartSlot:
		return phase0.Slot(c.cfg.Start.Slot), string(StartSlot)
	default: // head
		return c.headSlot(ctx), string(StartHead)
	}
}

// newCursor builds a fresh cursor for a start slot/mode.
func (c *Coordinator) newCursor(startSlot phase0.Slot, mode string) *indexer.Cursor {
	cursor := &indexer.Cursor{Mode: mode, StartSlot: uint64(startSlot), ProcessedSlot: -1}
	if mode == string(StartHead) {
		// No historic scan: begin live at head.
		cursor.ProcessedSlot = int64(startSlot)
		cursor.BackfillComplete = true
	} else if startSlot > 0 {
		cursor.ProcessedSlot = int64(startSlot) - 1
	}
	c.log.WithFields(logrus.Fields{"mode": mode, "start_slot": startSlot}).Info("initialized index cursor")
	return cursor
}

func (c *Coordinator) headSlot(ctx context.Context) phase0.Slot {
	fin, err := c.beacon.Finality(ctx)
	if err != nil {
		c.log.WithError(err).Warn("could not fetch head slot")
		return 0
	}
	return fin.HeadSlot
}

// pendingBlock is a block awaiting indexing, with its (canonical or orphaned)
// root.
type pendingBlock struct {
	block *all.SignedBeaconBlock
	root  phase0.Root
}

// attCandidate / propCandidate carry a detected candidate plus the head epoch at
// detection time (for submittability classification).
type attCandidate struct {
	cand indexer.SlashingCandidate
	head phase0.Epoch
}

type propCandidate struct {
	cand indexer.ProposerCandidate
	head phase0.Epoch
}

// indexBlocks runs detection for a batch of blocks in a single index
// transaction, so every dirtied min/max chunk is flushed once for the whole
// batch rather than once per block. This is what makes backfill write-efficient
// (consecutive blocks in an epoch-chunk would otherwise rewrite the same 8 KB
// chunk repeatedly). Evidence building and submission run afterward, outside the
// processing lock, since they perform network I/O. sync controls whether the
// commit is fsync'd (true for live, false for bulk backfill).
func (c *Coordinator) indexBlocks(items []pendingBlock, sync bool) {
	if len(items) == 0 {
		return
	}

	c.procMu.Lock()
	txn := c.store.NewTxn()
	var attHits []attCandidate
	var propHits []propCandidate

	for _, it := range items {
		res, err := c.ingest.Block(c.ctx, it.block, it.root)
		if err != nil {
			c.log.WithError(err).Warn("ingest block")
			continue
		}

		// Keep the local slashed mirror current from on-chain slashings.
		if len(res.Slashed) > 0 {
			if err := c.store.MarkSlashed(res.Slashed...); err != nil {
				c.log.WithError(err).Warn("mark slashed")
			}
		}

		epoch := c.spec.EpochOfSlot(res.Proposal.Slot)
		if uint64(epoch) > c.frontier.Load() {
			c.frontier.Store(uint64(epoch))
		}
		// Detection uses the global frontier (chain head), not this block's epoch,
		// so the window floor and max-target fill stay consistent when out-of-order
		// (reconciled or orphaned) blocks are processed.
		frontier := phase0.Epoch(c.frontier.Load())

		if c.cfg.Detection.Attester {
			cands, err := c.detect.ProcessAttestations(txn, res.Votes, frontier)
			if err != nil {
				c.log.WithError(err).Error("process attestations")
				continue
			}
			for _, cand := range cands {
				attHits = append(attHits, attCandidate{cand, frontier})
			}
		}
		if c.cfg.Detection.Proposer {
			cand, err := c.detect.ProcessProposal(txn, res.Proposal)
			if err != nil {
				c.log.WithError(err).Error("process proposal")
				continue
			}
			if cand != nil {
				propHits = append(propHits, propCandidate{*cand, frontier})
			}
		}
	}

	if err := txn.Commit(sync); err != nil {
		c.log.WithError(err).Error("commit index batch")
		txn.Close()
		c.procMu.Unlock()
		return
	}
	txn.Close()
	c.procMu.Unlock()

	// Evidence building and submission (network I/O) run without the lock.
	for _, h := range attHits {
		c.handleAttesterCandidate(h.cand, h.head)
	}
	for _, h := range propHits {
		c.handleProposerCandidate(h.cand, h.head)
	}
}

func (c *Coordinator) handleAttesterCandidate(cand indexer.SlashingCandidate, headEpoch phase0.Epoch) {
	ev, err := c.evidence.BuildAttester(c.ctx, cand, headEpoch)
	if err != nil {
		c.log.WithError(err).Warn("build attester evidence")
		return
	}
	if ev == nil {
		return
	}
	c.log.WithFields(logrus.Fields{
		"type":        cand.Type.String(),
		"validator":   cand.Validator,
		"submittable": ev.Submittable,
		"offenders":   len(ev.Offenders),
		"op1_slot":    cand.Att1.Slot,
		"op1_block":   fmt.Sprintf("0x%x", cand.Att1.BlockRoot),
		"op1_attidx":  cand.Att1.AttIndex,
		"op2_slot":    cand.Att2.Slot,
		"op2_block":   fmt.Sprintf("0x%x", cand.Att2.BlockRoot),
		"op2_attidx":  cand.Att2.AttIndex,
	}).Warn("attester slashing detected")
	if err := c.submitter.SubmitAttester(c.ctx, ev); err != nil {
		c.log.WithError(err).Error("submit attester slashing")
	}
}

func (c *Coordinator) handleProposerCandidate(cand indexer.ProposerCandidate, headEpoch phase0.Epoch) {
	ev, err := c.evidence.BuildProposer(c.ctx, cand, headEpoch)
	if err != nil {
		c.log.WithError(err).Warn("build proposer evidence")
		return
	}
	if ev == nil {
		return
	}
	c.log.WithFields(logrus.Fields{
		"proposer":    cand.ProposerIndex,
		"slot":        cand.Slot,
		"submittable": ev.Submittable,
		"block1":      fmt.Sprintf("0x%x", cand.Root1),
		"block2":      fmt.Sprintf("0x%x", cand.Root2),
	}).Warn("proposer slashing detected")
	if err := c.submitter.SubmitProposer(c.ctx, ev); err != nil {
		c.log.WithError(err).Error("submit proposer slashing")
	}
}
