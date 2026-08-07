// Package dora is a client for a Dora explorer instance. Dora is slashoor's
// durable store: it serves full block bodies for canonical AND orphaned blocks
// (via the block-download route) and attester committees for historic epochs
// (via the duties API), neither of which a beacon node can provide for pruned
// history. Only compact locators are kept locally; the full evidence is
// re-fetched from here when an offense is found.
package dora

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ethpandaops/go-eth2-client/spec/all"
	"github.com/ethpandaops/go-eth2-client/spec/phase0"
	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/slashoor/pkg/chainspec"
)

// SlotInfo is a block entry from the Dora slots API.
type SlotInfo struct {
	Slot       phase0.Slot
	Epoch      phase0.Epoch
	Proposer   phase0.ValidatorIndex
	BlockRoot  phase0.Root
	ParentRoot phase0.Root
	Status     string
	Orphaned   bool
	Finalized  bool
}

// EpochDuties holds the attester committees for an epoch.
type EpochDuties struct {
	Epoch             phase0.Epoch
	CommitteesPerSlot uint64
	// Slots maps slot -> [committeeIndex][position] -> validator index.
	Slots map[phase0.Slot][][]phase0.ValidatorIndex
}

// ValidatorStatus is validator state relevant to the slashed_any filter.
type ValidatorStatus struct {
	Index             phase0.ValidatorIndex
	Slashed           bool
	Status            string
	ActivationEpoch   phase0.Epoch
	ExitEpoch         phase0.Epoch
	WithdrawableEpoch phase0.Epoch
}

// WithOrphaned selects orphaned-block inclusion in the slots listing.
type WithOrphaned int

const (
	// OrphanedExclude returns only canonical blocks.
	OrphanedExclude WithOrphaned = 0
	// OrphanedInclude returns canonical and orphaned blocks.
	OrphanedInclude WithOrphaned = 1
	// OrphanedOnly returns only orphaned blocks.
	OrphanedOnly WithOrphaned = 2
)

// Client is the Dora API client interface.
type Client interface {
	// SetSpec provides the chain spec used to decode downloaded block SSZ. It
	// must be called before BlockByRoot.
	SetSpec(spec *chainspec.Spec)

	// SlotsRange lists blocks in [minSlot, maxSlot] with the given orphaned mode.
	SlotsRange(ctx context.Context, minSlot, maxSlot phase0.Slot, mode WithOrphaned) ([]SlotInfo, error)
	// BlockByRoot downloads and decodes a block by root. slot is required to
	// determine the fork version for SSZ decoding.
	BlockByRoot(ctx context.Context, root phase0.Root, slot phase0.Slot) (*all.SignedBeaconBlock, error)
	// EpochDuties returns the attester committees for an epoch, or nil if Dora
	// has no duties for it.
	EpochDuties(ctx context.Context, epoch phase0.Epoch) (*EpochDuties, error)
	// ValidatorsStatus returns validator state for the given indices.
	ValidatorsStatus(ctx context.Context, indices []phase0.ValidatorIndex) (map[phase0.ValidatorIndex]ValidatorStatus, error)
}

type client struct {
	cfg    *Config
	log    logrus.FieldLogger
	http   *http.Client
	base   string
	specMu sync.RWMutex
	spec   *chainspec.Spec
}

// New builds a Dora client.
func New(cfg *Config, log logrus.FieldLogger) Client {
	return &client{
		cfg:  cfg,
		log:  log.WithField("package", "dora"),
		http: &http.Client{Timeout: cfg.Timeout},
		base: strings.TrimRight(cfg.URL, "/"),
	}
}

func (c *client) SetSpec(spec *chainspec.Spec) {
	c.specMu.Lock()
	c.spec = spec
	c.specMu.Unlock()
}

func (c *client) getSpec() *chainspec.Spec {
	c.specMu.RLock()
	defer c.specMu.RUnlock()
	return c.spec
}

func (c *client) SlotsRange(ctx context.Context, minSlot, maxSlot phase0.Slot, mode WithOrphaned) ([]SlotInfo, error) {
	var out []SlotInfo
	page := uint64(0)
	for {
		path := fmt.Sprintf("/api/v1/slots?with_orphaned=%d&min_slot=%d&max_slot=%d&limit=1000&page=%d",
			int(mode), minSlot, maxSlot, page)
		var resp struct {
			Status string `json:"status"`
			Data   struct {
				Slots []struct {
					Slot       uint64 `json:"slot"`
					Epoch      uint64 `json:"epoch"`
					Status     string `json:"status"`
					Proposer   uint64 `json:"proposer"`
					BlockRoot  string `json:"block_root"`
					ParentRoot string `json:"parent_root"`
					Finalized  bool   `json:"finalized"`
				} `json:"slots"`
				NextPage *uint64 `json:"next_page"`
			} `json:"data"`
		}
		if err := c.getJSON(ctx, path, &resp); err != nil {
			return nil, err
		}
		for _, s := range resp.Data.Slots {
			out = append(out, SlotInfo{
				Slot:       phase0.Slot(s.Slot),
				Epoch:      phase0.Epoch(s.Epoch),
				Proposer:   phase0.ValidatorIndex(s.Proposer),
				BlockRoot:  decodeRoot(s.BlockRoot),
				ParentRoot: decodeRoot(s.ParentRoot),
				Status:     s.Status,
				Orphaned:   strings.EqualFold(s.Status, "orphaned"),
				Finalized:  s.Finalized,
			})
		}
		if resp.Data.NextPage == nil {
			break
		}
		page = *resp.Data.NextPage
	}
	return out, nil
}

