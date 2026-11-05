package watcher

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestNtfy_Setup_DefaultServer(t *testing.T) {
	a := &NtfyAdapter{}
	err := a.Setup(context.Background(), AdapterConfig{
		Type:     "ntfy",
		Name:     "test",
		Settings: map[string]string{"topic": "test-topic"},
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if a.server != "https://ntfy.sh" {
		t.Errorf("expected default server https://ntfy.sh, got %q", a.server)
	}
}

func TestNtfy_Setup_CustomServer(t *testing.T) {
	a := &NtfyAdapter{}
	err := a.Setup(context.Background(), AdapterConfig{
		Type:     "ntfy",
		Name:     "test",
		Settings: map[string]string{"topic": "test-topic", "server": "https://my.ntfy.example.com"},
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if a.server != "https://my.ntfy.example.com" {
		t.Errorf("expected custom server, got %q", a.server)
	}
}

func TestNtfy_Setup_MissingTopic(t *testing.T) {
	a := &NtfyAdapter{}
	err := a.Setup(context.Background(), AdapterConfig{
		Type:     "ntfy",
		Name:     "test",
		Settings: map[string]string{},
	})
	if err == nil {
		t.Fatal("expected error for missing topic")
	}
}

func TestNtfy_Listen_ReceivesMessages(t *testing.T) {
	// Mock server that sends 2 message events
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher, _ := w.(http.Flusher)

		msgs := []ntfyMessage{
			{ID: "msg1", Time: time.Now().Unix(), Event: "message", Topic: "test", Message: "hello"},
			{ID: "msg2", Time: time.Now().Unix(), Event: "message", Topic: "test", Message: "world"},
		}
		for _, msg := range msgs {
			data, _ := json.Marshal(msg)
			fmt.Fprintf(w, "%s\n", data)
			if flusher != nil {
				flusher.Flush()
			}
		}
		// Hold connection open until client disconnects
		<-r.Context().Done()
	}))
	defer ts.Close()

	a := &NtfyAdapter{}
	err := a.Setup(context.Background(), AdapterConfig{
		Type:     "ntfy",
		Name:     "test",
		Settings: map[string]string{"topic": "test", "server": ts.URL},
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	events := make(chan Event, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	listenErr := make(chan error, 1)
	go func() {
		listenErr <- a.Listen(ctx, events)
	}()

	// Collect 2 events
	var received []Event
	for i := 0; i < 2; i++ {
		select {
		case evt := <-events:
			received = append(received, evt)
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout waiting for event %d", i+1)
		}
	}

	if len(received) != 2 {
		t.Fatalf("expected 2 events, got %d", len(received))
	}
	for _, evt := range received {
		if evt.Source != "ntfy" {
			t.Errorf("expected Source=ntfy, got %q", evt.Source)
		}
	}
	if received[0].Body != "hello" {
		t.Errorf("expected first message body=hello, got %q", received[0].Body)
	}
	if received[1].Body != "world" {
		t.Errorf("expected second message body=world, got %q", received[1].Body)
	}

	cancel()
	<-listenErr
}

func TestNtfy_Listen_SkipsOpenAndKeepalive(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher, _ := w.(http.Flusher)

		lines := []ntfyMessage{
			{ID: "open1", Time: time.Now().Unix(), Event: "open", Topic: "test"},
			{ID: "ka1", Time: time.Now().Unix(), Event: "keepalive", Topic: "test"},
			{ID: "msg1", Time: time.Now().Unix(), Event: "message", Topic: "test", Message: "real event"},
			{ID: "ka2", Time: time.Now().Unix(), Event: "keepalive", Topic: "test"},
		}
		for _, msg := range lines {
			data, _ := json.Marshal(msg)
			fmt.Fprintf(w, "%s\n", data)
			if flusher != nil {
				flusher.Flush()
			}
		}
		<-r.Context().Done()
	}))
	defer ts.Close()

	a := &NtfyAdapter{}
	_ = a.Setup(context.Background(), AdapterConfig{
		Type:     "ntfy",
		Name:     "test",
		Settings: map[string]string{"topic": "test", "server": ts.URL},
	})

	events := make(chan Event, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = a.Listen(ctx, events)
	}()

	// Should only get 1 event (the message)
	select {
	case evt := <-events:
		if evt.Body != "real event" {
			t.Errorf("expected Body='real event', got %q", evt.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for message event")
	}

	// Verify no more events come through
	select {
	case evt := <-events:
		t.Errorf("unexpected extra event: %+v", evt)
	case <-time.After(200 * time.Millisecond):
		// Expected: no more events
	}

	cancel()
}

func TestNtfy_Normalization(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher, _ := w.(http.Flusher)

		msgs := []ntfyMessage{
			{ID: "m1", Time: 1700000000, Event: "message", Topic: "alerts", Title: "Build Failed", Message: "CI pipeline #42 failed"},
			{ID: "m2", Time: 1700000001, Event: "message", Topic: "alerts", Message: "first line\nsecond line"},
		}
		for _, msg := range msgs {
			data, _ := json.Marshal(msg)
			fmt.Fprintf(w, "%s\n", data)
			if flusher != nil {
				flusher.Flush()
			}
		}
		<-r.Context().Done()
	}))
	defer ts.Close()

	a := &NtfyAdapter{}
	_ = a.Setup(context.Background(), AdapterConfig{
		Type:     "ntfy",
		Name:     "test",
		Settings: map[string]string{"topic": "alerts", "server": ts.URL},
	})

	events := make(chan Event, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = a.Listen(ctx, events)
	}()

	// First event: has title, should use as Subject
	select {
	case evt := <-events:
		if evt.Subject != "Build Failed" {
			t.Errorf("expected Subject='Build Failed', got %q", evt.Subject)
		}
		if evt.Sender == "" {
			t.Error("expected non-empty Sender")
		}
		expectedTime := time.Unix(1700000000, 0)
		if !evt.Timestamp.Equal(expectedTime) {
			t.Errorf("expected timestamp %v, got %v", expectedTime, evt.Timestamp)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}

	// Second event: no title, should use first line of message
	select {
	case evt := <-events:
		if evt.Subject != "first line" {
			t.Errorf("expected Subject='first line', got %q", evt.Subject)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}

	cancel()
}

func TestNtfy_Listen_ReconnectsOnDisconnect(t *testing.T) {
	var connCount atomic.Int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := connCount.Add(1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher, _ := w.(http.Flusher)

		msg := ntfyMessage{
			ID:      fmt.Sprintf("msg%d", n),
			Time:    time.Now().Unix(),
			Event:   "message",
			Topic:   "test",
			Message: fmt.Sprintf("connection %d", n),
		}
		data, _ := json.Marshal(msg)
		fmt.Fprintf(w, "%s\n", data)
		if flusher != nil {
			flusher.Flush()
		}

		if n == 1 {
			// Close first connection immediately to trigger reconnect
			return
		}
		// Keep second connection open
		<-r.Context().Done()
	}))
	defer ts.Close()

	a := &NtfyAdapter{}
	_ = a.Setup(context.Background(), AdapterConfig{
		Type:     "ntfy",
		Name:     "test",
		Settings: map[string]string{"topic": "test", "server": ts.URL},
	})
	// Override initial backoff for faster test
	a.initialBackoff = 100 * time.Millisecond

	events := make(chan Event, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = a.Listen(ctx, events)
	}()

	// Should get 2 events across 2 connections
	var received []Event
	for i := 0; i < 2; i++ {
		select {
		case evt := <-events:
			received = append(received, evt)
		case <-time.After(10 * time.Second):
			t.Fatalf("timeout waiting for event %d (got %d so far)", i+1, len(received))
		}
	}

	if len(received) != 2 {
		t.Fatalf("expected 2 events across reconnect, got %d", len(received))
	}
	if received[0].Body != "connection 1" {
		t.Errorf("first event body: got %q", received[0].Body)
	}
	if received[1].Body != "connection 2" {
		t.Errorf("second event body: got %q", received[1].Body)
	}

	if count := connCount.Load(); count < 2 {
		t.Errorf("expected at least 2 connections, got %d", count)
	}

	cancel()
}

