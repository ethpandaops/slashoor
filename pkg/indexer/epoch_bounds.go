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
type EpochBounds struct {
	mu sync.RWMutex
	m  map[phase0.Epoch]phase0.Epoch
	mM map[phase0.Epoch]phase0.Epoch
}

// NewEpochBounds creates a new epoch bounds tracker.
func NewEpochBounds() *EpochBounds {
	return &EpochBounds{
		m:  make(map[phase0.Epoch]phase0.Epoch, 1024),
		mM: make(map[phase0.Epoch]phase0.Epoch, 1024),
	}
}

// Update updates the epoch bounds with a new attestation.
func (e *EpochBounds) Update(sourceEpoch, targetEpoch phase0.Epoch) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for i := phase0.Epoch(0); i < sourceEpoch; i++ {
		if current, exists := e.m[i]; !exists || targetEpoch < current {
			e.m[i] = targetEpoch
		}
	}

	for i := sourceEpoch + 1; i <= targetEpoch; i++ {
		if current, exists := e.mM[i]; !exists || targetEpoch > current {
			e.mM[i] = targetEpoch
		}
	}
}

// GetMinTarget returns m(i): minimum target epoch for attestations with source > i.
func (e *EpochBounds) GetMinTarget(sourceEpoch phase0.Epoch) phase0.Epoch {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if target, exists := e.m[sourceEpoch]; exists {
		return target
	}

	return maxEpochValue
}

// GetMaxTarget returns M(i): maximum target epoch for attestations with source < i.
func (e *EpochBounds) GetMaxTarget(sourceEpoch phase0.Epoch) phase0.Epoch {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if target, exists := e.mM[sourceEpoch]; exists {
		return target
	}

	return minEpochValue
}

// CheckSurroundVote checks if an attestation (s, t) could have a surround violation.
// Returns true if t > m(s), meaning there might be a surrounding vote.
func (e *EpochBounds) CheckSurroundVote(sourceEpoch, targetEpoch phase0.Epoch) bool {
	minTarget := e.GetMinTarget(sourceEpoch)

	return targetEpoch > minTarget
}

// CheckSurroundedVote checks if an attestation (s, t) could be surrounded.
// Returns true if t < M(s), meaning there might be an attestation that surrounds this one.
func (e *EpochBounds) CheckSurroundedVote(sourceEpoch, targetEpoch phase0.Epoch) bool {
	maxTarget := e.GetMaxTarget(sourceEpoch)

	return targetEpoch < maxTarget
}

// Prune removes epochs older than the given epoch.
func (e *EpochBounds) Prune(beforeEpoch phase0.Epoch) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for epoch := range e.m {
		if epoch < beforeEpoch {
			delete(e.m, epoch)
		}
	}

	for epoch := range e.mM {
		if epoch < beforeEpoch {
			delete(e.mM, epoch)
		}
	}
}
