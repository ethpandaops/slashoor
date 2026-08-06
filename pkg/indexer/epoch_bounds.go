package indexer

import (
	"sync"

	"github.com/attestantio/go-eth2-client/spec/phase0"
)

const (
	maxEpochValue = phase0.Epoch(^uint64(0))
	minEpochValue = phase0.Epoch(0)
)

// EpochBounds tracks the m(i) and M(i) functions for lazy slasher detection.
// m(i): minimum target epoch for attestations with source > i
// M(i): maximum target epoch for attestations with source < i
//
// Per-source extrema are stored (one entry per distinct source epoch) and
// m/M answers are memoized; an update only invalidates the memos when it
// improves a per-source extremum, which after warm-up is rare. This keeps
// Update O(1) instead of O(current epoch) per attestation.
type EpochBounds struct {
	mu                sync.Mutex
	minTargetBySource map[phase0.Epoch]phase0.Epoch
	maxTargetBySource map[phase0.Epoch]phase0.Epoch
	minMemo           map[phase0.Epoch]phase0.Epoch
	maxMemo           map[phase0.Epoch]phase0.Epoch
}

// NewEpochBounds creates a new epoch bounds tracker.
func NewEpochBounds() *EpochBounds {
	return &EpochBounds{
		minTargetBySource: make(map[phase0.Epoch]phase0.Epoch, 1024),
		maxTargetBySource: make(map[phase0.Epoch]phase0.Epoch, 1024),
		minMemo:           make(map[phase0.Epoch]phase0.Epoch, 64),
		maxMemo:           make(map[phase0.Epoch]phase0.Epoch, 64),
	}
}

// Update updates the epoch bounds with a new attestation.
func (e *EpochBounds) Update(sourceEpoch, targetEpoch phase0.Epoch) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if current, exists := e.minTargetBySource[sourceEpoch]; !exists || targetEpoch < current {
		e.minTargetBySource[sourceEpoch] = targetEpoch
		clear(e.minMemo)
	}

	if current, exists := e.maxTargetBySource[sourceEpoch]; !exists || targetEpoch > current {
		e.maxTargetBySource[sourceEpoch] = targetEpoch
		clear(e.maxMemo)
	}
}

// GetMinTarget returns m(i): minimum target epoch for attestations with source > i.
func (e *EpochBounds) GetMinTarget(sourceEpoch phase0.Epoch) phase0.Epoch {
	e.mu.Lock()
	defer e.mu.Unlock()

	if target, exists := e.minMemo[sourceEpoch]; exists {
		return target
	}

	result := maxEpochValue

	for source, target := range e.minTargetBySource {
		if source > sourceEpoch && target < result {
			result = target
		}
	}

	e.minMemo[sourceEpoch] = result

	return result
}

// GetMaxTarget returns M(i): maximum target epoch for attestations with source < i.
func (e *EpochBounds) GetMaxTarget(sourceEpoch phase0.Epoch) phase0.Epoch {
	e.mu.Lock()
	defer e.mu.Unlock()

	if target, exists := e.maxMemo[sourceEpoch]; exists {
		return target
	}

	result := minEpochValue

	for source, target := range e.maxTargetBySource {
		if source < sourceEpoch && target > result {
			result = target
		}
	}

	e.maxMemo[sourceEpoch] = result

	return result
}

// CheckSurroundVote checks if an attestation (s, t) could have a surround violation.
// Returns true if t > m(s), meaning there might be a surrounding vote.
func (e *EpochBounds) CheckSurroundVote(sourceEpoch, targetEpoch phase0.Epoch) bool {
	return targetEpoch > e.GetMinTarget(sourceEpoch)
}

// CheckSurroundedVote checks if an attestation (s, t) could be surrounded.
// Returns true if t < M(s), meaning there might be an attestation that surrounds this one.
func (e *EpochBounds) CheckSurroundedVote(sourceEpoch, targetEpoch phase0.Epoch) bool {
	return targetEpoch < e.GetMaxTarget(sourceEpoch)
}

// Prune removes entries for source epochs older than the given epoch.
func (e *EpochBounds) Prune(beforeEpoch phase0.Epoch) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for epoch := range e.minTargetBySource {
		if epoch < beforeEpoch {
			delete(e.minTargetBySource, epoch)
		}
	}

	for epoch := range e.maxTargetBySource {
		if epoch < beforeEpoch {
			delete(e.maxTargetBySource, epoch)
		}
	}

	clear(e.minMemo)
	clear(e.maxMemo)
}