func TestNtfy_Listen_BackoffIncreases(t *testing.T) {
	// Server that always returns error
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	a := &NtfyAdapter{}
	_ = a.Setup(context.Background(), AdapterConfig{
		Type:     "ntfy",
		Name:     "test",
		Settings: map[string]string{"topic": "test", "server": ts.URL},
	})

	// Use tiny backoff for test speed
	a.initialBackoff = 10 * time.Millisecond
	a.maxBackoff = 100 * time.Millisecond

	events := make(chan Event, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	_ = a.Listen(ctx, events)
	elapsed := time.Since(start)

	// With initial=10ms, doubling to cap=100ms, in 500ms we should see
	// backoffs of 10, 20, 40, 80, 100, 100... (at least 4 retries)
	// Just verify it ran for roughly the expected duration and didn't return instantly
	if elapsed < 200*time.Millisecond {
		t.Errorf("Listen returned too quickly (%v); backoff may not be working", elapsed)
	}
}

func TestNtfy_HealthCheck_Reachable(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintln(w, `{"event":"open"}`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer ts.Close()

	a := &NtfyAdapter{}
	_ = a.Setup(context.Background(), AdapterConfig{
		Type:     "ntfy",
		Name:     "test",
		Settings: map[string]string{"topic": "test", "server": ts.URL},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Listen(ctx, make(chan Event)) }()
	defer func() { cancel(); <-done }()

	// A connected, idle subscription remains healthy without routed events.
	deadline := time.After(2 * time.Second)
	for {
		if err := a.HealthCheck(); err == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("subscription never became healthy")
		case <-time.After(10 * time.Millisecond):
		}
	}
	tracker := NewHealthTracker("ntfy", 10080)
	for i := 0; i < 5; i++ {
		if err := a.HealthCheck(); err != nil {
			tracker.SetAdapterHealth(false)
			tracker.RecordError()
		} else {
			tracker.SetAdapterHealth(true)
		}
	}
	state := tracker.Check()
	if state.Status != HealthStatusHealthy || state.ConsecutiveErrors != 0 {
		t.Errorf("idle connected stream: health = %s, consecutive errors = %d", state.Status, state.ConsecutiveErrors)
	}
}

func TestNtfy_HealthCheck_KeepaliveKeepsIdleStreamHealthy(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintln(w, `{"event":"open"}`)
		w.(http.Flusher).Flush()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				fmt.Fprintln(w, `{"event":"keepalive"}`)
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
		}
	}))
	defer ts.Close()
	a := &NtfyAdapter{livenessTimeout: 40 * time.Millisecond}
	if err := a.Setup(context.Background(), AdapterConfig{Settings: map[string]string{"topic": "test", "server": ts.URL}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Listen(ctx, make(chan Event)) }()
	defer func() { cancel(); <-done }()
	deadline := time.After(2 * time.Second)
	for a.HealthCheck() != nil {
		select {
		case <-deadline:
			t.Fatal("subscription never became healthy")
		case <-time.After(5 * time.Millisecond):
		}
	}
	tracker := NewHealthTracker("ntfy", 10080)
	for i := 0; i < 8; i++ {
		time.Sleep(15 * time.Millisecond)
		if err := a.HealthCheck(); err != nil {
			tracker.SetAdapterHealth(false)
			tracker.RecordError()
		} else {
			tracker.SetAdapterHealth(true)
		}
	}
	state := tracker.Check()
	if state.Status != HealthStatusHealthy || state.ConsecutiveErrors != 0 || !state.LastEventTime.IsZero() {
		t.Errorf("idle stream: health = %s, errors = %d, last event = %v", state.Status, state.ConsecutiveErrors, state.LastEventTime)
	}
}

