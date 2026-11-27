package beacon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/attestantio/go-eth2-client/spec/phase0"
)

type fakeNode struct {
	headRoot  phase0.Root
	stateRoot phase0.Root

	mu            sync.Mutex
	committeePath []string
	posts         int
	postStatus    int
}

func (f *fakeNode) handler(t *testing.T) http.Handler {
	t.Helper()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/eth/v1/beacon/headers/"):
			writeJSON(t, w, map[string]any{"data": map[string]any{
				"root": fmt.Sprintf("0x%x", f.headRoot),
				"header": map[string]any{
					"message": map[string]any{
						"slot":           "64",
						"proposer_index": "1",
						"parent_root":    "0x" + repeat64("00"),
						"state_root":     fmt.Sprintf("0x%x", f.stateRoot),
						"body_root":      "0x" + repeat64("00"),
					},
					"signature": "0x" + strings.Repeat("ab", 2404),
				},
			}})
		case r.URL.Path == fmt.Sprintf("/eth/v2/beacon/blocks/0x%x/attestations", f.headRoot):
			writeJSON(t, w, map[string]any{"version": "pq", "data": []any{map[string]any{
				"aggregation_bits": "0x07",
				"committee_bits":   "0x01",
				"data": map[string]any{
					"slot":              "63",
					"index":             "0",
					"beacon_block_root": "0x" + repeat64("11"),
					"source":            map[string]any{"epoch": "0", "root": "0x" + repeat64("00")},
					"target":            map[string]any{"epoch": "1", "root": "0x" + repeat64("22")},
				},
			}}})
		case strings.HasPrefix(r.URL.Path, "/eth/v1/beacon/states/"):
			f.mu.Lock()
			f.committeePath = append(f.committeePath, r.URL.Path)
			f.mu.Unlock()

			if r.URL.Path != fmt.Sprintf("/eth/v1/beacon/states/0x%x/committees", f.stateRoot) {
				w.WriteHeader(http.StatusNotFound)

				return
			}

			writeJSON(t, w, map[string]any{"data": []any{
				map[string]any{"index": "0", "slot": "63", "validators": []string{"40", "41"}},
			}})
		case strings.HasPrefix(r.URL.Path, "/eth/v1/beacon/pool/") || strings.HasPrefix(r.URL.Path, "/eth/v2/beacon/pool/"):
			f.mu.Lock()
			f.posts++
			status := f.postStatus
			f.mu.Unlock()

			if status == 0 {
				status = http.StatusOK
			}

			w.WriteHeader(status)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode: %v", err)
	}
}

func TestCommitteesResolveFromTheAttestingBlockState(t *testing.T) {
	canonical := &fakeNode{headRoot: phase0.Root{0xc1}, stateRoot: phase0.Root{0xc2}}
	fork := &fakeNode{headRoot: phase0.Root{0xf1}, stateRoot: phase0.Root{0xf2}}

	canonicalSrv := httptest.NewServer(canonical.handler(t))
	defer canonicalSrv.Close()

	forkSrv := httptest.NewServer(fork.handler(t))
	defer forkSrv.Close()

	s := newTestService(t, forkSrv.URL, canonicalSrv.URL)

	atts, err := s.GetBlockAttestationsByRoot(context.Background(), fork.headRoot)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(atts) != 1 {
		t.Fatalf("got %d attestations, want 1", len(atts))
	}

	att := atts[0]
	if att.BlockRoot != fork.headRoot || att.StateRoot != fork.stateRoot || att.ServedBy != forkSrv.URL {
		t.Fatalf("attestation not stamped with its block: %+v", att)
	}

	got, err := s.ResolveAttestingValidators(context.Background(), att)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(got) != 2 || got[0] != 40 || got[1] != 41 {
		t.Fatalf("got %v, want [40 41]", got)
	}

	if len(fork.committeePath) != 1 || len(canonical.committeePath) != 0 {
		t.Fatalf("committee lookups fork=%v canonical=%v, want one on the serving node",
			fork.committeePath, canonical.committeePath)
	}

	for _, path := range fork.committeePath {
		if strings.Contains(path, "/head/") {
			t.Fatalf("committees resolved from head: %s", path)
		}
	}
}

