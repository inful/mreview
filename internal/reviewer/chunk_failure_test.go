package reviewer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/llm"
)

// flakyLLM is an LLM stub whose first `failNTimes` calls return a
// transient error (wrapping context.DeadlineExceeded, the only
// transient our classifier recognises). Calls after that return
// success.
//
// Used by the retry tests to confirm the chunk-level retry loop
// catches transient blips without the reviewer bubbling the
// failure. We model the transient as a timeout — in production the
// chunk-level retry catches the timeout case after openai-go's own
// 5xx retry layer has already exhausted.
type flakyLLM struct {
	calls     atomic.Int32
	failTimes int32 // first N calls return a transient error
	bodies    []string
}

func newFlakyLLM(t *testing.T, failTimes int32, okBody string) *flakyLLM {
	t.Helper()
	return &flakyLLM{failTimes: failTimes, bodies: []string{okBody}}
}

func (f *flakyLLM) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	n := f.calls.Add(1)
	if n <= f.failTimes {
		return nil, fmt.Errorf("llm: chat completion: %w", context.DeadlineExceeded)
	}
	// Return a valid openai-go-shaped body.
	return &llm.ChatResponse{
		Content: f.bodies[0],
		Model:   "m",
	}, nil
}

// TestReviewMR_ChunkFailure_Atomic confirms that when one chunk's
// LLM call fails AND AllowPartial is false (the default), the
// reviewer fails the whole review: ReviewMR returns *ChunkFailureError,
// and no summary note or inline discussion is posted to GitLab.
//
// This is the primary acceptance criterion of issue #31.
func TestReviewMR_ChunkFailure_Atomic(t *testing.T) {
	g := newFakeGitLab(t)
	g.enqueue(http.StatusOK, mrFixture)
	g.enqueue(http.StatusOK, twoFilesFixture) // 2 chunks → 2 LLM calls
	// ListDiscussions, PostSummary, PostDiscussion must NOT be
	// called after the chunk failure. If anything queues a 4th
	// response here, the test fails — the reviewer's request
	// counter would exceed expectations.

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	glt, _ := gitlab.NewClient(g.URL, "test-token", gitlab.RetryConfig{}, logger)
	r, err := NewReviewer(Config{
		GitLab:       glt,
		LLM:          newFlakyLLM(t, 100, `{"findings":[],"summary":"ok"}`), // every call fails
		Model:        "m",
		MaxDiffBytes: 4096,
		Logger:       logger,
		ChunkRetries: 0, // no retries — first failure is final
		AllowPartial: false,
	})
	if err != nil {
		t.Fatalf("NewReviewer: %v", err)
	}

	result, err := r.ReviewMR(context.Background(), "group/project", 42)
	if err == nil {
		t.Fatal("expected error from ReviewMR, got nil")
	}
	if result != nil {
		t.Errorf("expected nil result, got %+v", result)
	}
	var cfe *ChunkFailureError
	if !errors.As(err, &cfe) {
		t.Errorf("expected *ChunkFailureError, got %T: %v", err, err)
	} else {
		if cfe.Batch != 1 {
			t.Errorf("Batch = %d, want 1", cfe.Batch)
		}
		if len(cfe.Files) == 0 || cfe.Files[0] != "a.go" {
			t.Errorf("Files = %v, want [a.go]", cfe.Files)
		}
	}
	// Error message must include chunk context for operators.
	msg := err.Error()
	if !strings.Contains(msg, "a.go") {
		t.Errorf("error message missing file path: %q", msg)
	}
	if !strings.Contains(msg, "batch 1") {
		t.Errorf("error message missing batch index: %q", msg)
	}
	// Verify NO POSTs hit GitLab: only the 2 GETs (FetchMR + FetchChanges).
	for i, req := range g.requests {
		if req.Method == http.MethodPost {
			t.Errorf("unexpected POST request #%d: %s %s", i, req.Method, req.Path)
		}
	}
}

