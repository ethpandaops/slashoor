package beacon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/sirupsen/logrus"
)

var (
	ErrNoEndpoint      = errors.New("no beacon endpoints configured")
	ErrRequestFailed   = errors.New("beacon request failed")
	ErrInvalidResponse = errors.New("invalid response from beacon node")
	ErrAllNodesFailed  = errors.New("all beacon nodes failed")
)

// AttestationData represents the data of an attestation.
type AttestationData struct {
	Slot            phase0.Slot
	Index           phase0.CommitteeIndex
	BeaconBlockRoot phase0.Root
	Source          *Checkpoint
	Target          *Checkpoint
}

// Checkpoint represents an epoch checkpoint.
type Checkpoint struct {
	Epoch phase0.Epoch
	Root  phase0.Root
}

// Attestation represents an attestation from a block.
type Attestation struct {
	AggregationBits []byte
	Data            *AttestationData
	Signature       phase0.BLSSignature
}

// IndexedAttestation represents an attestation with validator indices.
type IndexedAttestation struct {
	AttestingIndices []phase0.ValidatorIndex
	Data             *AttestationData
	Signature        phase0.BLSSignature
}

// AttesterSlashing represents a slashing proof for double/surround voting.
type AttesterSlashing struct {
	Attestation1 *IndexedAttestation
	Attestation2 *IndexedAttestation
}

// Committee represents a beacon committee.
type Committee struct {
	Index      phase0.CommitteeIndex
	Slot       phase0.Slot
	Validators []phase0.ValidatorIndex
}

// HeadEvent represents a new head event.
type HeadEvent struct {
	Slot  phase0.Slot
	Block phase0.Root
}

// BlockEvent represents a new block event from SSE.
type BlockEvent struct {
	Slot          phase0.Slot
	Block         phase0.Root
	ProposerIndex phase0.ValidatorIndex
}

// BeaconBlockHeader represents a beacon block header.
type BeaconBlockHeader struct {
	Slot          phase0.Slot
	ProposerIndex phase0.ValidatorIndex
	ParentRoot    phase0.Root
	StateRoot     phase0.Root
	BodyRoot      phase0.Root
}

// SignedBeaconBlockHeader represents a signed beacon block header.
type SignedBeaconBlockHeader struct {
	Message   *BeaconBlockHeader
	Signature phase0.BLSSignature
}

// ProposerSlashing represents a proposer slashing proof.
type ProposerSlashing struct {
	SignedHeader1 *SignedBeaconBlockHeader
	SignedHeader2 *SignedBeaconBlockHeader
}

// Finality contains the current finality checkpoints.
type Finality struct {
	FinalizedEpoch phase0.Epoch
	FinalizedSlot  phase0.Slot
	HeadSlot       phase0.Slot
}

// Service defines the interface for beacon node interactions.
type Service interface {
	Start(ctx context.Context) error
	Stop() error
	GetFinality(ctx context.Context) (*Finality, error)
	GetCommittees(ctx context.Context, slot phase0.Slot) ([]*Committee, error)
	GetBlockAttestations(ctx context.Context, slot phase0.Slot) ([]*Attestation, error)
	GetSignedBlockHeader(ctx context.Context, blockRoot phase0.Root) (*SignedBeaconBlockHeader, error)
	SubmitAttesterSlashing(ctx context.Context, slashing *AttesterSlashing) error
	SubmitProposerSlashing(ctx context.Context, slashing *ProposerSlashing) error
	SubscribeToHeads(ctx context.Context, handler func(*HeadEvent)) error
	SubscribeToBlocks(ctx context.Context, handler func(*BlockEvent)) error
	ResolveAttestingValidators(ctx context.Context, att *Attestation) ([]phase0.ValidatorIndex, error)
}

type node struct {
	endpoint string
	client   *http.Client
	stream   *Stream
	healthy  bool
	mu       sync.RWMutex
}

type committeeKey struct {
	slot  phase0.Slot
	index phase0.CommitteeIndex
}

