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
	"testing"
	"time"
)

const mrWithLabel = `{
  "object_kind": "merge_request",
  "event_type": "merge_request",
  "project": {"path_with_namespace": "group/project"},
  "object_attributes": {
    "iid": 42,
    "action": "open",
    "labels": [{"title": "needs-review"}, {"title": "bug"}]
  }
}`

const mrNoLabel = `{
  "object_kind": "merge_request",
  "event_type": "merge_request",
  "project": {"path_with_namespace": "group/project"},
  "object_attributes": {
    "iid": 42,
    "action": "open",
    "labels": [{"title": "bug"}]
  }
}`

const mrEmptyLabels = `{
  "object_kind": "merge_request",
  "event_type": "merge_request",
  "project": {"path_with_namespace": "group/project"},
  "object_attributes": {
    "iid": 42,
    "action": "open",
    "labels": []
  }
}`

func postWebhook(t *testing.T, srvURL, secret, payload string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srvURL+"/webhook", bytes.NewReader([]byte(payload)))
	req.Header.Set("X-Gitlab-Token", secret)
	req.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

func newLabelFilterServer(t *testing.T, required string) (*testServer, string, context.CancelFunc) {
	ts := newTestServer(t, "secret")
	// Override the captured handler with one that also respects the
	// required label via cfg.RequiredLabel. The test server's
	// Run() captures cfg at start time, but we can construct our
	// own Server here directly to inject RequiredLabel.
	cfg := Config{
		Addr:            "127.0.0.1:0",
		WebhookSecret:   "secret",
		Handler:         ts.recordHandler,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		ShutdownTimeout: 5 * time.Second,
		RequiredLabel:   required,
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts.Server = s
	return ts, listenAndServe(t, ts), cancelRun(t, ts)
}

func listenAndServe(t *testing.T, ts *testServer) string {
	t.Helper()
	origListener := newListener
	t.Cleanup(func() { newListener = origListener })
	listenCh := make(chan net.Listener, 1)
	newListener = func(_ context.Context, addr string) (net.Listener, error) {
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
		_ = cancel
		// Stash the context+done for shutdown.
		t.Cleanup(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
		})
		return "http://" + ln.Addr().String()
	case <-time.After(2 * time.Second):
		t.Fatal("server did not start in time")
		return ""
	}
}

func cancelRun(t *testing.T, ts *testServer) context.CancelFunc {
	t.Helper()
	return func() {} // unused — listenAndServe handles shutdown
}

func TestWebhook_RequiredLabel_Present_Accepted(t *testing.T) {
	ts, url, _ := newLabelFilterServer(t, "needs-review")
	defer ts.pool.wait(time.Second)

	resp := postWebhook(t, url, "secret", mrWithLabel)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("MR with required label should be accepted, got %d", resp.StatusCode)
	}
}

func TestWebhook_RequiredLabel_Missing_Silenced(t *testing.T) {
	ts, url, _ := newLabelFilterServer(t, "needs-review")
	defer ts.pool.wait(time.Second)

	resp := postWebhook(t, url, "secret", mrNoLabel)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("MR without required label should be 204, got %d", resp.StatusCode)
	}
}

func TestWebhook_RequiredLabel_EmptyLabels_Silenced(t *testing.T) {
	ts, url, _ := newLabelFilterServer(t, "needs-review")
	defer ts.pool.wait(time.Second)

	resp := postWebhook(t, url, "secret", mrEmptyLabels)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("MR with empty labels should be 204, got %d", resp.StatusCode)
	}
}

func TestWebhook_RequiredLabel_EmptyConfig_AcceptsAll(t *testing.T) {
	// Empty RequiredLabel = no gate = all MRs accepted.
	ts, url, _ := newLabelFilterServer(t, "")
	defer ts.pool.wait(time.Second)

	resp := postWebhook(t, url, "secret", mrNoLabel)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("no required label should accept every MR, got %d", resp.StatusCode)
	}
}

func TestWebhook_RequiredLabel_CaseSensitive(t *testing.T) {
	// GitLab preserves label casing. "Needs-Review" ≠ "needs-review".
	ts, url, _ := newLabelFilterServer(t, "needs-review")
	defer ts.pool.wait(time.Second)

	caps := strings.Replace(mrWithLabel, `"needs-review"`, `"Needs-Review"`, 1)
	resp := postWebhook(t, url, "secret", caps)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("case mismatch should NOT match; expected 204, got %d", resp.StatusCode)
	}
}

func TestHasLabel(t *testing.T) {
	cases := []struct {
		name   string
		labels []label
		needle string
		want   bool
	}{
		{"empty", nil, "x", false},
		{"zero value", []label{}, "x", false},
		{"single match", []label{{Title: "x"}}, "x", true},
		{"multi match", []label{{Title: "a"}, {Title: "x"}}, "x", true},
		{"no match", []label{{Title: "a"}, {Title: "b"}}, "x", false},
		{"case mismatch", []label{{Title: "X"}}, "x", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasLabel(tc.labels, tc.needle); got != tc.want {
				t.Errorf("hasLabel(%v, %q) = %v, want %v", tc.labels, tc.needle, got, tc.want)
			}
		})
	}
}

// suppress unused-import lint for json if no test uses it.
var _ = json.Marshal
