// Package chainspec holds the parsed beacon-chain configuration: slot/epoch
// math, the fork schedule (fork version and DataVersion by epoch/slot) and a
// dynamic-ssz encoder built from the active preset. Everything that needs to
// interpret a slot, epoch or fork-dependent SSZ layout depends on this package.
package chainspec

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/ethpandaops/go-eth2-client/spec/phase0"
	"github.com/ethpandaops/go-eth2-client/spec/version"
	dynssz "github.com/pk910/dynamic-ssz"
)

// ForkVersion is a 4-byte consensus fork version.
type ForkVersion [4]byte

// String renders the fork version as a 0x-prefixed hex string.
func (v ForkVersion) String() string {
	return "0x" + hex.EncodeToString(v[:])
}

// Fork is one scheduled hard fork.
type Fork struct {
	Name    string
	Version ForkVersion
	Epoch   phase0.Epoch
	Data    version.DataVersion
}

// forkDef pairs a fork's DataVersion with the spec keys that describe it.
type forkDef struct {
	name       string
	data       version.DataVersion
	versionKey string
	epochKey   string
}

// forkDefs lists every fork slashoor understands, oldest first. Genesis is
// always active from epoch 0; the rest are read from the spec and dropped when
// unscheduled (missing or far-future epoch).
var forkDefs = []forkDef{
	{"phase0", version.DataVersionPhase0, "GENESIS_FORK_VERSION", ""},
	{"altair", version.DataVersionAltair, "ALTAIR_FORK_VERSION", "ALTAIR_FORK_EPOCH"},
	{"bellatrix", version.DataVersionBellatrix, "BELLATRIX_FORK_VERSION", "BELLATRIX_FORK_EPOCH"},
	{"capella", version.DataVersionCapella, "CAPELLA_FORK_VERSION", "CAPELLA_FORK_EPOCH"},
	{"deneb", version.DataVersionDeneb, "DENEB_FORK_VERSION", "DENEB_FORK_EPOCH"},
	{"electra", version.DataVersionElectra, "ELECTRA_FORK_VERSION", "ELECTRA_FORK_EPOCH"},
	{"fulu", version.DataVersionFulu, "FULU_FORK_VERSION", "FULU_FORK_EPOCH"},
	{"gloas", version.DataVersionGloas, "GLOAS_FORK_VERSION", "GLOAS_FORK_EPOCH"},
	{"heze", version.DataVersionHeze, "HEZE_FORK_VERSION", "HEZE_FORK_EPOCH"},
}

const farFutureEpoch = phase0.Epoch(^uint64(0))

// Spec is the parsed chain configuration plus a dynssz encoder for the active
// preset. It is immutable after construction and safe for concurrent use.
type Spec struct {
	raw            map[string]any
	slotsPerEpoch  uint64
	secondsPerSlot uint64
	forks          []Fork // scheduled forks, ascending by epoch
	dynSSZ         *dynssz.DynSsz
}

// New parses a raw /eth/v1/config/spec map (values may be strings or numbers)
// into a Spec. The map is retained for the dynssz encoder, so callers must not
// mutate it afterwards.
func New(raw map[string]any) (*Spec, error) {
	if raw == nil {
		return nil, fmt.Errorf("nil spec map")
	}

	slotsPerEpoch, err := uintValue(raw, "SLOTS_PER_EPOCH")
	if err != nil {
		return nil, err
	}
	if slotsPerEpoch == 0 {
		return nil, fmt.Errorf("SLOTS_PER_EPOCH is zero")
	}

	secondsPerSlot, err := uintValue(raw, "SECONDS_PER_SLOT")
	if err != nil {
		// Non-fatal: only used for wall-clock estimates.
		secondsPerSlot = 12
	}

	forks, err := parseForks(raw)
	if err != nil {
		return nil, err
	}

	return &Spec{
		raw:            raw,
		slotsPerEpoch:  slotsPerEpoch,
		secondsPerSlot: secondsPerSlot,
		forks:          forks,
		dynSSZ:         dynssz.NewDynSsz(raw),
	}, nil
}

func parseForks(raw map[string]any) ([]Fork, error) {
	forks := make([]Fork, 0, len(forkDefs))
	for _, def := range forkDefs {
		fv, ok, err := forkVersionValue(raw, def.versionKey)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue // fork version not present in spec
		}

		epoch := phase0.Epoch(0)
		if def.epochKey != "" {
			e, err := uintValue(raw, def.epochKey)
			if err != nil {
				continue // unscheduled fork
			}
			if phase0.Epoch(e) == farFutureEpoch {
				continue
			}
			epoch = phase0.Epoch(e)
		}

		forks = append(forks, Fork{
			Name:    def.name,
			Version: fv,
			Epoch:   epoch,
			Data:    def.data,
		})
	}

	if len(forks) == 0 {
		return nil, fmt.Errorf("no forks found in spec (missing GENESIS_FORK_VERSION?)")
	}

	sort.SliceStable(forks, func(i, j int) bool {
		return forks[i].Epoch < forks[j].Epoch
	})
	return forks, nil
}

