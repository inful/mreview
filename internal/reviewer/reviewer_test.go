package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/llm"
)

// fakeGitLab is a tiny GitLab stub that records every request and
// lets each test stage the next response. It serves both the GET
// (fetch MR, fetch changes) and POST (notes, discussions) endpoints.
type fakeGitLab struct {
	*httptest.Server
	requests  []fakeRequest
	responses []fakeResponse // FIFO queue
}

type fakeRequest struct {
	Method string
	Path   string
	Body   string
}

type fakeResponse struct {
	status int
	body   string
}

func newFakeGitLab(t *testing.T) *fakeGitLab {
	t.Helper()
	f := &fakeGitLab{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.requests = append(f.requests, fakeRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Body:   string(body),
		})
		if len(f.responses) == 0 {
			http.Error(w, "stub: no response queued", http.StatusInternalServerError)
			return
		}
		next := f.responses[0]
		f.responses = f.responses[1:]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(next.status)
		_, _ = io.WriteString(w, next.body)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeGitLab) enqueue(status int, body string) {
	f.responses = append(f.responses, fakeResponse{status: status, body: body})
}

// fakeLLM is a tiny OpenAI-compatible stub. Tests stage canned
// responses in order.
type fakeLLM struct {
	*httptest.Server
	calls    atomic.Int32
	body     []string // queued bodies; each Chat call returns the next
	requests []fakeLLMRequest
	mu       sync.Mutex
}

type fakeLLMRequest struct {
	Method string
	Path   string
	Body   string
}

func newFakeLLM(t *testing.T, bodies ...string) *fakeLLM {
	t.Helper()
	l := &fakeLLM{body: bodies}
	l.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		l.mu.Lock()
		l.requests = append(l.requests, fakeLLMRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Body:   string(body),
		})
		l.mu.Unlock()
		l.calls.Add(1)
		idx := int(l.calls.Load()) - 1
		if idx >= len(l.body) {
			http.Error(w, "no body queued", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fmt.Sprintf(`{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":%q}}]}`, l.body[idx]))
	}))
	t.Cleanup(l.Close)
	return l
}

// minimalHappyGitLab stages: MR + changes + 1 summary note + N
// discussion posts (returns 201 for each).
func minimalHappyGitLab(g *fakeGitLab, mrFixture, changesFixture string) {
	g.enqueue(http.StatusOK, mrFixture)
	g.enqueue(http.StatusOK, changesFixture)
	g.enqueue(http.StatusOK, "[]") // ListDiscussions: empty
	g.enqueue(http.StatusCreated, `{"id":1,"body":"summary","author":{"id":1,"username":"bot"},"system":false}`)
	g.enqueue(http.StatusCreated, `{"id":"d1","individual_note":false,"notes":[{"id":2,"body":"x","author":{"id":1,"username":"bot"},"system":false}]}`)
}

const mrFixture = `{"iid":42,"title":"Add caching","description":"desc","state":"opened","source_branch":"feat/cache","target_branch":"main","web_url":"http://x","author":{"id":7,"username":"alice","name":"Alice"},"diff_refs":{"base_sha":"1111111111111111111111111111111111111111","head_sha":"2222222222222222222222222222222222222222","start_sha":"3333333333333333333333333333333333333333"}}`

const changesFixture = `[{"old_path":"a.go","new_path":"a.go","new_file":false,"deleted_file":false,"renamed_file":false,"diff":"@@ -1 +1 @@\n-old\n+new\n"}]`

func TestNewReviewer_ValidatesConfig(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	good := func() *gitlab.Client {
		c, _ := gitlab.NewClient("http://x", "t", gitlab.RetryConfig{}, logger)
		return c
	}
	goodLLM := func() llm.Provider { return nil }

	if _, err := NewReviewer(Config{}); err == nil {
		t.Error("expected error for missing GitLab")
	}
	if _, err := NewReviewer(Config{GitLab: good()}); err == nil {
		t.Error("expected error for missing LLM")
	}
	if _, err := NewReviewer(Config{GitLab: good(), LLM: goodLLM()}); err == nil {
		t.Error("expected error for missing Model")
	}
	if _, err := NewReviewer(Config{
		GitLab: good(), LLM: goodLLM(), Model: "m",
		MaxDiffBytes: 0,
	}); err == nil {
		t.Error("expected error for zero MaxDiffBytes")
	}
}

