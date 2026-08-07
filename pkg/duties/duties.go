// Package duties resolves attester committees for an epoch and expands
// block-included attestations into their attesting validator indices.
//
// Committees come from Dora for historic epochs (a beacon node cannot recompute
// old committees without replaying state) and from a beacon node for the freshest
// epoch that Dora may not have indexed yet. Results are cached per epoch.
package duties

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/ethpandaops/go-eth2-client/spec/all"
	"github.com/ethpandaops/go-eth2-client/spec/phase0"
	"github.com/ethpandaops/go-eth2-client/spec/version"
	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/slashoor/pkg/beacon"
	"github.com/ethpandaops/slashoor/pkg/chainspec"
	"github.com/ethpandaops/slashoor/pkg/dora"
)

// Committees maps slot -> [committeeIndex][position] -> validator index.
type Committees map[phase0.Slot][][]phase0.ValidatorIndex

// Resolver resolves committees and expands attestations.
type Resolver struct {
	beacon beacon.Service
	dora   dora.Client // may be nil when Dora is disabled
	spec   *chainspec.Spec
	log    logrus.FieldLogger

	mu        sync.Mutex
	cache     map[phase0.Epoch]Committees
	order     []phase0.Epoch
	maxEpochs int
}

// New builds a resolver. dora may be nil.
func New(beaconSvc beacon.Service, doraClient dora.Client, spec *chainspec.Spec, log logrus.FieldLogger) *Resolver {
	return &Resolver{
		beacon:    beaconSvc,
		dora:      doraClient,
		spec:      spec,
		log:       log.WithField("package", "duties"),
		cache:     make(map[phase0.Epoch]Committees),
		maxEpochs: 64,
	}
}

// Warm fetches committees for the given epochs concurrently, populating the
// cache. Used to parallelize the otherwise-serial per-epoch duty loads during a
// backfill sweep. Errors are ignored here; the subsequent per-attestation
// resolution surfaces them.
func (r *Resolver) Warm(ctx context.Context, epochs []phase0.Epoch) {
	const workers = 16
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, e := range epochs {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(e phase0.Epoch) {
			defer wg.Done()
			defer func() { <-sem }()
			_, _ = r.Committees(ctx, e)
		}(e)
	}
	wg.Wait()
}

// Committees returns the attester committees for an epoch, fetching from Dora
// (historic) with a beacon-node fallback (freshest epoch), and caching the
// result.
func (r *Resolver) Committees(ctx context.Context, epoch phase0.Epoch) (Committees, error) {
	r.mu.Lock()
	if c, ok := r.cache[epoch]; ok {
		r.mu.Unlock()
		return c, nil
	}
	r.mu.Unlock()

	committees, err := r.fetch(ctx, epoch)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	r.put(epoch, committees)
	r.mu.Unlock()
	return committees, nil
}

func (r *Resolver) fetch(ctx context.Context, epoch phase0.Epoch) (Committees, error) {
	if r.dora != nil {
		duties, err := r.dora.EpochDuties(ctx, epoch)
		if err != nil {
			r.log.WithError(err).WithField("epoch", epoch).Debug("dora duties failed, trying beacon")
		} else if duties != nil && len(duties.Slots) > 0 {
			return Committees(duties.Slots), nil
		}
	}
	committees, err := r.beacon.Committees(ctx, epoch)
	if err != nil {
		return nil, fmt.Errorf("resolve committees for epoch %d: %w", epoch, err)
	}
	if len(committees) == 0 {
		return nil, fmt.Errorf("no committees available for epoch %d", epoch)
	}
	return Committees(committees), nil
}

// put inserts into the cache with simple oldest-eviction.
func (r *Resolver) put(epoch phase0.Epoch, c Committees) {
	if _, ok := r.cache[epoch]; ok {
		r.cache[epoch] = c
		return
	}
	r.cache[epoch] = c
	r.order = append(r.order, epoch)
	for len(r.order) > r.maxEpochs {
		oldest := r.order[0]
		r.order = r.order[1:]
		delete(r.cache, oldest)
	}
}

// AttestingIndices expands a block-included attestation into its sorted
// attesting validator indices.
func (r *Resolver) AttestingIndices(ctx context.Context, att *all.Attestation) ([]phase0.ValidatorIndex, error) {
	if att.Data == nil {
		return nil, fmt.Errorf("attestation has no data")
	}
	slot := att.Data.Slot
	epoch := r.spec.EpochOfSlot(slot)
	committees, err := r.Committees(ctx, epoch)
	if err != nil {
		return nil, err
	}
	slotCommittees := committees[slot]
	if slotCommittees == nil {
		return nil, fmt.Errorf("no committees for slot %d", slot)
	}
	electra := att.Version >= version.DataVersionElectra
	return ExpandAttestation(att, slotCommittees, electra)
}

// ExpandAttestation resolves an attestation's aggregation bits (and, from
// Electra, committee bits) into sorted, unique validator indices. It implements
// get_attesting_indices for both the pre-Electra single-committee layout and the
// EIP-7549 concatenated-committee layout.
func ExpandAttestation(att *all.Attestation, slotCommittees [][]phase0.ValidatorIndex, electra bool) ([]phase0.ValidatorIndex, error) {
	seen := make(map[phase0.ValidatorIndex]struct{})
	var out []phase0.ValidatorIndex
	add := func(v phase0.ValidatorIndex) {
		if _, dup := seen[v]; dup {
			return
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}

	if electra {
		committeeIndices := att.CommitteeBits.BitIndices()
		offset := 0
		for _, ci := range committeeIndices {
			if ci < 0 || ci >= len(slotCommittees) {
				return nil, fmt.Errorf("committee index %d out of range (%d committees)", ci, len(slotCommittees))
			}
			committee := slotCommittees[ci]
			for i, v := range committee {
				if att.AggregationBits.BitAt(uint64(offset + i)) {
					add(v)
				}
			}
			offset += len(committee)
		}
	} else {
		ci := int(att.Data.Index)
		if ci >= len(slotCommittees) {
			return nil, fmt.Errorf("committee index %d out of range (%d committees)", ci, len(slotCommittees))
		}
		committee := slotCommittees[ci]
		for i, v := range committee {
			if att.AggregationBits.BitAt(uint64(i)) {
				add(v)
			}
		}
	}

	slices.Sort(out)
	return out, nil
}