// SlotsPerEpoch returns the configured slots per epoch.
func (s *Spec) SlotsPerEpoch() uint64 { return s.slotsPerEpoch }

// SecondsPerSlot returns the configured seconds per slot.
func (s *Spec) SecondsPerSlot() uint64 { return s.secondsPerSlot }

// DynSSZ returns the dynamic-ssz encoder configured for the active preset.
func (s *Spec) DynSSZ() *dynssz.DynSsz { return s.dynSSZ }

// EpochOfSlot returns the epoch containing the given slot.
func (s *Spec) EpochOfSlot(slot phase0.Slot) phase0.Epoch {
	return phase0.Epoch(uint64(slot) / s.slotsPerEpoch)
}

// EpochStartSlot returns the first slot of the given epoch.
func (s *Spec) EpochStartSlot(epoch phase0.Epoch) phase0.Slot {
	return phase0.Slot(uint64(epoch) * s.slotsPerEpoch)
}

// Forks returns the scheduled forks, ascending by epoch.
func (s *Spec) Forks() []Fork {
	out := make([]Fork, len(s.forks))
	copy(out, s.forks)
	return out
}

// ForkAtEpoch returns the fork active at the given epoch.
func (s *Spec) ForkAtEpoch(epoch phase0.Epoch) Fork {
	active := s.forks[0]
	for _, f := range s.forks {
		if f.Epoch <= epoch {
			active = f
		} else {
			break
		}
	}
	return active
}

// DataVersionAtEpoch returns the DataVersion active at the given epoch.
func (s *Spec) DataVersionAtEpoch(epoch phase0.Epoch) version.DataVersion {
	return s.ForkAtEpoch(epoch).Data
}

// DataVersionAtSlot returns the DataVersion active at the given slot.
func (s *Spec) DataVersionAtSlot(slot phase0.Slot) version.DataVersion {
	return s.DataVersionAtEpoch(s.EpochOfSlot(slot))
}

// CurrentAndPreviousForkEpoch returns, for a head epoch, the activation epochs
// of the currently active fork and the fork immediately before it. Evidence
// signed for a target epoch below the previous fork epoch can no longer be
// validated on chain (get_domain reconstructs only two fork versions), so this
// is the boundary between submittable and report-only offenses.
func (s *Spec) CurrentAndPreviousForkEpoch(headEpoch phase0.Epoch) (current, previous phase0.Epoch) {
	currentIdx := 0
	for i, f := range s.forks {
		if f.Epoch <= headEpoch {
			currentIdx = i
		} else {
			break
		}
	}
	current = s.forks[currentIdx].Epoch
	if currentIdx > 0 {
		previous = s.forks[currentIdx-1].Epoch
	}
	return current, previous
}

// SubmittableFromEpoch returns the lowest target epoch whose attester evidence
// is still submittable against a chain at headEpoch. Offenses with a target
// epoch below this cannot pass BLS domain verification.
func (s *Spec) SubmittableFromEpoch(headEpoch phase0.Epoch) phase0.Epoch {
	_, previous := s.CurrentAndPreviousForkEpoch(headEpoch)
	return previous
}

// uintValue reads a spec key that may be encoded as a decimal string or a
// numeric type and returns it as uint64.
func uintValue(raw map[string]any, key string) (uint64, error) {
	v, ok := raw[key]
	if !ok {
		return 0, fmt.Errorf("spec key %q not found", key)
	}
	switch t := v.(type) {
	case string:
		return strconv.ParseUint(t, 10, 64)
	case float64:
		return uint64(t), nil
	case uint64:
		return t, nil
	case int:
		return uint64(t), nil
	case int64:
		return uint64(t), nil
	default:
		return 0, fmt.Errorf("spec key %q has unsupported type %T", key, v)
	}
}

// forkVersionValue reads a fork version key (0x-prefixed hex string or byte
// slice). ok is false when the key is absent.
func forkVersionValue(raw map[string]any, key string) (ForkVersion, bool, error) {
	var fv ForkVersion
	v, ok := raw[key]
	if !ok {
		return fv, false, nil
	}
	switch t := v.(type) {
	case string:
		b, err := hex.DecodeString(strings.TrimPrefix(t, "0x"))
		if err != nil {
			return fv, false, fmt.Errorf("spec key %q: %w", key, err)
		}
		if len(b) != 4 {
			return fv, false, fmt.Errorf("spec key %q: expected 4 bytes, got %d", key, len(b))
		}
		copy(fv[:], b)
		return fv, true, nil
	case []byte:
		if len(t) != 4 {
			return fv, false, fmt.Errorf("spec key %q: expected 4 bytes, got %d", key, len(t))
		}
		copy(fv[:], t)
		return fv, true, nil
	default:
		return fv, false, fmt.Errorf("spec key %q has unsupported type %T", key, v)
	}
}