type service struct {
	cfg   *Config
	log   logrus.FieldLogger
	nodes []*node

	committeeMu    sync.RWMutex
	committeeCache map[committeeKey][]phase0.ValidatorIndex

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a new beacon service.
func New(cfg *Config, log logrus.FieldLogger) Service {
	nodes := make([]*node, 0, len(cfg.Endpoints))

	for _, endpoint := range cfg.Endpoints {
		nodes = append(nodes, &node{
			endpoint: endpoint,
			client: &http.Client{
				Timeout: cfg.Timeout,
			},
			healthy: true,
		})
	}

	return &service{
		cfg:            cfg,
		log:            log.WithField("package", "beacon"),
		nodes:          nodes,
		committeeCache: make(map[committeeKey][]phase0.ValidatorIndex, 2048),
	}
}

// Start initializes the beacon service.
func (s *service) Start(ctx context.Context) error {
	if err := s.cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	go s.cleanupCommitteeCache(ctx)

	s.log.WithField("endpoints", s.cfg.Endpoints).Info("beacon service started")

	return nil
}

// Stop shuts down the beacon service.
func (s *service) Stop() error {
	if s.cancel != nil {
		s.cancel()
	}

	s.wg.Wait()

	for _, n := range s.nodes {
		if n.stream != nil {
			n.stream.Close()
		}
	}

	s.log.Info("beacon service stopped")

	return nil
}

func (s *service) cleanupCommitteeCache(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.committeeMu.Lock()

			if len(s.committeeCache) > 1000 {
				for key := range s.committeeCache {
					delete(s.committeeCache, key)

					if len(s.committeeCache) <= 500 {
						break
					}
				}
			}

			s.committeeMu.Unlock()
		}
	}
}

// GetFinality retrieves the current finality checkpoints.
func (s *service) GetFinality(ctx context.Context) (*Finality, error) {
	var lastErr error

	for _, n := range s.getHealthyNodes() {
		resp, err := s.doNodeRequest(ctx, n, "GET", "/eth/v1/beacon/states/head/finality_checkpoints", nil)
		if err != nil {
			lastErr = err
			s.markNodeUnhealthy(n)

			continue
		}

		defer resp.Body.Close()

		var result struct {
			Data struct {
				Finalized struct {
					Epoch string `json:"epoch"`
				} `json:"finalized"`
			} `json:"data"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			lastErr = fmt.Errorf("failed to decode finality response: %w", err)

			continue
		}

		finalizedEpoch, _ := parseUint64(result.Data.Finalized.Epoch)

		headResp, err := s.doNodeRequest(ctx, n, "GET", "/eth/v1/beacon/headers/head", nil)
		if err != nil {
			lastErr = err

			continue
		}

		defer headResp.Body.Close()

		var headResult struct {
			Data struct {
				Header struct {
					Message struct {
						Slot string `json:"slot"`
					} `json:"message"`
				} `json:"header"`
			} `json:"data"`
		}

		if err := json.NewDecoder(headResp.Body).Decode(&headResult); err != nil {
			lastErr = fmt.Errorf("failed to decode head response: %w", err)

			continue
		}

		headSlot, _ := parseUint64(headResult.Data.Header.Message.Slot)

		s.markNodeHealthy(n)

		return &Finality{
			FinalizedEpoch: phase0.Epoch(finalizedEpoch),
			FinalizedSlot:  phase0.Slot(finalizedEpoch * 32),
			HeadSlot:       phase0.Slot(headSlot),
		}, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}

	return nil, ErrAllNodesFailed
}

// GetCommittees retrieves beacon committees for a specific slot.
func (s *service) GetCommittees(ctx context.Context, slot phase0.Slot) ([]*Committee, error) {
	path := fmt.Sprintf("/eth/v1/beacon/states/head/committees?slot=%d", slot)

	var lastErr error

	for _, n := range s.getHealthyNodes() {
		resp, err := s.doNodeRequest(ctx, n, "GET", path, nil)
		if err != nil {
			lastErr = err
			s.markNodeUnhealthy(n)

			continue
		}

		defer resp.Body.Close()

		var result struct {
			Data []struct {
				Index      string   `json:"index"`
				Slot       string   `json:"slot"`
				Validators []string `json:"validators"`
			} `json:"data"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			lastErr = fmt.Errorf("failed to decode committees response: %w", err)

			continue
		}

		committees := make([]*Committee, 0, len(result.Data))

		for _, c := range result.Data {
			idx, _ := parseUint64(c.Index)
			cSlot, _ := parseUint64(c.Slot)

			validators := make([]phase0.ValidatorIndex, 0, len(c.Validators))
			for _, v := range c.Validators {
				vidx, _ := parseUint64(v)
				validators = append(validators, phase0.ValidatorIndex(vidx))
			}

			committee := &Committee{
				Index:      phase0.CommitteeIndex(idx),
				Slot:       phase0.Slot(cSlot),
				Validators: validators,
			}

			committees = append(committees, committee)

			s.cacheCommittee(committee)
		}

		return committees, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}

	return nil, ErrAllNodesFailed
}