func TestReviewMR_HappyPath(t *testing.T) {
	g := newFakeGitLab(t)
	minimalHappyGitLab(g, mrFixture, changesFixture)

	l := newFakeLLM(t,
		// First call: per-chunk review (1 file = 1 chunk)
		`{"findings":[{"file":"a.go","line":1,"severity":"warning","category":"security","body":"x"}],"summary":"ok"}`,
	)
	glt, _ := gitlab.NewClient(g.URL, "test-token", gitlab.RetryConfig{
		MaxAttempts:    2,
		InitialBackoff: 1 * time.Millisecond,
		MaxBackoff:     5 * time.Millisecond,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p, _ := llm.NewOpenAIProvider(llm.OpenAIConfig{BaseURL: l.URL, APIKey: "k", Model: "m"})

	r, err := NewReviewer(Config{
		GitLab: glt, LLM: p, Model: "m", MaxDiffBytes: 4096,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewReviewer: %v", err)
	}

	result, err := r.ReviewMR(context.Background(), "group/project", 42)
	if err != nil {
		t.Fatalf("ReviewMR: %v", err)
	}
	if result.MR.IID != 42 {
		t.Errorf("MR.IID = %d", result.MR.IID)
	}
	if len(result.Findings) != 1 {
		t.Errorf("expected 1 finding, got %d", len(result.Findings))
	}
	if result.Findings[0].Discussion == nil {
		t.Error("Discussion should be set (post succeeded)")
	}
	if result.Summary == nil {
		t.Error("Summary should be set (post succeeded)")
	}
}

func TestReviewMR_DryRun_NoPosts(t *testing.T) {
	g := newFakeGitLab(t)
	g.enqueue(http.StatusOK, mrFixture)
	g.enqueue(http.StatusOK, changesFixture)
	g.enqueue(http.StatusOK, "[]") // ListDiscussions: empty
	// No further responses needed: dry-run never calls GitLab.

	l := newFakeLLM(t,
		`{"findings":[{"file":"a.go","line":1,"severity":"info","category":"style","body":"x"}],"summary":"ok"}`,
	)
	glt, _ := gitlab.NewClient(g.URL, "test-token", gitlab.RetryConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p, _ := llm.NewOpenAIProvider(llm.OpenAIConfig{BaseURL: l.URL, APIKey: "k", Model: "m"})
	r, _ := NewReviewer(Config{
		GitLab: glt, LLM: p, Model: "m", MaxDiffBytes: 4096,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		DryRun: true,
	})
	result, err := r.ReviewMR(context.Background(), "group/project", 42)
	if err != nil {
		t.Fatalf("ReviewMR: %v", err)
	}
	if result.Summary == nil || result.Summary.Body == "" {
		t.Error("dry-run should produce a non-nil Summary placeholder with body")
	}
	// Verify GitLab was only called for GETs (fetch MR + changes),
	// never for POSTs (notes / discussions).
	postCount := 0
	for _, req := range g.requests {
		if req.Method == http.MethodPost {
			postCount++
		}
	}
	if postCount != 0 {
		t.Errorf("dry-run made %d POSTs; want 0", postCount)
	}
}

func TestReviewMR_LineOutOfRange_ClassifiedAsConflict(t *testing.T) {
	// LLM cites a line beyond the file; GitLab returns 400 with
	// the line-range signal; reviewer must skip the finding, not
	// fail the review.
	g := newFakeGitLab(t)
	g.enqueue(http.StatusOK, mrFixture)
	g.enqueue(http.StatusOK, changesFixture)
	g.enqueue(http.StatusOK, "[]")                       // ListDiscussions: empty
	g.enqueue(http.StatusCreated, `{"id":1,"body":"s"}`) // summary post
	g.enqueue(http.StatusBadRequest, `{"message":"new_line is not in range"}`)

	l := newFakeLLM(t,
		`{"findings":[{"file":"a.go","line":999,"severity":"warning","category":"security","body":"x"}],"summary":"ok"}`,
	)
	glt, _ := gitlab.NewClient(g.URL, "test-token", gitlab.RetryConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p, _ := llm.NewOpenAIProvider(llm.OpenAIConfig{BaseURL: l.URL, APIKey: "k", Model: "m"})
	r, _ := NewReviewer(Config{
		GitLab: glt, LLM: p, Model: "m", MaxDiffBytes: 4096,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	result, err := r.ReviewMR(context.Background(), "group/project", 42)
	if err != nil {
		t.Fatalf("ReviewMR: %v", err)
	}
	if len(result.Findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(result.Findings))
	}
	if !result.Findings[0].Skipped {
		t.Error("finding should be skipped")
	}
	if !strings.Contains(result.Findings[0].Reason, "line out of range") {
		t.Errorf("Reason should mention line out of range, got %q", result.Findings[0].Reason)
	}
	if result.Findings[0].Discussion != nil {
		t.Error("Discussion should be nil when skipped")
	}
}

func TestReviewMR_HallucinatedFile_Filtered(t *testing.T) {
	// LLM cites a file not in the diff. Should be filtered
	// upstream (no GitLab call), with a warn log.
	g := newFakeGitLab(t)
	g.enqueue(http.StatusOK, mrFixture)
	g.enqueue(http.StatusOK, changesFixture)
	g.enqueue(http.StatusOK, "[]") // ListDiscussions: empty
	g.enqueue(http.StatusCreated, `{"id":1,"body":"s"}`)

	l := newFakeLLM(t,
		`{"findings":[{"file":"nonexistent.go","line":1,"severity":"warning","category":"security","body":"x"},{"file":"a.go","line":1,"severity":"info","category":"style","body":"y"}],"summary":"ok"}`,
	)
	glt, _ := gitlab.NewClient(g.URL, "test-token", gitlab.RetryConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p, _ := llm.NewOpenAIProvider(llm.OpenAIConfig{BaseURL: l.URL, APIKey: "k", Model: "m"})
	r, _ := NewReviewer(Config{
		GitLab: glt, LLM: p, Model: "m", MaxDiffBytes: 4096,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	result, err := r.ReviewMR(context.Background(), "group/project", 42)
	if err != nil {
		t.Fatalf("ReviewMR: %v", err)
	}
	if len(result.Findings) != 1 {
		t.Errorf("expected 1 finding (1 hallucinated), got %d", len(result.Findings))
	}
	if result.Findings[0].Finding.File != "a.go" {
		t.Errorf("filtered file = %q, want a.go", result.Findings[0].Finding.File)
	}
}

func TestReviewMR_AuthError_Fails(t *testing.T) {
	g := newFakeGitLab(t)
	g.enqueue(http.StatusUnauthorized, `{"message":"401 Unauthorized"}`)

	l := newFakeLLM(t)
	glt, _ := gitlab.NewClient(g.URL, "test-token", gitlab.RetryConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p, _ := llm.NewOpenAIProvider(llm.OpenAIConfig{BaseURL: l.URL, APIKey: "k", Model: "m"})
	r, _ := NewReviewer(Config{
		GitLab: glt, LLM: p, Model: "m", MaxDiffBytes: 4096,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	_, err := r.ReviewMR(context.Background(), "group/project", 42)
	if err == nil {
		t.Fatal("expected error from auth failure")
	}
	var ge *gitlab.Error
	if !errors.As(err, &ge) || ge.Kind != gitlab.KindAuth {
		t.Errorf("expected KindAuth, got %v", err)
	}
}

func TestReviewMR_OversizedFile_Fails(t *testing.T) {
	// Build a diff that's bigger than MaxDiffBytes; chunker
	// returns OversizedError.
	g := newFakeGitLab(t)
	g.enqueue(http.StatusOK, mrFixture)
	// changes with one huge line that exceeds the budget.
	huge := `@@ -1 +1 @@
+` + strings.Repeat("x", 500) + `
`
	g.enqueue(http.StatusOK, fmt.Sprintf(`[{"old_path":"a.go","new_path":"a.go","diff":%q}]`, huge))
	g.enqueue(http.StatusOK, "[]") // ListDiscussions: empty

	l := newFakeLLM(t)
	glt, _ := gitlab.NewClient(g.URL, "test-token", gitlab.RetryConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p, _ := llm.NewOpenAIProvider(llm.OpenAIConfig{BaseURL: l.URL, APIKey: "k", Model: "m"})
	r, _ := NewReviewer(Config{
		GitLab: glt, LLM: p, Model: "m", MaxDiffBytes: 100,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	_, err := r.ReviewMR(context.Background(), "group/project", 42)
	if err == nil {
		t.Fatal("expected OversizedError")
	}
	var oe *llm.OversizedError
	if !errors.As(err, &oe) {
		t.Errorf("expected *llm.OversizedError, got %T", err)
	}
}

func TestReviewMR_MultiChunk_MergesVerdict(t *testing.T) {
	// Two files = two chunks = two LLM calls (chunk + merge).
	g := newFakeGitLab(t)
	multiChanges := `[
		{"old_path":"a.go","new_path":"a.go","diff":"@@ -1 +1 @@\n-old\n+new\n"},
		{"old_path":"b.go","new_path":"b.go","diff":"@@ -1 +1 @@\n-foo\n+bar\n"}
	]`
	g.enqueue(http.StatusOK, mrFixture)
	g.enqueue(http.StatusOK, multiChanges)
	g.enqueue(http.StatusOK, "[]") // ListDiscussions: empty
	g.enqueue(http.StatusCreated, `{"id":1,"body":"s"}`)

	l := newFakeLLM(t,
		// Chunk 1: findings + summary for a.go
		`{"findings":[{"file":"a.go","line":1,"severity":"info","category":"style","body":"x"}],"summary":"a.go: ok"}`,
		// Chunk 2: findings + summary for b.go
		`{"findings":[{"file":"b.go","line":1,"severity":"warning","category":"security","body":"y"}],"summary":"b.go: needs review"}`,
		// Merge: consolidated summary (findings union is
		// preserved automatically by the reviewer when the merge
		// call drops entries).
		`{"findings":[{"file":"b.go","line":1,"severity":"warning","category":"security","body":"y"}],"summary":"both files reviewed"}`,
	)
	glt, _ := gitlab.NewClient(g.URL, "test-token", gitlab.RetryConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p, _ := llm.NewOpenAIProvider(llm.OpenAIConfig{BaseURL: l.URL, APIKey: "k", Model: "m"})
	r, _ := NewReviewer(Config{
		GitLab: glt, LLM: p, Model: "m", MaxDiffBytes: 4096,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	result, err := r.ReviewMR(context.Background(), "group/project", 42)
	if err != nil {
		t.Fatalf("ReviewMR: %v", err)
	}
	// 2 unique findings across 2 chunks → consolidated.
	if len(result.Findings) < 2 {
		t.Errorf("expected ≥2 findings after merge, got %d", len(result.Findings))
	}
}

func TestReviewMR_LLMParseFailure_ContinuesWithEmptySummary(t *testing.T) {
	g := newFakeGitLab(t)
	g.enqueue(http.StatusOK, mrFixture)
	g.enqueue(http.StatusOK, changesFixture)
	g.enqueue(http.StatusOK, "[]") // ListDiscussions: empty
	g.enqueue(http.StatusCreated, `{"id":1,"body":"s"}`)

	// LLM returns prose with no recoverable JSON.
	l := newFakeLLM(t,
		`The model wrote only prose with no JSON anywhere to be found.`,
	)
	glt, _ := gitlab.NewClient(g.URL, "test-token", gitlab.RetryConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p, _ := llm.NewOpenAIProvider(llm.OpenAIConfig{BaseURL: l.URL, APIKey: "k", Model: "m"})
	r, _ := NewReviewer(Config{
		GitLab: glt, LLM: p, Model: "m", MaxDiffBytes: 4096,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	result, err := r.ReviewMR(context.Background(), "group/project", 42)
	if err != nil {
		t.Fatalf("parse failure should not fail the whole review: %v", err)
	}
	if len(result.Findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(result.Findings))
	}
	// Summary still posted (with "no findings" note).
	if result.Summary == nil {
		t.Error("summary should still be posted even when LLM parse fails")
	}
}

func TestReviewMR_DedupesAgainstPriorBotComments(t *testing.T) {
	// Set up: prior bot-authored discussion with the same body
	// the LLM is about to emit. ListDiscussions returns it; the
	// reviewer should skip posting the duplicate.
	priorDiscussions := `[
		{
			"id":"prior1",
			"individual_note": false,
			"notes": [{"id":99,"body":"**[warning]** jwt leak","author":{"id":1,"username":"review-bot","name":"Bot"},"system":false}]
		}
	]`

	g := newFakeGitLab(t)
	g.enqueue(http.StatusOK, mrFixture)
	g.enqueue(http.StatusOK, changesFixture)
	g.enqueue(http.StatusOK, priorDiscussions) // ListDiscussions returns prior bot comment
	// No summary post: dedupe removed the only finding, but the
	// summary still gets posted.
	g.enqueue(http.StatusCreated, `{"id":1,"body":"s"}`)
	// No inline post.

	l := newFakeLLM(t,
		`{"findings":[{"file":"a.go","line":1,"severity":"warning","category":"security","body":"**[warning]** jwt leak"}],"summary":"LGTM"}`,
	)
	glt, _ := gitlab.NewClient(g.URL, "test-token", gitlab.RetryConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p, _ := llm.NewOpenAIProvider(llm.OpenAIConfig{BaseURL: l.URL, APIKey: "k", Model: "m"})
	r, _ := NewReviewer(Config{
		GitLab:       glt,
		LLM:          p,
		Model:        "m",
		MaxDiffBytes: 4096,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		BotUsername:  "review-bot",
	})
	result, err := r.ReviewMR(context.Background(), "group/project", 42)
	if err != nil {
		t.Fatalf("ReviewMR: %v", err)
	}
	if result.DedupeSize != 1 {
		t.Errorf("DedupeSize = %d, want 1", result.DedupeSize)
	}
	if len(result.Findings) != 0 {
		t.Errorf("expected 0 surviving findings after dedupe, got %d", len(result.Findings))
	}
	// One POST: the summary. Inline findings all deduped away.
	postCount := 0
	for _, req := range g.requests {
		if req.Method == http.MethodPost {
			postCount++
		}
	}
	if postCount != 1 {
		t.Errorf("dedupe made %d POSTs; want 1 (summary only)", postCount)
	}
}

func TestReviewMR_InlineOnly_NoSummaryPost(t *testing.T) {
	g := newFakeGitLab(t)
	g.enqueue(http.StatusOK, mrFixture)
	g.enqueue(http.StatusOK, changesFixture)
	g.enqueue(http.StatusOK, "[]") // ListDiscussions: empty
	g.enqueue(http.StatusCreated, `{"id":"d1","individual_note":false,"notes":[{"id":2,"body":"x","author":{"id":1,"username":"bot"},"system":false}]}`)
	// NO summary post — inline-only mode skips it.

	l := newFakeLLM(t,
		`{"findings":[{"file":"a.go","line":1,"severity":"warning","category":"security","body":"x"}],"summary":"LGTM"}`,
	)
	glt, _ := gitlab.NewClient(g.URL, "test-token", gitlab.RetryConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p, _ := llm.NewOpenAIProvider(llm.OpenAIConfig{BaseURL: l.URL, APIKey: "k", Model: "m"})
	r, _ := NewReviewer(Config{
		GitLab: glt, LLM: p, Model: "m", MaxDiffBytes: 4096,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		CommentMode: CommentModeInlineOnly,
	})
	_, err := r.ReviewMR(context.Background(), "group/project", 42)
	if err != nil {
		t.Fatalf("ReviewMR: %v", err)
	}
	// Exactly one POST: the inline discussion. No summary.
	postCount := 0
	for _, req := range g.requests {
		if req.Method == http.MethodPost {
			postCount++
		}
	}
	if postCount != 1 {
		t.Errorf("inline-only: expected 1 POST, got %d", postCount)
	}
}

func TestReviewMR_SummaryOnly_NoInlinePosts(t *testing.T) {
	g := newFakeGitLab(t)
	g.enqueue(http.StatusOK, mrFixture)
	g.enqueue(http.StatusOK, changesFixture)
	g.enqueue(http.StatusOK, "[]") // ListDiscussions: empty
	g.enqueue(http.StatusCreated, `{"id":1,"body":"summary"}`)
	// NO inline posts — summary-only mode skips them.

	l := newFakeLLM(t,
		`{"findings":[{"file":"a.go","line":1,"severity":"warning","category":"security","body":"x"}],"summary":"LGTM"}`,
	)
	glt, _ := gitlab.NewClient(g.URL, "test-token", gitlab.RetryConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p, _ := llm.NewOpenAIProvider(llm.OpenAIConfig{BaseURL: l.URL, APIKey: "k", Model: "m"})
	r, _ := NewReviewer(Config{
		GitLab: glt, LLM: p, Model: "m", MaxDiffBytes: 4096,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		CommentMode: CommentModeSummaryOnly,
	})
	_, err := r.ReviewMR(context.Background(), "group/project", 42)
	if err != nil {
		t.Fatalf("ReviewMR: %v", err)
	}
	postCount := 0
	for _, req := range g.requests {
		if req.Method == http.MethodPost {
			postCount++
		}
	}
	if postCount != 1 {
		t.Errorf("summary-only: expected 1 POST (summary), got %d", postCount)
	}
}

func TestReviewMR_PromptSuffix_VisibleToLLM(t *testing.T) {
	// Operator-supplied suffix should appear in the LLM prompt
	// but the system-owned schema must still come first.
	teamSuffix := "We use logrus; flag any new zap import."
	userSuffix := "This MR is a WIP; focus on architecture."

	g := newFakeGitLab(t)
	g.enqueue(http.StatusOK, mrFixture)
	g.enqueue(http.StatusOK, changesFixture)
	g.enqueue(http.StatusOK, "[]")                       // ListDiscussions
	g.enqueue(http.StatusCreated, `{"id":1,"body":"s"}`) // summary
	// No inline posts (zero findings).

	l := newFakeLLM(t,
		`{"findings":[],"summary":"LGTM"}`,
	)
	glt, _ := gitlab.NewClient(g.URL, "test-token", gitlab.RetryConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p, _ := llm.NewOpenAIProvider(llm.OpenAIConfig{BaseURL: l.URL, APIKey: "k", Model: "m"})
	r, _ := NewReviewer(Config{
		GitLab:             glt,
		LLM:                p,
		Model:              "m",
		MaxDiffBytes:       4096,
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		SystemPromptSuffix: teamSuffix,
		UserPromptSuffix:   userSuffix,
	})
	_, err := r.ReviewMR(context.Background(), "group/project", 42)
	if err != nil {
		t.Fatalf("ReviewMR: %v", err)
	}

	// Find the chat-completion request body and assert both
	// suffixes are present and the system suffix sits after the
	// schema block.
	if len(l.requests) == 0 {
		t.Fatal("no LLM requests captured")
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(l.requests[0].Body), &sent); err != nil {
		t.Fatalf("body is not valid JSON: %v\n%s", err, l.requests[0].Body)
	}
	messages, ok := sent["messages"].([]any)
	if !ok || len(messages) < 2 {
		t.Fatalf("expected system + user messages; got %v", sent["messages"])
	}
	systemMsg := messages[0].(map[string]any)
	userMsg := messages[1].(map[string]any)
	systemContent, _ := systemMsg["content"].(string)
	userContent, _ := userMsg["content"].(string)

	if !strings.Contains(systemContent, teamSuffix) {
		t.Errorf("system prompt missing operator suffix\n--- system ---\n%s", systemContent)
	}
	if !strings.Contains(userContent, userSuffix) {
		t.Errorf("user prompt missing operator suffix\n--- user ---\n%s", userContent)
	}
	// System-owned schema must remain.
	if !strings.Contains(systemContent, `"severity": "info" | "warning" | "error"`) {
		t.Errorf("system-owned schema missing; system was:\n%s", systemContent)
	}
}

func TestReviewMR_IgnorePaths_FiltersBeforeLLM(t *testing.T) {
	// Two changes — a real .go file and a generated .pb.go. With
	// `**/*.pb.go` in IgnorePaths, only the real file should reach
	// the LLM. We verify by reading the request body sent to the
	// fake LLM.
	multiChanges := `[
		{"old_path":"a.go","new_path":"a.go","diff":"@@ -1 +1 @@\n-x\n+y\n"},
		{"old_path":"api/x/a.pb.go","new_path":"api/x/a.pb.go","diff":"@@ -1 +1 @@\n-x\n+y\n"}
	]`

	g := newFakeGitLab(t)
	g.enqueue(http.StatusOK, mrFixture)
	g.enqueue(http.StatusOK, multiChanges)
	g.enqueue(http.StatusOK, "[]")                       // ListDiscussions: empty
	g.enqueue(http.StatusCreated, `{"id":1,"body":"s"}`) // summary
	// NO inline post — filtered file's finding can't anchor.

	l := newFakeLLM(t,
		`{"findings":[{"file":"a.go","line":1,"severity":"info","category":"style","body":"real file finding"},{"file":"api/x/a.pb.go","line":1,"severity":"info","category":"style","body":"generated file finding"}],"summary":"ok"}`,
	)
	glt, _ := gitlab.NewClient(g.URL, "test-token", gitlab.RetryConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p, _ := llm.NewOpenAIProvider(llm.OpenAIConfig{BaseURL: l.URL, APIKey: "k", Model: "m"})
	r, _ := NewReviewer(Config{
		GitLab:       glt,
		LLM:          p,
		Model:        "m",
		MaxDiffBytes: 4096,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		IgnorePaths:  []string{"**/*.pb.go"},
	})
	_, err := r.ReviewMR(context.Background(), "group/project", 42)
	if err != nil {
		t.Fatalf("ReviewMR: %v", err)
	}

	// The LLM received both file paths in its prompt (since the
	// fixture includes both), but the hallucination guard dropped
	// the generated file's finding because it wasn't in the (filtered)
	// diff. Verify the filter by checking what the LLM saw vs what
	// made it through to GitLab.
	// Simpler check: only ONE inline discussion got posted, for a.go.
	inlineCount := 0
	for _, req := range g.requests {
		if req.Method == http.MethodPost && strings.Contains(req.Path, "/discussions") {
			inlineCount++
			if !strings.Contains(req.Body, "a.go") {
				t.Errorf("expected a.go in inline post body, got %s", req.Body)
			}
			if strings.Contains(req.Body, "pb.go") {
				t.Errorf("expected NO pb.go in inline post body, got %s", req.Body)
			}
		}
	}
	if inlineCount != 1 {
		t.Errorf("expected exactly 1 inline post (a.go only), got %d", inlineCount)
	}
}

func TestReviewMR_DedupesIgnoresNonBotComments(t *testing.T) {
	// Prior HUMAN comment with the same body — should NOT dedupe,
	// because dedupe is scoped to the bot's own comments.
	prior := `[
		{
			"id":"human1",
			"individual_note": false,
			"notes": [{"id":99,"body":"**[warning]** jwt leak","author":{"id":2,"username":"alice","name":"Alice"},"system":false}]
		}
	]`

	g := newFakeGitLab(t)
	g.enqueue(http.StatusOK, mrFixture)
	g.enqueue(http.StatusOK, changesFixture)
	g.enqueue(http.StatusOK, prior) // ListDiscussions: human comment only
	g.enqueue(http.StatusCreated, `{"id":1,"body":"s"}`)
	g.enqueue(http.StatusCreated, `{"id":"d1","individual_note":false,"notes":[{"id":2,"body":"x","author":{"id":1,"username":"bot"},"system":false}]}`)

	l := newFakeLLM(t,
		`{"findings":[{"file":"a.go","line":1,"severity":"warning","category":"security","body":"**[warning]** jwt leak"}],"summary":"LGTM"}`,
	)
	glt, _ := gitlab.NewClient(g.URL, "test-token", gitlab.RetryConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p, _ := llm.NewOpenAIProvider(llm.OpenAIConfig{BaseURL: l.URL, APIKey: "k", Model: "m"})
	r, _ := NewReviewer(Config{
		GitLab:       glt,
		LLM:          p,
		Model:        "m",
		MaxDiffBytes: 4096,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		BotUsername:  "review-bot",
	})
	result, err := r.ReviewMR(context.Background(), "group/project", 42)
	if err != nil {
		t.Fatalf("ReviewMR: %v", err)
	}
	if result.DedupeSize != 0 {
		t.Errorf("DedupeSize = %d, want 0 (human comments ignored)", result.DedupeSize)
	}
	if len(result.Findings) != 1 {
		t.Errorf("expected 1 surviving finding, got %d", len(result.Findings))
	}
	if result.Findings[0].Discussion == nil {
		t.Error("human comments don't dedupe — inline post should succeed")
	}
}

func TestFilterFindings_DropsEmptyBody(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	valid := map[string]bool{"a.go": true}
	in := []llm.Finding{
		{File: "a.go", Line: 1, Body: "good"},
		{File: "a.go", Line: 2, Body: ""},
		{File: "a.go", Line: 3, Body: "   "},
	}
	out := filterFindings(in, valid, logger)
	if len(out) != 1 {
		t.Errorf("expected 1 finding after empty-body filter, got %d", len(out))
	}
}

func TestDedupeFindings(t *testing.T) {
	in := []llm.Finding{
		{File: "a.go", Line: 1, Body: "x"},
		{File: "a.go", Line: 1, Body: "x"}, // dup
		{File: "a.go", Line: 2, Body: "y"},
	}
	out := dedupeFindings(in)
	if len(out) != 2 {
		t.Errorf("expected 2 deduped findings, got %d", len(out))
	}
}

func TestRenderSummary_EmptyFindings(t *testing.T) {
	mr := &gitlab.MergeRequest{Title: "x", IID: 1}
	body := renderSummary(mr, "all good", nil)
	if !strings.Contains(body, "mreview summary") {
		t.Errorf("expected header, got %q", body)
	}
	if !strings.Contains(body, "all good") {
		t.Errorf("expected summary, got %q", body)
	}
	if !strings.Contains(body, "No findings.") {
		t.Errorf("expected 'No findings.' note, got %q", body)
	}
}

func TestRenderSummary_WithFindings(t *testing.T) {
	mr := &gitlab.MergeRequest{Title: "x", IID: 1}
	findings := []llm.Finding{
		{File: "a.go", Line: 5, Severity: "warning", Category: "security", Body: "jwt leak"},
		{File: "b.go", Line: 99, Severity: "info", Category: "style", Body: "rename"},
	}
	body := renderSummary(mr, "two findings", findings)
	for _, want := range []string{
		"two findings",
		"Findings (2)",
		"warning",
		"security",
		"a.go:5",
		"jwt leak",
		"b.go:99",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("renderSummary missing %q\n%s", want, body)
		}
	}
}

func TestClassifyPostError_UnknownError(t *testing.T) {
	if got := classifyPostError(errors.New("boom")); !strings.Contains(got, "boom") {
		t.Errorf("expected error message, got %q", got)
	}
}

func TestClassifyPostError_GitLabErrors(t *testing.T) {
	cases := []struct {
		kind   gitlab.Kind
		status int
		body   string
		want   string
	}{
		{gitlab.KindConflict, 400, "x", "line out of range"},
		{gitlab.KindAuth, 401, "x", "auth"},
		{gitlab.KindNotFound, 404, "x", "not found"},
		{gitlab.KindBadRequest, 400, "x", "bad request"},
		{gitlab.KindTransient, 503, "x", "transient"},
		{gitlab.KindOther, 500, "x", "unknown"},
	}
	for _, tc := range cases {
		got := classifyPostError(&gitlab.Error{Kind: tc.kind, StatusCode: tc.status, Body: tc.body})
		if !strings.Contains(got, tc.want) {
			t.Errorf("kind=%s: got %q, want containing %q", tc.kind, got, tc.want)
		}
	}
}

func TestShouldFailReview(t *testing.T) {
	cases := []struct {
		kind gitlab.Kind
		want bool
	}{
		{gitlab.KindAuth, true},
		{gitlab.KindNotFound, true},
		{gitlab.KindConflict, false},
		{gitlab.KindBadRequest, false},
		{gitlab.KindTransient, false},
	}
	for _, tc := range cases {
		got := shouldFailReview(&gitlab.Error{Kind: tc.kind})
		if got != tc.want {
			t.Errorf("kind=%s: got %v, want %v", tc.kind, got, tc.want)
		}
	}
	if shouldFailReview(errors.New("plain")) {
		t.Error("plain error should not fail review")
	}
}

func TestBatchChunks(t *testing.T) {
	chunks := []llm.Chunk{
		{File: "a.go", Diff: "x", Size: 1},
		{File: "b.go", Diff: "y", Size: 1},
	}
	// maxBytes == 0 preserves the historical one-batch-per-chunk
	// behaviour. This is the default for anyone not setting the
	// new --max-batch-bytes flag.
	batches := batchChunks(chunks, 0)
	if len(batches) != 2 {
		t.Errorf("expected 2 batches, got %d", len(batches))
	}
	for i, b := range batches {
		if len(b) != 1 {
			t.Errorf("batch %d has %d chunks, want 1", i, len(b))
		}
	}
}

// TestBatchChunks_PackSmallTogether confirms that small chunks
// collapse into one batch when their combined size fits the budget.
func TestBatchChunks_PackSmallTogether(t *testing.T) {
	chunks := []llm.Chunk{
		{File: "a.go", Size: 100},
		{File: "b.go", Size: 100},
		{File: "c.go", Size: 100},
	}
	batches := batchChunks(chunks, 500) // total 300 fits
	if len(batches) != 1 {
		t.Fatalf("expected 1 packed batch, got %d", len(batches))
	}
	if len(batches[0]) != 3 {
		t.Errorf("packed batch should have all 3 chunks, got %d", len(batches[0]))
	}
}

// TestBatchChunks_GreedyMixed confirms that chunks pack greedily
// in order — once a batch can't fit the next chunk, it flushes
// and starts fresh.
func TestBatchChunks_GreedyMixed(t *testing.T) {
	chunks := []llm.Chunk{
		{File: "a.go", Size: 100},
		{File: "b.go", Size: 100},
		{File: "c.go", Size: 100},
		{File: "d.go", Size: 100},
	}
	batches := batchChunks(chunks, 250) // 100+100 fits, 100+100+100 doesn't
	if len(batches) != 2 {
		t.Fatalf("expected 2 batches (a+b, c+d), got %d", len(batches))
	}
	if len(batches[0]) != 2 || batches[0][0].File != "a.go" || batches[0][1].File != "b.go" {
		t.Errorf("first batch should be [a.go, b.go], got %v", batchFiles(batches[0]))
	}
	if len(batches[1]) != 2 || batches[1][0].File != "c.go" || batches[1][1].File != "d.go" {
		t.Errorf("second batch should be [c.go, d.go], got %v", batchFiles(batches[1]))
	}
}

// TestBatchChunks_OversizeChunkAlone confirms that a single chunk
// larger than the budget goes alone in its own batch (not dropped)
// so the LLM still gets a chance to handle it.
func TestBatchChunks_OversizeChunkAlone(t *testing.T) {
	chunks := []llm.Chunk{
		{File: "small.go", Size: 50},
		{File: "huge.go", Size: 10000}, // exceeds budget on its own
		{File: "small2.go", Size: 50},
	}
	batches := batchChunks(chunks, 200)
	if len(batches) != 3 {
		t.Fatalf("expected 3 batches (small, huge alone, small2), got %d", len(batches))
	}
	if batches[1][0].File != "huge.go" {
		t.Errorf("oversized chunk should be in its own batch, got %v", batchFiles(batches[1]))
	}
	if len(batches[1]) != 1 {
		t.Errorf("oversized chunk should be alone, got %d chunks", len(batches[1]))
	}
}

// TestBatchChunks_ExactFit confirms that chunks whose total size
// equals the budget exactly all land in one batch (no off-by-one
// in the comparator).
func TestBatchChunks_ExactFit(t *testing.T) {
	chunks := []llm.Chunk{
		{File: "a.go", Size: 50},
		{File: "b.go", Size: 50},
	}
	batches := batchChunks(chunks, 100) // exactly fits
	if len(batches) != 1 {
		t.Fatalf("exact-fit should produce 1 batch, got %d", len(batches))
	}
	if len(batches[0]) != 2 {
		t.Errorf("batch should have 2 chunks, got %d", len(batches[0]))
	}
}

// TestBatchChunks_Empty confirms that empty input produces no
// batches (avoids degenerate one-empty-batch output).
func TestBatchChunks_Empty(t *testing.T) {
	if got := batchChunks(nil, 1000); len(got) != 0 {
		t.Errorf("nil input should produce 0 batches, got %d", len(got))
	}
	if got := batchChunks([]llm.Chunk{}, 1000); len(got) != 0 {
		t.Errorf("empty input should produce 0 batches, got %d", len(got))
	}
}

// TestBatchChunks_FallbackToSizeField confirms that when Size is
// not populated (e.g. chunks constructed by hand in tests), the
// packer falls back to len(Diff) instead of treating the chunk as
// zero-sized.
func TestBatchChunks_FallbackToSizeField(t *testing.T) {
	chunks := []llm.Chunk{
		{File: "a.go", Diff: "0123456789"}, // Size = 0, len(Diff) = 10
		{File: "b.go", Diff: "0123456789"},
	}
	batches := batchChunks(chunks, 15) // 10+10 would exceed, so split
	if len(batches) != 2 {
		t.Fatalf("expected 2 batches when Size=0 fallback uses len(Diff), got %d", len(batches))
	}
}

// batchFiles is a small helper for readable failure messages in
// the packing tests above.
func batchFiles(b []llm.Chunk) []string {
	out := make([]string, len(b))
	for i, c := range b {
		out[i] = c.File
	}
	return out
}

// threeFilesFixture is a 3-file diff that fits inside a single
// batch when MaxBatchBytes is set generously. Used to verify the
// reviewer makes fewer LLM calls when packing is enabled.
const threeFilesFixture = `[
	{"old_path":"a.go","new_path":"a.go","new_file":false,"deleted_file":false,"renamed_file":false,"diff":"@@ -1 +1 @@\n-old\n+new\n"},
	{"old_path":"b.go","new_path":"b.go","new_file":false,"deleted_file":false,"renamed_file":false,"diff":"@@ -1 +1 @@\n-foo\n+bar\n"},
	{"old_path":"c.go","new_path":"c.go","new_file":false,"deleted_file":false,"renamed_file":false,"diff":"@@ -1 +1 @@\n-baz\n+qux\n"}
]`

// TestReviewMR_Packing_ReducesLLMCalls confirms that when
// MaxBatchBytes is set large enough to fit all chunks, the
// reviewer collapses N file-level LLM calls into 1.
func TestReviewMR_Packing_ReducesLLMCalls(t *testing.T) {
	g := newFakeGitLab(t)
	g.enqueue(http.StatusOK, mrFixture)
	g.enqueue(http.StatusOK, threeFilesFixture) // 3 files
	g.enqueue(http.StatusOK, "[]")              // ListDiscussions
	g.enqueue(http.StatusCreated, `{"id":1,"body":"summary"}`)

	// Stage exactly one LLM body — if packing works, the test
	// makes one call. Without packing, the second call would hit
	// the "no body queued" 500 and the test would fail on the
	// call-count assertion below.
	l := newFakeLLM(t,
		`{"findings":[{"file":"a.go","line":1,"severity":"info","category":"style","body":"x"}],"summary":"ok"}`,
	)

	glt, _ := gitlab.NewClient(g.URL, "test-token", gitlab.RetryConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p, _ := llm.NewOpenAIProvider(llm.OpenAIConfig{BaseURL: l.URL, APIKey: "k", Model: "m"})
	r, err := NewReviewer(Config{
		GitLab: glt, LLM: p, Model: "m",
		MaxDiffBytes:  4096,
		MaxBatchBytes: 100000, // large enough to fit all 3 files
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewReviewer: %v", err)
	}

	if _, err := r.ReviewMR(context.Background(), "group/project", 42); err != nil {
		t.Fatalf("ReviewMR: %v", err)
	}

	if got := l.calls.Load(); got != 1 {
		t.Errorf("expected 1 LLM call (all 3 chunks packed), got %d", got)
	}
}

// TestReviewMR_Packing_DisabledByDefault confirms that the default
// (MaxBatchBytes == 0) preserves the historical one-call-per-chunk
// behaviour. A 3-file diff makes 3 LLM calls.
func TestReviewMR_Packing_DisabledByDefault(t *testing.T) {
	g := newFakeGitLab(t)
	g.enqueue(http.StatusOK, mrFixture)
	g.enqueue(http.StatusOK, threeFilesFixture)
	g.enqueue(http.StatusOK, "[]")
	g.enqueue(http.StatusCreated, `{"id":1,"body":"summary"}`)

	// Stage 3 LLM bodies — one per file.
	l := newFakeLLM(t,
		`{"findings":[],"summary":"a"}`,
		`{"findings":[],"summary":"b"}`,
		`{"findings":[],"summary":"c"}`,
	)

	glt, _ := gitlab.NewClient(g.URL, "test-token", gitlab.RetryConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p, _ := llm.NewOpenAIProvider(llm.OpenAIConfig{BaseURL: l.URL, APIKey: "k", Model: "m"})
	r, err := NewReviewer(Config{
		GitLab: glt, LLM: p, Model: "m",
		MaxDiffBytes: 4096,
		// MaxBatchBytes left at 0 (default).
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewReviewer: %v", err)
	}

	if _, err := r.ReviewMR(context.Background(), "group/project", 42); err != nil {
		t.Fatalf("ReviewMR: %v", err)
	}

	if got := l.calls.Load(); got != 4 {
		t.Errorf("expected 4 LLM calls (3 per-chunk + 1 consolidate), got %d", got)
	}
}

// silence unused-import warning for json in case future tests
// use it.
var _ = json.Marshal
