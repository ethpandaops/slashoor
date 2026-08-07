package beacon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ethpandaops/go-eth2-client/spec/phase0"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestService(t *testing.T, handler http.HandlerFunc) *service {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfg := &Config{Endpoints: []string{srv.URL}, Timeout: 5 * time.Second, HealthCheckInterval: time.Minute}
	return New(cfg, logrus.New()).(*service)
}

func TestParseRoot(t *testing.T) {
	r := parseRoot("0x0102030000000000000000000000000000000000000000000000000000000000")
	assert.Equal(t, byte(0x01), r[0])
	assert.Equal(t, byte(0x02), r[1])
	assert.Equal(t, byte(0x03), r[2])
}

func TestRedactCredentials(t *testing.T) {
	assert.Equal(t, "http://***@node:5052", redact("http://user:pass@node:5052"))
	assert.Equal(t, "http://node:5052", redact("http://node:5052"))
}

func TestSpec(t *testing.T) {
	s := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/eth/v1/config/spec", r.URL.Path)
		_, _ = w.Write([]byte(`{"data":{"SLOTS_PER_EPOCH":"32","ELECTRA_FORK_EPOCH":"100"}}`))
	})
	spec, err := s.Spec(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "32", spec["SLOTS_PER_EPOCH"])
	assert.Equal(t, "100", spec["ELECTRA_FORK_EPOCH"])
}

func TestCommittees(t *testing.T) {
	s := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/eth/v1/beacon/states/head/committees", r.URL.Path)
		assert.Equal(t, "5", r.URL.Query().Get("epoch"))
		_, _ = w.Write([]byte(`{"data":[
			{"index":"0","slot":"160","validators":["1","2","3"]},
			{"index":"1","slot":"160","validators":["4","5"]}
		]}`))
	})
	committees, err := s.Committees(context.Background(), 5)
	require.NoError(t, err)
	require.Contains(t, committees, phase0.Slot(160))
	require.Len(t, committees[160], 2)
	assert.Equal(t, []phase0.ValidatorIndex{1, 2, 3}, committees[160][0])
	assert.Equal(t, []phase0.ValidatorIndex{4, 5}, committees[160][1])
}

func TestValidatorsStatus(t *testing.T) {
	s := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		_, _ = w.Write([]byte(`{"data":[
			{"index":"7","status":"active_ongoing","validator":{"slashed":true,"activation_epoch":"0","exit_epoch":"18446744073709551615","withdrawable_epoch":"18446744073709551615"}}
		]}`))
	})
	got, err := s.ValidatorsStatus(context.Background(), []phase0.ValidatorIndex{7})
	require.NoError(t, err)
	require.Contains(t, got, phase0.ValidatorIndex(7))
	assert.True(t, got[7].Slashed)
	assert.Equal(t, "active_ongoing", got[7].Status)
}

func TestBlockByRootNotFound(t *testing.T) {
	s := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	blk, err := s.BlockByRoot(context.Background(), phase0.Root{0x01})
	require.NoError(t, err)
	assert.Nil(t, blk, "missing block yields (nil, nil)")
}

func TestFailoverMarksUnhealthy(t *testing.T) {
	// One always-erroring endpoint plus a good one: reads still succeed.
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"SLOTS_PER_EPOCH":"32"}}`))
	}))
	t.Cleanup(good.Close)

	cfg := &Config{
		Endpoints:           []string{"http://127.0.0.1:0", good.URL},
		Timeout:             time.Second,
		HealthCheckInterval: time.Minute,
	}
	s := New(cfg, logrus.New()).(*service)
	spec, err := s.Spec(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "32", spec["SLOTS_PER_EPOCH"])
}