func TestNtfy_HealthCheck_StalledStreamReconnects(t *testing.T) {
	var connections atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := connections.Add(1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintln(w, `{"event":"open"}`)
		w.(http.Flusher).Flush()
		if n == 1 {
			<-r.Context().Done()
			return
		}
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				fmt.Fprintln(w, `{"event":"keepalive"}`)
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
		}
	}))
	defer ts.Close()
	a := &NtfyAdapter{initialBackoff: 10 * time.Millisecond, maxBackoff: 20 * time.Millisecond, livenessTimeout: 40 * time.Millisecond}
	if err := a.Setup(context.Background(), AdapterConfig{Settings: map[string]string{"topic": "test", "server": ts.URL}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Listen(ctx, make(chan Event)) }()
	defer func() { cancel(); <-done }()
	deadline := time.After(2 * time.Second)
	connected := false
	stalled := false
	for {
		err := a.HealthCheck()
		if err == nil && connections.Load() == 1 {
			connected = true
		}
		if connected && err != nil && connections.Load() == 1 {
			stalled = true
		}
		if stalled && connections.Load() >= 2 && err == nil {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("stalled stream did not fail and recover (stalled=%v, connections=%d)", stalled, connections.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestNtfy_HealthCheck_DroppedStreamRecovers(t *testing.T) {
	var connections atomic.Int32
	firstOpened := make(chan struct{})
	dropFirst := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := connections.Add(1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintln(w, `{"event":"open"}`)
		w.(http.Flusher).Flush()
		if n == 1 {
			close(firstOpened)
			<-dropFirst
			return
		}
		<-r.Context().Done()
	}))
	defer ts.Close()
	a := &NtfyAdapter{initialBackoff: 100 * time.Millisecond, maxBackoff: 100 * time.Millisecond}
	if err := a.Setup(context.Background(), AdapterConfig{Settings: map[string]string{"topic": "test", "server": ts.URL}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Listen(ctx, make(chan Event)) }()
	defer func() { cancel(); <-done }()
	<-firstOpened
	deadline := time.After(2 * time.Second)
	for a.HealthCheck() != nil {
		select {
		case <-deadline:
			t.Fatal("first stream never became healthy")
		case <-time.After(5 * time.Millisecond):
		}
	}
	close(dropFirst)
	for a.HealthCheck() == nil {
		select {
		case <-deadline:
			t.Fatal("closed stream never became unhealthy")
		case <-time.After(5 * time.Millisecond):
		}
	}
	for {
		if connections.Load() >= 2 && a.HealthCheck() == nil {
			return
		}
		select {
		case <-deadline:
			t.Fatal("new stream never recovered")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestNtfy_HealthCheck_Disconnected(t *testing.T) {
	a := &NtfyAdapter{}
	_ = a.Setup(context.Background(), AdapterConfig{
		Type:     "ntfy",
		Name:     "test",
		Settings: map[string]string{"topic": "test", "server": "http://127.0.0.1:19997"},
	})

	if err := a.HealthCheck(); err == nil {
		t.Error("expected error from HealthCheck without a stream")
	}
}

func TestNtfy_Listen_StopNoLeaks(t *testing.T) {
	defer goleak.VerifyNone(t,
		goleak.IgnoreTopFunction("database/sql.(*DB).connectionOpener"),
		goleak.IgnoreTopFunction("database/sql.(*DB).connectionResetter"),
		goleak.IgnoreAnyFunction("modernc.org"),
		goleak.IgnoreAnyFunction("poll.runtime_pollWait"),
		// Plan 17-01: adding the Google client pulls in go.opencensus.io, whose
		// stats worker is started from an init() and lives for the test binary.
		goleak.IgnoreTopFunction("go.opencensus.io/stats/view.(*worker).start"),
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher, _ := w.(http.Flusher)

		msg := ntfyMessage{ID: "m1", Time: time.Now().Unix(), Event: "message", Topic: "test", Message: "leak test"}
		data, _ := json.Marshal(msg)
		fmt.Fprintf(w, "%s\n", data)
		if flusher != nil {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer ts.Close()

	a := &NtfyAdapter{}
	_ = a.Setup(context.Background(), AdapterConfig{
		Type:     "ntfy",
		Name:     "test",
		Settings: map[string]string{"topic": "test", "server": ts.URL},
	})

	events := make(chan Event, 10)
	ctx, cancel := context.WithCancel(context.Background())

	listenErr := make(chan error, 1)
	go func() {
		listenErr <- a.Listen(ctx, events)
	}()

	// Wait for event
	select {
	case <-events:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for event")
	}

	cancel()
	select {
	case <-listenErr:
	case <-time.After(5 * time.Second):
		t.Fatal("Listen did not return after cancel")
	}
	// goleak.VerifyNone checks for leaked goroutines
}

func TestNtfy_Setup_TrailingSlash(t *testing.T) {
	a := &NtfyAdapter{}
	err := a.Setup(context.Background(), AdapterConfig{
		Type:     "ntfy",
		Name:     "test",
		Settings: map[string]string{"topic": "test", "server": "https://ntfy.sh/"},
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if a.server != "https://ntfy.sh" {
		t.Errorf("expected trailing slash trimmed, got %q", a.server)
	}
}
