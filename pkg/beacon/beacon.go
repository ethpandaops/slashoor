package beacon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
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

	ErrMissingBlockState  = errors.New("attestation has no block state to resolve committees from")
	ErrSlashingRejected   = errors.New("slashing rejected by beacon node")
	ErrPreviouslyRejected = errors.New("slashing was already rejected")
	ErrAlreadySubmitted   = errors.New("slashing was already submitted")
	ErrPQAttesterSlashing = errors.New("pq attester slashings need a per-attestation proof")
	errNotFound           = errors.New("not found")
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
// For Electra and later, AggregationBits concatenates the participation bits
// of every committee flagged in CommitteeBits (EIP-7549); CommitteeBits is
// empty for pre-Electra attestations, where Data.Index selects the committee.
type Attestation struct {
	AggregationBits []byte
	CommitteeBits   []byte
	Data            *AttestationData
	Signature       phase0.BLSSignature
	// Version is the consensus fork name of the block the attestation was
	// read from, as reported by the beacon API (e.g. "electra").
	Version   string
	BlockRoot phase0.Root
	StateRoot phase0.Root
	ServedBy  string
}

// IndexedAttestation represents an attestation with validator indices.
type IndexedAttestation struct {
	AttestingIndices []phase0.ValidatorIndex
	Data             *AttestationData
	Signature        phase0.BLSSignature
	Proof            []byte
}

// AttesterSlashing represents a slashing proof for double/surround voting.
type AttesterSlashing struct {
	Attestation1 *IndexedAttestation
	Attestation2 *IndexedAttestation
	// Version is the consensus fork name governing the slashing's container
	// shape, used for the Eth-Consensus-Version header on submission.
	Version string
}

// forkOrder lists consensus fork names oldest-first. Unknown names rank
// newest, so future forks keep the Electra-shaped containers and endpoints
// until this list says otherwise.
var forkOrder = []string{"phase0", "altair", "bellatrix", "capella", "deneb", "electra", "fulu", "gloas", "pq"}

func forkRank(version string) int {
	for i, name := range forkOrder {
		if name == version {
			return i
		}
	}

	return len(forkOrder)
}

// IsPQ reports whether a fork name is pq or later.
func IsPQ(version string) bool {
	return forkRank(version) >= forkRank("pq")
}

// NewerForkVersion returns whichever of the two fork names is more recent.
func NewerForkVersion(a, b string) string {
	if forkRank(b) > forkRank(a) {
		return b
	}

	return a
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
	Signature []byte
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
	GetBlockAttestations(ctx context.Context, slot phase0.Slot) ([]*Attestation, error)
	GetBlockAttestationsByRoot(ctx context.Context, blockRoot phase0.Root) ([]*Attestation, error)
	GetSignedBlockHeader(ctx context.Context, blockRoot phase0.Root) (*SignedBeaconBlockHeader, error)
	SubmitAttesterSlashing(ctx context.Context, slashing *AttesterSlashing) error
	SubmitProposerSlashing(ctx context.Context, slashing *ProposerSlashing) error
	SubscribeToHeads(ctx context.Context, handler func(*HeadEvent)) error
	SubscribeToBlocks(ctx context.Context, handler func(*BlockEvent)) error
	ResolveAttestingValidators(ctx context.Context, att *Attestation) ([]phase0.ValidatorIndex, error)
}

type node struct {
	endpoint string
	// display is the endpoint with any userinfo stripped — endpoints can
	// carry basic-auth credentials, so only display may ever be logged.
	display string
	client  *http.Client
	stream  *Stream
	healthy bool
	mu      sync.RWMutex
}

// redactEndpoint strips userinfo from an endpoint URL for safe logging.
func redactEndpoint(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		// Unparseable: fall back to dropping anything before an '@'.
		if i := strings.LastIndex(endpoint, "@"); i >= 0 {
			return "<redacted>@" + endpoint[i+1:]
		}

		return endpoint
	}

	u.User = nil

	return u.String()
}

