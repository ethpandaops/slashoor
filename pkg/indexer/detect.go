package indexer

import (
	"math"

	"github.com/ethpandaops/go-eth2-client/spec/phase0"
)

// The min/max target arrays follow the chunked design of Lighthouse and Prysm
// (https://hackmd.io/@sproul/min-max-slasher), reasoned about entirely in
// absolute target-epoch space and stored as uint16 distances from the array
// index epoch.
//
//   minTargets[v][e] = min target over v's attestations with source > e.
//     A new attestation (s,t) SURROUNDS an existing one iff t > minTargets[v][s].
//   maxTargets[v][e] = max target over v's attestations with source < e.
//     A new attestation (s,t) IS SURROUNDED iff t < maxTargets[v][s].
//
// Distances keep values small and highly compressible. neutralMin encodes
// +infinity (no constraint); neutralMax encodes the index epoch itself (also no
// constraint, since a valid new attestation always has target > source).

// maxDistance is the largest distance a min cell can hold without colliding with
// the neutral sentinel.
const maxDistance = uint64(neutralMin) - 1 // 0xFFFE

// decodeMinTarget returns the absolute min target stored at index epoch e, or
// math.MaxUint64 for the neutral (no-constraint) cell.
func decodeMinTarget(e phase0.Epoch, cell uint16) uint64 {
	if cell == neutralMin {
		return math.MaxUint64
	}
	return uint64(e) + uint64(cell)
}

// encodeMinDistance encodes an absolute target as a min-cell distance from e.
// Targets of source>e attestations are always > e, so the distance is >= 1;
// absurdly large distances saturate below the neutral sentinel.
func encodeMinDistance(e phase0.Epoch, target phase0.Epoch) uint16 {
	d := uint64(target) - uint64(e)
	if d > maxDistance {
		return uint16(maxDistance)
	}
	return uint16(d)
}

// decodeMaxTarget returns the absolute max target stored at index epoch e. The
// neutral cell (0) decodes to e, which never triggers a surround.
func decodeMaxTarget(e phase0.Epoch, cell uint16) uint64 {
	return uint64(e) + uint64(cell)
}

// encodeMaxDistance encodes an absolute target as a max-cell distance from e.
// Callers only store when target > e, so the distance is >= 1 and never
// collides with the neutral sentinel (0).
func encodeMaxDistance(e phase0.Epoch, target phase0.Epoch) uint16 {
	d := uint64(target) - uint64(e)
	if d > uint64(math.MaxUint16) {
		return math.MaxUint16
	}
	return uint16(d)
}

// processVote runs the double-vote and surround checks for a single vote across
// all of its attesting validators, then applies the vote to the index. Checks
// always precede application so an attestation never conflicts with itself.
func (d *Detector) processVote(t *Txn, v *Vote, currentEpoch, floor phase0.Epoch) ([]SlashingCandidate, error) {
	var dataRoot8 [8]byte
	copy(dataRoot8[:], v.DataRoot[:8])

	var out []SlashingCandidate
	for _, idx := range v.Indices {
		// 1. Double vote: same target epoch, different data.
		dv, err := d.checkDoubleVote(t, idx, v, dataRoot8)
		if err != nil {
			return nil, err
		}
		if dv != nil {
			out = append(out, *dv)
		}

		// 2. Surround / surrounded.
		sv, err := d.checkSurround(t, idx, v)
		if err != nil {
			return nil, err
		}
		out = append(out, sv...)

		// 3. Apply.
		if err := d.applyVote(t, idx, v, dataRoot8, currentEpoch, floor); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (d *Detector) checkDoubleVote(t *Txn, idx phase0.ValidatorIndex, v *Vote, dataRoot8 [8]byte) (*SlashingCandidate, error) {
	loc, existingRoot8, ok, err := d.locate(t, idx, v.Target)
	if err != nil || !ok {
		return nil, err
	}
	if existingRoot8 == dataRoot8 {
		return nil, nil // same attestation, not a conflict
	}
	return &SlashingCandidate{
		Type:      ViolationDoubleVote,
		Validator: idx,
		Att1:      loc,
		Att2:      v.Locator,
	}, nil
}

func (d *Detector) checkSurround(t *Txn, idx phase0.ValidatorIndex, v *Vote) ([]SlashingCandidate, error) {
	vc := d.cfg.ValidatorChunk(idx)
	cell := d.cfg.CellIndex(idx, v.Source)
	var out []SlashingCandidate

	// New surrounds existing: t > minTarget(v, source).
	minChunk, err := t.MinChunk(d.cfg.EpochChunk(v.Source), vc)
	if err != nil {
		return nil, err
	}
	minAbs := decodeMinTarget(v.Source, minChunk[cell])
	if minAbs != math.MaxUint64 && uint64(v.Target) > minAbs {
		loc, _, ok, err := d.locate(t, idx, phase0.Epoch(minAbs))
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, SlashingCandidate{
				Type:      ViolationSurroundVote,
				Validator: idx,
				Att1:      v.Locator, // surrounder
				Att2:      loc,       // surrounded
			})
		}
	}

	// Existing surrounds new: t < maxTarget(v, source).
	maxChunk, err := t.MaxChunk(d.cfg.EpochChunk(v.Source), vc)
	if err != nil {
		return nil, err
	}
	maxAbs := decodeMaxTarget(v.Source, maxChunk[cell])
	if uint64(v.Target) < maxAbs {
		loc, _, ok, err := d.locate(t, idx, phase0.Epoch(maxAbs))
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, SlashingCandidate{
				Type:      ViolationSurroundVote,
				Validator: idx,
				Att1:      loc,       // surrounder
				Att2:      v.Locator, // surrounded
			})
		}
	}
	return out, nil
}

