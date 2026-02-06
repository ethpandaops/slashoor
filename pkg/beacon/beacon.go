package beacon

import (
	"context"
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

// Service defines the interface for beacon node interactions.
type Service interface {
	Start(ctx context.Context) error
	Stop() error
	GetGenesisTime(ctx context.Context) (time.Time, error)
	GetCurrentSlot(ctx context.Context) (phase0.Slot, error)
	GetAttestations(ctx context.Context, slot phase0.Slot) ([]*IndexedAttestation, error)
	SubmitAttesterSlashing(ctx context.Context, slashing *AttesterSlashing) error
	SubscribeToAttestations(ctx context.Context, handler func(*IndexedAttestation)) error
}

type node struct {
	endpoint string
	client   *http.Client
	stream   *Stream
	healthy  bool
	mu       sync.RWMutex
}

type service struct {
	cfg   *Config
	log   logrus.FieldLogger
	nodes []*node

	mu     sync.RWMutex
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
		cfg:   cfg,
		log:   log.WithField("package", "beacon"),
		nodes: nodes,
	}
}

// Start initializes the beacon service.
func (s *service) Start(ctx context.Context) error {
	if err := s.cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

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

// GetGenesisTime retrieves the genesis time from the beacon node.
func (s *service) GetGenesisTime(ctx context.Context) (time.Time, error) {
	var lastErr error

	for _, n := range s.getHealthyNodes() {
		resp, err := s.doNodeRequest(ctx, n, "GET", "/eth/v1/beacon/genesis", nil)
		if err != nil {
			lastErr = err
			s.markNodeUnhealthy(n)

			continue
		}

		defer resp.Body.Close()

		var result struct {
			Data struct {
				GenesisTime string `json:"genesis_time"`
			} `json:"data"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			lastErr = fmt.Errorf("failed to decode genesis response: %w", err)

			continue
		}

		genesisTime, err := time.Parse(time.RFC3339, result.Data.GenesisTime)
		if err != nil {
			var unixTime int64
			if _, parseErr := fmt.Sscanf(result.Data.GenesisTime, "%d", &unixTime); parseErr == nil {
				return time.Unix(unixTime, 0), nil
			}

			lastErr = fmt.Errorf("failed to parse genesis time: %w", err)

			continue
		}

		return genesisTime, nil
	}

	if lastErr != nil {
		return time.Time{}, lastErr
	}

	return time.Time{}, ErrAllNodesFailed
}

// GetCurrentSlot retrieves the current slot from the beacon node.
func (s *service) GetCurrentSlot(ctx context.Context) (phase0.Slot, error) {
	var lastErr error

	for _, n := range s.getHealthyNodes() {
		resp, err := s.doNodeRequest(ctx, n, "GET", "/eth/v1/beacon/headers/head", nil)
		if err != nil {
			lastErr = err
			s.markNodeUnhealthy(n)

			continue
		}

		defer resp.Body.Close()

		var result struct {
			Data struct {
				Header struct {
					Message struct {
						Slot string `json:"slot"`
					} `json:"message"`
				} `json:"header"`
			} `json:"data"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			lastErr = fmt.Errorf("failed to decode header response: %w", err)

			continue
		}

		var slot uint64
		if _, err := fmt.Sscanf(result.Data.Header.Message.Slot, "%d", &slot); err != nil {
			lastErr = fmt.Errorf("failed to parse slot: %w", err)

			continue
		}

		return phase0.Slot(slot), nil
	}

	if lastErr != nil {
		return 0, lastErr
	}

	return 0, ErrAllNodesFailed
}

// GetAttestations retrieves attestations from a specific slot.
func (s *service) GetAttestations(
	ctx context.Context,
	slot phase0.Slot,
) ([]*IndexedAttestation, error) {
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

		attestations := make([]*IndexedAttestation, 0, len(result.Data))

		for _, raw := range result.Data {
			att, err := s.parseAttestation(raw)
			if err != nil {
				s.log.WithError(err).Warn("failed to parse attestation")

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

// SubmitAttesterSlashing submits an attester slashing to all beacon nodes.
func (s *service) SubmitAttesterSlashing(ctx context.Context, slashing *AttesterSlashing) error {
	body, err := json.Marshal(slashing)
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

// SubscribeToAttestations subscribes to attestation events from all beacon nodes.
func (s *service) SubscribeToAttestations(
	ctx context.Context,
	handler func(*IndexedAttestation),
) error {
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	seen := newAttestationDeduplicator()

	wrappedHandler := func(att *IndexedAttestation) {
		if seen.isDuplicate(att) {
			return
		}

		handler(att)
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
	handler func(*IndexedAttestation),
) {
	log := s.log.WithField("endpoint", n.endpoint)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n.stream = NewStream(n.endpoint, s.log)

		err := n.stream.Subscribe(ctx, "attestation", func(data []byte) {
			att, err := s.parseAttestationEvent(data)
			if err != nil {
				log.WithError(err).Debug("failed to parse attestation event")

				return
			}

			handler(att)
		})

		if err != nil && ctx.Err() == nil {
			log.WithError(err).Warn("SSE subscription ended, reconnecting")
			s.markNodeUnhealthy(n)

			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
				s.markNodeHealthy(n)
			}
		}
	}
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

func (s *service) parseAttestation(raw json.RawMessage) (*IndexedAttestation, error) {
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

	return &IndexedAttestation{
		AttestingIndices: []phase0.ValidatorIndex{},
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

func (s *service) parseAttestationEvent(data []byte) (*IndexedAttestation, error) {
	return s.parseAttestation(data)
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

	s = s[2:]
	if len(s) != 64 {
		return root, errors.New("invalid root length")
	}

	for i := 0; i < 32; i++ {
		var b byte

		_, err := fmt.Sscanf(s[i*2:i*2+2], "%02x", &b)
		if err != nil {
			return root, err
		}

		root[i] = b
	}

	return root, nil
}

func parseSignature(s string) (phase0.BLSSignature, error) {
	var sig phase0.BLSSignature

	if len(s) < 2 {
		return sig, errors.New("signature too short")
	}

	s = s[2:]
	if len(s) != 192 {
		return sig, errors.New("invalid signature length")
	}

	for i := 0; i < 96; i++ {
		var b byte

		_, err := fmt.Sscanf(s[i*2:i*2+2], "%02x", &b)
		if err != nil {
			return sig, err
		}

		sig[i] = b
	}

	return sig, nil
}

// attestationDeduplicator tracks seen attestations to prevent duplicates from multiple nodes.
type attestationDeduplicator struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func newAttestationDeduplicator() *attestationDeduplicator {
	d := &attestationDeduplicator{
		seen: make(map[string]time.Time, 4096),
	}

	go d.cleanup()

	return d
}

func (d *attestationDeduplicator) isDuplicate(att *IndexedAttestation) bool {
	key := fmt.Sprintf("%d:%d:%d:%x",
		att.Data.Slot,
		att.Data.Source.Epoch,
		att.Data.Target.Epoch,
		att.Signature[:8],
	)

	d.mu.Lock()
	defer d.mu.Unlock()

	if _, exists := d.seen[key]; exists {
		return true
	}

	d.seen[key] = time.Now()

	return false
}

func (d *attestationDeduplicator) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		d.mu.Lock()

		cutoff := time.Now().Add(-10 * time.Minute)

		for key, ts := range d.seen {
			if ts.Before(cutoff) {
				delete(d.seen, key)
			}
		}

		d.mu.Unlock()
	}
}