// TestReviewMR_AllChunksFail_Atomic confirms that when every
// chunk fails, the first failure aborts the loop — we don't keep
// hammering the LLM through subsequent chunks.
//
// With the default ChunkRetries=1 (1 retry, 2 total attempts per
// chunk), batch 1 gets 2 attempts, batch 2 is never reached.
func TestReviewMR_AllChunksFail_Atomic(t *testing.T) {
	g := newFakeGitLab(t)
	g.enqueue(http.StatusOK, mrFixture)
	g.enqueue(http.StatusOK, twoFilesFixture)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	glt, _ := gitlab.NewClient(g.URL, "test-token", gitlab.RetryConfig{}, logger)
	flaky := newFlakyLLM(t, 100, `{"findings":[],"summary":"ok"}`)
	r, err := NewReviewer(Config{
		GitLab:       glt,
		LLM:          flaky,
		Model:        "m",
		MaxDiffBytes: 4096,
		Logger:       logger,
		// ChunkRetries left 0 → resolves to default 1 (one retry).
	})
	if err != nil {
		t.Fatalf("NewReviewer: %v", err)
	}

	_, err = r.ReviewMR(context.Background(), "group/project", 42)
	if err == nil {
		t.Fatal("expected error from ReviewMR")
	}
	// Default ChunkRetries=1 → 2 attempts on chunk 1 (both fail),
	// then atomic-failure aborts — never reaches chunk 2.
	if got := flaky.calls.Load(); got > 2 {
		t.Errorf("expected ≤2 LLM calls (chunk 1 retries then abort), got %d", got)
	}
}

// TestReviewMR_ChunkFailure_AllowPartial_RestoresLegacyBehaviour
// confirms the --allow-partial escape hatch: when set, the reviewer
// keeps the historical "log warn + substitute empty response"
// behaviour, the review continues, and the summary note posts.
func TestReviewMR_ChunkFailure_AllowPartial_RestoresLegacyBehaviour(t *testing.T) {
	g := newFakeGitLab(t)
	g.enqueue(http.StatusOK, mrFixture)
	g.enqueue(http.StatusOK, twoFilesFixture)
	g.enqueue(http.StatusOK, "[]") // ListDiscussions
	g.enqueue(http.StatusCreated, `{"id":1,"body":"summary","author":{"id":1,"username":"bot"},"system":false}`)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	glt, _ := gitlab.NewClient(g.URL, "test-token", gitlab.RetryConfig{}, logger)
	// The flaky LLM fails the chunk LLM calls (each call returns
	// context.DeadlineExceeded) but the merge LLM succeeds when
	// reached. Under AllowPartial=true the review must keep
	// going: chunk 1 fails (allow partial substitutes empty),
	// chunk 2 also fails, merge succeeds.
	//
	// With 2 chunks and default ChunkRetries=1, the LLM is called
	// 2 attempts per chunk = 4 chunk calls + 1 merge call = 5.
	// failTimes=4 covers all chunk attempts and lets call #5
	// (the merge) succeed.
	r, err := NewReviewer(Config{
		GitLab:       glt,
		LLM:          newFlakyLLM(t, 4, `{"findings":[],"summary":"merged"}`),
		Model:        "m",
		MaxDiffBytes: 4096,
		Logger:       logger,
		AllowPartial: true, // ← escape hatch
	})
	if err != nil {
		t.Fatalf("NewReviewer: %v", err)
	}

	if _, err := r.ReviewMR(context.Background(), "group/project", 42); err != nil {
		t.Fatalf("ReviewMR should succeed under AllowPartial when merge succeeds, got %v", err)
	}

	// Summary must have been posted (legacy behaviour).
	sawSummaryPost := false
	for _, req := range g.requests {
		if req.Method == http.MethodPost && strings.Contains(req.Path, "/notes") {
			sawSummaryPost = true
		}
	}
	if !sawSummaryPost {
		t.Errorf("expected summary POST in AllowPartial mode; requests=%+v", g.requests)
	}
	// The chunk-review-failed WARN must carry batch + file context
	// so operators can identify which chunk was dropped without
	// re-reading every prompt (issue #14 visibility). Both chunks
	// fail here (the flaky LLM exhausts retries on every call),
	// so both file paths must surface — once for batch 1 and
	// once for batch 2.
	logs := logBuf.String()
	if !strings.Contains(logs, "chunk review failed") {
		t.Errorf("expected the legacy WARN log; got:\n%s", logs)
	}
	if !strings.Contains(logs, "batch=1") {
		t.Errorf("expected batch=1 in warn log (first chunk failed), got:\n%s", logs)
	}
	if !strings.Contains(logs, "batch=2") {
		t.Errorf("expected batch=2 in warn log (second chunk failed), got:\n%s", logs)
	}
	if !strings.Contains(logs, "files=a.go") {
		t.Errorf("expected files=a.go in warn log (first chunk's file), got:\n%s", logs)
	}
	if !strings.Contains(logs, "files=b.go") {
		t.Errorf("expected files=b.go in warn log (second chunk's file), got:\n%s", logs)
	}
}