// locate returns the recorded attestation locator and data-root prefix for a
// validator at a target epoch, using the attester group -> epoch dict mapping.
func (d *Detector) locate(t *Txn, idx phase0.ValidatorIndex, target phase0.Epoch) (Locator, [8]byte, bool, error) {
	vc := d.cfg.ValidatorChunk(idx)
	refs, err := t.Group(target, vc)
	if err != nil {
		return Locator{}, [8]byte{}, false, err
	}
	ref := refs[uint64(idx)%d.cfg.ValidatorChunkSize]
	if ref == 0 {
		return Locator{}, [8]byte{}, false, nil
	}
	dict, err := t.Dict(target)
	if err != nil {
		return Locator{}, [8]byte{}, false, err
	}
	entry, ok := dict.Get(ref)
	if !ok {
		return Locator{}, [8]byte{}, false, nil
	}
	return Locator{Slot: entry.Slot, BlockRoot: entry.BlockRoot, AttIndex: entry.AttIndex}, entry.DataRoot8, true, nil
}

// applyVote records the vote in the attester group/dict and updates the min/max
// target arrays. The first attestation recorded for a (validator, target) wins;
// a conflicting one is reported by checkDoubleVote but does not overwrite it.
func (d *Detector) applyVote(t *Txn, idx phase0.ValidatorIndex, v *Vote, dataRoot8 [8]byte, currentEpoch, floor phase0.Epoch) error {
	vc := d.cfg.ValidatorChunk(idx)
	pos := uint64(idx) % d.cfg.ValidatorChunkSize

	refs, err := t.Group(v.Target, vc)
	if err != nil {
		return err
	}
	if refs[pos] == 0 {
		dict, err := t.Dict(v.Target)
		if err != nil {
			return err
		}
		ref := dict.Intern(DictEntry{
			DataRoot8: dataRoot8,
			Slot:      v.Locator.Slot,
			BlockRoot: v.Locator.BlockRoot,
			AttIndex:  v.Locator.AttIndex,
		})
		t.DirtyDict(v.Target)
		refs[pos] = ref
		t.DirtyGroup(v.Target, vc)
	}

	if err := d.updateMin(t, idx, v.Source, v.Target, floor); err != nil {
		return err
	}
	return d.updateMax(t, idx, v.Source, v.Target, currentEpoch)
}

// updateMin lowers minTarget for all epochs in [floor, source-1] toward target,
// propagating backward with early termination (the array is non-increasing as
// the index epoch decreases).
func (d *Detector) updateMin(t *Txn, idx phase0.ValidatorIndex, source, target, floor phase0.Epoch) error {
	if source == 0 {
		return nil
	}
	vc := d.cfg.ValidatorChunk(idx)
	e := source - 1
	var chunk []uint16
	var curEC uint32
	loaded := false
	for {
		ec := d.cfg.EpochChunk(e)
		if !loaded || ec != curEC {
			c, err := t.MinChunk(ec, vc)
			if err != nil {
				return err
			}
			chunk, curEC, loaded = c, ec, true
		}
		cell := d.cfg.CellIndex(idx, e)
		if uint64(target) < decodeMinTarget(e, chunk[cell]) {
			chunk[cell] = encodeMinDistance(e, target)
			t.DirtyMinChunk(ec, vc)
		} else {
			break // early termination
		}
		if e <= floor || e == 0 {
			break
		}
		e--
	}
	return nil
}

// updateMax raises maxTarget for all epochs in [source+1, ceil] toward target,
// propagating forward with early termination (the array is non-decreasing as the
// index epoch increases). Only epochs below target are stored, since a target at
// or below the index epoch can never surround a future attestation there.
func (d *Detector) updateMax(t *Txn, idx phase0.ValidatorIndex, source, target, ceil phase0.Epoch) error {
	vc := d.cfg.ValidatorChunk(idx)
	var chunk []uint16
	var curEC uint32
	loaded := false
	for e := source + 1; e <= ceil; e++ {
		if uint64(target) <= uint64(e) {
			break // distance would be non-positive; higher e only worse
		}
		ec := d.cfg.EpochChunk(e)
		if !loaded || ec != curEC {
			c, err := t.MaxChunk(ec, vc)
			if err != nil {
				return err
			}
			chunk, curEC, loaded = c, ec, true
		}
		cell := d.cfg.CellIndex(idx, e)
		if uint64(target) > decodeMaxTarget(e, chunk[cell]) {
			chunk[cell] = encodeMaxDistance(e, target)
			t.DirtyMaxChunk(ec, vc)
		} else {
			break // early termination
		}
	}
	return nil
}
