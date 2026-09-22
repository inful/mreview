package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testServer builds a Server with a captured handler that
// records every Job it sees.
type testServer struct {
	*Server
	handlerCalls *atomic.Int32
	gotJobs      *[]Job
	gotMu        *sync.Mutex
}

func newTestServer(t *testing.T, secret string) *testServer {
	t.Helper()
	var calls atomic.Int32
	var mu sync.Mutex
	var jobs []Job
	ts := &testServer{
		handlerCalls: &calls,
		gotJobs:      &jobs,
		gotMu:        &mu,
	}

	cfg := Config{
		Addr:            "127.0.0.1:0", // ephemeral
		WebhookSecret:   secret,
		Handler:         ts.recordHandler,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		ShutdownTimeout: 5 * time.Second,
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts.Server = s
	return ts
}

func (ts *testServer) recordHandler(_ context.Context, job Job) {
	ts.handlerCalls.Add(1)
	ts.gotMu.Lock()
	*ts.gotJobs = append(*ts.gotJobs, job)
	ts.gotMu.Unlock()
}

// startAndGetAddr starts the server on an ephemeral port and
// returns its actual URL. Caller is responsible for cancelling
// the ctx to shut it down.
func startAndGetAddr(t *testing.T, ts *testServer) (string, context.CancelFunc) {
	t.Helper()
	// Override the listener function so we can listen on :0 and
	// discover the assigned port.
	origListener := newListener
	t.Cleanup(func() { newListener = origListener })

	listenCh := make(chan net.Listener, 1)
	newListener = func(_ context.Context, addr string) (net.Listener, error) {
		// Wrap the listener so we can grab its Addr.
		ln, err := origListener(context.Background(), addr)
		if err != nil {
			return nil, err
		}
		listenCh <- ln
		return ln, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ts.Run(ctx) }()

	select {
	case ln := <-listenCh:
		return "http://" + ln.Addr().String(), cancel
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("server did not start in time")
		return "", cancel
	}
}

// We can't import net in this file's imports without re-arranging.
// Trick: redeclare a local alias.

func TestVerifyToken(t *testing.T) {
	if verifyToken("hello", "hello") != true {
		t.Error("identical strings should match")
	}
	if verifyToken("hello", "world") != false {
		t.Error("different strings should not match")
	}
	if verifyToken("", "anything") {
		t.Error("empty provided should not match")
	}
	if verifyToken("anything", "") {
		t.Error("empty expected should not match")
	}
	if verifyToken("short", "longer-string") {
		t.Error("different lengths should not match")
	}
}

func TestShouldReview(t *testing.T) {
	for _, action := range []string{"open", "reopen", "update"} {
		if !shouldReview(action) {
			t.Errorf("%q should be reviewed", action)
		}
	}
	for _, action := range []string{"close", "approved", "merge", "", "approved_by"} {
		if shouldReview(action) {
			t.Errorf("%q should be ignored", action)
		}
	}
}

func TestWebhookHandler_MissingToken(t *testing.T) {
	ts := newTestServer(t, "secret123")
	url, cancel := startAndGetAddr(t, ts)
	defer cancel()
	defer ts.pool.wait(time.Second)

	req, _ := http.NewRequest(http.MethodPost, url+"/webhook", strings.NewReader("{}"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestWebhookHandler_WrongToken(t *testing.T) {
	ts := newTestServer(t, "secret123")
	url, cancel := startAndGetAddr(t, ts)
	defer cancel()
	defer ts.pool.wait(time.Second)

	req, _ := http.NewRequest(http.MethodPost, url+"/webhook", strings.NewReader("{}"))
	req.Header.Set("X-Gitlab-Token", "wrong-secret")
	req.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestWebhookHandler_Accepted(t *testing.T) {
	ts := newTestServer(t, "secret123")
	url, cancel := startAndGetAddr(t, ts)
	defer cancel()
	defer ts.pool.wait(2 * time.Second)

	body, _ := json.Marshal(map[string]any{
		"object_kind": "merge_request",
		"event_type":  "merge_request",
		"project":     map[string]string{"path_with_namespace": "group/project"},
		"object_attributes": map[string]any{
			"iid":    42,
			"action": "open",
		},
	})

	req, _ := http.NewRequest(http.MethodPost, url+"/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret123")
	req.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 202; body=%s", resp.StatusCode, body)
	}

	// Wait for the worker to pick it up.
	deadline := time.Now().Add(2 * time.Second)
	for ts.handlerCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ts.handlerCalls.Load() == 0 {
		t.Fatal("worker did not pick up job in time")
	}
	ts.gotMu.Lock()
	defer ts.gotMu.Unlock()
	if len(*ts.gotJobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(*ts.gotJobs))
	}
	if (*ts.gotJobs)[0].Project != "group/project" {
		t.Errorf("Project = %q", (*ts.gotJobs)[0].Project)
	}
	if (*ts.gotJobs)[0].IID != 42 {
		t.Errorf("IID = %d", (*ts.gotJobs)[0].IID)
	}
}

func TestWebhookHandler_RejectsIgnoredActions(t *testing.T) {
	for _, action := range []string{"close", "approved", "merge"} {
		t.Run(action, func(t *testing.T) {
			ts := newTestServer(t, "secret123")
			url, cancel := startAndGetAddr(t, ts)
			defer cancel()
			defer ts.pool.wait(time.Second)

			body, _ := json.Marshal(map[string]any{
				"object_kind": "merge_request",
				"project":     map[string]string{"path_with_namespace": "group/project"},
				"object_attributes": map[string]any{
					"iid":    42,
					"action": action,
				},
			})
			req, _ := http.NewRequest(http.MethodPost, url+"/webhook", bytes.NewReader(body))
			req.Header.Set("X-Gitlab-Token", "secret123")
			req.Header.Set("X-Gitlab-Event", "Merge Request Hook")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusNoContent {
				t.Errorf("status = %d, want 204 for ignored action", resp.StatusCode)
			}
		})
	}
}

func TestWebhookHandler_RejectsWrongEventType(t *testing.T) {
	ts := newTestServer(t, "secret123")
	url, cancel := startAndGetAddr(t, ts)
	defer cancel()
	defer ts.pool.wait(time.Second)

	body, _ := json.Marshal(map[string]any{"object_kind": "push"})
	req, _ := http.NewRequest(http.MethodPost, url+"/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret123")
	req.Header.Set("X-Gitlab-Event", "Push Hook")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204 for non-MR event", resp.StatusCode)
	}
}

func TestWebhookHandler_RejectsBadJSON(t *testing.T) {
	ts := newTestServer(t, "secret123")
	url, cancel := startAndGetAddr(t, ts)
	defer cancel()
	defer ts.pool.wait(time.Second)

	req, _ := http.NewRequest(http.MethodPost, url+"/webhook", strings.NewReader("not json"))
	req.Header.Set("X-Gitlab-Token", "secret123")
	req.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestWebhookHandler_QueueFull(t *testing.T) {
	// Test the pool directly — the worker handler is captured at
	// start time, so we can't easily swap it after the server
	// begins. The Submit path is what the webhook handler calls.
	ts := newTestServer(t, "secret123")
	ts.pool.SetCapacity(1)
	capBefore := cap(ts.pool.jobs)
	for i := 0; i < capBefore; i++ {
		if err := ts.pool.Submit(Job{Project: "x", IID: i, Action: "open"}); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	if err := ts.pool.Submit(Job{Project: "x", IID: 99, Action: "open"}); err == nil {
		t.Error("expected queue full error")
	}
}

func TestHealthz(t *testing.T) {
	ts := newTestServer(t, "secret123")
	url, cancel := startAndGetAddr(t, ts)
	defer cancel()
	defer ts.pool.wait(time.Second)

	resp, err := http.Get(url + "/healthz")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "queue_depth") {
		t.Errorf("expected queue_depth in body: %s", body)
	}
}

func TestNew_RejectsMissingFields(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := New(Config{Logger: logger}); err == nil {
		t.Error("expected error for empty Addr")
	}
	if _, err := New(Config{Addr: ":0", Logger: logger}); err == nil {
		t.Error("expected error for empty WebhookSecret")
	}
	if _, err := New(Config{Addr: ":0", WebhookSecret: "x", Logger: logger}); err == nil {
		t.Error("expected error for nil Handler")
	}
}

func TestErrQueueFull_Error(t *testing.T) {
	if ErrQueueFull.Error() == "" {
		t.Error("expected non-empty error message")
	}
}
