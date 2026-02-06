package proposer

import (
	"context"
	"fmt"
	"sync"

	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/sirupsen/logrus"

	"github.com/slashoor/slashoor/pkg/beacon"
)

// SlashingHandler is called when a proposer slashing proof is ready.
type SlashingHandler func(*beacon.ProposerSlashing)

// Service defines the interface for the proposer slashing detector.
type Service interface {
	Start(ctx context.Context) error
	Stop() error
	HandleBlock(ctx context.Context, event *beacon.BlockEvent)
	OnSlashing(handler SlashingHandler)
}

// blockRecord stores information about a seen block.
type blockRecord struct {
	slot          phase0.Slot
	blockRoot     phase0.Root
	proposerIndex phase0.ValidatorIndex
}

type service struct {
	cfg    *Config
	log    logrus.FieldLogger
	beacon beacon.Service

	mu               sync.RWMutex
	slashingHandlers []SlashingHandler
	slashingsCreated uint64

	// blocksBySlot tracks all blocks seen for each slot.
	// Key is slot, value is list of blocks seen for that slot.
	blocksBySlot map[phase0.Slot][]*blockRecord

	// processedSlashings tracks which proposer slashings we've already created
	// to avoid duplicates. Key is "proposerIndex:slot".
	processedSlashings map[string]struct{}
}

// New creates a new proposer slashing detector service.
func New(cfg *Config, beacon beacon.Service, log logrus.FieldLogger) Service {
	return &service{
		cfg:                cfg,
		log:                log.WithField("package", "proposer"),
		beacon:             beacon,
		slashingHandlers:   make([]SlashingHandler, 0, 4),
		blocksBySlot:       make(map[phase0.Slot][]*blockRecord, 128),
		processedSlashings: make(map[string]struct{}, 64),
	}
}

// Start initializes the proposer slashing detector service.
func (s *service) Start(ctx context.Context) error {
	if err := s.cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	s.log.WithField("enabled", s.cfg.Enabled).Info("proposer slashing detector started")

	return nil
}

// Stop shuts down the proposer slashing detector service.
func (s *service) Stop() error {
	s.log.WithField("slashings_created", s.slashingsCreated).Info("proposer slashing detector stopped")

	return nil
}

// OnSlashing registers a handler for proposer slashing proofs.
func (s *service) OnSlashing(handler SlashingHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.slashingHandlers = append(s.slashingHandlers, handler)
}

// HandleBlock processes a block event and checks for double proposals.
func (s *service) HandleBlock(ctx context.Context, event *beacon.BlockEvent) {
	if !s.cfg.Enabled {
		return
	}

	s.mu.Lock()

	// Check if we've seen other blocks for this slot from the same proposer
	existingBlocks := s.blocksBySlot[event.Slot]

	var conflictingBlock *blockRecord

	for _, block := range existingBlocks {
		// Same proposer, same slot, different block root = double proposal
		if block.proposerIndex == event.ProposerIndex && block.blockRoot != event.Block {
			conflictingBlock = block

			break
		}
	}

	// Store this block
	record := &blockRecord{
		slot:          event.Slot,
		blockRoot:     event.Block,
		proposerIndex: event.ProposerIndex,
	}
	s.blocksBySlot[event.Slot] = append(existingBlocks, record)

	s.mu.Unlock()

	if conflictingBlock != nil {
		s.handleDoubleProposal(ctx, conflictingBlock, record)
	}

	// Cleanup old slots periodically
	s.cleanupOldSlots(event.Slot)
}

func (s *service) handleDoubleProposal(
	ctx context.Context,
	block1, block2 *blockRecord,
) {
	// Check if we've already processed this slashing
	key := fmt.Sprintf("%d:%d", block1.proposerIndex, block1.slot)

	s.mu.Lock()
	if _, exists := s.processedSlashings[key]; exists {
		s.mu.Unlock()

		return
	}

	s.processedSlashings[key] = struct{}{}
	s.mu.Unlock()

	s.log.WithFields(logrus.Fields{
		"slot":           block1.slot,
		"proposer_index": block1.proposerIndex,
		"block1":         fmt.Sprintf("0x%x", block1.blockRoot[:8]),
		"block2":         fmt.Sprintf("0x%x", block2.blockRoot[:8]),
	}).Warn("double proposal detected!")

	// Fetch signed headers for both blocks
	header1, err := s.beacon.GetSignedBlockHeader(ctx, block1.blockRoot)
	if err != nil {
		s.log.WithError(err).WithField("block", fmt.Sprintf("0x%x", block1.blockRoot[:8])).
			Warn("failed to get signed header for block 1")

		return
	}

	if header1 == nil {
		s.log.WithField("block", fmt.Sprintf("0x%x", block1.blockRoot[:8])).
			Warn("block 1 header not found (orphaned)")

		return
	}

	header2, err := s.beacon.GetSignedBlockHeader(ctx, block2.blockRoot)
	if err != nil {
		s.log.WithError(err).WithField("block", fmt.Sprintf("0x%x", block2.blockRoot[:8])).
			Warn("failed to get signed header for block 2")

		return
	}

	if header2 == nil {
		s.log.WithField("block", fmt.Sprintf("0x%x", block2.blockRoot[:8])).
			Warn("block 2 header not found (orphaned)")

		return
	}

	// Verify both headers are from the same proposer
	if header1.Message.ProposerIndex != header2.Message.ProposerIndex {
		s.log.Warn("proposer mismatch in headers - not a valid slashing")

		return
	}

	slashing := &beacon.ProposerSlashing{
		SignedHeader1: header1,
		SignedHeader2: header2,
	}

	s.mu.Lock()
	s.slashingsCreated++
	s.mu.Unlock()

	s.log.WithFields(logrus.Fields{
		"slot":           block1.slot,
		"proposer_index": block1.proposerIndex,
	}).Info("created proposer slashing proof")

	s.notifyHandlers(slashing)
}

func (s *service) notifyHandlers(slashing *beacon.ProposerSlashing) {
	s.mu.RLock()
	handlers := s.slashingHandlers
	s.mu.RUnlock()

	for _, handler := range handlers {
		handler(slashing)
	}
}

func (s *service) cleanupOldSlots(currentSlot phase0.Slot) {
	// Keep blocks from the last 64 slots
	const keepSlots = 64

	if currentSlot < keepSlots {
		return
	}

	cutoff := currentSlot - keepSlots

	s.mu.Lock()
	defer s.mu.Unlock()

	for slot := range s.blocksBySlot {
		if slot < cutoff {
			delete(s.blocksBySlot, slot)
		}
	}
}
