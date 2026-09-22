package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// gitlabStub is a tiny in-process GitLab API server. It records
// every request and lets each test stage the next response.
//
// Endpoints implemented:
//   - GET /api/v4/projects/:pid/merge_requests/:iid
//   - GET /api/v4/projects/:pid/merge_requests/:iid/diffs
//
// All other endpoints return 404.
type gitlabStub struct {
	*httptest.Server
	requests  []stubRequest
	responses []stubResponse // queue; popped per request
}

type stubRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   string
}

type stubResponse struct {
	status      int
	body        string
	contentType string
	header      http.Header
}

func newGitlabStub(t *testing.T) *gitlabStub {
	t.Helper()
	stub := &gitlabStub{}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		stub.requests = append(stub.requests, stubRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Header: r.Header.Clone(),
			Body:   string(body),
		})

		if len(stub.responses) == 0 {
			http.Error(w, "stub: no response queued", http.StatusInternalServerError)
			return
		}
		next := stub.responses[0]
		stub.responses = stub.responses[1:]
		for k, vs := range next.header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		ct := next.contentType
		if ct == "" {
			ct = "application/json"
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(next.status)
		_, _ = io.WriteString(w, next.body)
	}))
	t.Cleanup(stub.Close)
	return stub
}

// enqueue stages the next response. The stub serves responses in
// FIFO order so tests can script sequences (success, then 503, then
// 200).
func (g *gitlabStub) enqueue(status int, body string) {
	g.responses = append(g.responses, stubResponse{status: status, body: body})
}

// lastAuthHeader returns the PRIVATE-TOKEN value the most recent
// request carried, so tests can assert the token is sent.
func (g *gitlabStub) lastAuthHeader() string {
	if len(g.requests) == 0 {
		return ""
	}
	return g.requests[len(g.requests)-1].Header.Get("Private-Token")
}

func newTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	var logBuf strings.Builder
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c, err := NewClient(baseURL, "test-token-abc",
		RetryConfig{
			MaxAttempts:    3,
			InitialBackoff: 1 * time.Millisecond,
			MaxBackoff:     10 * time.Millisecond,
			Logger:         logger,
		},
		logger,
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() {
		if t.Failed() && logBuf.Len() > 0 {
			t.Logf("client log:\n%s", logBuf.String())
		}
	})
	return c
}

const mrFixture = `{
  "id": 1,
  "iid": 42,
  "title": "Add caching layer",
  "description": "Caches expensive calls in memory.",
  "state": "opened",
  "source_branch": "feat/cache",
  "target_branch": "main",
  "web_url": "https://gitlab.example.com/group/project/-/merge_requests/42",
  "author": {
    "id": 7,
    "username": "alice",
    "name": "Alice Example"
  },
  "diff_refs": {
    "base_sha": "1111111111111111111111111111111111111111",
    "head_sha": "2222222222222222222222222222222222222222",
    "start_sha": "3333333333333333333333333333333333333333"
  }
}`

const diffsFixture = `[
  {"old_path":"a.go","new_path":"a.go","new_file":false,"deleted_file":false,"renamed_file":false,"diff":"@@ -1 +1 @@\n-old\n+new\n"},
  {"old_path":"","new_path":"b.go","new_file":true,"deleted_file":false,"renamed_file":false,"diff":"@@ -0,0 +1 @@\n+new\n"},
  {"old_path":"c.go","new_path":"d.go","new_file":false,"deleted_file":false,"renamed_file":true,"diff":"@@ -1 +1 @@\n-x\n+y\n"}
]`

func TestFetchMR_Success(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusOK, mrFixture)
	c := newTestClient(t, stub.URL)

	mr, err := c.FetchMR(context.Background(), "group/project", 42)
	if err != nil {
		t.Fatalf("FetchMR: %v", err)
	}
	if mr.IID != 42 {
		t.Errorf("IID = %d, want 42", mr.IID)
	}
	if mr.Title != "Add caching layer" {
		t.Errorf("Title = %q", mr.Title)
	}
	if mr.Author.Username != "alice" {
		t.Errorf("Author.Username = %q", mr.Author.Username)
	}
	if mr.BaseSHA != "1111111111111111111111111111111111111111" {
		t.Errorf("BaseSHA = %q", mr.BaseSHA)
	}
	if mr.HeadSHA != "2222222222222222222222222222222222222222" {
		t.Errorf("HeadSHA = %q", mr.HeadSHA)
	}
	if mr.StartSHA != "3333333333333333333333333333333333333333" {
		t.Errorf("StartSHA = %q", mr.StartSHA)
	}

	// Verify the request shape.
	if len(stub.requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(stub.requests))
	}
	req := stub.requests[0]
	if req.Method != http.MethodGet {
		t.Errorf("method = %s, want GET", req.Method)
	}
	if req.Path != "/api/v4/projects/group/project/merge_requests/42" {
		t.Errorf("path = %q", req.Path)
	}
	if got := stub.lastAuthHeader(); got != "test-token-abc" {
		t.Errorf("Private-Token = %q, want %q", got, "test-token-abc")
	}
}

