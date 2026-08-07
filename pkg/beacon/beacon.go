// Package beacon is a small multi-node consensus-layer client. Reads fail over
// across the configured endpoints (with background health recovery) and
// slashing submissions are broadcast to every healthy node. Blocks are decoded
// into the fork-agnostic go-eth2-client union types.
package beacon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethpandaops/go-eth2-client/spec/all"
	"github.com/ethpandaops/go-eth2-client/spec/phase0"
	"github.com/ethpandaops/go-eth2-client/spec/version"
	"github.com/sirupsen/logrus"
)

// ErrNoEndpoint is returned when no beacon endpoints are configured.
var ErrNoEndpoint = errors.New("no beacon endpoints configured")

// Finality summarizes the chain's finality and head.
type Finality struct {
	FinalizedEpoch phase0.Epoch
	FinalizedSlot  phase0.Slot
	HeadSlot       phase0.Slot
}

// ValidatorStatus is the subset of validator state relevant to slashing.
type ValidatorStatus struct {
	Index             phase0.ValidatorIndex
	Slashed           bool
	Status            string
	ActivationEpoch   phase0.Epoch
	ExitEpoch         phase0.Epoch
	WithdrawableEpoch phase0.Epoch
}

// HeadEvent is a head SSE event.
type HeadEvent struct {
	Slot  phase0.Slot
	Block phase0.Root
}

// BlockEvent is a block SSE event.
type BlockEvent struct {
	Slot  phase0.Slot
	Block phase0.Root
}

// Service is the beacon client interface.
type Service interface {
	Start(ctx context.Context) error
	Stop() error

	Spec(ctx context.Context) (map[string]any, error)
	Finality(ctx context.Context) (*Finality, error)
	BlockByRoot(ctx context.Context, root phase0.Root) (*all.SignedBeaconBlock, error)
	BlockBySlot(ctx context.Context, slot phase0.Slot) (*all.SignedBeaconBlock, error)
	Committees(ctx context.Context, epoch phase0.Epoch) (map[phase0.Slot][][]phase0.ValidatorIndex, error)
	ValidatorsStatus(ctx context.Context, indices []phase0.ValidatorIndex) (map[phase0.ValidatorIndex]ValidatorStatus, error)

	SubmitAttesterSlashing(ctx context.Context, slashing *all.AttesterSlashing) error
	SubmitProposerSlashing(ctx context.Context, slashing *phase0.ProposerSlashing) error

	SubscribeToBlocks(ctx context.Context, handler func(BlockEvent))
	SubscribeToHeads(ctx context.Context, handler func(HeadEvent))
}

type node struct {
	url     string
	client  *http.Client
	healthy atomic.Bool
}

type service struct {
	cfg   *Config
	log   logrus.FieldLogger
	nodes []*node

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New builds a beacon service.
func New(cfg *Config, log logrus.FieldLogger) Service {
	nodes := make([]*node, 0, len(cfg.Endpoints))
	for _, ep := range cfg.Endpoints {
		n := &node{
			url:    strings.TrimRight(ep, "/"),
			client: &http.Client{Timeout: cfg.Timeout},
		}
		n.healthy.Store(true)
		nodes = append(nodes, n)
	}
	return &service{
		cfg:   cfg,
		log:   log.WithField("package", "beacon"),
		nodes: nodes,
	}
}

func (s *service) Start(ctx context.Context) error {
	if err := s.cfg.Validate(); err != nil {
		return err
	}
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.wg.Add(1)
	go s.healthLoop()
	return nil
}

func (s *service) Stop() error {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	return nil
}

// healthLoop re-probes unhealthy nodes so failover recovers automatically.
func (s *service) healthLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.cfg.HealthCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			for _, n := range s.nodes {
				if n.healthy.Load() {
					continue
				}
				if s.probe(n) {
					n.healthy.Store(true)
					s.log.WithField("endpoint", redact(n.url)).Info("beacon node recovered")
				}
			}
		}
	}
}

