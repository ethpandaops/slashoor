package coordinator

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/ethpandaops/go-eth2-client/spec/all"
	"github.com/ethpandaops/go-eth2-client/spec/phase0"

	"github.com/ethpandaops/slashoor/pkg/beacon"
	"github.com/ethpandaops/slashoor/pkg/dora"
	"github.com/ethpandaops/slashoor/pkg/indexer"
)

// backfill sweeps blocks chronologically from the cursor to the chain head,
// then hands off to live following. It advances (and persists) the cursor per
// window so a restart resumes rather than rescans.
func (c *Coordinator) backfill(cursor *indexer.Cursor) error {
	from := phase0.Slot(cursor.StartSlot)
	if cursor.ProcessedSlot >= 0 {
		from = phase0.Slot(cursor.ProcessedSlot + 1)
	}
	head := c.headSlot(c.ctx)
	c.log.WithField("from_slot", from).WithField("head_slot", head).Info("backfill starting")

	window := phase0.Slot(c.cfg.BackfillWindowSlots)
	for start := from; ; start += window {
		if c.ctx.Err() != nil {
			return c.ctx.Err()
		}
		if start > head {
			// The chain advanced while we scanned; re-check before finishing.
			head = c.headSlot(c.ctx)
			if start > head {
				break
			}
		}
		end := min(start+window-1, head)
		if err := c.processSlotRange(start, end); err != nil {
			return err
		}
		cursor.ProcessedSlot = int64(end)
		if err := c.store.SetCursor(cursor); err != nil {
			return err
		}
		c.log.WithField("processed_slot", end).Debug("backfill progress")
	}

	cursor.BackfillComplete = true
	if err := c.store.SetCursor(cursor); err != nil {
		return err
	}
	c.log.Info("backfill complete")
	return nil
}

// processSlotRange processes every block (canonical and orphaned) in a slot
// range, in ascending slot order.
func (c *Coordinator) processSlotRange(from, to phase0.Slot) error {
	if c.dora != nil {
		slots, err := c.dora.SlotsRange(c.ctx, from, to, dora.OrphanedInclude)
		if err != nil {
			return err
		}
		// Keep only slots that actually contain a block (skip missed slots, which
		// the listing returns with a zero root).
		blocked := slots[:0]
		for _, si := range slots {
			if si.BlockRoot != (phase0.Root{}) {
				blocked = append(blocked, si)
			}
		}
		sort.Slice(blocked, func(i, j int) bool { return blocked[i].Slot < blocked[j].Slot })

		// Warm the per-epoch duties cache for the window concurrently so the
		// serial processing below doesn't stall on each new epoch's duty load.
		var epochs []phase0.Epoch
		for e := c.spec.EpochOfSlot(from); e <= c.spec.EpochOfSlot(to); e++ {
			epochs = append(epochs, e)
		}
		c.duties.Warm(c.ctx, epochs)

		// Blocks are fetched concurrently (network-bound) but indexed in slot order
		// (detection is order-sensitive) in a single transaction for the whole
		// window (one flush).
		blocks := c.prefetchBlocks(blocked)
		items := make([]pendingBlock, 0, len(blocked))
		for i, si := range blocked {
			if blocks[i] != nil {
				items = append(items, pendingBlock{block: blocks[i], root: si.BlockRoot})
			}
		}
		if c.ctx.Err() != nil {
			return c.ctx.Err()
		}
		c.indexBlocks(items, false)
		return nil
	}

	// No Dora: canonical-only via the beacon node (orphaned blocks unavailable).
	items := make([]pendingBlock, 0, to-from+1)
	for slot := from; slot <= to; slot++ {
		if c.ctx.Err() != nil {
			return c.ctx.Err()
		}
		block, err := c.beacon.BlockBySlot(c.ctx, slot)
		if err != nil || block == nil {
			continue
		}
		root, err := c.blockRoot(block)
		if err != nil {
			continue
		}
		items = append(items, pendingBlock{block: block, root: root})
	}
	c.indexBlocks(items, false)
	return nil
}

// prefetchBlocks downloads the blocks for the given slots concurrently,
// returning them in the same order (nil for any that failed to fetch). Each
// fetch has a bounded timeout so a single slow/flaky block cannot stall the
// whole window's commit.
func (c *Coordinator) prefetchBlocks(infos []dora.SlotInfo) []*all.SignedBeaconBlock {
	blocks := make([]*all.SignedBeaconBlock, len(infos))
	const workers = 64
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i := range infos {
		if c.ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			fctx, cancel := context.WithTimeout(c.ctx, 20*time.Second)
			defer cancel()
			block, err := c.fetchBlock(fctx, infos[i].BlockRoot, infos[i].Slot)
			if err != nil {
				c.log.WithError(err).WithField("slot", infos[i].Slot).Warn("fetch block during backfill")
				return
			}
			blocks[i] = block
		}(i)
	}
	wg.Wait()
	return blocks
}

