package indexer

import (
	"sync"

	"github.com/attestantio/go-eth2-client/spec/phase0"

	"github.com/slashoor/slashoor/pkg/beacon"
)

// ValidatorAttestation stores attestation data for a specific validator.
type ValidatorAttestation struct {
	SourceEpoch phase0.Epoch
	TargetEpoch phase0.Epoch
	TargetRoot  phase0.Root
	Attestation *beacon.IndexedAttestation
}

// ValidatorStore tracks attestations per validator for slashing detection.
type ValidatorStore struct {
	mu           sync.RWMutex
	attestations map[phase0.ValidatorIndex]map[phase0.Epoch]*ValidatorAttestation
}

// NewValidatorStore creates a new validator store.
func NewValidatorStore() *ValidatorStore {
	return &ValidatorStore{
		attestations: make(map[phase0.ValidatorIndex]map[phase0.Epoch]*ValidatorAttestation, 1024),
	}
}

// Add adds an attestation to the store for the given validators.
func (v *ValidatorStore) Add(att *beacon.IndexedAttestation) {
	v.mu.Lock()
	defer v.mu.Unlock()

	valAtt := &ValidatorAttestation{
		SourceEpoch: att.Data.Source.Epoch,
		TargetEpoch: att.Data.Target.Epoch,
		TargetRoot:  att.Data.Target.Root,
		Attestation: att,
	}

	for _, idx := range att.AttestingIndices {
		if _, exists := v.attestations[idx]; !exists {
			v.attestations[idx] = make(map[phase0.Epoch]*ValidatorAttestation, 64)
		}

		v.attestations[idx][att.Data.Target.Epoch] = valAtt
	}
}

// GetByTargetEpoch retrieves attestation for a validator at a specific target epoch.
func (v *ValidatorStore) GetByTargetEpoch(
	validatorIdx phase0.ValidatorIndex,
	targetEpoch phase0.Epoch,
) *ValidatorAttestation {
	v.mu.RLock()
	defer v.mu.RUnlock()

	validatorAtts, exists := v.attestations[validatorIdx]
	if !exists {
		return nil
	}

	return validatorAtts[targetEpoch]
}

// GetAllForValidator retrieves all attestations for a validator.
func (v *ValidatorStore) GetAllForValidator(
	validatorIdx phase0.ValidatorIndex,
) []*ValidatorAttestation {
	v.mu.RLock()
	defer v.mu.RUnlock()

	validatorAtts, exists := v.attestations[validatorIdx]
	if !exists {
		return nil
	}

	result := make([]*ValidatorAttestation, 0, len(validatorAtts))
	for _, att := range validatorAtts {
		result = append(result, att)
	}

	return result
}

// FindDoubleVote finds a conflicting attestation for the same target epoch.
func (v *ValidatorStore) FindDoubleVote(
	validatorIdx phase0.ValidatorIndex,
	targetEpoch phase0.Epoch,
	targetRoot phase0.Root,
) *ValidatorAttestation {
	v.mu.RLock()
	defer v.mu.RUnlock()

	validatorAtts, exists := v.attestations[validatorIdx]
	if !exists {
		return nil
	}

	existing, exists := validatorAtts[targetEpoch]
	if !exists {
		return nil
	}

	if existing.TargetRoot != targetRoot {
		return existing
	}

	return nil
}

// FindSurroundingVote finds an attestation that surrounds the given attestation.
// Surrounding: existing (s', t') surrounds new (s, t) if s' < s AND t < t'
func (v *ValidatorStore) FindSurroundingVote(
	validatorIdx phase0.ValidatorIndex,
	sourceEpoch, targetEpoch phase0.Epoch,
) *ValidatorAttestation {
	v.mu.RLock()
	defer v.mu.RUnlock()

	validatorAtts, exists := v.attestations[validatorIdx]
	if !exists {
		return nil
	}

	for _, existing := range validatorAtts {
		if existing.SourceEpoch < sourceEpoch && targetEpoch < existing.TargetEpoch {
			return existing
		}
	}

	return nil
}

// FindSurroundedVote finds an attestation that is surrounded by the given attestation.
// Surrounded: new (s, t) surrounds existing (s', t') if s < s' AND t' < t
func (v *ValidatorStore) FindSurroundedVote(
	validatorIdx phase0.ValidatorIndex,
	sourceEpoch, targetEpoch phase0.Epoch,
) *ValidatorAttestation {
	v.mu.RLock()
	defer v.mu.RUnlock()

	validatorAtts, exists := v.attestations[validatorIdx]
	if !exists {
		return nil
	}

	for _, existing := range validatorAtts {
		if sourceEpoch < existing.SourceEpoch && existing.TargetEpoch < targetEpoch {
			return existing
		}
	}

	return nil
}

// Prune removes attestations older than the given epoch.
func (v *ValidatorStore) Prune(beforeEpoch phase0.Epoch) {
	v.mu.Lock()
	defer v.mu.Unlock()

	for validatorIdx, validatorAtts := range v.attestations {
		for epoch := range validatorAtts {
			if epoch < beforeEpoch {
				delete(validatorAtts, epoch)
			}
		}

		if len(validatorAtts) == 0 {
			delete(v.attestations, validatorIdx)
		}
	}
}