func (s *service) cacheCommittee(c *Committee) {
	s.committeeMu.Lock()
	defer s.committeeMu.Unlock()

	key := committeeKey{slot: c.Slot, index: c.Index}
	s.committeeCache[key] = c.Validators
}

func (s *service) getCachedCommittee(
	slot phase0.Slot,
	index phase0.CommitteeIndex,
) []phase0.ValidatorIndex {
	s.committeeMu.RLock()
	defer s.committeeMu.RUnlock()

	return s.committeeCache[committeeKey{slot: slot, index: index}]
}

// GetBlockAttestations retrieves attestations from a specific block.
func (s *service) GetBlockAttestations(
	ctx context.Context,
	slot phase0.Slot,
) ([]*Attestation, error) {
	path := fmt.Sprintf("/eth/v1/beacon/blocks/%d/attestations", slot)

	var lastErr error

	for _, n := range s.getHealthyNodes() {
		resp, err := s.doNodeRequest(ctx, n, "GET", path, nil)
		if err != nil {
			lastErr = err
			s.markNodeUnhealthy(n)

			continue
		}

		defer resp.Body.Close()

		if resp.StatusCode == http.StatusNotFound {
			return nil, nil
		}

		var result struct {
			Data []json.RawMessage `json:"data"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			lastErr = fmt.Errorf("failed to decode attestations response: %w", err)

			continue
		}

		attestations := make([]*Attestation, 0, len(result.Data))

		for _, raw := range result.Data {
			att, err := s.parseAttestation(raw)
			if err != nil {
				s.log.WithError(err).Debug("failed to parse attestation")

				continue
			}

			attestations = append(attestations, att)
		}

		return attestations, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}

	return nil, ErrAllNodesFailed
}

// GetSignedBlockHeader retrieves a signed block header by block root.
func (s *service) GetSignedBlockHeader(
	ctx context.Context,
	blockRoot phase0.Root,
) (*SignedBeaconBlockHeader, error) {
	path := fmt.Sprintf("/eth/v1/beacon/headers/0x%x", blockRoot)

	var lastErr error

	for _, n := range s.getHealthyNodes() {
		resp, err := s.doNodeRequest(ctx, n, "GET", path, nil)
		if err != nil {
			lastErr = err
			s.markNodeUnhealthy(n)

			continue
		}

		defer resp.Body.Close()

		if resp.StatusCode == http.StatusNotFound {
			return nil, nil
		}

		var result struct {
			Data struct {
				Header struct {
					Message struct {
						Slot          string `json:"slot"`
						ProposerIndex string `json:"proposer_index"`
						ParentRoot    string `json:"parent_root"`
						StateRoot     string `json:"state_root"`
						BodyRoot      string `json:"body_root"`
					} `json:"message"`
					Signature string `json:"signature"`
				} `json:"header"`
			} `json:"data"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			lastErr = fmt.Errorf("failed to decode header response: %w", err)

			continue
		}

		slot, _ := parseUint64(result.Data.Header.Message.Slot)
		proposerIndex, _ := parseUint64(result.Data.Header.Message.ProposerIndex)
		parentRoot, _ := parseRoot(result.Data.Header.Message.ParentRoot)
		stateRoot, _ := parseRoot(result.Data.Header.Message.StateRoot)
		bodyRoot, _ := parseRoot(result.Data.Header.Message.BodyRoot)
		sig, _ := parseSignature(result.Data.Header.Signature)

		s.markNodeHealthy(n)

		return &SignedBeaconBlockHeader{
			Message: &BeaconBlockHeader{
				Slot:          phase0.Slot(slot),
				ProposerIndex: phase0.ValidatorIndex(proposerIndex),
				ParentRoot:    parentRoot,
				StateRoot:     stateRoot,
				BodyRoot:      bodyRoot,
			},
			Signature: sig,
		}, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}

	return nil, ErrAllNodesFailed
}