// TestFetchMR_NumericProjectID verifies that passing a numeric
// project ID (instead of a slug path) flows through to the
// underlying client-go call unmodified. This is the documented
// workaround for GitLab instances where the /discussions endpoint
// mis-parses URL-encoded project paths but handles numeric IDs
// cleanly. See the README troubleshooting section.
func TestFetchMR_NumericProjectID(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusOK, mrFixture)
	c := newTestClient(t, stub.URL)

	// Pass "12345" (numeric) instead of "group/project" (slug).
	// client-go accepts `pid any`; mreview must not transform it.
	_, err := c.FetchMR(context.Background(), "12345", 42)
	if err != nil {
		t.Fatalf("FetchMR: %v", err)
	}

	if len(stub.requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(stub.requests))
	}
	// The stub records the path the upstream library actually
	// sent. With a numeric ID, client-go builds /projects/12345/...
	// (no URL encoding, no slashes to escape). This is what we want.
	if stub.requests[0].Path != "/api/v4/projects/12345/merge_requests/42" {
		t.Errorf("path = %q; want /api/v4/projects/12345/merge_requests/42 (no URL encoding)",
			stub.requests[0].Path)
	}
}

func TestFetchMR_AuthError(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusUnauthorized, `{"message":"401 Unauthorized"}`)
	c := newTestClient(t, stub.URL)

	_, err := c.FetchMR(context.Background(), "group/project", 42)
	if err == nil {
		t.Fatal("expected error")
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("expected *Error, got %T", err)
	}
	if e.Kind != KindAuth {
		t.Errorf("Kind = %s, want auth", e.Kind)
	}
	if e.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want %d", e.StatusCode, http.StatusUnauthorized)
	}
	// Auth errors should NOT retry — exactly one HTTP call.
	if len(stub.requests) != 1 {
		t.Errorf("expected 1 request (no retry on auth), got %d", len(stub.requests))
	}
}

func TestFetchMR_NotFound(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusNotFound, `{"message":"404 Not found"}`)
	c := newTestClient(t, stub.URL)

	_, err := c.FetchMR(context.Background(), "group/project", 9999)
	if err == nil {
		t.Fatal("expected error")
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("expected *Error, got %T", err)
	}
	if e.Kind != KindNotFound {
		t.Errorf("Kind = %s, want not_found", e.Kind)
	}
}

func TestFetchMR_TransientThenSuccess(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusServiceUnavailable, `{"message":"try again"}`)
	stub.enqueue(http.StatusOK, mrFixture)
	c := newTestClient(t, stub.URL)

	mr, err := c.FetchMR(context.Background(), "group/project", 42)
	if err != nil {
		t.Fatalf("FetchMR: %v", err)
	}
	if mr.Title != "Add caching layer" {
		t.Errorf("Title = %q", mr.Title)
	}
	if len(stub.requests) != 2 {
		t.Errorf("expected 2 requests (retry), got %d", len(stub.requests))
	}
}

func TestFetchMR_TransientExhausted(t *testing.T) {
	stub := newGitlabStub(t)
	for i := 0; i < 3; i++ {
		stub.enqueue(http.StatusBadGateway, `{"message":"bad gateway"}`)
	}
	c := newTestClient(t, stub.URL)

	_, err := c.FetchMR(context.Background(), "group/project", 42)
	if err == nil {
		t.Fatal("expected error after exhausted retries")
	}
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindTransient {
		t.Errorf("expected KindTransient, got %v", err)
	}
	if len(stub.requests) != 3 {
		t.Errorf("expected 3 requests (max attempts), got %d", len(stub.requests))
	}
}

func TestFetchMR_Server422NoRetry(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusUnprocessableEntity, `{"message":"locked"}`)
	c := newTestClient(t, stub.URL)

	_, err := c.FetchMR(context.Background(), "group/project", 42)
	if err == nil {
		t.Fatal("expected error")
	}
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindBadRequest {
		t.Errorf("expected KindBadRequest, got %v", err)
	}
	if len(stub.requests) != 1 {
		t.Errorf("expected 1 request (no retry), got %d", len(stub.requests))
	}
}