type committeeKey struct {
	stateRoot phase0.Root
	slot      phase0.Slot
	index     phase0.CommitteeIndex
}

type submissionOutcome int

const (
	submissionAccepted submissionOutcome = iota + 1
	submissionRejected
)

const maxRememberedSubmissions = 4096

type service struct {
	cfg   *Config
	log   logrus.FieldLogger
	nodes []*node

	committeeMu    sync.RWMutex
	committeeCache map[committeeKey][]phase0.ValidatorIndex

	submissionMu sync.Mutex
	submissions  map[[32]byte]submissionOutcome

	// slotsPerEpoch is read from /eth/v1/config/spec at startup; written only
	// in Start, before any concurrent readers exist.
	slotsPerEpoch uint64

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a new beacon service.
func New(cfg *Config, log logrus.FieldLogger) Service {
	nodes := make([]*node, 0, len(cfg.Endpoints))

	for _, endpoint := range cfg.Endpoints {
		nodes = append(nodes, &node{
			endpoint: endpoint,
			display:  redactEndpoint(endpoint),
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
		submissions:    make(map[[32]byte]submissionOutcome, 256),
		slotsPerEpoch:  32,
	}
}

// Start initializes the beacon service.
func (s *service) Start(ctx context.Context) error {
	if err := s.cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	s.fetchSpec(ctx)

	go s.cleanupCommitteeCache(ctx)

	displays := make([]string, 0, len(s.nodes))
	for _, n := range s.nodes {
		displays = append(displays, n.display)
	}

	s.log.WithFields(logrus.Fields{
		"endpoints":       displays,
		"slots_per_epoch": s.slotsPerEpoch,
	}).Info("beacon service started")

	return nil
}

// fetchSpec reads chain parameters from the beacon node. Failures are
// non-fatal; mainnet defaults are kept.
func (s *service) fetchSpec(ctx context.Context) {
	for _, n := range s.getHealthyNodes() {
		resp, err := s.doNodeRequest(ctx, n, "GET", "/eth/v1/config/spec", nil, nil)
		if err != nil {
			continue
		}

		var result struct {
			Data struct {
				SlotsPerEpoch string `json:"SLOTS_PER_EPOCH"`
			} `json:"data"`
		}

		err = json.NewDecoder(resp.Body).Decode(&result)

		if closeErr := resp.Body.Close(); closeErr != nil {
			s.log.WithError(closeErr).Debug("failed to close spec response body")
		}

		if err != nil {
			continue
		}

		if v, err := parseUint64(result.Data.SlotsPerEpoch); err == nil && v > 0 {
			s.slotsPerEpoch = v

			return
		}
	}

	s.log.Warn("failed to fetch chain spec, using default SLOTS_PER_EPOCH=32")
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

// GetFinality retrieves the current finality checkpoints, taking the most
// advanced head across all healthy nodes so a lagging node cannot anchor the
// view of the chain behind the real head.
func (s *service) GetFinality(ctx context.Context) (*Finality, error) {
	var (
		lastErr error
		best    *Finality
	)

	for _, n := range s.getHealthyNodes() {
		resp, err := s.doNodeRequest(ctx, n, "GET", "/eth/v1/beacon/states/head/finality_checkpoints", nil, nil)
		if err != nil {
			lastErr = err
			s.markNodeUnhealthy(n)

			continue
		}

		var result struct {
			Data struct {
				Finalized struct {
					Epoch string `json:"epoch"`
				} `json:"finalized"`
			} `json:"data"`
		}

		err = json.NewDecoder(resp.Body).Decode(&result)

		if closeErr := resp.Body.Close(); closeErr != nil {
			s.log.WithError(closeErr).Debug("failed to close finality response body")
		}

		if err != nil {
			lastErr = fmt.Errorf("failed to decode finality response: %w", err)

			continue
		}

		finalizedEpoch, _ := parseUint64(result.Data.Finalized.Epoch)

		headResp, err := s.doNodeRequest(ctx, n, "GET", "/eth/v1/beacon/headers/head", nil, nil)
		if err != nil {
			lastErr = err

			continue
		}

		var headResult struct {
			Data struct {
				Header struct {
					Message struct {
						Slot string `json:"slot"`
					} `json:"message"`
				} `json:"header"`
			} `json:"data"`
		}

		err = json.NewDecoder(headResp.Body).Decode(&headResult)

		if closeErr := headResp.Body.Close(); closeErr != nil {
			s.log.WithError(closeErr).Debug("failed to close head response body")
		}

		if err != nil {
			lastErr = fmt.Errorf("failed to decode head response: %w", err)

			continue
		}

		headSlot, _ := parseUint64(headResult.Data.Header.Message.Slot)

		s.markNodeHealthy(n)

		// Keep the most advanced view: a stuck or lagging node would otherwise
		// anchor backfill thousands of slots behind the real head.
		if best == nil || phase0.Slot(headSlot) > best.HeadSlot {
			best = &Finality{
				FinalizedEpoch: phase0.Epoch(finalizedEpoch),
				FinalizedSlot:  phase0.Slot(finalizedEpoch * s.slotsPerEpoch),
				HeadSlot:       phase0.Slot(headSlot),
			}
		}
	}

	if best != nil {
		return best, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}

	return nil, ErrAllNodesFailed
}

func (s *service) fetchStateCommittees(ctx context.Context, att *Attestation) error {
	epoch := uint64(att.Data.Slot) / s.slotsPerEpoch
	path := fmt.Sprintf("/eth/v1/beacon/states/0x%x/committees?epoch=%d", att.StateRoot, epoch)

	var lastErr error

	for _, n := range s.nodesPreferring(att.ServedBy) {
		committees, err := s.getCommitteesFrom(ctx, n, path)
		if err != nil {
			lastErr = err

			continue
		}

		for _, c := range committees {
			s.cacheCommittee(att.StateRoot, c)
		}

		return nil
	}

	if lastErr != nil {
		return lastErr
	}

	return ErrAllNodesFailed
}

func (s *service) nodesPreferring(endpoint string) []*node {
	healthy := s.getHealthyNodes()
	ordered := make([]*node, 0, len(healthy))

	for _, n := range healthy {
		if n.endpoint == endpoint {
			ordered = append(ordered, n)
		}
	}

	for _, n := range healthy {
		if n.endpoint != endpoint {
			ordered = append(ordered, n)
		}
	}

	return ordered
}

func (s *service) getCommitteesFrom(
	ctx context.Context,
	n *node,
	path string,
) ([]*Committee, error) {
	resp, err := s.doNodeRequest(ctx, n, "GET", path, nil, nil)
	if err != nil {
		s.markNodeUnhealthy(n)

		return nil, err
	}

	defer func() {
		if err := resp.Body.Close(); err != nil {
			s.log.WithError(err).Debug("failed to close committees response body")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: committees request returned status %d", ErrRequestFailed, resp.StatusCode)
	}

	var result struct {
		Data []struct {
			Index      string   `json:"index"`
			Slot       string   `json:"slot"`
			Validators []string `json:"validators"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode committees response: %w", err)
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

		committees = append(committees, &Committee{
			Index:      phase0.CommitteeIndex(idx),
			Slot:       phase0.Slot(cSlot),
			Validators: validators,
		})
	}

	return committees, nil
}

func (s *service) cacheCommittee(stateRoot phase0.Root, c *Committee) {
	s.committeeMu.Lock()
	defer s.committeeMu.Unlock()

	s.committeeCache[committeeKey{stateRoot: stateRoot, slot: c.Slot, index: c.Index}] = c.Validators
}

func (s *service) getCachedCommittee(key committeeKey) []phase0.ValidatorIndex {
	s.committeeMu.RLock()
	defer s.committeeMu.RUnlock()

	return s.committeeCache[key]
}

// GetBlockAttestations retrieves the attestations of the block at a slot.
func (s *service) GetBlockAttestations(
	ctx context.Context,
	slot phase0.Slot,
) ([]*Attestation, error) {
	return s.blockAttestations(ctx, fmt.Sprintf("%d", slot))
}

// GetBlockAttestationsByRoot retrieves the attestations of a block by root.
func (s *service) GetBlockAttestationsByRoot(
	ctx context.Context,
	blockRoot phase0.Root,
) ([]*Attestation, error) {
	return s.blockAttestations(ctx, fmt.Sprintf("0x%x", blockRoot))
}

func (s *service) blockAttestations(ctx context.Context, blockID string) ([]*Attestation, error) {
	var (
		lastErr  error
		notFound bool
	)

	for _, n := range s.getHealthyNodes() {
		header, err := s.fetchHeader(ctx, n, blockID)
		if errors.Is(err, errNotFound) {
			notFound = true

			continue
		}

		if err != nil {
			lastErr = err

			continue
		}

		attestations, err := s.attestationsFrom(ctx, n, header.root)
		if errors.Is(err, errNotFound) {
			notFound = true

			continue
		}

		if err != nil {
			lastErr = err

			continue
		}

		for _, att := range attestations {
			att.BlockRoot = header.root
			att.StateRoot = header.signed.Message.StateRoot
			att.ServedBy = n.endpoint
		}

		return attestations, nil
	}

	if notFound {
		return nil, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}

	return nil, ErrAllNodesFailed
}

func (s *service) attestationsFrom(ctx context.Context, n *node, blockRoot phase0.Root) ([]*Attestation, error) {
	resp, err := s.doNodeRequest(ctx, n, "GET", fmt.Sprintf("/eth/v2/beacon/blocks/0x%x/attestations", blockRoot), nil, nil)
	if err != nil {
		s.markNodeUnhealthy(n)

		return nil, err
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, errNotFound
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: attestations request returned status %d", ErrRequestFailed, resp.StatusCode)
	}

	var result struct {
		Version string            `json:"version"`
		Data    []json.RawMessage `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode attestations response: %w", err)
	}

	attestations := make([]*Attestation, 0, len(result.Data))

	for _, raw := range result.Data {
		att, err := s.parseAttestation(raw, result.Version)
		if err != nil {
			s.log.WithError(err).Debug("failed to parse attestation")

			continue
		}

		attestations = append(attestations, att)
	}

	return attestations, nil
}

type headerResult struct {
	root   phase0.Root
	signed *SignedBeaconBlockHeader
}

func (s *service) fetchHeader(ctx context.Context, n *node, blockID string) (*headerResult, error) {
	resp, err := s.doNodeRequest(ctx, n, "GET", "/eth/v1/beacon/headers/"+blockID, nil, nil)
	if err != nil {
		s.markNodeUnhealthy(n)

		return nil, err
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, errNotFound
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: header request returned status %d", ErrRequestFailed, resp.StatusCode)
	}

	var result struct {
		Data struct {
			Root   string `json:"root"`
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
		return nil, fmt.Errorf("failed to decode header response: %w", err)
	}

	root, err := parseRoot(result.Data.Root)
	if err != nil {
		return nil, fmt.Errorf("invalid header root: %w", err)
	}

	stateRoot, err := parseRoot(result.Data.Header.Message.StateRoot)
	if err != nil {
		return nil, fmt.Errorf("invalid header state root: %w", err)
	}

	slot, _ := parseUint64(result.Data.Header.Message.Slot)
	proposerIndex, _ := parseUint64(result.Data.Header.Message.ProposerIndex)
	parentRoot, _ := parseRoot(result.Data.Header.Message.ParentRoot)
	bodyRoot, _ := parseRoot(result.Data.Header.Message.BodyRoot)
	sig, _ := parseHexBytes(result.Data.Header.Signature)

	s.markNodeHealthy(n)

	return &headerResult{
		root: root,
		signed: &SignedBeaconBlockHeader{
			Message: &BeaconBlockHeader{
				Slot:          phase0.Slot(slot),
				ProposerIndex: phase0.ValidatorIndex(proposerIndex),
				ParentRoot:    parentRoot,
				StateRoot:     stateRoot,
				BodyRoot:      bodyRoot,
			},
			Signature: sig,
		},
	}, nil
}

// GetSignedBlockHeader retrieves a signed block header by block root.
func (s *service) GetSignedBlockHeader(
	ctx context.Context,
	blockRoot phase0.Root,
) (*SignedBeaconBlockHeader, error) {
	var (
		lastErr  error
		notFound bool
	)

	for _, n := range s.getHealthyNodes() {
		header, err := s.fetchHeader(ctx, n, fmt.Sprintf("0x%x", blockRoot))
		if errors.Is(err, errNotFound) {
			notFound = true

			continue
		}

		if err != nil {
			lastErr = err

			continue
		}

		return header.signed, nil
	}

	if notFound {
		return nil, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}

	return nil, ErrAllNodesFailed
}

// ResolveAttestingValidators resolves the attesting validators for an
// attestation, returned sorted ascending as required for attesting_indices.
func (s *service) ResolveAttestingValidators(
	ctx context.Context,
	att *Attestation,
) ([]phase0.ValidatorIndex, error) {
	var (
		validators []phase0.ValidatorIndex
		err        error
	)

	if len(att.CommitteeBits) > 0 {
		validators, err = s.resolveElectraValidators(ctx, att)
	} else {
		validators, err = s.resolvePhase0Validators(ctx, att)
	}

	if err != nil {
		return nil, err
	}

	slices.Sort(validators)

	return validators, nil
}

// resolveElectraValidators handles EIP-7549 attestations: aggregation_bits is
// the concatenation of the participation bits of every committee flagged in
// committee_bits, in increasing committee order.
func (s *service) resolveElectraValidators(
	ctx context.Context,
	att *Attestation,
) ([]phase0.ValidatorIndex, error) {
	validators := make([]phase0.ValidatorIndex, 0, 128)
	offset := 0

	for _, committeeIdx := range committeeIndicesFromBits(att.CommitteeBits) {
		committee, err := s.committeeAt(ctx, att, committeeIdx)
		if err != nil {
			return nil, err
		}

		for i, validatorIdx := range committee {
			bit := offset + i
			if bit >= len(att.AggregationBits)*8 {
				return nil, fmt.Errorf(
					"aggregation bits (%d bits) shorter than combined committee size at slot %d",
					len(att.AggregationBits)*8, att.Data.Slot,
				)
			}

			if att.AggregationBits[bit/8]&(1<<(bit%8)) != 0 {
				validators = append(validators, validatorIdx)
			}
		}

		offset += len(committee)
	}

	return validators, nil
}

// resolvePhase0Validators handles pre-Electra attestations, where data.index
// selects the single committee covered by aggregation_bits.
func (s *service) resolvePhase0Validators(
	ctx context.Context,
	att *Attestation,
) ([]phase0.ValidatorIndex, error) {
	committee, err := s.committeeAt(ctx, att, att.Data.Index)
	if err != nil {
		return nil, err
	}

	validators := make([]phase0.ValidatorIndex, 0, len(committee))

	for i, validatorIdx := range committee {
		if i >= len(att.AggregationBits)*8 {
			break
		}

		if att.AggregationBits[i/8]&(1<<(i%8)) != 0 {
			validators = append(validators, validatorIdx)
		}
	}

	return validators, nil
}

// committeeIndicesFromBits returns the committee indices flagged in a
// committee_bits bitvector, in increasing order.
func committeeIndicesFromBits(bits []byte) []phase0.CommitteeIndex {
	indices := make([]phase0.CommitteeIndex, 0, 8)

	for i := 0; i < len(bits)*8; i++ {
		if bits[i/8]&(1<<(i%8)) != 0 {
			indices = append(indices, phase0.CommitteeIndex(i))
		}
	}

	return indices
}

func (s *service) committeeAt(
	ctx context.Context,
	att *Attestation,
	index phase0.CommitteeIndex,
) ([]phase0.ValidatorIndex, error) {
	if att.StateRoot == (phase0.Root{}) {
		return nil, fmt.Errorf("%w: slot %d", ErrMissingBlockState, att.Data.Slot)
	}

	key := committeeKey{stateRoot: att.StateRoot, slot: att.Data.Slot, index: index}

	if committee := s.getCachedCommittee(key); committee != nil {
		return committee, nil
	}

	if err := s.fetchStateCommittees(ctx, att); err != nil {
		return nil, fmt.Errorf("failed to fetch committees from state 0x%x: %w", att.StateRoot, err)
	}

	committee := s.getCachedCommittee(key)
	if committee == nil {
		return nil, fmt.Errorf("committee not found for slot %d index %d in state 0x%x", att.Data.Slot, index, att.StateRoot)
	}

	return committee, nil
}

// SubmitAttesterSlashing submits an attester slashing to all beacon nodes.
func (s *service) SubmitAttesterSlashing(ctx context.Context, slashing *AttesterSlashing) error {
	version := slashing.Version
	if version == "" {
		version = "electra"
	}

	if IsPQ(version) {
		return ErrPQAttesterSlashing
	}

	body, err := s.marshalSlashing(slashing)
	if err != nil {
		return fmt.Errorf("failed to marshal slashing: %w", err)
	}

	path := "/eth/v2/beacon/pool/attester_slashings"
	headers := map[string]string{"Eth-Consensus-Version": version}

	if forkRank(version) < forkRank("electra") {
		path = "/eth/v1/beacon/pool/attester_slashings"
		headers = nil
	}

	return s.submit(ctx, path, body, headers)
}

func (s *service) submit(ctx context.Context, path string, body []byte, headers map[string]string) error {
	key := sha256.Sum256(append([]byte(path), body...))

	s.submissionMu.Lock()
	outcome := s.submissions[key]
	s.submissionMu.Unlock()

	switch outcome {
	case submissionRejected:
		return ErrPreviouslyRejected
	case submissionAccepted:
		return ErrAlreadySubmitted
	}

	var (
		canary  *node
		lastErr error
	)

	for _, n := range s.getHealthyNodes() {
		status, reason, err := s.post(ctx, n, path, body, headers)
		if err != nil {
			lastErr = err

			continue
		}

		if status == http.StatusOK || status == http.StatusAccepted {
			canary = n

			break
		}

		if status == http.StatusBadRequest {
			s.remember(key, submissionRejected)

			s.log.WithFields(logrus.Fields{
				"endpoint": n.display,
				"status":   status,
				"reason":   reason,
				"payload":  string(body),
			}).Warn("slashing rejected by validating node, not broadcasting")

			return fmt.Errorf("%w: %s: %s", ErrSlashingRejected, n.display, reason)
		}

		lastErr = fmt.Errorf("%w: %s returned status %d: %s", ErrRequestFailed, n.display, status, reason)
	}

	if canary == nil {
		if lastErr != nil {
			return lastErr
		}

		return ErrAllNodesFailed
	}

	s.remember(key, submissionAccepted)

	successCount := 1

	for _, n := range s.nodes {
		if n == canary {
			continue
		}

		status, reason, err := s.post(ctx, n, path, body, headers)
		if err != nil {
			s.log.WithError(err).WithField("endpoint", n.display).Debug("failed to broadcast slashing")

			continue
		}

		if status == http.StatusOK || status == http.StatusAccepted {
			successCount++

			continue
		}

		s.log.WithFields(logrus.Fields{
			"endpoint": n.display,
			"status":   status,
			"reason":   reason,
		}).Debug("node did not accept broadcast slashing")
	}

	s.log.WithFields(logrus.Fields{
		"path":  path,
		"nodes": successCount,
	}).Info("submitted slashing to beacon nodes")

	return nil
}

func (s *service) post(
	ctx context.Context,
	n *node,
	path string,
	body []byte,
	headers map[string]string,
) (int, string, error) {
	resp, err := s.doNodeRequest(ctx, n, "POST", path, body, headers)
	if err != nil {
		return 0, "", err
	}

	defer func() { _ = resp.Body.Close() }()

	reason, _ := io.ReadAll(io.LimitReader(resp.Body, 512))

	return resp.StatusCode, string(reason), nil
}

func (s *service) remember(key [32]byte, outcome submissionOutcome) {
	s.submissionMu.Lock()
	defer s.submissionMu.Unlock()

	if len(s.submissions) >= maxRememberedSubmissions {
		clear(s.submissions)
	}

	s.submissions[key] = outcome
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
		Signature string `json:"signature,omitempty"`
		Proof     string `json:"proof,omitempty"`
	}

	pq := IsPQ(slashing.Version)

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
		}
	}

	withAuth := func(att *IndexedAttestation) apiAttestation {
		a := toAPI(att)
		if pq {
			a.Proof = fmt.Sprintf("0x%x", att.Proof)
		} else {
			a.Signature = fmt.Sprintf("0x%x", att.Signature)
		}

		return a
	}

	apiSlashing := struct {
		Attestation1 apiAttestation `json:"attestation_1"`
		Attestation2 apiAttestation `json:"attestation_2"`
	}{
		Attestation1: withAuth(slashing.Attestation1),
		Attestation2: withAuth(slashing.Attestation2),
	}

	return json.Marshal(apiSlashing)
}

// SubmitProposerSlashing submits a proposer slashing to all beacon nodes.
func (s *service) SubmitProposerSlashing(ctx context.Context, slashing *ProposerSlashing) error {
	body, err := s.marshalProposerSlashing(slashing)
	if err != nil {
		return fmt.Errorf("failed to marshal proposer slashing: %w", err)
	}

	return s.submit(ctx, "/eth/v1/beacon/pool/proposer_slashings", body, nil)
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
	log := s.log.WithField("endpoint", n.display)

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
	log := s.log.WithField("endpoint", n.display)

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
		s.log.WithField("endpoint", n.display).Warn("beacon node marked unhealthy")
	}
}

func (s *service) markNodeHealthy(n *node) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if !n.healthy {
		n.healthy = true
		s.log.WithField("endpoint", n.display).Info("beacon node marked healthy")
	}
}

func (s *service) doNodeRequest(
	ctx context.Context,
	n *node,
	method, path string,
	body []byte,
	headers map[string]string,
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

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	return resp, nil
}

func (s *service) parseAttestation(raw json.RawMessage, version string) (*Attestation, error) {
	var att struct {
		AggregationBits string `json:"aggregation_bits"`
		CommitteeBits   string `json:"committee_bits"`
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

	var committeeBits []byte
	if att.CommitteeBits != "" {
		committeeBits, _ = parseAggregationBits(att.CommitteeBits)
	}

	return &Attestation{
		AggregationBits: aggBits,
		CommitteeBits:   committeeBits,
		Version:         version,
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

func parseHexBytes(s string) ([]byte, error) {
	if len(s) < 2 {
		return nil, errors.New("hex value too short")
	}

	return hex.DecodeString(strings.TrimPrefix(s, "0x"))
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