// ResolveAttestingValidators resolves the attesting validators for an attestation.
func (s *service) ResolveAttestingValidators(
	ctx context.Context,
	att *Attestation,
) ([]phase0.ValidatorIndex, error) {
	committee := s.getCachedCommittee(att.Data.Slot, att.Data.Index)

	if committee == nil {
		if _, err := s.GetCommittees(ctx, att.Data.Slot); err != nil {
			return nil, fmt.Errorf("failed to fetch committees: %w", err)
		}

		committee = s.getCachedCommittee(att.Data.Slot, att.Data.Index)
		if committee == nil {
			return nil, fmt.Errorf("committee not found for slot %d index %d", att.Data.Slot, att.Data.Index)
		}
	}

	validators := make([]phase0.ValidatorIndex, 0, len(committee))

	for i, validatorIdx := range committee {
		if i >= len(att.AggregationBits)*8 {
			break
		}

		byteIdx := i / 8
		bitIdx := i % 8

		if att.AggregationBits[byteIdx]&(1<<bitIdx) != 0 {
			validators = append(validators, validatorIdx)
		}
	}

	return validators, nil
}

// SubmitAttesterSlashing submits an attester slashing to all beacon nodes.
func (s *service) SubmitAttesterSlashing(ctx context.Context, slashing *AttesterSlashing) error {
	body, err := s.marshalSlashing(slashing)
	if err != nil {
		return fmt.Errorf("failed to marshal slashing: %w", err)
	}

	var (
		successCount int
		lastErr      error
	)

	for _, n := range s.nodes {
		resp, err := s.doNodeRequest(ctx, n, "POST", "/eth/v1/beacon/pool/attester_slashings", body)
		if err != nil {
			lastErr = err

			continue
		}

		resp.Body.Close()

		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted {
			successCount++

			s.log.WithField("endpoint", n.endpoint).Debug("submitted slashing to node")
		} else {
			s.log.WithFields(logrus.Fields{
				"endpoint": n.endpoint,
				"status":   resp.StatusCode,
			}).Warn("failed to submit slashing")
		}
	}

	if successCount == 0 {
		if lastErr != nil {
			return lastErr
		}

		return ErrAllNodesFailed
	}

	s.log.WithField("nodes", successCount).Info("submitted slashing to beacon nodes")

	return nil
}

func (s *service) marshalSlashing(slashing *AttesterSlashing) ([]byte, error) {
	type apiAttestation struct {
		AttestingIndices []string `json:"attesting_indices"`
		Data             struct {
			Slot            string `json:"slot"`
			Index           string `json:"index"`
			BeaconBlockRoot string `json:"beacon_block_root"`
			Source          struct {
				Epoch string `json:"epoch"`
				Root  string `json:"root"`
			} `json:"source"`
			Target struct {
				Epoch string `json:"epoch"`
				Root  string `json:"root"`
			} `json:"target"`
		} `json:"data"`
		Signature string `json:"signature"`
	}

	toAPI := func(att *IndexedAttestation) apiAttestation {
		indices := make([]string, 0, len(att.AttestingIndices))
		for _, idx := range att.AttestingIndices {
			indices = append(indices, fmt.Sprintf("%d", idx))
		}

		return apiAttestation{
			AttestingIndices: indices,
			Data: struct {
				Slot            string `json:"slot"`
				Index           string `json:"index"`
				BeaconBlockRoot string `json:"beacon_block_root"`
				Source          struct {
					Epoch string `json:"epoch"`
					Root  string `json:"root"`
				} `json:"source"`
				Target struct {
					Epoch string `json:"epoch"`
					Root  string `json:"root"`
				} `json:"target"`
			}{
				Slot:            fmt.Sprintf("%d", att.Data.Slot),
				Index:           fmt.Sprintf("%d", att.Data.Index),
				BeaconBlockRoot: fmt.Sprintf("0x%x", att.Data.BeaconBlockRoot),
				Source: struct {
					Epoch string `json:"epoch"`
					Root  string `json:"root"`
				}{
					Epoch: fmt.Sprintf("%d", att.Data.Source.Epoch),
					Root:  fmt.Sprintf("0x%x", att.Data.Source.Root),
				},
				Target: struct {
					Epoch string `json:"epoch"`
					Root  string `json:"root"`
				}{
					Epoch: fmt.Sprintf("%d", att.Data.Target.Epoch),
					Root:  fmt.Sprintf("0x%x", att.Data.Target.Root),
				},
			},
			Signature: fmt.Sprintf("0x%x", att.Signature),
		}
	}

	apiSlashing := struct {
		Attestation1 apiAttestation `json:"attestation_1"`
		Attestation2 apiAttestation `json:"attestation_2"`
	}{
		Attestation1: toAPI(slashing.Attestation1),
		Attestation2: toAPI(slashing.Attestation2),
	}

	return json.Marshal(apiSlashing)
}