func (s *service) probe(n *node) bool {
	req, err := http.NewRequestWithContext(s.ctx, http.MethodGet, n.url+"/eth/v1/node/health", nil)
	if err != nil {
		return false
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode < 500
}

// Spec fetches /eth/v1/config/spec as a raw string map.
func (s *service) Spec(ctx context.Context) (map[string]any, error) {
	var env struct {
		Data map[string]any `json:"data"`
	}
	if _, err := s.getJSON(ctx, "/eth/v1/config/spec", &env); err != nil {
		return nil, err
	}
	return env.Data, nil
}

// Finality reads finality checkpoints and head.
func (s *service) Finality(ctx context.Context) (*Finality, error) {
	var fc struct {
		Data struct {
			Finalized struct {
				Epoch string `json:"epoch"`
			} `json:"finalized"`
		} `json:"data"`
	}
	if _, err := s.getJSON(ctx, "/eth/v1/beacon/states/head/finality_checkpoints", &fc); err != nil {
		return nil, err
	}
	var hdr struct {
		Data struct {
			Header struct {
				Message struct {
					Slot string `json:"slot"`
				} `json:"message"`
			} `json:"header"`
		} `json:"data"`
	}
	if _, err := s.getJSON(ctx, "/eth/v1/beacon/headers/head", &hdr); err != nil {
		return nil, err
	}
	return &Finality{
		FinalizedEpoch: phase0.Epoch(parseUint(fc.Data.Finalized.Epoch)),
		HeadSlot:       phase0.Slot(parseUint(hdr.Data.Header.Message.Slot)),
	}, nil
}

// BlockByRoot fetches a block by its root, or nil if the node does not have it.
func (s *service) BlockByRoot(ctx context.Context, root phase0.Root) (*all.SignedBeaconBlock, error) {
	return s.fetchBlock(ctx, fmt.Sprintf("/eth/v2/beacon/blocks/0x%x", root))
}

// BlockBySlot fetches the canonical block at a slot, or nil if missing/skipped.
func (s *service) BlockBySlot(ctx context.Context, slot phase0.Slot) (*all.SignedBeaconBlock, error) {
	return s.fetchBlock(ctx, fmt.Sprintf("/eth/v2/beacon/blocks/%d", slot))
}

func (s *service) fetchBlock(ctx context.Context, path string) (*all.SignedBeaconBlock, error) {
	body, status, err := s.getRaw(ctx, path, "application/json")
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("beacon %s: status %d", path, status)
	}
	var env struct {
		Version string          `json:"version"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode block envelope: %w", err)
	}
	ver, err := version.DataVersionFromString(env.Version)
	if err != nil {
		return nil, fmt.Errorf("unknown block version %q: %w", env.Version, err)
	}
	blk := &all.SignedBeaconBlock{Version: ver}
	if err := blk.UnmarshalJSON(env.Data); err != nil {
		return nil, fmt.Errorf("decode block (version %s): %w", env.Version, err)
	}
	return blk, nil
}

// Committees returns the attester committees for an epoch as
// slot -> [committeeIndex][position] -> validator index.
func (s *service) Committees(ctx context.Context, epoch phase0.Epoch) (map[phase0.Slot][][]phase0.ValidatorIndex, error) {
	var env struct {
		Data []struct {
			Index      string   `json:"index"`
			Slot       string   `json:"slot"`
			Validators []string `json:"validators"`
		} `json:"data"`
	}
	path := fmt.Sprintf("/eth/v1/beacon/states/head/committees?epoch=%d", epoch)
	if _, err := s.getJSON(ctx, path, &env); err != nil {
		return nil, err
	}
	out := make(map[phase0.Slot][][]phase0.ValidatorIndex)
	for _, c := range env.Data {
		slot := phase0.Slot(parseUint(c.Slot))
		idx := int(parseUint(c.Index))
		vals := make([]phase0.ValidatorIndex, len(c.Validators))
		for i, v := range c.Validators {
			vals[i] = phase0.ValidatorIndex(parseUint(v))
		}
		committees := out[slot]
		for len(committees) <= idx {
			committees = append(committees, nil)
		}
		committees[idx] = vals
		out[slot] = committees
	}
	return out, nil
}

// ValidatorsStatus fetches validator state for the given indices via a POST body.
func (s *service) ValidatorsStatus(ctx context.Context, indices []phase0.ValidatorIndex) (map[phase0.ValidatorIndex]ValidatorStatus, error) {
	out := make(map[phase0.ValidatorIndex]ValidatorStatus, len(indices))
	if len(indices) == 0 {
		return out, nil
	}
	ids := make([]string, len(indices))
	for i, idx := range indices {
		ids[i] = strconv.FormatUint(uint64(idx), 10)
	}
	reqBody, _ := json.Marshal(map[string]any{"ids": ids})

	var env struct {
		Data []struct {
			Index     string `json:"index"`
			Status    string `json:"status"`
			Validator struct {
				Slashed           bool   `json:"slashed"`
				ActivationEpoch   string `json:"activation_epoch"`
				ExitEpoch         string `json:"exit_epoch"`
				WithdrawableEpoch string `json:"withdrawable_epoch"`
			} `json:"validator"`
		} `json:"data"`
	}
	if err := s.postJSONOne(ctx, "/eth/v1/beacon/states/head/validators", reqBody, &env); err != nil {
		return nil, err
	}
	for _, v := range env.Data {
		idx := phase0.ValidatorIndex(parseUint(v.Index))
		out[idx] = ValidatorStatus{
			Index:             idx,
			Slashed:           v.Validator.Slashed,
			Status:            v.Status,
			ActivationEpoch:   phase0.Epoch(parseUint(v.Validator.ActivationEpoch)),
			ExitEpoch:         phase0.Epoch(parseUint(v.Validator.ExitEpoch)),
			WithdrawableEpoch: phase0.Epoch(parseUint(v.Validator.WithdrawableEpoch)),
		}
	}
	return out, nil
}

// SubmitAttesterSlashing broadcasts an attester slashing to all healthy nodes,
// using the v2 pool endpoint (with the consensus-version header) and falling
// back to v1 for nodes that do not expose v2.
func (s *service) SubmitAttesterSlashing(ctx context.Context, slashing *all.AttesterSlashing) error {
	body, err := slashing.MarshalJSON()
	if err != nil {
		return fmt.Errorf("marshal attester slashing: %w", err)
	}
	headers := map[string]string{"Eth-Consensus-Version": slashing.Version.String()}
	return s.broadcast(ctx, func(ctx context.Context, n *node) error {
		status, err := s.postTo(ctx, n, "/eth/v2/beacon/pool/attester_slashings", body, headers)
		if err == nil && (status == http.StatusNotFound || status == http.StatusMethodNotAllowed) {
			status, err = s.postTo(ctx, n, "/eth/v1/beacon/pool/attester_slashings", body, nil)
		}
		return submitResult(status, err)
	})
}

// SubmitProposerSlashing broadcasts a proposer slashing to all healthy nodes.
func (s *service) SubmitProposerSlashing(ctx context.Context, slashing *phase0.ProposerSlashing) error {
	body, err := slashing.MarshalJSON()
	if err != nil {
		return fmt.Errorf("marshal proposer slashing: %w", err)
	}
	return s.broadcast(ctx, func(ctx context.Context, n *node) error {
		status, err := s.postTo(ctx, n, "/eth/v1/beacon/pool/proposer_slashings", body, nil)
		return submitResult(status, err)
	})
}

func submitResult(status int, err error) error {
	if err != nil {
		return err
	}
	if status == http.StatusOK || status == http.StatusAccepted {
		return nil
	}
	return fmt.Errorf("submission rejected: status %d", status)
}

// broadcast runs fn against every healthy node and succeeds if at least one
// accepts.
func (s *service) broadcast(ctx context.Context, fn func(context.Context, *node) error) error {
	var lastErr error
	accepted := false
	for _, n := range s.nodes {
		if !n.healthy.Load() {
			continue
		}
		if err := fn(ctx, n); err != nil {
			lastErr = err
			continue
		}
		accepted = true
	}
	if accepted {
		return nil
	}
	if lastErr == nil {
		lastErr = errors.New("no healthy nodes")
	}
	return lastErr
}

// --- request helpers ---

func (s *service) getJSON(ctx context.Context, path string, out any) (int, error) {
	body, status, err := s.getRaw(ctx, path, "application/json")
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return status, fmt.Errorf("GET %s: status %d", path, status)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return status, fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return status, nil
}

// getRaw tries each healthy node until one responds without a transport error.
func (s *service) getRaw(ctx context.Context, path, accept string) ([]byte, int, error) {
	var lastErr error
	for _, n := range s.healthyNodes() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.url+path, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Accept", accept)
		resp, err := n.client.Do(req)
		if err != nil {
			s.markUnhealthy(n, err)
			lastErr = err
			continue
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		return body, resp.StatusCode, nil
	}
	if lastErr == nil {
		lastErr = ErrNoEndpoint
	}
	return nil, 0, lastErr
}

func (s *service) postJSONOne(ctx context.Context, path string, body []byte, out any) error {
	for _, n := range s.healthyNodes() {
		status, respBody, err := s.postRawTo(ctx, n, path, body, nil)
		if err != nil {
			s.markUnhealthy(n, err)
			continue
		}
		if status != http.StatusOK {
			return fmt.Errorf("POST %s: status %d", path, status)
		}
		if out != nil {
			if err := json.Unmarshal(respBody, out); err != nil {
				return fmt.Errorf("decode %s: %w", path, err)
			}
		}
		return nil
	}
	return ErrNoEndpoint
}

func (s *service) postTo(ctx context.Context, n *node, path string, body []byte, headers map[string]string) (int, error) {
	status, _, err := s.postRawTo(ctx, n, path, body, headers)
	if err != nil {
		s.markUnhealthy(n, err)
	}
	return status, err
}

func (s *service) postRawTo(ctx context.Context, n *node, path string, body []byte, headers map[string]string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, respBody, nil
}

func (s *service) healthyNodes() []*node {
	healthy := make([]*node, 0, len(s.nodes))
	for _, n := range s.nodes {
		if n.healthy.Load() {
			healthy = append(healthy, n)
		}
	}
	if len(healthy) == 0 {
		return s.nodes // last resort: try everything
	}
	return healthy
}

func (s *service) markUnhealthy(n *node, err error) {
	if n.healthy.CompareAndSwap(true, false) {
		s.log.WithField("endpoint", redact(n.url)).WithError(err).Warn("beacon node marked unhealthy")
	}
}

func parseUint(s string) uint64 {
	v, _ := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 10, 64)
	return v
}

func redact(endpoint string) string {
	if i := strings.Index(endpoint, "@"); i >= 0 {
		if j := strings.Index(endpoint, "://"); j >= 0 {
			return endpoint[:j+3] + "***@" + endpoint[i+1:]
		}
	}
	return endpoint
}