// TestReviewMR_ChunkFailure_RetryThenSuccess confirms the chunk-
// level retry mechanism (#17): when the first attempt fails but a
// subsequent attempt succeeds, the review proceeds normally with
// no error and a posted summary.
func TestReviewMR_ChunkFailure_RetryThenSuccess(t *testing.T) {
	g := newFakeGitLab(t)
	g.enqueue(http.StatusOK, mrFixture)
	g.enqueue(http.StatusOK, `[
		{"old_path":"a.go","new_path":"a.go","new_file":false,"deleted_file":false,"renamed_file":false,"diff":"@@ -1 +1 @@\n-old\n+new\n"}
	]`) // 1 file → 1 chunk
	g.enqueue(http.StatusOK, "[]") // ListDiscussions
	g.enqueue(http.StatusCreated, `{"id":1,"body":"summary","author":{"id":1,"username":"bot"},"system":false}`)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	glt, _ := gitlab.NewClient(g.URL, "test-token", gitlab.RetryConfig{}, logger)
	flaky := newFlakyLLM(t, 1, `{"findings":[{"file":"a.go","line":1,"severity":"info","category":"style","body":"x"}],"summary":"ok"}`)
	r, err := NewReviewer(Config{
		GitLab:       glt,
		LLM:          flaky,
		Model:        "m",
		MaxDiffBytes: 4096,
		Logger:       logger,
		ChunkRetries: 3, // plenty of budget
	})
	if err != nil {
		t.Fatalf("NewReviewer: %v", err)
	}

	if _, err := r.ReviewMR(context.Background(), "group/project", 42); err != nil {
		t.Fatalf("ReviewMR should succeed when retry succeeds, got %v", err)
	}
	// 2 LLM calls: 1 failed + 1 succeeded.
	if got := flaky.calls.Load(); got != 2 {
		t.Errorf("expected 2 LLM calls (1 fail + 1 retry), got %d", got)
	}
	// Verify the summary POSTed.
	sawSummaryPost := false
	for _, req := range g.requests {
		if req.Method == http.MethodPost && strings.Contains(req.Path, "/notes") {
			sawSummaryPost = true
		}
	}
	if !sawSummaryPost {
		t.Errorf("summary should post after recovery; requests=%+v", g.requests)
	}
}

// TestReviewMR_ConsolidateFallback_GatedByChunkFailure confirms
// that when a chunk fails (AllowPartial=false default) and the
// merge LLM call ALSO fails, the review returns an error rather
// than silently falling back to the "concatenate per-chunk
// summaries" path.
func TestReviewMR_ConsolidateFallback_GatedByChunkFailure(t *testing.T) {
	g := newFakeGitLab(t)
	g.enqueue(http.StatusOK, mrFixture)
	g.enqueue(http.StatusOK, `[
		{"old_path":"a.go","new_path":"a.go","new_file":false,"deleted_file":false,"renamed_file":false,"diff":"@@ -1 +1 @@\n-old\n+new\n"},
		{"old_path":"b.go","new_path":"b.go","new_file":false,"deleted_file":false,"renamed_file":false,"diff":"@@ -1 +1 @@\n-foo\n+bar\n"}
	]`)
	// ListDiscussions, PostSummary, PostDiscussion should NOT fire
	// (chunk 1 fails atomically → no posts).

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	glt, _ := gitlab.NewClient(g.URL, "test-token", gitlab.RetryConfig{}, logger)
	r, err := NewReviewer(Config{
		GitLab:       glt,
		LLM:          newFlakyLLM(t, 100, `{"findings":[],"summary":"ok"}`),
		Model:        "m",
		MaxDiffBytes: 4096,
		Logger:       logger,
		ChunkRetries: 0,
	})
	if err != nil {
		t.Fatalf("NewReviewer: %v", err)
	}

	_, err = r.ReviewMR(context.Background(), "group/project", 42)
	if err == nil {
		t.Fatal("expected error from ReviewMR (atomic failure on chunk)")
	}
	// Confirm we don't even get to the merge step.
	for i, req := range g.requests {
		if req.Method == http.MethodPost {
			t.Errorf("unexpected POST after atomic failure #%d: %s %s", i, req.Method, req.Path)
		}
	}
}

