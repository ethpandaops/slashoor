package beacon

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ethpandaops/go-eth2-client/spec/phase0"
)

// SubscribeToBlocks subscribes to block SSE events on every node and invokes
// handler once per distinct block (deduplicated across nodes). It returns
// immediately; delivery runs until the service is stopped.
func (s *service) SubscribeToBlocks(ctx context.Context, handler func(BlockEvent)) {
	dedup := newEventDedup()
	for _, n := range s.nodes {
		s.wg.Add(1)
		go func(n *node) {
			defer s.wg.Done()
			s.streamTopic(ctx, n, "block", func(data []byte) {
				var ev struct {
					Slot  string `json:"slot"`
					Block string `json:"block"`
				}
				if err := json.Unmarshal(data, &ev); err != nil {
					return
				}
				root := parseRoot(ev.Block)
				if dedup.seen(root) {
					return
				}
				handler(BlockEvent{Slot: phase0.Slot(parseUint(ev.Slot)), Block: root})
			})
		}(n)
	}
}

// SubscribeToHeads subscribes to head SSE events on every node.
func (s *service) SubscribeToHeads(ctx context.Context, handler func(HeadEvent)) {
	dedup := newEventDedup()
	for _, n := range s.nodes {
		s.wg.Add(1)
		go func(n *node) {
			defer s.wg.Done()
			s.streamTopic(ctx, n, "head", func(data []byte) {
				var ev struct {
					Slot  string `json:"slot"`
					Block string `json:"block"`
				}
				if err := json.Unmarshal(data, &ev); err != nil {
					return
				}
				root := parseRoot(ev.Block)
				if dedup.seen(root) {
					return
				}
				handler(HeadEvent{Slot: phase0.Slot(parseUint(ev.Slot)), Block: root})
			})
		}(n)
	}
}

// streamTopic maintains an SSE connection to one node's topic, reconnecting on
// error until the context is cancelled.
func (s *service) streamTopic(ctx context.Context, n *node, topic string, onData func([]byte)) {
	url := n.url + "/eth/v1/events?topics=" + topic
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if err := s.readSSE(ctx, url, onData); err != nil && ctx.Err() == nil {
			s.log.WithField("endpoint", redact(n.url)).WithField("topic", topic).
				WithError(err).Debug("SSE reconnecting")
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
}

func (s *service) readSSE(ctx context.Context, url string, onData func([]byte)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	// SSE is a long-lived stream: use a client without the per-request timeout.
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return errStatus(resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var data strings.Builder
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := scanner.Text()
		if line == "" {
			if data.Len() > 0 {
				onData([]byte(data.String()))
				data.Reset()
			}
			continue
		}
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			data.WriteString(strings.TrimSpace(v))
		}
	}
	return scanner.Err()
}

type statusError int

func (e statusError) Error() string { return "unexpected SSE status" }

func errStatus(code int) error { return statusError(code) }

func parseRoot(s string) phase0.Root {
	var r phase0.Root
	s = strings.TrimPrefix(s, "0x")
	for i := 0; i < len(r) && i*2+1 < len(s); i++ {
		r[i] = hexByte(s[i*2])<<4 | hexByte(s[i*2+1])
	}
	return r
}

func hexByte(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	default:
		return 0
	}
}

// eventDedup drops repeated events (same block root) seen across multiple nodes
// within a short window.
type eventDedup struct {
	mu    sync.Mutex
	seen1 map[phase0.Root]time.Time
	last  time.Time
}

func newEventDedup() *eventDedup {
	return &eventDedup{seen1: make(map[phase0.Root]time.Time)}
}

func (d *eventDedup) seen(root phase0.Root) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	if _, ok := d.seen1[root]; ok {
		return true
	}
	d.seen1[root] = now
	// Opportunistic cleanup of entries older than two minutes.
	if now.Sub(d.last) > time.Minute {
		for k, ts := range d.seen1 {
			if now.Sub(ts) > 2*time.Minute {
				delete(d.seen1, k)
			}
		}
		d.last = now
	}
	return false
}
