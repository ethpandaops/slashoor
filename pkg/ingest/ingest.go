// Package ingest converts a signed beacon block into detection inputs: the
// attester votes (each attestation expanded to its validator indices and tagged
// with a locator), the proposal record, and the set of validators the block
// itself slashes on chain (used to keep the local slashed mirror current).
package ingest

import (
	"context"

	"github.com/ethpandaops/go-eth2-client/spec/all"
	"github.com/ethpandaops/go-eth2-client/spec/phase0"
	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/slashoor/pkg/chainspec"
	"github.com/ethpandaops/slashoor/pkg/duties"
	"github.com/ethpandaops/slashoor/pkg/indexer"
)

// Result is the detection input extracted from one block.
type Result struct {
	Proposal indexer.Proposal
	Votes    []indexer.Vote
	// Slashed are validators this block slashes on chain (from included
	// attester/proposer slashings).
	Slashed []phase0.ValidatorIndex
}

// Extractor turns blocks into detection inputs using a committee resolver.
type Extractor struct {
	spec   *chainspec.Spec
	duties *duties.Resolver
	log    logrus.FieldLogger
}

// New builds an extractor.
func New(spec *chainspec.Spec, dutiesResolver *duties.Resolver, log logrus.FieldLogger) *Extractor {
	return &Extractor{
		spec:   spec,
		duties: dutiesResolver,
		log:    log.WithField("package", "ingest"),
	}
}

// Block extracts detection inputs from a signed beacon block located at
// blockRoot (which may be canonical or orphaned).
func (e *Extractor) Block(ctx context.Context, block *all.SignedBeaconBlock, blockRoot phase0.Root) (*Result, error) {
	if block == nil || block.Message == nil || block.Message.Body == nil {
		return &Result{}, nil
	}
	msg := block.Message
	body := msg.Body

	res := &Result{
		Proposal: indexer.Proposal{
			Slot:          msg.Slot,
			ProposerIndex: msg.ProposerIndex,
			BlockRoot:     blockRoot,
		},
	}

	for i, att := range body.Attestations {
		if att.Data == nil || att.Data.Source == nil || att.Data.Target == nil {
			continue
		}
		indices, err := e.duties.AttestingIndices(ctx, att)
		if err != nil {
			// Committees unavailable (e.g. Dora pruned the epoch's duties): the
			// attestation cannot be expanded, so it is skipped for detection.
			e.log.WithError(err).WithField("slot", msg.Slot).Debug("skipping attestation: cannot resolve committees")
			continue
		}
		if len(indices) == 0 {
			continue
		}
		dataRoot, err := att.Data.HashTreeRoot()
		if err != nil {
			e.log.WithError(err).Debug("skipping attestation: cannot hash data")
			continue
		}
		res.Votes = append(res.Votes, indexer.Vote{
			Source:   att.Data.Source.Epoch,
			Target:   att.Data.Target.Epoch,
			DataRoot: phase0.Root(dataRoot),
			Indices:  indices,
			Locator: indexer.Locator{
				Slot:      msg.Slot,
				BlockRoot: blockRoot,
				AttIndex:  uint16(i),
			},
		})
	}

	res.Slashed = slashedByBlock(body)
	return res, nil
}

// slashedByBlock returns the validators slashed by the block's included
// attester and proposer slashings.
func slashedByBlock(body *all.BeaconBlockBody) []phase0.ValidatorIndex {
	seen := make(map[phase0.ValidatorIndex]struct{})
	var out []phase0.ValidatorIndex
	add := func(v phase0.ValidatorIndex) {
		if _, dup := seen[v]; dup {
			return
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}

	for _, as := range body.AttesterSlashings {
		if as.Attestation1 == nil || as.Attestation2 == nil {
			continue
		}
		set := make(map[uint64]struct{}, len(as.Attestation1.AttestingIndices))
		for _, idx := range as.Attestation1.AttestingIndices {
			set[idx] = struct{}{}
		}
		for _, idx := range as.Attestation2.AttestingIndices {
			if _, ok := set[idx]; ok {
				add(phase0.ValidatorIndex(idx))
			}
		}
	}
	for _, ps := range body.ProposerSlashings {
		if ps.SignedHeader1 != nil && ps.SignedHeader1.Message != nil {
			add(ps.SignedHeader1.Message.ProposerIndex)
		}
	}
	return out
}