func TestFetchMR_InvalidArgs(t *testing.T) {
	c := newTestClient(t, "http://unused")
	cases := []struct {
		name    string
		project string
		iid     int
	}{
		{"empty project", "", 1},
		{"whitespace project", "foo bar", 1},
		{"zero iid", "group/project", 0},
		{"negative iid", "group/project", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.FetchMR(context.Background(), tc.project, tc.iid)
			if err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}

func TestNewClient_ValidatesArgs(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := RetryConfig{MaxAttempts: 1, InitialBackoff: 1 * time.Millisecond}
	if _, err := NewClient("", "tok", cfg, logger); err == nil {
		t.Error("expected error for empty baseURL")
	}
	if _, err := NewClient("http://x", "", cfg, logger); err == nil {
		t.Error("expected error for empty token")
	}
	// nil logger should fall back to slog.Default().
	if _, err := NewClient("http://x", "tok", cfg, nil); err != nil {
		t.Errorf("nil logger should fall back to default, got %v", err)
	}
}

func TestFetchChanges_Success(t *testing.T) {
	stub := newGitlabStub(t)
	stub.enqueue(http.StatusOK, diffsFixture)
	c := newTestClient(t, stub.URL)

	diffs, err := c.FetchChanges(context.Background(), "group/project", 42)
	if err != nil {
		t.Fatalf("FetchChanges: %v", err)
	}
	if len(diffs) != 3 {
		t.Fatalf("expected 3 diffs, got %d", len(diffs))
	}
	// Verify each fixture was decoded.
	if diffs[0].Path() != "a.go" || diffs[0].NewFile {
		t.Errorf("first diff wrong: %+v", diffs[0])
	}
	if diffs[1].Path() != "b.go" || !diffs[1].NewFile {
		t.Errorf("second diff (new file) wrong: %+v", diffs[1])
	}
	if diffs[2].Path() != "d.go" || !diffs[2].RenamedFile {
		t.Errorf("third diff (rename) wrong: %+v", diffs[2])
	}
	// Verify URL shape.
	if len(stub.requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(stub.requests))
	}
	if stub.requests[0].Path != "/api/v4/projects/group/project/merge_requests/42/diffs" {
		t.Errorf("path = %q", stub.requests[0].Path)
	}
}

func TestFetchChanges_TransientExhausted(t *testing.T) {
	stub := newGitlabStub(t)
	for i := 0; i < 3; i++ {
		stub.enqueue(http.StatusTooManyRequests, `{"message":"rate limited"}`)
	}
	c := newTestClient(t, stub.URL)

	_, err := c.FetchChanges(context.Background(), "group/project", 42)
	if err == nil {
		t.Fatal("expected error after exhausted retries")
	}
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindTransient {
		t.Errorf("expected KindTransient, got %v", err)
	}
	if len(stub.requests) != 3 {
		t.Errorf("expected 3 requests, got %d", len(stub.requests))
	}
}

func TestFetchChanges_InvalidArgs(t *testing.T) {
	c := newTestClient(t, "http://unused")
	if _, err := c.FetchChanges(context.Background(), "", 1); err == nil {
		t.Error("expected error for empty project")
	}
	if _, err := c.FetchChanges(context.Background(), "ok", 0); err == nil {
		t.Error("expected error for zero iid")
	}
}

// TestReadResponseBody_Oversized asserts that readResponseBody caps
// the read at 64 KiB even when the server sends a much larger body,
// so a hostile GitLab error page can't OOM the process.
func TestReadResponseBody_Oversized(t *testing.T) {
	big := strings.Repeat("X", 200*1024)
	rc := io.NopCloser(strings.NewReader(big))
	got := readResponseBody(rc)
	if len(got) > 65*1024 {
		t.Errorf("readResponseBody returned %d bytes, expected <= 65536", len(got))
	}
}

// TestReadResponseBody_Nil verifies the nil-body path returns "".
func TestReadResponseBody_Nil(t *testing.T) {
	if got := readResponseBody(nil); got != "" {
		t.Errorf("readResponseBody(nil) = %q, want empty", got)
	}
}

// helper: ensure ChangeFile projection is stable JSON
func TestChangeFileJSONRoundtrip(t *testing.T) {
	in := ChangeFile{OldPath: "a.go", NewPath: "b.go", RenamedFile: true, Diff: "@@ -1 +1 @@\n-x\n+y\n"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out ChangeFile
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("roundtrip mismatch:\n in: %+v\nout: %+v", in, out)
	}
}

// helper: ensure MergeRequest projection is stable JSON
func TestMergeRequestJSONRoundtrip(t *testing.T) {
	in := MergeRequest{
		IID:          42,
		Title:        "t",
		Description:  "d",
		State:        "opened",
		SourceBranch: "a",
		TargetBranch: "b",
		Author:       User{Username: "u", Name: "n"},
		WebURL:       "https://x",
		DiffRefs:     DiffRefs{BaseSHA: "1", HeadSHA: "2", StartSHA: "3"},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out MergeRequest
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("roundtrip mismatch:\n in: %+v\nout: %+v", in, out)
	}
}
