package watcher

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// NtfyAdapter implements WatcherAdapter by subscribing to an ntfy topic via
// NDJSON streaming (one JSON object per line). It auto-reconnects with exponential
// backoff on disconnection and resumes from the last received message ID.
type NtfyAdapter struct {
	server string       // ntfy server URL (e.g., "https://ntfy.sh")
	topic  string       // topic name
	token  string       // optional ntfy access token, sent as a Bearer header
	client *http.Client // HTTP client for requests

	lastID       string             // `since` for the next subscription: last handed-on message ID or the persisted resume position
	lastFrame    time.Time          // last frame on the active subscription; zero while disconnected
	streamCancel context.CancelFunc // cancels a stalled subscription so Listen can reconnect
	mu           sync.Mutex         // protects stream state and lastID

	// initialBackoff and maxBackoff are configurable for testing.
	// Production defaults: 2s initial, 30s max.
	initialBackoff  time.Duration
	maxBackoff      time.Duration
	livenessTimeout time.Duration
}

// ntfyMessage represents a single NDJSON line from the ntfy /topic/json endpoint.
type ntfyMessage struct {
	ID      string   `json:"id"`
	Time    int64    `json:"time"`
	Event   string   `json:"event"` // "open", "keepalive", "message"
	Topic   string   `json:"topic"`
	Message string   `json:"message"`
	Title   string   `json:"title"`
	Tags    []string `json:"tags"`
}

// Setup initializes the adapter with the ntfy server URL and topic. The topic is
// required; the server defaults to "https://ntfy.sh".
//
// Settings:
//   - "topic": required ntfy topic name
//   - "server": ntfy server URL (default "https://ntfy.sh")
//   - "token": optional ntfy access token for a protected topic
func (a *NtfyAdapter) Setup(_ context.Context, config AdapterConfig) error {
	a.topic = config.Settings["topic"]
	if a.topic == "" {
		return errors.New("ntfy adapter requires Settings[\"topic\"]")
	}

	a.server = config.Settings["server"]
	if a.server == "" {
		a.server = "https://ntfy.sh"
	}
	a.server = strings.TrimRight(a.server, "/")
	a.token = config.Settings["token"]
	warnPublicNtfyTopic("ntfy", a.server, a.topic, a.token)

	// Streaming client: no timeout on body reads (context handles cancellation).
	a.client = &http.Client{Timeout: 0}

	// Resume from the position the engine persisted, so messages published
	// while agent-deck was down are fetched on the first subscription (#2538).
	a.mu.Lock()
	a.lastID = config.ResumeSince
	a.lastFrame = time.Time{}
	a.streamCancel = nil
	a.mu.Unlock()

	// Set defaults for backoff if not already set (tests may override before Listen).
	if a.initialBackoff == 0 {
		a.initialBackoff = 2 * time.Second
	}
	if a.maxBackoff == 0 {
		a.maxBackoff = 30 * time.Second
	}
	if a.livenessTimeout == 0 {
		// ntfy sends keepalives every 45s by default. Allow one missed frame.
		a.livenessTimeout = 90 * time.Second
	}

	return nil
}

// Listen connects to the ntfy NDJSON stream and emits normalized Events on the
// provided channel. On disconnection, it reconnects with exponential backoff
// (initial 2s, 2x factor, 30s cap) per D-08. Listen only returns when the context
// is cancelled.
func (a *NtfyAdapter) Listen(ctx context.Context, events chan<- Event) error {
	backoff := a.initialBackoff

	for {
		uptime, err := a.streamOnce(ctx, events)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A stream that stayed up at least the cap was healthy: the drop
		// starts a fresh backoff instead of staying at the cap from earlier
		// failures. One that drops at once keeps backing off.
		if uptime >= a.maxBackoff {
			backoff = a.initialBackoff
		}
		// Connection dropped or error; wait with backoff before reconnecting
		_ = err // logged by engine's runAdapter
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > a.maxBackoff {
			backoff = a.maxBackoff
		}
	}
}

// streamOnce opens a single NDJSON streaming connection and reads events until
// the connection closes or the context is cancelled. It resumes from the last
// received message ID on reconnection, and returns how long the stream stayed
// up after the server accepted it (0 if it never did).
func (a *NtfyAdapter) streamOnce(ctx context.Context, events chan<- Event) (time.Duration, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() {
		a.mu.Lock()
		a.lastFrame = time.Time{}
		a.streamCancel = nil
		a.mu.Unlock()
	}()
	a.mu.Lock()
	lastID := a.lastID
	a.mu.Unlock()

	streamURL := a.server + "/" + a.topic + "/json"
	if lastID != "" {
		streamURL += "?since=" + lastID
	}

	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, streamURL, nil)
	if err != nil {
		return 0, err
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("ntfy server returned status %d", resp.StatusCode)
	}

	host := hostFromURL(a.server)

	connected := time.Now()
	a.mu.Lock()
	a.lastFrame = connected
	a.streamCancel = cancel
	a.mu.Unlock()
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		var msg ntfyMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue // malformed JSON line, skip per T-14-07
		}
		a.mu.Lock()
		a.lastFrame = time.Now()
		a.mu.Unlock()
		if msg.Event != "message" {
			continue // skip "open" and "keepalive" events
		}

		// Normalize to Event
		subject := msg.Title
		if subject == "" {
			subject = firstLine(msg.Message, 200)
		}
		if subject == "" {
			subject = "ntfy notification"
		}

		evt := Event{
			Source:     "ntfy",
			Sender:     fmt.Sprintf("ntfy:%s@%s", a.topic, host),
			Subject:    subject,
			Body:       msg.Message,
			Timestamp:  time.Unix(msg.Time, 0),
			RawPayload: json.RawMessage(append([]byte(nil), scanner.Bytes()...)),
			Cursor:     msg.ID,
		}

		// Blocking send: the stream applies backpressure instead of dropping
		// (a restart can replay a large backlog). The resume position only
		// moves past a message once it was handed on (#2538).
		select {
		case events <- evt:
		case <-ctx.Done():
			return time.Since(connected), ctx.Err()
		}
		a.mu.Lock()
		a.lastID = msg.ID
		a.mu.Unlock()
	}

	return time.Since(connected), scanner.Err()
}

// Teardown is a no-op. The streaming HTTP connection is closed by context
// cancellation in Listen.
func (a *NtfyAdapter) Teardown() error {
	return nil
}

// HealthCheck checks the subscription that actually delivers messages. A reachable
// server cannot prove that a held stream is alive. Cancel a stalled stream so Listen
// reconnects instead of waiting indefinitely for its next frame.
func (a *NtfyAdapter) HealthCheck() error {
	if a.client == nil {
		return errors.New("ntfy adapter is not set up")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lastFrame.IsZero() {
		return errors.New("ntfy stream is disconnected")
	}
	if time.Since(a.lastFrame) > a.livenessTimeout {
		a.streamCancel()
		return errors.New("ntfy stream stopped receiving frames")
	}
	return nil
}

// hostFromURL extracts the hostname from a URL string (e.g., "ntfy.sh" from
// "https://ntfy.sh"). Falls back to the raw URL if parsing fails.
func hostFromURL(serverURL string) string {
	u, err := url.Parse(serverURL)
	if err != nil {
		return serverURL
	}
	return u.Host
}
