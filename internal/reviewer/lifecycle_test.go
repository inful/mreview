package reviewer

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/llm"
)

func TestShouldPostSummary(t *testing.T) {
	cases := []struct {
		action string
		want   bool
	}{
		{"open", true},     // fresh MR — post
		{"reopen", true},   // reopened — post
		{"update", false},  // push — skip
		{"", true},         // CLI / unknown — post (operator expects full report)
		{"approved", true}, // unknown action — default to post
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			if got := shouldPostSummary(tc.action); got != tc.want {
				t.Errorf("shouldPostSummary(%q) = %v, want %v", tc.action, got, tc.want)
			}
		})
	}
}

// TestReviewMR_OpenAction_PostsSummary confirms `open` triggers a
// summary post (the default behavior).
func TestReviewMR_OpenAction_PostsSummary(t *testing.T) {
	g := newFakeGitLab(t)
	g.enqueue(http.StatusOK, mrFixture)
	g.enqueue(http.StatusOK, changesFixture)
	g.enqueue(http.StatusOK, "[]")                             // ListDiscussions: empty
	g.enqueue(http.StatusCreated, `{"id":1,"body":"summary"}`) // summary POST

	l := newFakeLLM(t,
		`{"findings":[],"summary":"ok"}`,
	)
	glt, _ := gitlab.NewClient(g.URL, "test-token", gitlab.RetryConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p, _ := llm.NewOpenAIProvider(llm.OpenAIConfig{BaseURL: l.URL, APIKey: "k", Model: "m"})
	r, _ := NewReviewer(Config{
		GitLab: glt, LLM: p, Model: "m", MaxDiffBytes: 4096,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	result, err := r.ReviewMR(context.Background(), "group/project", 42, "open")
	if err != nil {
		t.Fatalf("ReviewMR: %v", err)
	}
	if result.Summary == nil {
		t.Error("open action should post summary, got nil")
	}

	// Verify exactly one POST (the summary) — no inline since
	// there were zero findings.
	postCount := 0
	for _, req := range g.requests {
		if req.Method == http.MethodPost {
			postCount++
		}
	}
	if postCount != 1 {
		t.Errorf("open: expected 1 POST (summary), got %d", postCount)
	}
}

// TestReviewMR_UpdateAction_SkipsSummary confirms `update` skips
// the summary post — the headline behavior of fix 2.
func TestReviewMR_UpdateAction_SkipsSummary(t *testing.T) {
	g := newFakeGitLab(t)
	g.enqueue(http.StatusOK, mrFixture)
	g.enqueue(http.StatusOK, changesFixture)
	g.enqueue(http.StatusOK, "[]") // ListDiscussions: empty
	// NO summary POST — update events skip it.

	l := newFakeLLM(t,
		`{"findings":[{"file":"a.go","line":1,"severity":"warning","category":"security","body":"x"}],"summary":"new issue"}`,
	)
	glt, _ := gitlab.NewClient(g.URL, "test-token", gitlab.RetryConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p, _ := llm.NewOpenAIProvider(llm.OpenAIConfig{BaseURL: l.URL, APIKey: "k", Model: "m"})
	r, _ := NewReviewer(Config{
		GitLab: glt, LLM: p, Model: "m", MaxDiffBytes: 4096,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	result, err := r.ReviewMR(context.Background(), "group/project", 42, "update")
	if err != nil {
		t.Fatalf("ReviewMR: %v", err)
	}
	if result.Summary != nil {
		t.Errorf("update action should NOT post summary, got %v", result.Summary)
	}

	// Verify exactly one POST (the inline discussion) — no summary.
	postCount := 0
	for _, req := range g.requests {
		if req.Method == http.MethodPost {
			postCount++
		}
	}
	if postCount != 1 {
		t.Errorf("update: expected 1 POST (inline), got %d", postCount)
	}
}

// TestReviewMR_NoAction_PostsSummary confirms the standalone CLI
// path (no action argument) still posts summary.
func TestReviewMR_NoAction_PostsSummary(t *testing.T) {
	g := newFakeGitLab(t)
	g.enqueue(http.StatusOK, mrFixture)
	g.enqueue(http.StatusOK, changesFixture)
	g.enqueue(http.StatusOK, "[]")
	g.enqueue(http.StatusCreated, `{"id":1,"body":"summary"}`)

	l := newFakeLLM(t, `{"findings":[],"summary":"ok"}`)
	glt, _ := gitlab.NewClient(g.URL, "test-token", gitlab.RetryConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p, _ := llm.NewOpenAIProvider(llm.OpenAIConfig{BaseURL: l.URL, APIKey: "k", Model: "m"})
	r, _ := NewReviewer(Config{
		GitLab: glt, LLM: p, Model: "m", MaxDiffBytes: 4096,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	// No action passed — simulate `mreview review ...` (CLI).
	result, err := r.ReviewMR(context.Background(), "group/project", 42)
	if err != nil {
		t.Fatalf("ReviewMR: %v", err)
	}
	if result.Summary == nil {
		t.Error("CLI (no action) should post summary, got nil")
	}
}