// SubmitProposerSlashing submits a proposer slashing to all beacon nodes.
func (s *service) SubmitProposerSlashing(ctx context.Context, slashing *ProposerSlashing) error {
	body, err := s.marshalProposerSlashing(slashing)
	if err != nil {
		return fmt.Errorf("failed to marshal proposer slashing: %w", err)
	}

	var (
		successCount int
		lastErr      error
	)

	for _, n := range s.nodes {
		resp, err := s.doNodeRequest(ctx, n, "POST", "/eth/v1/beacon/pool/proposer_slashings", body)
		if err != nil {
			lastErr = err

			continue
		}

		resp.Body.Close()

		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted {
			successCount++

			s.log.WithField("endpoint", n.endpoint).Debug("submitted proposer slashing to node")
		} else {
			s.log.WithFields(logrus.Fields{
				"endpoint": n.endpoint,
				"status":   resp.StatusCode,
			}).Warn("failed to submit proposer slashing")
		}
	}

	if successCount == 0 {
		if lastErr != nil {
			return lastErr
		}

		return ErrAllNodesFailed
	}

	s.log.WithField("nodes", successCount).Info("submitted proposer slashing to beacon nodes")

	return nil
}

func (s *service) marshalProposerSlashing(slashing *ProposerSlashing) ([]byte, error) {
	type apiHeader struct {
		Message struct {
			Slot          string `json:"slot"`
			ProposerIndex string `json:"proposer_index"`
			ParentRoot    string `json:"parent_root"`
			StateRoot     string `json:"state_root"`
			BodyRoot      string `json:"body_root"`
		} `json:"message"`
		Signature string `json:"signature"`
	}

	toAPI := func(h *SignedBeaconBlockHeader) apiHeader {
		return apiHeader{
			Message: struct {
				Slot          string `json:"slot"`
				ProposerIndex string `json:"proposer_index"`
				ParentRoot    string `json:"parent_root"`
				StateRoot     string `json:"state_root"`
				BodyRoot      string `json:"body_root"`
			}{
				Slot:          fmt.Sprintf("%d", h.Message.Slot),
				ProposerIndex: fmt.Sprintf("%d", h.Message.ProposerIndex),
				ParentRoot:    fmt.Sprintf("0x%x", h.Message.ParentRoot),
				StateRoot:     fmt.Sprintf("0x%x", h.Message.StateRoot),
				BodyRoot:      fmt.Sprintf("0x%x", h.Message.BodyRoot),
			},
			Signature: fmt.Sprintf("0x%x", h.Signature),
		}
	}

	apiSlashing := struct {
		SignedHeader1 apiHeader `json:"signed_header_1"`
		SignedHeader2 apiHeader `json:"signed_header_2"`
	}{
		SignedHeader1: toAPI(slashing.SignedHeader1),
		SignedHeader2: toAPI(slashing.SignedHeader2),
	}

	return json.Marshal(apiSlashing)
}

// SubscribeToHeads subscribes to head events from all beacon nodes.
func (s *service) SubscribeToHeads(
	ctx context.Context,
	handler func(*HeadEvent),
) error {
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	seen := newHeadDeduplicator()

	wrappedHandler := func(event *HeadEvent) {
		if seen.isDuplicate(event) {
			return
		}

		handler(event)
	}

	for _, n := range s.nodes {
		n := n

		s.wg.Add(1)

		go func() {
			defer s.wg.Done()

			s.subscribeNode(ctx, n, wrappedHandler)
		}()
	}

	<-ctx.Done()

	return ctx.Err()
}