func (c *client) BlockByRoot(ctx context.Context, root phase0.Root, slot phase0.Slot) (*all.SignedBeaconBlock, error) {
	spec := c.getSpec()
	if spec == nil {
		return nil, errors.New("dora: spec not set")
	}
	path := fmt.Sprintf("/slot/0x%x?download=block-ssz", root)
	body, status, err := c.getRaw(ctx, path)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("dora block download 0x%x: status %d", root, status)
	}
	blk := &all.SignedBeaconBlock{Version: spec.DataVersionAtSlot(slot)}
	if err := spec.DynSSZ().UnmarshalSSZ(blk, body); err != nil {
		return nil, fmt.Errorf("decode dora block 0x%x (version %s): %w", root, blk.Version, err)
	}
	return blk, nil
}

func (c *client) EpochDuties(ctx context.Context, epoch phase0.Epoch) (*EpochDuties, error) {
	path := fmt.Sprintf("/api/v1/epoch/%d/duties", epoch)
	body, status, err := c.getRaw(ctx, path)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, nil // duties unavailable for this epoch
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("dora duties epoch %d: status %d", epoch, status)
	}
	var resp struct {
		Data struct {
			Epoch             uint64 `json:"epoch"`
			CommitteesPerSlot uint64 `json:"committees_per_slot"`
			Slots             []struct {
				Slot       uint64     `json:"slot"`
				Committees [][]uint64 `json:"committees"`
			} `json:"slots"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode dora duties epoch %d: %w", epoch, err)
	}
	duties := &EpochDuties{
		Epoch:             epoch,
		CommitteesPerSlot: resp.Data.CommitteesPerSlot,
		Slots:             make(map[phase0.Slot][][]phase0.ValidatorIndex, len(resp.Data.Slots)),
	}
	for _, s := range resp.Data.Slots {
		committees := make([][]phase0.ValidatorIndex, len(s.Committees))
		for ci, members := range s.Committees {
			vals := make([]phase0.ValidatorIndex, len(members))
			for i, m := range members {
				vals[i] = phase0.ValidatorIndex(m)
			}
			committees[ci] = vals
		}
		duties.Slots[phase0.Slot(s.Slot)] = committees
	}
	return duties, nil
}

func (c *client) ValidatorsStatus(ctx context.Context, indices []phase0.ValidatorIndex) (map[phase0.ValidatorIndex]ValidatorStatus, error) {
	out := make(map[phase0.ValidatorIndex]ValidatorStatus, len(indices))
	if len(indices) == 0 {
		return out, nil
	}
	ids := make([]uint64, len(indices))
	for i, idx := range indices {
		ids[i] = uint64(idx)
	}
	reqBody, _ := json.Marshal(map[string]any{"indices": ids})

	var resp struct {
		Data struct {
			Validators []struct {
				Index             uint64 `json:"index"`
				Status            string `json:"status"`
				Slashed           bool   `json:"slashed"`
				ActivationEpoch   uint64 `json:"activation_epoch"`
				ExitEpoch         uint64 `json:"exit_epoch"`
				WithdrawableEpoch uint64 `json:"withdrawable_epoch"`
			} `json:"validators"`
		} `json:"data"`
	}
	if err := c.postJSON(ctx, "/api/v1/validators/status", reqBody, &resp); err != nil {
		return nil, err
	}
	for _, v := range resp.Data.Validators {
		idx := phase0.ValidatorIndex(v.Index)
		out[idx] = ValidatorStatus{
			Index:             idx,
			Slashed:           v.Slashed,
			Status:            v.Status,
			ActivationEpoch:   phase0.Epoch(v.ActivationEpoch),
			ExitEpoch:         phase0.Epoch(v.ExitEpoch),
			WithdrawableEpoch: phase0.Epoch(v.WithdrawableEpoch),
		}
	}
	return out, nil
}

// --- HTTP helpers with retry/backoff ---

func (c *client) getJSON(ctx context.Context, path string, out any) error {
	body, status, err := c.getRaw(ctx, path)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("dora GET %s: status %d", path, status)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("dora decode %s: %w", path, err)
	}
	return nil
}

func (c *client) getRaw(ctx context.Context, path string) ([]byte, int, error) {
	return c.do(ctx, http.MethodGet, path, nil)
}

func (c *client) postJSON(ctx context.Context, path string, body []byte, out any) error {
	respBody, status, err := c.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("dora POST %s: status %d", path, status)
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("dora decode %s: %w", path, err)
	}
	return nil
}

func (c *client) do(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	url := c.base + path
	backoff := 500 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		var reader io.Reader
		if body != nil {
			reader = strings.NewReader(string(body))
		}
		req, err := http.NewRequestWithContext(ctx, method, url, reader)
		if err != nil {
			return nil, 0, err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if c.cfg.AuthToken != "" {
			req.Header.Set("Authorization", "Bearer "+c.cfg.AuthToken)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
		} else {
			respBody, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil {
				lastErr = readErr
			} else if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
				lastErr = fmt.Errorf("dora %s: status %d", path, resp.StatusCode)
			} else {
				return respBody, resp.StatusCode, nil
			}
		}
		if attempt < c.cfg.MaxRetries {
			select {
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}
	}
	return nil, 0, fmt.Errorf("dora %s failed after %d attempts: %w", path, c.cfg.MaxRetries+1, lastErr)
}

func decodeRoot(s string) phase0.Root {
	var r phase0.Root
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err == nil && len(b) == len(r) {
		copy(r[:], b)
	}
	return r
}