// TestIsTransientLLMError pins the classification used by the retry
// loop. Only context.DeadlineExceeded is treated as transient:
// context.Canceled (operator SIGINT) and parse failures propagate
// without retry.
func TestIsTransientLLMError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context.DeadlineExceeded", context.DeadlineExceeded, true},
		{"wrapped DeadlineExceeded", errWrap("llm: chat", context.DeadlineExceeded), true},
		{"context.Canceled", context.Canceled, false},
		{"wrapped Canceled", errWrap("llm: chat", context.Canceled), false},
		{"generic error", errors.New("500 Internal Server Error"), false},
		{"parse error", errors.New("parse: bad json"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientLLMError(tc.err); got != tc.want {
				t.Errorf("isTransientLLMError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// errWrap is a tiny helper to make test cases concise.
func errWrap(prefix string, cause error) error {
	return &wrappedErr{prefix: prefix, cause: cause}
}

type wrappedErr struct {
	prefix string
	cause  error
}

func (e *wrappedErr) Error() string { return e.prefix + ": " + e.cause.Error() }
func (e *wrappedErr) Unwrap() error { return e.cause }

// TestChunkFailureError_Message pins the error format operators see
// in logs / CLI output: must include batch index, file list, retry
// count, and underlying error.
func TestChunkFailureError_Message(t *testing.T) {
	e := &ChunkFailureError{
		Batch:    3,
		Files:    []string{"foo.go", "bar.go"},
		Attempts: 4,
		Cause:    errors.New("timeout"),
	}
	msg := e.Error()
	for _, want := range []string{"batch 3", "foo.go", "bar.go", "4 attempts", "timeout"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q: %q", want, msg)
		}
	}
	// Cause is reachable for errors.Is/As.
	if !errors.Is(e, e.Cause) {
		t.Errorf("errors.Is should reach Cause")
	}
}

// TestConfig_ChunkRetriesDefault verifies that NewReviewer sets
// ChunkRetries=1 when the caller leaves it zero, and that AllowPartial
// defaults to false.
func TestConfig_ChunkRetriesDefault(t *testing.T) {
	// Default path: ChunkRetries=0 → set to 1 by NewReviewer.
	r, err := NewReviewer(Config{
		GitLab:       &gitlab.Client{},
		LLM:          stubProvider{},
		Model:        "m",
		MaxDiffBytes: 1,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewReviewer: %v", err)
	}
	if r.cfg.ChunkRetries != 1 {
		t.Errorf("ChunkRetries = %d, want 1 (default)", r.cfg.ChunkRetries)
	}
	if r.cfg.AllowPartial {
		t.Errorf("AllowPartial = true, want false (default)")
	}
	// Negative ChunkRetries is rejected.
	if _, err := NewReviewer(Config{
		GitLab:       &gitlab.Client{},
		LLM:          stubProvider{},
		Model:        "m",
		MaxDiffBytes: 1,
		ChunkRetries: -1,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}); err == nil {
		t.Errorf("expected error for negative ChunkRetries")
	}
}

// stubProvider is a no-op llm.Provider used by config-validation
// tests that don't exercise Chat.
type stubProvider struct{}

func (stubProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{}, nil
}

// _ ensures time package is referenced (used in other tests in the
// suite; included here so gofmt doesn't strip it).
var _ = time.Second
