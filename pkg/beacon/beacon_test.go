package beacon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/sirupsen/logrus"
)

func newTestService(t *testing.T, endpoints ...string) *service {
	t.Helper()

	if len(endpoints) == 0 {
		endpoints = []string{"http://localhost:5052"}
	}

	log := logrus.New()
	log.SetLevel(logrus.PanicLevel)

	svc := New(&Config{Endpoints: endpoints, Timeout: 5 * time.Second}, log)

	s, ok := svc.(*service)
	if !ok {
		t.Fatal("New did not return *service")
	}

	return s
}

func TestCommitteeIndicesFromBits(t *testing.T) {
	tests := []struct {
		name string
		bits []byte
		want []phase0.CommitteeIndex
	}{
		{name: "empty", bits: nil, want: []phase0.CommitteeIndex{}},
		{name: "single committee zero", bits: []byte{0x01}, want: []phase0.CommitteeIndex{0}},
		{name: "committees 0 and 2", bits: []byte{0x05}, want: []phase0.CommitteeIndex{0, 2}},
		{name: "cross byte boundary", bits: []byte{0x80, 0x01}, want: []phase0.CommitteeIndex{7, 8}},
		{name: "full first byte", bits: []byte{0xff}, want: []phase0.CommitteeIndex{0, 1, 2, 3, 4, 5, 6, 7}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := committeeIndicesFromBits(tt.bits)

			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}

			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestResolveElectraValidators(t *testing.T) {
	s := newTestService(t)

	slot := phase0.Slot(100)

	// Committee 1 has validators 10,11,12; committee 3 has 20,21.
	s.cacheCommittee(&Committee{Index: 1, Slot: slot, Validators: []phase0.ValidatorIndex{10, 11, 12}})
	s.cacheCommittee(&Committee{Index: 3, Slot: slot, Validators: []phase0.ValidatorIndex{20, 21}})

	// aggregation_bits concatenates committee 1 (3 bits) then committee 3
	// (2 bits): set bits 0, 2 (validators 10, 12) and bit 4 (validator 21),
	// plus the SSZ bitlist delimiter at bit 5.
	att := &Attestation{
		AggregationBits: []byte{0b00110101},
		CommitteeBits:   []byte{0b00001010},
		Data:            &AttestationData{Slot: slot, Index: 0},
		Version:         "electra",
	}

	got, err := s.ResolveAttestingValidators(context.Background(), att)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []phase0.ValidatorIndex{10, 12, 21}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestResolveElectraValidatorsBitsTooShort(t *testing.T) {
	s := newTestService(t)

	slot := phase0.Slot(100)
	s.cacheCommittee(&Committee{Index: 0, Slot: slot, Validators: make([]phase0.ValidatorIndex, 20)})

	att := &Attestation{
		AggregationBits: []byte{0xff}, // 8 bits for a 20-validator committee
		CommitteeBits:   []byte{0x01},
		Data:            &AttestationData{Slot: slot},
	}

	if _, err := s.ResolveAttestingValidators(context.Background(), att); err == nil {
		t.Fatal("expected error for truncated aggregation bits")
	}
}

func TestResolvePhase0Validators(t *testing.T) {
	s := newTestService(t)

	slot := phase0.Slot(50)
	s.cacheCommittee(&Committee{Index: 2, Slot: slot, Validators: []phase0.ValidatorIndex{7, 5, 9}})

	att := &Attestation{
		AggregationBits: []byte{0b00000101},
		Data:            &AttestationData{Slot: slot, Index: 2},
	}

	got, err := s.ResolveAttestingValidators(context.Background(), att)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Committee members 7 and 9 attest; result must be sorted ascending.
	want := []phase0.ValidatorIndex{7, 9}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestRedactEndpoint(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "https://eth:secret@bn-node.example.com", want: "https://bn-node.example.com"},
		{in: "https://bn-node.example.com", want: "https://bn-node.example.com"},
		{in: "http://user@localhost:5052", want: "http://localhost:5052"},
	}

	for _, tt := range tests {
		if got := redactEndpoint(tt.in); got != tt.want {
			t.Fatalf("redactEndpoint(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestForkVersionOrdering(t *testing.T) {
	if NewerForkVersion("electra", "phase0") != "electra" {
		t.Fatal("electra should rank above phase0")
	}

	if NewerForkVersion("electra", "gloas") != "gloas" {
		t.Fatal("gloas should rank above electra")
	}

	// Unknown future forks rank newest.
	if NewerForkVersion("electra", "heze") != "heze" {
		t.Fatal("unknown fork should rank above electra")
	}

	if forkRank("deneb") >= forkRank("electra") {
		t.Fatal("deneb should rank below electra")
	}
}

func TestParseAttestationCommitteeBits(t *testing.T) {
	s := newTestService(t)

	raw := json.RawMessage(`{
		"aggregation_bits": "0x0103",
		"committee_bits": "0x0500000000000000",
		"data": {
			"slot": "123",
			"index": "0",
			"beacon_block_root": "0x` + repeat64("aa") + `",
			"source": {"epoch": "3", "root": "0x` + repeat64("bb") + `"},
			"target": {"epoch": "4", "root": "0x` + repeat64("cc") + `"}
		},
		"signature": "0x` + repeat192("dd") + `"
	}`)

	att, err := s.parseAttestation(raw, "electra")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if att.Version != "electra" {
		t.Fatalf("version = %q, want electra", att.Version)
	}

	if len(att.CommitteeBits) != 8 || att.CommitteeBits[0] != 0x05 {
		t.Fatalf("committee bits = %x, want 05 followed by zeros", att.CommitteeBits)
	}

	if att.Data.Slot != 123 || att.Data.Source.Epoch != 3 || att.Data.Target.Epoch != 4 {
		t.Fatalf("unexpected attestation data: %+v", att.Data)
	}
}

func TestSubmitAttesterSlashingEndpointSelection(t *testing.T) {
	tests := []struct {
		name        string
		version     string
		wantPath    string
		wantVersion string
	}{
		{name: "electra uses v2", version: "electra", wantPath: "/eth/v2/beacon/pool/attester_slashings", wantVersion: "electra"},
		{name: "gloas uses v2", version: "gloas", wantPath: "/eth/v2/beacon/pool/attester_slashings", wantVersion: "gloas"},
		{name: "empty defaults to electra v2", version: "", wantPath: "/eth/v2/beacon/pool/attester_slashings", wantVersion: "electra"},
		{name: "phase0 uses v1", version: "phase0", wantPath: "/eth/v1/beacon/pool/attester_slashings", wantVersion: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath, gotVersion string

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotVersion = r.Header.Get("Eth-Consensus-Version")
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			s := newTestService(t, server.URL)

			slashing := &AttesterSlashing{
				Attestation1: testIndexedAttestation(),
				Attestation2: testIndexedAttestation(),
				Version:      tt.version,
			}

			if err := s.SubmitAttesterSlashing(context.Background(), slashing); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if gotPath != tt.wantPath {
				t.Fatalf("path = %q, want %q", gotPath, tt.wantPath)
			}

			if gotVersion != tt.wantVersion {
				t.Fatalf("Eth-Consensus-Version = %q, want %q", gotVersion, tt.wantVersion)
			}
		})
	}
}

func testIndexedAttestation() *IndexedAttestation {
	return &IndexedAttestation{
		AttestingIndices: []phase0.ValidatorIndex{1, 2, 3},
		Data: &AttestationData{
			Slot:   100,
			Source: &Checkpoint{Epoch: 2},
			Target: &Checkpoint{Epoch: 3},
		},
	}
}

func repeat64(hexByte string) string {
	out := ""
	for range 32 {
		out += hexByte
	}

	return out
}

func repeat192(hexByte string) string {
	out := ""
	for range 96 {
		out += hexByte
	}

	return out
}