func (s *service) subscribeNode(
	ctx context.Context,
	n *node,
	handler func(*HeadEvent),
) {
	log := s.log.WithField("endpoint", n.endpoint)

	n.stream = NewStream(n.endpoint, log)

	err := n.stream.Subscribe(ctx, "head", func(data []byte) {
		event, err := s.parseHeadEvent(data)
		if err != nil {
			log.WithError(err).Debug("failed to parse head event")

			return
		}

		handler(event)
	})

	if err != nil && ctx.Err() == nil {
		log.WithError(err).Warn("head subscription ended")
	}
}

func (s *service) parseHeadEvent(data []byte) (*HeadEvent, error) {
	var event struct {
		Slot  string `json:"slot"`
		Block string `json:"block"`
	}

	if err := json.Unmarshal(data, &event); err != nil {
		return nil, fmt.Errorf("failed to unmarshal head event: %w", err)
	}

	slot, _ := parseUint64(event.Slot)
	block, _ := parseRoot(event.Block)

	return &HeadEvent{
		Slot:  phase0.Slot(slot),
		Block: block,
	}, nil
}

// SubscribeToBlocks subscribes to block events from all beacon nodes.
// Block events are broadcast immediately when received, before potential reorgs.
func (s *service) SubscribeToBlocks(
	ctx context.Context,
	handler func(*BlockEvent),
) error {
	for _, n := range s.nodes {
		n := n

		s.wg.Add(1)

		go func() {
			defer s.wg.Done()

			s.subscribeNodeBlocks(ctx, n, handler)
		}()
	}

	<-ctx.Done()

	return ctx.Err()
}

func (s *service) subscribeNodeBlocks(
	ctx context.Context,
	n *node,
	handler func(*BlockEvent),
) {
	log := s.log.WithField("endpoint", n.endpoint)

	stream := NewStream(n.endpoint, log)

	err := stream.Subscribe(ctx, "block", func(data []byte) {
		event, err := s.parseBlockEvent(data)
		if err != nil {
			log.WithError(err).Debug("failed to parse block event")

			return
		}

		handler(event)
	})

	if err != nil && ctx.Err() == nil {
		log.WithError(err).Warn("block subscription ended")
	}
}

func (s *service) parseBlockEvent(data []byte) (*BlockEvent, error) {
	var event struct {
		Slot                string `json:"slot"`
		Block               string `json:"block"`
		ProposerIndex       string `json:"proposer_index"`
		ExecutionOptimistic bool   `json:"execution_optimistic"`
	}

	if err := json.Unmarshal(data, &event); err != nil {
		return nil, fmt.Errorf("failed to unmarshal block event: %w", err)
	}

	slot, _ := parseUint64(event.Slot)
	block, _ := parseRoot(event.Block)
	proposerIndex, _ := parseUint64(event.ProposerIndex)

	return &BlockEvent{
		Slot:          phase0.Slot(slot),
		Block:         block,
		ProposerIndex: phase0.ValidatorIndex(proposerIndex),
	}, nil
}

func (s *service) getHealthyNodes() []*node {
	healthy := make([]*node, 0, len(s.nodes))

	for _, n := range s.nodes {
		n.mu.RLock()
		isHealthy := n.healthy
		n.mu.RUnlock()

		if isHealthy {
			healthy = append(healthy, n)
		}
	}

	if len(healthy) == 0 {
		return s.nodes
	}

	return healthy
}

func (s *service) markNodeUnhealthy(n *node) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.healthy {
		n.healthy = false
		s.log.WithField("endpoint", n.endpoint).Warn("beacon node marked unhealthy")
	}
}

func (s *service) markNodeHealthy(n *node) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if !n.healthy {
		n.healthy = true
		s.log.WithField("endpoint", n.endpoint).Info("beacon node marked healthy")
	}
}