func TestCommitteesNeverFallBackToHead(t *testing.T) {
	s := newTestService(t)

	att := &Attestation{
		AggregationBits: []byte{0x03},
		CommitteeBits:   []byte{0x01},
		Data:            &AttestationData{Slot: 10},
	}

	if _, err := s.ResolveAttestingValidators(context.Background(), att); !errors.Is(err, ErrMissingBlockState) {
		t.Fatalf("err = %v, want ErrMissingBlockState", err)
	}
}

func TestRejectedSlashingIsNotResent(t *testing.T) {
	validator := &fakeNode{postStatus: http.StatusBadRequest}
	other := &fakeNode{}

	validatorSrv := httptest.NewServer(validator.handler(t))
	defer validatorSrv.Close()

	otherSrv := httptest.NewServer(other.handler(t))
	defer otherSrv.Close()

	s := newTestService(t, validatorSrv.URL, otherSrv.URL)

	slashing := &AttesterSlashing{
		Attestation1: testIndexedAttestation(),
		Attestation2: testIndexedAttestation(),
		Version:      "gloas",
	}

	if err := s.SubmitAttesterSlashing(context.Background(), slashing); !errors.Is(err, ErrSlashingRejected) {
		t.Fatalf("err = %v, want ErrSlashingRejected", err)
	}

	if err := s.SubmitAttesterSlashing(context.Background(), slashing); !errors.Is(err, ErrPreviouslyRejected) {
		t.Fatalf("err = %v, want ErrPreviouslyRejected", err)
	}

	if validator.posts != 1 || other.posts != 0 {
		t.Fatalf("posts validator=%d other=%d, want 1 and 0", validator.posts, other.posts)
	}
}

func TestAcceptedSlashingIsBroadcastOnce(t *testing.T) {
	a, b := &fakeNode{}, &fakeNode{}

	aSrv := httptest.NewServer(a.handler(t))
	defer aSrv.Close()

	bSrv := httptest.NewServer(b.handler(t))
	defer bSrv.Close()

	s := newTestService(t, aSrv.URL, bSrv.URL)

	slashing := &AttesterSlashing{
		Attestation1: testIndexedAttestation(),
		Attestation2: testIndexedAttestation(),
		Version:      "electra",
	}

	if err := s.SubmitAttesterSlashing(context.Background(), slashing); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := s.SubmitAttesterSlashing(context.Background(), slashing); !errors.Is(err, ErrAlreadySubmitted) {
		t.Fatalf("err = %v, want ErrAlreadySubmitted", err)
	}

	if a.posts != 1 || b.posts != 1 {
		t.Fatalf("posts a=%d b=%d, want 1 each", a.posts, b.posts)
	}
}

func TestNonDefinitiveCanaryFallsThrough(t *testing.T) {
	unsupported := &fakeNode{postStatus: http.StatusNotImplemented}
	ok := &fakeNode{}

	unsupportedSrv := httptest.NewServer(unsupported.handler(t))
	defer unsupportedSrv.Close()

	okSrv := httptest.NewServer(ok.handler(t))
	defer okSrv.Close()

	s := newTestService(t, unsupportedSrv.URL, okSrv.URL)

	slashing := &AttesterSlashing{
		Attestation1: testIndexedAttestation(),
		Attestation2: testIndexedAttestation(),
		Version:      "electra",
	}

	if err := s.SubmitAttesterSlashing(context.Background(), slashing); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ok.posts != 1 {
		t.Fatalf("ok node posts = %d, want 1", ok.posts)
	}
}

