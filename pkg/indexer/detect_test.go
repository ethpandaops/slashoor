package indexer

import (
	"math/rand"
	"testing"

	"github.com/ethpandaops/go-eth2-client/spec/phase0"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

// testAtt is a synthetic attestation whose AttestationData identity is captured
// by (Target, Data): two atts have equal data iff Data matches.
type testAtt struct {
	val    phase0.ValidatorIndex
	source phase0.Epoch
	target phase0.Epoch
	data   phase0.Root
	loc    Locator
}

// isSlashable is the spec predicate is_slashable_attestation_data over the
// synthetic atts, used as the detection oracle.
func isSlashable(a, b testAtt) bool {
	doubleVote := a.data != b.data && a.target == b.target
	surround := a.source < b.source && b.target < a.target
	surroundRev := b.source < a.source && a.target < b.target
	return doubleVote || surround || surroundRev
}

func (a testAtt) vote() Vote {
	return Vote{
		Source:   a.source,
		Target:   a.target,
		DataRoot: a.data,
		Indices:  []phase0.ValidatorIndex{a.val},
		Locator:  a.loc,
	}
}

func newDetector(t *testing.T) (*Detector, *Store) {
	t.Helper()
	s, err := Open(t.TempDir(), DefaultConfig(), logrus.New())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return NewDetector(s.Config(), logrus.New()), s
}

func locFor(i int) Locator {
	var r phase0.Root
	r[0] = byte(i)
	r[1] = byte(i >> 8)
	return Locator{Slot: phase0.Slot(i), BlockRoot: r, AttIndex: uint16(i % 8)}
}

// dataFor builds a distinct attestation-data root. Real hash_tree_root outputs
// differ across all bytes; the index interns by the first 8, so the identity
// must live there.
func dataFor(n int) phase0.Root {
	var r phase0.Root
	r[0] = byte(n)
	r[1] = byte(n >> 8)
	r[2] = byte(n >> 16)
	return r
}

func TestDetectDoubleVote(t *testing.T) {
	d, s := newDetector(t)
	txn := s.NewTxn()
	defer txn.Close()

	a := testAtt{val: 7, source: 3, target: 5, data: dataFor(1), loc: locFor(1)}
	b := testAtt{val: 7, source: 3, target: 5, data: dataFor(2), loc: locFor(2)}

	c1, err := d.ProcessAttestations(txn, []Vote{a.vote()}, 5)
	require.NoError(t, err)
	require.Empty(t, c1)

	c2, err := d.ProcessAttestations(txn, []Vote{b.vote()}, 5)
	require.NoError(t, err)
	require.Len(t, c2, 1)
	require.Equal(t, ViolationDoubleVote, c2[0].Type)
	require.Equal(t, phase0.ValidatorIndex(7), c2[0].Validator)
}

func TestDetectSurround(t *testing.T) {
	d, s := newDetector(t)
	txn := s.NewTxn()
	defer txn.Close()

	// Inner attestation first: source 4, target 5.
	inner := testAtt{val: 9, source: 4, target: 5, data: dataFor(1), loc: locFor(1)}
	// Outer surrounds inner: source 3 < 4, target 6 > 5.
	outer := testAtt{val: 9, source: 3, target: 6, data: dataFor(2), loc: locFor(2)}

	_, err := d.ProcessAttestations(txn, []Vote{inner.vote()}, 6)
	require.NoError(t, err)
	c, err := d.ProcessAttestations(txn, []Vote{outer.vote()}, 6)
	require.NoError(t, err)
	require.Len(t, c, 1)
	require.Equal(t, ViolationSurroundVote, c[0].Type)
	// Outer surrounds inner: Att1 is the surrounder (outer), Att2 the surrounded.
	require.Equal(t, outer.loc, c[0].Att1)
	require.Equal(t, inner.loc, c[0].Att2)
}

func TestDetectSurrounded(t *testing.T) {
	d, s := newDetector(t)
	txn := s.NewTxn()
	defer txn.Close()

	// Outer first: source 3, target 8.
	outer := testAtt{val: 2, source: 3, target: 8, data: dataFor(1), loc: locFor(1)}
	// Inner is surrounded by outer: source 4 > 3, target 6 < 8.
	inner := testAtt{val: 2, source: 4, target: 6, data: dataFor(2), loc: locFor(2)}

	_, err := d.ProcessAttestations(txn, []Vote{outer.vote()}, 8)
	require.NoError(t, err)
	c, err := d.ProcessAttestations(txn, []Vote{inner.vote()}, 8)
	require.NoError(t, err)
	require.Len(t, c, 1)
	require.Equal(t, ViolationSurroundVote, c[0].Type)
	// Existing outer surrounds new inner: Att1 surrounder (outer), Att2 surrounded (inner).
	require.Equal(t, outer.loc, c[0].Att1)
	require.Equal(t, inner.loc, c[0].Att2)
}

func TestDetectHonestChainNoFalsePositives(t *testing.T) {
	d, s := newDetector(t)
	txn := s.NewTxn()
	defer txn.Close()

	// A well-behaved validator: consecutive non-overlapping (e, e+1) votes.
	for e := 0; e < 40; e++ {
		a := testAtt{
			val:    5,
			source: phase0.Epoch(e),
			target: phase0.Epoch(e + 1),
			data:   dataFor(e),
			loc:    locFor(e),
		}
		c, err := d.ProcessAttestations(txn, []Vote{a.vote()}, phase0.Epoch(e+1))
		require.NoError(t, err)
		require.Empty(t, c, "no slashings on an honest chain (epoch %d)", e)
	}
}

func TestDetectIdempotentReprocessing(t *testing.T) {
	d, s := newDetector(t)
	txn := s.NewTxn()
	defer txn.Close()

	inner := testAtt{val: 1, source: 4, target: 5, data: dataFor(1), loc: locFor(1)}
	outer := testAtt{val: 1, source: 3, target: 6, data: dataFor(2), loc: locFor(2)}

	_, err := d.ProcessAttestations(txn, []Vote{inner.vote()}, 6)
	require.NoError(t, err)
	c1, err := d.ProcessAttestations(txn, []Vote{outer.vote()}, 6)
	require.NoError(t, err)
	require.Len(t, c1, 1)

	// Reprocessing the same attestations (canonical + orphaned inclusion) must
	// not corrupt state; the same conflict is re-detected, nothing new.
	_, err = d.ProcessAttestations(txn, []Vote{inner.vote()}, 6)
	require.NoError(t, err)
	c2, err := d.ProcessAttestations(txn, []Vote{outer.vote()}, 6)
	require.NoError(t, err)
	require.Len(t, c2, 1)
	require.Equal(t, c1[0], c2[0])
}

// TestDetectOracle feeds random attestations (at most one per validator/target,
// so no same-target double votes) in random inclusion order and checks the
// detector against a naive O(n^2) is_slashable_attestation_data oracle.
func TestDetectOracle(t *testing.T) {
	const (
		validators = 12
		epochs     = 40
		attsPerVal = 8
	)
	rng := rand.New(rand.NewSource(1))

	// Build atts: for each validator pick distinct targets, random earlier source.
	var atts []testAtt
	locID := 1
	for v := 0; v < validators; v++ {
		targets := rng.Perm(epochs)[:attsPerVal]
		for _, tgt := range targets {
			if tgt == 0 {
				continue
			}
			src := phase0.Epoch(rng.Intn(tgt)) // source < target
			atts = append(atts, testAtt{
				val:    phase0.ValidatorIndex(v),
				source: src,
				target: phase0.Epoch(tgt),
				data:   dataFor(locID),
				loc:    locFor(locID),
			})
			locID++
		}
	}

	// Oracle: validators that have at least one slashable pair.
	oracleValidators := make(map[phase0.ValidatorIndex]bool)
	for i := range atts {
		for j := i + 1; j < len(atts); j++ {
			if atts[i].val == atts[j].val && isSlashable(atts[i], atts[j]) {
				oracleValidators[atts[i].val] = true
			}
		}
	}

	// Process in random inclusion order with a running frontier.
	order := rng.Perm(len(atts))
	byLoc := make(map[Locator]testAtt, len(atts))
	for _, a := range atts {
		byLoc[a.loc] = a
	}

	d, s := newDetector(t)
	txn := s.NewTxn()
	defer txn.Close()

	detectedValidators := make(map[phase0.ValidatorIndex]bool)
	frontier := phase0.Epoch(0)
	for _, oi := range order {
		a := atts[oi]
		if a.target > frontier {
			frontier = a.target
		}
		cands, err := d.ProcessAttestations(txn, []Vote{a.vote()}, frontier)
		require.NoError(t, err)
		for _, c := range cands {
			// Soundness: every candidate is a genuinely slashable pair.
			x, okx := byLoc[c.Att1]
			y, oky := byLoc[c.Att2]
			require.True(t, okx && oky)
			require.Equal(t, x.val, y.val, "candidate spans one validator")
			require.True(t, isSlashable(x, y), "unsound candidate: %+v vs %+v", x, y)
			detectedValidators[c.Validator] = true
		}
	}

	// Existence completeness: every validator the oracle flags is detected.
	for v := range oracleValidators {
		require.True(t, detectedValidators[v], "missed offending validator %d", v)
	}
	// And no spurious offenders.
	for v := range detectedValidators {
		require.True(t, oracleValidators[v], "spurious offender %d", v)
	}
}

func TestDetectProposerDoubleProposal(t *testing.T) {
	d, s := newDetector(t)
	txn := s.NewTxn()
	defer txn.Close()

	rootA := phase0.Root{0xaa}
	rootB := phase0.Root{0xbb}

	c, err := d.ProcessProposal(txn, Proposal{Slot: 64, ProposerIndex: 3, BlockRoot: rootA})
	require.NoError(t, err)
	require.Nil(t, c)

	// Same root again: no conflict.
	c, err = d.ProcessProposal(txn, Proposal{Slot: 64, ProposerIndex: 3, BlockRoot: rootA})
	require.NoError(t, err)
	require.Nil(t, c)

	// Different root, same slot+proposer: double proposal.
	c, err = d.ProcessProposal(txn, Proposal{Slot: 64, ProposerIndex: 3, BlockRoot: rootB})
	require.NoError(t, err)
	require.NotNil(t, c)
	require.Equal(t, rootA, c.Root1)
	require.Equal(t, rootB, c.Root2)
}
