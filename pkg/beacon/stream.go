package beacon

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
)

// Stream handles SSE connections to the beacon node.
type Stream struct {
	endpoint string
	log      logrus.FieldLogger
	client   *http.Client
	mu       sync.Mutex
	closed   bool
	cancel   context.CancelFunc
}

// NewStream creates a new SSE stream handler.
func NewStream(endpoint string, log logrus.FieldLogger) *Stream {
	return &Stream{
		endpoint: endpoint,
		log:      log.WithField("component", "stream"),
		client:   &http.Client{},
	}
}

// Subscribe subscribes to an SSE event topic.
func (s *Stream) Subscribe(ctx context.Context, topic string, handler func([]byte)) error {
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	url := fmt.Sprintf("%s/eth/v1/events?topics=%s", s.endpoint, topic)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create SSE request: %w", err)
	}

	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Connection", "keep-alive")

	go s.handleStream(ctx, req, handler)

	return nil
}

// Close closes the SSE stream.
func (s *Stream) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return
	}

	s.closed = true

	if s.cancel != nil {
		s.cancel()
	}
}

func (s *Stream) handleStream(ctx context.Context, req *http.Request, handler func([]byte)) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := s.connectAndRead(ctx, req, handler); err != nil {
			s.log.WithError(err).Warn("SSE connection error, reconnecting")
		}

		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

func (s *Stream) connectAndRead(
	ctx context.Context,
	req *http.Request,
	handler func([]byte),
) error {
	resp, err := s.client.Do(req.Clone(ctx))
	if err != nil {
		return fmt.Errorf("failed to connect to SSE: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	s.log.Info("connected to SSE stream")

	scanner := bufio.NewScanner(resp.Body)
	var eventData strings.Builder

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line := scanner.Text()

		if line == "" {
			if eventData.Len() > 0 {
				handler([]byte(eventData.String()))
				eventData.Reset()
			}

			continue
		}

		if strings.HasPrefix(line, "data:") {
			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimSpace(data)
			eventData.WriteString(data)
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scanner error: %w", err)
	}

	return nil
}