func TestPQAttesterSlashingIsNotSubmitted(t *testing.T) {
	node := &fakeNode{}

	srv := httptest.NewServer(node.handler(t))
	defer srv.Close()

	s := newTestService(t, srv.URL)

	slashing := &AttesterSlashing{
		Attestation1: testIndexedAttestation(),
		Attestation2: testIndexedAttestation(),
		Version:      "pq",
	}

	if err := s.SubmitAttesterSlashing(context.Background(), slashing); !errors.Is(err, ErrPQAttesterSlashing) {
		t.Fatalf("err = %v, want ErrPQAttesterSlashing", err)
	}

	if node.posts != 0 {
		t.Fatalf("posts = %d, want 0", node.posts)
	}
}

func TestPQIndexedAttestationShape(t *testing.T) {
	s := newTestService(t)

	att := testIndexedAttestation()
	att.Proof = []byte{0xde, 0xad}

	body, err := s.marshalSlashing(&AttesterSlashing{Attestation1: att, Attestation2: att, Version: "pq"}, "pq")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var decoded map[string]map[string]json.RawMessage
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	att1 := decoded["attestation_1"]
	if _, ok := att1["signature"]; ok {
		t.Fatalf("pq indexed attestation carries a signature: %s", body)
	}

	if string(att1["proof"]) != `"0xdead"` {
		t.Fatalf("proof = %s, want 0xdead", att1["proof"])
	}

	body, err = s.marshalSlashing(&AttesterSlashing{Attestation1: att, Attestation2: att, Version: "gloas"}, "gloas")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, ok := decoded["attestation_1"]["proof"]; ok {
		t.Fatalf("pre-pq indexed attestation carries a proof: %s", body)
	}
}

func TestPQProposerSlashingKeepsXMSSSignature(t *testing.T) {
	node := &fakeNode{headRoot: phase0.Root{0x01}, stateRoot: phase0.Root{0x02}}

	srv := httptest.NewServer(node.handler(t))
	defer srv.Close()

	s := newTestService(t, srv.URL)

	header, err := s.GetSignedBlockHeader(context.Background(), node.headRoot)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(header.Signature) != 2404 {
		t.Fatalf("signature length = %d, want 2404", len(header.Signature))
	}

	body, err := s.marshalProposerSlashing(&ProposerSlashing{SignedHeader1: header, SignedHeader2: header})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(string(body), strings.Repeat("ab", 2404)) {
		t.Fatal("proposer slashing lost the xmss signature")
	}
}

func TestUnknownForkNamesAreNotPQ(t *testing.T) {
	for _, version := range []string{"", "electra", "gloas", "future-fork"} {
		if IsPQ(version) {
			t.Fatalf("IsPQ(%q) = true", version)
		}
	}

	if !IsPQ("pq") {
		t.Fatal(`IsPQ("pq") = false`)
	}
}

func TestEmptyVersionSlashingIsElectraShaped(t *testing.T) {
	node := &fakeNode{postStatus: http.StatusOK}
	srv := httptest.NewServer(node.handler(t))
	defer srv.Close()

	s := newTestService(t, srv.URL)

	att := testIndexedAttestation()
	if err := s.SubmitAttesterSlashing(context.Background(), &AttesterSlashing{Attestation1: att, Attestation2: att}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	body, err := s.marshalSlashing(&AttesterSlashing{Attestation1: att, Attestation2: att}, "electra")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(string(body), `"signature"`) || strings.Contains(string(body), `"proof"`) {
		t.Fatalf("empty-version slashing is not electra-shaped: %s", body)
	}
}

func TestSlashingBodyIgnoresPairOrder(t *testing.T) {
	s := newTestService(t)

	first := testIndexedAttestation()
	second := testIndexedAttestation()
	second.Data.Target.Epoch++

	forward, err := s.marshalSlashing(&AttesterSlashing{Attestation1: first, Attestation2: second}, "electra")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	reversed, err := s.marshalSlashing(&AttesterSlashing{Attestation1: second, Attestation2: first}, "electra")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if string(forward) != string(reversed) {
		t.Fatalf("pair order changes the body:\n%s\n%s", forward, reversed)
	}
}