func (s *service) doNodeRequest(
	ctx context.Context,
	n *node,
	method, path string,
	body []byte,
) (*http.Response, error) {
	url := n.endpoint + path

	var bodyReader io.Reader
	if body != nil {
		bodyReader = &readCloser{data: body}
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	return resp, nil
}

func (s *service) parseAttestation(raw json.RawMessage) (*Attestation, error) {
	var att struct {
		AggregationBits string `json:"aggregation_bits"`
		Data            struct {
			Slot            string `json:"slot"`
			Index           string `json:"index"`
			BeaconBlockRoot string `json:"beacon_block_root"`
			Source          struct {
				Epoch string `json:"epoch"`
				Root  string `json:"root"`
			} `json:"source"`
			Target struct {
				Epoch string `json:"epoch"`
				Root  string `json:"root"`
			} `json:"target"`
		} `json:"data"`
		Signature string `json:"signature"`
	}

	if err := json.Unmarshal(raw, &att); err != nil {
		return nil, fmt.Errorf("failed to unmarshal attestation: %w", err)
	}

	slot, _ := parseUint64(att.Data.Slot)
	index, _ := parseUint64(att.Data.Index)
	sourceEpoch, _ := parseUint64(att.Data.Source.Epoch)
	targetEpoch, _ := parseUint64(att.Data.Target.Epoch)

	beaconBlockRoot, _ := parseRoot(att.Data.BeaconBlockRoot)
	sourceRoot, _ := parseRoot(att.Data.Source.Root)
	targetRoot, _ := parseRoot(att.Data.Target.Root)
	sig, _ := parseSignature(att.Signature)
	aggBits, _ := parseAggregationBits(att.AggregationBits)

	return &Attestation{
		AggregationBits: aggBits,
		Data: &AttestationData{
			Slot:            phase0.Slot(slot),
			Index:           phase0.CommitteeIndex(index),
			BeaconBlockRoot: beaconBlockRoot,
			Source: &Checkpoint{
				Epoch: phase0.Epoch(sourceEpoch),
				Root:  sourceRoot,
			},
			Target: &Checkpoint{
				Epoch: phase0.Epoch(targetEpoch),
				Root:  targetRoot,
			},
		},
		Signature: sig,
	}, nil
}

func parseAggregationBits(s string) ([]byte, error) {
	if len(s) < 2 {
		return nil, errors.New("aggregation bits too short")
	}

	if s[:2] == "0x" {
		s = s[2:]
	}

	return hex.DecodeString(s)
}

type readCloser struct {
	data   []byte
	offset int
}

func (r *readCloser) Read(p []byte) (int, error) {
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}

	n := copy(p, r.data[r.offset:])
	r.offset += n

	return n, nil
}

func parseUint64(s string) (uint64, error) {
	var v uint64

	_, err := fmt.Sscanf(s, "%d", &v)

	return v, err
}

func parseRoot(s string) (phase0.Root, error) {
	var root phase0.Root

	if len(s) < 2 {
		return root, errors.New("root too short")
	}

	if s[:2] == "0x" {
		s = s[2:]
	}

	if len(s) != 64 {
		return root, errors.New("invalid root length")
	}

	bytes, err := hex.DecodeString(s)
	if err != nil {
		return root, err
	}

	copy(root[:], bytes)

	return root, nil
}

func parseSignature(s string) (phase0.BLSSignature, error) {
	var sig phase0.BLSSignature

	if len(s) < 2 {
		return sig, errors.New("signature too short")
	}

	if s[:2] == "0x" {
		s = s[2:]
	}

	if len(s) != 192 {
		return sig, errors.New("invalid signature length")
	}

	bytes, err := hex.DecodeString(s)
	if err != nil {
		return sig, err
	}

	copy(sig[:], bytes)

	return sig, nil
}

// headDeduplicator tracks seen head events to prevent duplicates from multiple nodes.
type headDeduplicator struct {
	mu   sync.Mutex
	seen map[phase0.Slot]time.Time
}

func newHeadDeduplicator() *headDeduplicator {
	d := &headDeduplicator{
		seen: make(map[phase0.Slot]time.Time, 64),
	}

	go d.cleanup()

	return d
}

func (d *headDeduplicator) isDuplicate(event *HeadEvent) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, exists := d.seen[event.Slot]; exists {
		return true
	}

	d.seen[event.Slot] = time.Now()

	return false
}

func (d *headDeduplicator) cleanup() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		d.mu.Lock()

		cutoff := time.Now().Add(-2 * time.Minute)

		for slot, ts := range d.seen {
			if ts.Before(cutoff) {
				delete(d.seen, slot)
			}
		}

		d.mu.Unlock()
	}
}
