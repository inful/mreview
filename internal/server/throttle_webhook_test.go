package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const throttleTestPayload = `{
  "object_kind": "merge_request",
  "event_type": "merge_request",
  "project": {"path_with_namespace": "group/project"},
  "object_attributes": {"iid": 42, "action": "open"}
}`

type discardLogger struct{}

func (discardLogger) Write(p []byte) (int, error) { return len(p), nil }

// newThrottleTestServer builds a server with the given config
// and starts the worker pool. The returned shutdown func cancels
// the worker context and closes the test server.
//
// Helper used by the throttle end-to-end tests below. Centralizing
// it here avoids the boilerplate of starting a pool / httptest
// server / goroutine cancellation in every test.
func newThrottleTestServer(t *testing.T, cfg Config) (*httptest.Server, func()) {
	t.Helper()
	pool := newPool(cfg.Logger)
	th := newThrottle(cfg.ThrottleWindow, 100)
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", makeWebhookHandler(cfg, pool, cfg.Logger, th))
	srv := httptest.NewServer(mux)

	ctx, cancel := context.WithCancel(context.Background())
	pool.start(ctx, cfg.Handler)
	t.Cleanup(func() {
		cancel()
		pool.wait(time.Second)
		srv.Close()
	})
	return srv, cancel
}

// TestWebhookHandler_ThrottleEnabled_FirstAccepted confirms a
// first delivery for an MR is accepted and reaches the worker.
func TestWebhookHandler_ThrottleEnabled_FirstAccepted(t *testing.T) {
	var handled atomic.Int32
	cfg := Config{
		Addr:            "127.0.0.1:0",
		WebhookSecret:   "secret",
		Handler:         func(_ context.Context, _ Job) { handled.Add(1) },
		Logger:          slog.New(slog.NewTextHandler(discardLogger{}, nil)),
		ShutdownTimeout: 5 * time.Second,
		ThrottleWindow:  30 * time.Second,
	}
	srv, _ := newThrottleTestServer(t, cfg)

	if got := postThrottleHTTP(t, srv.URL); got != http.StatusAccepted {
		t.Errorf("first delivery: status = %d, want 202", got)
	}
	for i := 0; i < 50 && handled.Load() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if handled.Load() != 1 {
		t.Errorf("expected 1 handled, got %d", handled.Load())
	}
}

// TestWebhookHandler_ThrottleEnabled_RapidDuplicateSkipped
// confirms a second delivery within the window is acknowledged
// (202) but NOT queued.
func TestWebhookHandler_ThrottleEnabled_RapidDuplicateSkipped(t *testing.T) {
	var handled atomic.Int32
	cfg := Config{
		Addr:            "127.0.0.1:0",
		WebhookSecret:   "secret",
		Handler:         func(_ context.Context, _ Job) { handled.Add(1) },
		Logger:          slog.New(slog.NewTextHandler(discardLogger{}, nil)),
		ShutdownTimeout: 5 * time.Second,
		ThrottleWindow:  30 * time.Second,
	}
	srv, _ := newThrottleTestServer(t, cfg)

	// First delivery — accepted and queued.
	if got := postThrottleHTTP(t, srv.URL); got != http.StatusAccepted {
		t.Fatalf("first delivery: status = %d, want 202", got)
	}
	// Second delivery within the window — throttled, but still
	// acknowledged with 202 so GitLab doesn't retry.
	if got := postThrottleHTTP(t, srv.URL); got != http.StatusAccepted {
		t.Errorf("rapid duplicate: status = %d, want 202", got)
	}
	for i := 0; i < 50 && handled.Load() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if got := handled.Load(); got != 1 {
		t.Errorf("handled = %d, want 1 (throttled delivery must not be queued)", got)
	}
}

// TestWebhookHandler_ThrottleDisabled_NoSkipping confirms that
// window=0 disables throttling entirely.
func TestWebhookHandler_ThrottleDisabled_NoSkipping(t *testing.T) {
	var handled atomic.Int32
	cfg := Config{
		Addr:            "127.0.0.1:0",
		WebhookSecret:   "secret",
		Handler:         func(_ context.Context, _ Job) { handled.Add(1) },
		Logger:          slog.New(slog.NewTextHandler(discardLogger{}, nil)),
		ShutdownTimeout: 5 * time.Second,
		ThrottleWindow:  0, // disabled
	}
	srv, _ := newThrottleTestServer(t, cfg)

	for i := 0; i < 5; i++ {
		_ = postThrottleHTTP(t, srv.URL)
	}
	for i := 0; i < 100 && handled.Load() < 5; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if got := handled.Load(); got != 5 {
		t.Errorf("no throttle: expected 5 handled, got %d", got)
	}
}

// TestWebhookHandler_ThrottleAfterWindowAccepts verifies that
// after the window expires, a fresh delivery IS accepted again.
func TestWebhookHandler_ThrottleAfterWindowAccepts(t *testing.T) {
	var handled atomic.Int32
	window := 100 * time.Millisecond
	cfg := Config{
		Addr:            "127.0.0.1:0",
		WebhookSecret:   "secret",
		Handler:         func(_ context.Context, _ Job) { handled.Add(1) },
		Logger:          slog.New(slog.NewTextHandler(discardLogger{}, nil)),
		ShutdownTimeout: 5 * time.Second,
		ThrottleWindow:  window,
	}
	srv, _ := newThrottleTestServer(t, cfg)

	_ = postThrottleHTTP(t, srv.URL)
	for i := 0; i < 50 && handled.Load() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	before := handled.Load()
	time.Sleep(window + 50*time.Millisecond)
	_ = postThrottleHTTP(t, srv.URL)
	for i := 0; i < 50 && handled.Load() == before; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if got := handled.Load(); got <= before {
		t.Errorf("after window: handled = %d, expected > %d", got, before)
	}
}

// postThrottleHTTP sends a webhook with the test secret and
// returns the HTTP status code.
func postThrottleHTTP(t *testing.T, srvURL string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srvURL+"/webhook", bytes.NewReader([]byte(throttleTestPayload)))
	req.Header.Set("X-Gitlab-Token", "secret")
	req.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}
