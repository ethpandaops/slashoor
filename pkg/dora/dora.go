package dora

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/sirupsen/logrus"

	"github.com/slashoor/slashoor/pkg/beacon"
)

// OrphanedBlock represents an orphaned block found via Dora.
type OrphanedBlock struct {
	Slot          phase0.Slot
	ProposerIndex phase0.ValidatorIndex
	BlockRoot     phase0.Root
}

// DoubleProposal represents a detected double-proposal (slashable offense).
type DoubleProposal struct {
	Slot          phase0.Slot
	ProposerIndex phase0.ValidatorIndex
	CanonicalRoot phase0.Root
	OrphanedRoot  phase0.Root
}

// Service defines the interface for the Dora client.
type Service interface {
	Start(ctx context.Context) error
	Stop() error
	GetOrphanedBlocks(ctx context.Context, startSlot, endSlot uint64) ([]*OrphanedBlock, error)
	GetSlotsWithMultipleBlocks(ctx context.Context, startSlot, endSlot uint64) (map[phase0.Slot][]*OrphanedBlock, error)
	GetSignedBlockHeader(ctx context.Context, blockRoot phase0.Root) (*beacon.SignedBeaconBlockHeader, error)
	GetAllDoubleProposals(ctx context.Context) ([]*DoubleProposal, error)
	IsValidatorSlashed(ctx context.Context, validatorIndex phase0.ValidatorIndex) (bool, error)
}

type service struct {
	cfg    *Config
	log    logrus.FieldLogger
	client *http.Client
}