// follow catches up any slots produced since backfill, then follows the head via
// SSE and periodically reconciles orphaned blocks from Dora.
func (c *Coordinator) follow() {
	if cursor, err := c.store.Cursor(); err == nil && cursor.ProcessedSlot >= 0 {
		if err := c.processSlotRange(phase0.Slot(cursor.ProcessedSlot+1), c.headSlot(c.ctx)); err != nil && c.ctx.Err() == nil {
			c.log.WithError(err).Warn("live catch-up")
		}
	}

	blockCh := make(chan beacon.BlockEvent, 2048)
	c.beacon.SubscribeToBlocks(c.ctx, func(ev beacon.BlockEvent) {
		select {
		case blockCh <- ev:
		default:
			c.log.WithField("slot", ev.Slot).Warn("live block queue full; relying on reconciliation")
		}
	})

	if c.dora != nil {
		c.wg.Add(1)
		go c.reconcileLoop()
	}

	c.log.Info("following chain head")
	for {
		select {
		case <-c.ctx.Done():
			return
		case ev := <-blockCh:
			block, err := c.fetchBlock(c.ctx, ev.Block, ev.Slot)
			if err != nil {
				c.log.WithError(err).WithField("slot", ev.Slot).Warn("fetch live block")
				continue
			}
			if block != nil {
				c.indexBlocks([]pendingBlock{{block: block, root: ev.Block}}, true)
			}
		}
	}
}

// reconcileLoop periodically re-scans the recent slot range from Dora so
// orphaned blocks (which may not arrive via SSE) are still indexed. Reprocessing
// is idempotent.
func (c *Coordinator) reconcileLoop() {
	defer c.wg.Done()
	interval := time.Duration(c.spec.SecondsPerSlot()*c.spec.SlotsPerEpoch()) * time.Second
	if interval <= 0 {
		interval = 6 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	lookback := phase0.Slot(4 * c.spec.SlotsPerEpoch())
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			head := c.headSlot(c.ctx)
			from := phase0.Slot(0)
			if head > lookback {
				from = head - lookback
			}
			slots, err := c.dora.SlotsRange(c.ctx, from, head, dora.OrphanedOnly)
			if err != nil {
				c.log.WithError(err).Debug("reconcile orphaned blocks")
				continue
			}
			var items []pendingBlock
			for _, si := range slots {
				if si.BlockRoot == (phase0.Root{}) {
					continue
				}
				block, err := c.fetchBlock(c.ctx, si.BlockRoot, si.Slot)
				if err != nil || block == nil {
					continue
				}
				items = append(items, pendingBlock{block: block, root: si.BlockRoot})
			}
			c.indexBlocks(items, true)
		}
	}
}

// pruneLoop drops index data older than the detection window as the frontier
// advances.
func (c *Coordinator) pruneLoop() {
	defer c.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	h := phase0.Epoch(c.cfg.Indexer.HistoryLength)
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			frontier := phase0.Epoch(c.frontier.Load())
			if frontier <= h {
				continue
			}
			keepFrom := frontier - h
			if keepFrom <= c.lastPruned {
				continue
			}
			if err := c.store.Prune(keepFrom, c.spec.SlotsPerEpoch()); err != nil {
				c.log.WithError(err).Warn("prune index")
				continue
			}
			c.lastPruned = keepFrom
			c.log.WithField("keep_from_epoch", keepFrom).Debug("pruned index")
		}
	}
}

// fetchBlock retrieves a block by root. It tries the beacon node first (fast for
// canonical blocks, including on archival nodes) and falls back to Dora, which
// is the completeness backstop for orphaned blocks and for history the beacon
// node has pruned.
func (c *Coordinator) fetchBlock(ctx context.Context, root phase0.Root, slot phase0.Slot) (*all.SignedBeaconBlock, error) {
	block, err := c.beacon.BlockByRoot(ctx, root)
	if err == nil && block != nil {
		return block, nil
	}
	if err != nil {
		c.log.WithError(err).Debug("beacon block fetch failed, trying dora")
	}
	if c.dora != nil {
		return c.dora.BlockByRoot(ctx, root, slot)
	}
	return block, err
}

// blockRoot computes a block's root using fork-aware hashing.
func (c *Coordinator) blockRoot(block *all.SignedBeaconBlock) (phase0.Root, error) {
	root, err := c.spec.DynSSZ().HashTreeRoot(block.Message)
	if err != nil {
		return phase0.Root{}, err
	}
	return phase0.Root(root), nil
}