// New creates a new Dora client service.
func New(cfg *Config, log logrus.FieldLogger) Service {
	return &service{
		cfg: cfg,
		log: log.WithField("package", "dora"),
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// Start initializes the Dora client service.
func (s *service) Start(ctx context.Context) error {
	if err := s.cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	s.log.WithFields(logrus.Fields{
		"enabled": s.cfg.Enabled,
		"url":     s.cfg.URL,
	}).Info("dora client started")

	return nil
}

// Stop shuts down the Dora client service.
func (s *service) Stop() error {
	s.log.Info("dora client stopped")

	return nil
}

// slotsResponse represents the Dora API response for /v1/slots.
type slotsResponse struct {
	Status string     `json:"status"`
	Data   *slotsData `json:"data"`
}

type slotsData struct {
	Slots      []*slotEntry `json:"slots"`
	TotalCount int          `json:"total_count"`
	Page       int          `json:"page"`
	NextPage   *int         `json:"next_page"`
}

type slotEntry struct {
	Slot       uint64 `json:"slot"`
	Proposer   uint64 `json:"proposer"`
	BlockRoot  string `json:"block_root"`
	ParentRoot string `json:"parent_root"`
	StateRoot  string `json:"state_root"`
	Status     string `json:"status"`
}

// singleSlotEntry has different JSON field names than slotEntry (Dora API inconsistency).
type singleSlotEntry struct {
	Slot       uint64 `json:"slot"`
	Proposer   uint64 `json:"proposer"`
	BlockRoot  string `json:"blockroot"`
	ParentRoot string `json:"parentroot"`
	StateRoot  string `json:"stateroot"`
	Status     string `json:"status"`
}

// GetOrphanedBlocks fetches orphaned blocks from Dora within the given slot range.
func (s *service) GetOrphanedBlocks(ctx context.Context, startSlot, endSlot uint64) ([]*OrphanedBlock, error) {
	if !s.cfg.Enabled {
		return nil, nil
	}

	var orphanedBlocks []*OrphanedBlock

	page := 0

	for {
		url := fmt.Sprintf("%s/api/v1/slots?with_orphaned=2&limit=100&page=%d",
			strings.TrimSuffix(s.cfg.URL, "/"), page)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}

		resp, err := s.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch orphaned blocks: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()

			return nil, fmt.Errorf("dora API returned status %d", resp.StatusCode)
		}

		var slotsResp slotsResponse
		if err := json.NewDecoder(resp.Body).Decode(&slotsResp); err != nil {
			resp.Body.Close()

			return nil, fmt.Errorf("failed to decode response: %w", err)
		}

		resp.Body.Close()

		if slotsResp.Data == nil || len(slotsResp.Data.Slots) == 0 {
			break
		}

		var minSlotInBatch uint64 = ^uint64(0)

		for _, slot := range slotsResp.Data.Slots {
			if slot.Slot < minSlotInBatch {
				minSlotInBatch = slot.Slot
			}

			if slot.Slot < startSlot || slot.Slot > endSlot {
				continue
			}

			blockRoot, err := parseRoot(slot.BlockRoot)
			if err != nil {
				s.log.WithError(err).WithField("block_root", slot.BlockRoot).Warn("failed to parse block root")

				continue
			}

			orphanedBlocks = append(orphanedBlocks, &OrphanedBlock{
				Slot:          phase0.Slot(slot.Slot),
				ProposerIndex: phase0.ValidatorIndex(slot.Proposer),
				BlockRoot:     blockRoot,
			})
		}

		// Stop if we've gone past the slot range
		if minSlotInBatch < startSlot {
			break
		}

		// Check if there's a next page
		if slotsResp.Data.NextPage == nil {
			break
		}

		page = *slotsResp.Data.NextPage

		select {
		case <-ctx.Done():
			return orphanedBlocks, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}

	return orphanedBlocks, nil
}

// GetSlotsWithMultipleBlocks fetches orphaned blocks and compares with canonical blocks
// to find slots where the same proposer created multiple blocks (double proposal).
func (s *service) GetSlotsWithMultipleBlocks(
	ctx context.Context,
	startSlot, endSlot uint64,
) (map[phase0.Slot][]*OrphanedBlock, error) {
	if !s.cfg.Enabled {
		return nil, nil
	}

	// First, fetch all orphaned blocks
	orphanedBlocks, err := s.GetOrphanedBlocks(ctx, startSlot, endSlot)
	if err != nil {
		return nil, fmt.Errorf("failed to get orphaned blocks: %w", err)
	}

	if len(orphanedBlocks) == 0 {
		return nil, nil
	}

	// For each orphaned block, fetch the canonical block at the same slot
	// and check if it's from the same proposer (double proposal)
	result := make(map[phase0.Slot][]*OrphanedBlock, 16)

	for _, orphaned := range orphanedBlocks {
		canonical, err := s.getCanonicalBlock(ctx, uint64(orphaned.Slot))
		if err != nil {
			s.log.WithError(err).WithField("slot", orphaned.Slot).Debug("failed to get canonical block")

			continue
		}

		if canonical == nil {
			continue
		}

		// Check if same proposer with different block roots
		if canonical.ProposerIndex == orphaned.ProposerIndex && canonical.BlockRoot != orphaned.BlockRoot {
			// Double proposal detected - add both blocks
			result[orphaned.Slot] = []*OrphanedBlock{canonical, orphaned}
		}

		// Avoid tight loops
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}

	return result, nil
}

// slotResponse represents the Dora API response for /v1/slot/{slot}.
type slotResponse struct {
	Status string           `json:"status"`
	Data   *singleSlotEntry `json:"data"`
}

// getCanonicalBlock fetches the canonical block for a given slot.
func (s *service) getCanonicalBlock(ctx context.Context, slot uint64) (*OrphanedBlock, error) {
	url := fmt.Sprintf("%s/api/v1/slot/%d", strings.TrimSuffix(s.cfg.URL, "/"), slot)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch slot: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dora API returned status %d", resp.StatusCode)
	}

	var slotResp slotResponse
	if err := json.NewDecoder(resp.Body).Decode(&slotResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if slotResp.Data == nil {
		return nil, nil
	}

	// Map the response to our block structure
	// Note: Dora uses different field names in slot response vs slots response
	blockRoot, err := parseRoot(slotResp.Data.BlockRoot)
	if err != nil {
		return nil, fmt.Errorf("failed to parse block root: %w", err)
	}

	return &OrphanedBlock{
		Slot:          phase0.Slot(slotResp.Data.Slot),
		ProposerIndex: phase0.ValidatorIndex(slotResp.Data.Proposer),
		BlockRoot:     blockRoot,
	}, nil
}

// parseRoot parses a hex-encoded root string into a phase0.Root.
func parseRoot(s string) (phase0.Root, error) {
	var root phase0.Root

	s = strings.TrimPrefix(s, "0x")

	if len(s) != 64 {
		return root, fmt.Errorf("invalid root length: %d", len(s))
	}

	for i := 0; i < 32; i++ {
		var b byte

		_, err := fmt.Sscanf(s[i*2:i*2+2], "%02x", &b)
		if err != nil {
			return root, fmt.Errorf("failed to parse byte at position %d: %w", i, err)
		}

		root[i] = b
	}

	return root, nil
}

// GetSignedBlockHeader fetches a signed block header from Dora's web interface.
// This is needed because Dora's API doesn't include signatures, but the web page does.
func (s *service) GetSignedBlockHeader(
	ctx context.Context,
	blockRoot phase0.Root,
) (*beacon.SignedBeaconBlockHeader, error) {
	if !s.cfg.Enabled {
		return nil, nil
	}

	url := fmt.Sprintf("%s/slot/0x%x", strings.TrimSuffix(s.cfg.URL, "/"), blockRoot)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch slot page: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dora returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	html := string(body)

	return s.parseSignedHeaderFromHTML(html)
}

// parseSignedHeaderFromHTML extracts signed block header data from Dora's HTML page.
// The HTML structure has roots in order: block_root, parent_root, state_root, body_root, signature.
func (s *service) parseSignedHeaderFromHTML(html string) (*beacon.SignedBeaconBlockHeader, error) {
	// Extract slot number from <span id="slot">976</span>
	slot, err := s.extractSlotFromHTML(html)
	if err != nil {
		return nil, fmt.Errorf("failed to extract slot: %w", err)
	}

	// Extract proposer index from /validator/608 link
	proposer, err := s.extractProposerFromHTML(html)
	if err != nil {
		return nil, fmt.Errorf("failed to extract proposer: %w", err)
	}

	// Extract all 32-byte roots from HTML - they appear in order:
	// block_root, parent_root, state_root, body_root
	roots := s.extractAllRootsFromHTML(html)
	if len(roots) < 4 {
		return nil, fmt.Errorf("not enough roots found in HTML (found %d, need 4)", len(roots))
	}

	parentRoot := roots[1]
	stateRoot := roots[2]
	bodyRoot := roots[3]

	// Extract the block signature (96 bytes / 192 hex chars)
	signature, err := s.extractSignatureFromHTML(html)
	if err != nil {
		return nil, fmt.Errorf("failed to extract signature: %w", err)
	}

	return &beacon.SignedBeaconBlockHeader{
		Message: &beacon.BeaconBlockHeader{
			Slot:          phase0.Slot(slot),
			ProposerIndex: phase0.ValidatorIndex(proposer),
			ParentRoot:    parentRoot,
			StateRoot:     stateRoot,
			BodyRoot:      bodyRoot,
		},
		Signature: signature,
	}, nil
}

// extractSlotFromHTML extracts the slot number from <span id="slot">N</span>.
func (s *service) extractSlotFromHTML(html string) (uint64, error) {
	re := regexp.MustCompile(`<span id="slot">(\d+)</span>`)
	matches := re.FindStringSubmatch(html)

	if len(matches) > 1 {
		var v uint64

		_, err := fmt.Sscanf(matches[1], "%d", &v)
		if err == nil {
			return v, nil
		}
	}

	return 0, fmt.Errorf("slot not found in HTML")
}

// extractProposerFromHTML extracts the proposer index from validator link.
func (s *service) extractProposerFromHTML(html string) (uint64, error) {
	// Look for pattern like href="/validator/608" in the Proposer section
	re := regexp.MustCompile(`Proposer[^<]*<[^>]*>[^<]*<[^>]*href="/validator/(\d+)"`)
	matches := re.FindStringSubmatch(html)

	if len(matches) > 1 {
		var v uint64

		_, err := fmt.Sscanf(matches[1], "%d", &v)
		if err == nil {
			return v, nil
		}
	}

	// Try alternate pattern
	re = regexp.MustCompile(`href="/validator/(\d+)"[^>]*>[^<]*</a>[^<]*</div>[^<]*<div[^>]*>Proposer`)
	matches = re.FindStringSubmatch(html)

	if len(matches) > 1 {
		var v uint64

		_, err := fmt.Sscanf(matches[1], "%d", &v)
		if err == nil {
			return v, nil
		}
	}

	// Try to find the first validator link after "Proposer" label
	proposerIdx := strings.Index(html, "Proposer:")
	if proposerIdx == -1 {
		proposerIdx = strings.Index(html, ">Proposer<")
	}

	if proposerIdx != -1 {
		// Look for validator link after this position
		subset := html[proposerIdx:min(proposerIdx+500, len(html))]
		re = regexp.MustCompile(`/validator/(\d+)`)
		matches = re.FindStringSubmatch(subset)

		if len(matches) > 1 {
			var v uint64

			_, err := fmt.Sscanf(matches[1], "%d", &v)
			if err == nil {
				return v, nil
			}
		}
	}

	return 0, fmt.Errorf("proposer not found in HTML")
}

// extractAllRootsFromHTML extracts all 32-byte roots (64 hex chars) from the HTML.
func (s *service) extractAllRootsFromHTML(html string) []phase0.Root {
	re := regexp.MustCompile(`0x([a-fA-F0-9]{64})`)
	matches := re.FindAllStringSubmatch(html, -1)

	roots := make([]phase0.Root, 0, len(matches))
	seen := make(map[string]bool, len(matches))

	for _, match := range matches {
		if len(match) > 1 {
			hexStr := match[1]

			// Skip duplicates
			if seen[hexStr] {
				continue
			}

			seen[hexStr] = true

			root, err := parseRoot("0x" + hexStr)
			if err == nil {
				roots = append(roots, root)
			}
		}
	}

	return roots
}

// extractSignatureFromHTML extracts a 96-byte BLS signature from Dora's HTML.
func (s *service) extractSignatureFromHTML(html string) (phase0.BLSSignature, error) {
	var sig phase0.BLSSignature

	// Look for 192 hex character signatures in the HTML
	re := regexp.MustCompile(`0x([a-fA-F0-9]{192})`)
	matches := re.FindAllStringSubmatch(html, -1)

	// Find the first unique signature (the block signature, not sync committee)
	// Block signature appears before sync committee signature in HTML
	for _, match := range matches {
		if len(match) > 1 {
			parsedSig, err := parseSignature("0x" + match[1])
			if err == nil {
				return parsedSig, nil
			}
		}
	}

	return sig, fmt.Errorf("signature not found in HTML")
}

// parseSignature parses a hex-encoded signature string into a phase0.BLSSignature.
func parseSignature(s string) (phase0.BLSSignature, error) {
	var sig phase0.BLSSignature

	s = strings.TrimPrefix(s, "0x")

	if len(s) != 192 {
		return sig, fmt.Errorf("invalid signature length: %d", len(s))
	}

	bytes, err := hex.DecodeString(s)
	if err != nil {
		return sig, fmt.Errorf("failed to decode signature hex: %w", err)
	}

	copy(sig[:], bytes)

	return sig, nil
}

// validatorResponse represents the Dora API response for /v1/validator/{index}.
type validatorResponse struct {
	Status string         `json:"status"`
	Data   *validatorData `json:"data"`
}

type validatorData struct {
	Index  uint64 `json:"index"`
	Status string `json:"status"`
}

// IsValidatorSlashed checks if a validator is already slashed via Dora API.
func (s *service) IsValidatorSlashed(
	ctx context.Context,
	validatorIndex phase0.ValidatorIndex,
) (bool, error) {
	if !s.cfg.Enabled {
		return false, nil
	}

	url := fmt.Sprintf("%s/api/v1/validator/%d", strings.TrimSuffix(s.cfg.URL, "/"), validatorIndex)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("failed to fetch validator: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("dora API returned status %d", resp.StatusCode)
	}

	var valResp validatorResponse
	if err := json.NewDecoder(resp.Body).Decode(&valResp); err != nil {
		return false, fmt.Errorf("failed to decode response: %w", err)
	}

	if valResp.Data == nil {
		return false, nil
	}

	// Check if status contains "slashed"
	status := strings.ToLower(valResp.Data.Status)

	return strings.Contains(status, "slashed"), nil
}

// GetAllDoubleProposals fetches all orphaned blocks from genesis using page-based pagination
// and checks each against canonical blocks to find double-proposals.
func (s *service) GetAllDoubleProposals(ctx context.Context) ([]*DoubleProposal, error) {
	if !s.cfg.Enabled {
		return nil, nil
	}

	s.log.Info("fetching all orphaned blocks from dora (from genesis)")

	// Fetch all orphaned blocks using page-based pagination
	var allOrphaned []*slotEntry

	page := 0

	for {
		url := fmt.Sprintf("%s/api/v1/slots?with_orphaned=2&limit=100&page=%d",
			strings.TrimSuffix(s.cfg.URL, "/"), page)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}

		resp, err := s.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch orphaned blocks: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()

			return nil, fmt.Errorf("dora API returned status %d", resp.StatusCode)
		}

		var slotsResp slotsResponse
		if err := json.NewDecoder(resp.Body).Decode(&slotsResp); err != nil {
			resp.Body.Close()

			return nil, fmt.Errorf("failed to decode response: %w", err)
		}

		resp.Body.Close()

		if slotsResp.Data == nil || len(slotsResp.Data.Slots) == 0 {
			break
		}

		allOrphaned = append(allOrphaned, slotsResp.Data.Slots...)

		s.log.WithFields(logrus.Fields{
			"page":      page,
			"fetched":   len(slotsResp.Data.Slots),
			"total":     len(allOrphaned),
			"next_page": slotsResp.Data.NextPage,
		}).Debug("fetched orphaned blocks batch")

		// Check if there's a next page
		if slotsResp.Data.NextPage == nil {
			break
		}

		page = *slotsResp.Data.NextPage

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}

	s.log.WithField("total_orphaned", len(allOrphaned)).Info("fetched all orphaned blocks")

	// Check each orphaned block against its canonical counterpart
	var doubleProposals []*DoubleProposal

	for i, orphaned := range allOrphaned {
		if ctx.Err() != nil {
			return doubleProposals, ctx.Err()
		}

		// Get canonical block for this slot
		canonical, err := s.getCanonicalBlock(ctx, orphaned.Slot)
		if err != nil {
			s.log.WithError(err).WithField("slot", orphaned.Slot).Debug("failed to get canonical block")

			continue
		}

		if canonical == nil {
			continue
		}

		// Same proposer + different block root = double proposal
		if canonical.ProposerIndex == phase0.ValidatorIndex(orphaned.Proposer) {
			orphanedRoot, err := parseRoot(orphaned.BlockRoot)
			if err != nil {
				continue
			}

			// Skip if same block root (not a double proposal)
			if canonical.BlockRoot == orphanedRoot {
				continue
			}

			s.log.WithFields(logrus.Fields{
				"slot":           orphaned.Slot,
				"proposer_index": orphaned.Proposer,
				"canonical_root": fmt.Sprintf("0x%x", canonical.BlockRoot[:8]),
				"orphaned_root":  fmt.Sprintf("0x%x", orphanedRoot[:8]),
			}).Warn("double-proposal detected")

			doubleProposals = append(doubleProposals, &DoubleProposal{
				Slot:          phase0.Slot(orphaned.Slot),
				ProposerIndex: phase0.ValidatorIndex(orphaned.Proposer),
				CanonicalRoot: canonical.BlockRoot,
				OrphanedRoot:  orphanedRoot,
			})
		}

		// Log progress every 50 blocks
		if (i+1)%50 == 0 {
			s.log.WithFields(logrus.Fields{
				"processed":        i + 1,
				"total":            len(allOrphaned),
				"double_proposals": len(doubleProposals),
			}).Info("scanning progress")
		}

		// Rate limit API calls
		select {
		case <-ctx.Done():
			return doubleProposals, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}

	s.log.WithField("double_proposals", len(doubleProposals)).Info("double-proposal scan complete")

	return doubleProposals, nil
}
