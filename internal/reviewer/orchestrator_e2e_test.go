package reviewer

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/inful/mreview/internal/gitlab"
)

// fakeGitLabClient is a minimal in-memory stub of the
// GitLab client surface the orchestrator depends on. It
// records every call (so tests can assert what the
// orchestrator did) and returns canned responses (so
// tests can drive the full Run() flow without a real
// GitLab instance).
//
// The fake satisfies the reviewer's gitlabClient
// interface (declared in gitlab_iface.go) — no
// production-code changes needed to use it.
type fakeGitLabClient struct {
	mu sync.Mutex

	// Stubbed responses.
	mergeRequest *gitlab.MergeRequest
	changes      []gitlab.ChangeFile
	discussions  []gitlab.Discussion

	// Recorded calls.
	postSummaryCalls    []postSummaryCall
	editSummaryCalls    []editSummaryCall
	postDiscussionCalls []postDiscussionCall
	resolveCalls        []string // discussion IDs

	// Failure injection (nil = no failure).
	failPostSummary    error
	failEditSummary    error
	failPostDiscussion error
}

type postSummaryCall struct {
	project any
	iid     int
	body    string
}
type editSummaryCall struct {
	project any
	iid     int
	noteID  int64
	body    string
}
type postDiscussionCall struct {
	project  any
	iid      int
	diffRefs gitlab.DiffRefs
	body     gitlab.InlineComment
}

// FetchMR returns the canned MR.
func (f *fakeGitLabClient) FetchMR(_ context.Context, _ any, _ int) (*gitlab.MergeRequest, error) {
	return f.mergeRequest, nil
}

// FetchChanges returns the canned change list.
func (f *fakeGitLabClient) FetchChanges(_ context.Context, _ any, _ int) ([]gitlab.ChangeFile, error) {
	return f.changes, nil
}

// ListDiscussions returns the canned discussion list.
func (f *fakeGitLabClient) ListDiscussions(_ context.Context, _ any, _ int) ([]gitlab.Discussion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]gitlab.Discussion, len(f.discussions))
	copy(out, f.discussions)
	return out, nil
}

// PostSummary records the call and returns a fake note.
func (f *fakeGitLabClient) PostSummary(_ context.Context, project any, iid int, body string) (*gitlab.Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.postSummaryCalls = append(f.postSummaryCalls, postSummaryCall{project, iid, body})
	if f.failPostSummary != nil {
		return nil, f.failPostSummary
	}
	return &gitlab.Note{ID: int64(9000 + len(f.postSummaryCalls)), Body: body}, nil
}

// EditSummary records the call.
func (f *fakeGitLabClient) EditSummary(_ context.Context, project any, iid int, noteID int64, body string) (*gitlab.Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.editSummaryCalls = append(f.editSummaryCalls, editSummaryCall{project, iid, noteID, body})
	if f.failEditSummary != nil {
		return nil, f.failEditSummary
	}
	return &gitlab.Note{ID: noteID, Body: body}, nil
}

// PostDiscussion records the call and returns a fake
// discussion.
func (f *fakeGitLabClient) PostDiscussion(_ context.Context, project any, iid int, diffRefs gitlab.DiffRefs, body gitlab.InlineComment) (*gitlab.Discussion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.postDiscussionCalls = append(f.postDiscussionCalls, postDiscussionCall{project, iid, diffRefs, body})
	if f.failPostDiscussion != nil {
		return nil, f.failPostDiscussion
	}
	return &gitlab.Discussion{
		ID: "new-disc-" + strconv.Itoa(len(f.postDiscussionCalls)),
		Notes: []gitlab.Note{{
			ID:     int64(8000 + len(f.postDiscussionCalls)),
			Body:   body.Body,
			Author: gitlab.User{Username: "mreview-bot"},
			Position: &gitlab.NotePosition{
				NewPath: body.File,
				NewLine: int64(body.NewLine),
			},
		}},
	}, nil
}

// ResolveDiscussion records the call.
func (f *fakeGitLabClient) ResolveDiscussion(_ context.Context, _ string, _ int, discussionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolveCalls = append(f.resolveCalls, discussionID)
	return nil
}

// Compile-time check that the fake satisfies the
// orchestrator's interface.
var _ gitlabClient = (*fakeGitLabClient)(nil)

// dummyRunner returns a canned LLM response.
type dummyRunner struct {
	resp string
}

func (d *dummyRunner) RunSync(_ context.Context, _, _ string) (string, error) {
	return d.resp, nil
}

// silentLogger is a slog.Logger that discards all output,
// so the e2e test doesn't pollute the test runner with
// the orchestrator's per-step info logs.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestOrchestrator_EndToEnd_EditPriorSummary covers the
// full "follow-up run" flow:
//
//   - A prior mreview summary exists for a DIFFERENT commit.
//   - The LLM is asked to evaluate 2 prior findings and
//     emit 1 new one.
//   - The orchestrator should:
//   - Edit the prior summary in place (NOT post fresh)
//   - Resolve 1 prior discussion (the one the LLM
//     marked resolved)
//   - Post 1 new inline discussion
//   - Leave the other prior alone (still_valid)
//   - Render a summary with all three categories
func TestOrchestrator_EndToEnd_EditPriorSummary(t *testing.T) {
	// Canned prior state: a summary discussion (with
	// the marker for commit "OLD") + 2 prior inline
	// findings (d-prior-1 carry-over, d-prior-2 to
	// resolve).
	const priorSummaryNoteID = int64(7000)
	priorSummary := gitlab.Discussion{
		ID: "d-summary",
		Notes: []gitlab.Note{{
			ID:     priorSummaryNoteID,
			Body:   "<!-- mreview:commit=OLD findings=d-prior-1,d-prior-2 -->\n# mreview summary\n\nOld summary.",
			Author: gitlab.User{Username: "mreview-bot"},
		}},
	}
	priorFinding1 := gitlab.Discussion{
		ID: "d-prior-1",
		Notes: []gitlab.Note{{
			ID:       7001,
			Body:     "Prior finding 1 (will be carried over).",
			Author:   gitlab.User{Username: "mreview-bot"},
			Position: &gitlab.NotePosition{NewPath: "old.go", NewLine: 10},
		}},
	}
	priorFinding2 := gitlab.Discussion{
		ID: "d-prior-2",
		Notes: []gitlab.Note{{
			ID:       7002,
			Body:     "Prior finding 2 (will be resolved).",
			Author:   gitlab.User{Username: "mreview-bot"},
			Position: &gitlab.NotePosition{NewPath: "old.go", NewLine: 20},
		}},
	}
	fake := &fakeGitLabClient{
		mergeRequest: &gitlab.MergeRequest{
			IID:          42,
			Title:        "Test MR",
			Description:  "Description",
			SHA:          "NEW",
			SourceBranch: "feature",
			TargetBranch: "main",
			Author:       gitlab.User{Username: "alice"},
			DiffRefs:     gitlab.DiffRefs{BaseSHA: "b", HeadSHA: "h", StartSHA: "s"},
		},
		changes: []gitlab.ChangeFile{
			{NewPath: "new.go", Diff: "@@ ... @@\n+new line"},
		},
		discussions: []gitlab.Discussion{priorSummary, priorFinding1, priorFinding2},
	}

	// LLM response: 1 new finding + 2 prior verdicts
	// (one still_valid, one resolved with rationale).
	llmResp := `{
		"findings": [
			{"file": "new.go", "line": 5, "severity": "warning", "category": "test", "body": "New finding."}
		],
		"prior_findings": [
			{"file": "old.go", "line": 10, "status": "still_valid"},
			{"file": "old.go", "line": 20, "status": "resolved", "rationale": "line was deleted"}
		],
		"summary": "Mostly clean."
	}`

	o, err := New(Config{
		GitLab:      fake,
		Runner:      &dummyRunner{resp: llmResp},
		BotUsername: "mreview-bot",
		Logger:      silentLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := o.Run(context.Background(), "group/project", 42)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res == nil {
		t.Fatal("Run returned nil result")
	}

	// Assert: the prior summary was EDITED, not posted
	// fresh. No PostSummary calls; one EditSummary call
	// on the prior note ID.
	if len(fake.postSummaryCalls) != 0 {
		t.Errorf("expected 0 PostSummary calls (prior exists); got %d", len(fake.postSummaryCalls))
	}
	if len(fake.editSummaryCalls) != 1 {
		t.Fatalf("expected 1 EditSummary call; got %d", len(fake.editSummaryCalls))
	}
	if got := fake.editSummaryCalls[0].noteID; got != priorSummaryNoteID {
		t.Errorf("EditSummary noteID = %d, want %d (the prior note)", got, priorSummaryNoteID)
	}

	// Assert: the resolved prior was auto-resolved.
	// The carried-over prior was left alone.
	if len(fake.resolveCalls) != 1 {
		t.Fatalf("expected 1 ResolveDiscussion call; got %d (calls=%v)", len(fake.resolveCalls), fake.resolveCalls)
	}
	if got := fake.resolveCalls[0]; got != "d-prior-2" {
		t.Errorf("ResolveDiscussion id = %q, want d-prior-2 (the LLM-marked-resolved prior)", got)
	}

	// Assert: one new inline discussion was posted.
	if len(fake.postDiscussionCalls) != 1 {
		t.Fatalf("expected 1 PostDiscussion call; got %d", len(fake.postDiscussionCalls))
	}
	if got := fake.postDiscussionCalls[0].body.File; got != "new.go" {
		t.Errorf("new finding file = %q, want new.go", got)
	}
	if got := fake.postDiscussionCalls[0].body.NewLine; got != 5 {
		t.Errorf("new finding line = %d, want 5", got)
	}

	// Assert: the summary body has the right shape.
	//   - Commit marker for NEW (the current SHA)
	//   - All three categories in the table
	//   - The new finding, the carried-over prior, and
	//     the resolved prior all appear
	editedBody := fake.editSummaryCalls[0].body
	for _, want := range []string{
		"<!-- mreview:commit=NEW",
		"Mostly clean.",    // the LLM's summary text
		"🆕 new",            // new finding status
		"New finding.",     // new finding body
		"↻ carried over",   // carried-over status
		"Prior finding 1",  // carried-over body
		"✓ resolved",       // resolved status
		"Prior finding 2",  // resolved body
		"line was deleted", // resolved rationale
	} {
		if !strings.Contains(editedBody, want) {
			t.Errorf("summary body missing %q; body:\n%s", want, editedBody)
		}
	}
	// The marker should reference BOTH the carried-over
	// prior and the newly-posted finding (the resolved
	// prior is auto-collapsed and not in the next-run's
	// set).
	marker := extractMarker(editedBody)
	if !strings.Contains(marker, "d-prior-1") {
		t.Errorf("marker missing d-prior-1 (carried-over); marker=%q", marker)
	}
	if !strings.Contains(marker, "new-disc-1") {
		t.Errorf("marker missing new-disc-1 (newly posted); marker=%q", marker)
	}
	if strings.Contains(marker, "d-prior-2") {
		t.Errorf("marker should NOT include d-prior-2 (resolved); marker=%q", marker)
	}
}

// TestOrchestrator_EndToNoPrior covers the first-run
// case: no prior summary, no prior findings. The
// orchestrator should post a fresh summary and not call
// EditSummary or ResolveDiscussion at all.
func TestOrchestrator_EndToNoPrior(t *testing.T) {
	fake := &fakeGitLabClient{
		mergeRequest: &gitlab.MergeRequest{
			IID:          42,
			Title:        "Test MR",
			SHA:          "NEW",
			SourceBranch: "feature",
			TargetBranch: "main",
			Author:       gitlab.User{Username: "alice"},
			DiffRefs:     gitlab.DiffRefs{BaseSHA: "b", HeadSHA: "h", StartSHA: "s"},
		},
		changes: []gitlab.ChangeFile{
			{NewPath: "new.go", Diff: "@@ ... @@\n+new line"},
		},
		discussions: nil, // no prior
	}
	llmResp := `{
		"findings": [
			{"file": "new.go", "line": 5, "severity": "warning", "category": "test", "body": "New finding."}
		],
		"summary": "Clean run."
	}`
	o, err := New(Config{
		GitLab: fake,
		Runner: &dummyRunner{resp: llmResp},
		Logger: silentLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := o.Run(context.Background(), "group/project", 42); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// First-run: exactly one PostSummary (single-write
	// design; the marker is included in the initial
	// post, no fill-in edit needed). Zero EditSummary
	// calls (nothing to edit yet).
	if len(fake.postSummaryCalls) != 1 {
		t.Errorf("expected 1 PostSummary call; got %d", len(fake.postSummaryCalls))
	}
	if len(fake.editSummaryCalls) != 0 {
		t.Errorf("expected 0 EditSummary calls on first run; got %d", len(fake.editSummaryCalls))
	}
	if len(fake.resolveCalls) != 0 {
		t.Errorf("expected 0 ResolveDiscussion calls; got %d (calls=%v)", len(fake.resolveCalls), fake.resolveCalls)
	}
	if len(fake.postDiscussionCalls) != 1 {
		t.Errorf("expected 1 PostDiscussion call; got %d", len(fake.postDiscussionCalls))
	}
	// The summary body should include the marker with
	// the new-discussion ID (the single-write design
	// bakes the marker in at post time, no separate
	// edit).
	posted := fake.postSummaryCalls[0]
	if !strings.Contains(posted.body, "new-disc-1") {
		t.Errorf("PostSummary body missing new-disc-1 in marker; body:\n%s", posted.body)
	}
}

// TestOrchestrator_EndToEnd_SkipSameCommit covers the
// "prior matches current commit" path: the orchestrator
// returns early with SkippedReason and posts nothing.
func TestOrchestrator_EndToEnd_SkipSameCommit(t *testing.T) {
	const priorSummaryNoteID = int64(7000)
	priorSummary := gitlab.Discussion{
		ID: "d-summary",
		Notes: []gitlab.Note{{
			ID:     priorSummaryNoteID,
			Body:   "<!-- mreview:commit=NEW findings= -->", // SAME as new
			Author: gitlab.User{Username: "mreview-bot"},
		}},
	}
	fake := &fakeGitLabClient{
		mergeRequest: &gitlab.MergeRequest{
			IID:          42,
			SHA:          "NEW", // same as the prior
			SourceBranch: "feature",
			TargetBranch: "main",
			Author:       gitlab.User{Username: "alice"},
		},
		discussions: []gitlab.Discussion{priorSummary},
	}
	o, err := New(Config{
		GitLab: fake,
		Runner: &dummyRunner{resp: "{}"},
		Logger: silentLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := o.Run(context.Background(), "group/project", 42)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SkippedReason == "" {
		t.Errorf("expected SkippedReason; got empty")
	}
	if len(fake.postSummaryCalls)+len(fake.editSummaryCalls) != 0 {
		t.Errorf("expected 0 summary writes on skip; got %d post + %d edit", len(fake.postSummaryCalls), len(fake.editSummaryCalls))
	}
	if len(fake.resolveCalls) != 0 {
		t.Errorf("expected 0 ResolveDiscussion on skip; got %d", len(fake.resolveCalls))
	}
}

// TestOrchestrator_EndToEnd_PriorAllResolved covers the
// "every prior finding was resolved" case: the marker
// for the next run should reference only the new
// findings (the carried-over IDs list is empty).
func TestOrchestrator_EndToEnd_PriorAllResolved(t *testing.T) {
	priorSummaryNoteID := int64(7000)
	priorSummary := gitlab.Discussion{
		ID: "d-summary",
		Notes: []gitlab.Note{{
			ID:     priorSummaryNoteID,
			Body:   "<!-- mreview:commit=OLD findings=d-prior-1 -->",
			Author: gitlab.User{Username: "mreview-bot"},
		}},
	}
	priorFinding1 := gitlab.Discussion{
		ID: "d-prior-1",
		Notes: []gitlab.Note{{
			ID:       7001,
			Body:     "Old finding.",
			Author:   gitlab.User{Username: "mreview-bot"},
			Position: &gitlab.NotePosition{NewPath: "old.go", NewLine: 10},
		}},
	}
	fake := &fakeGitLabClient{
		mergeRequest: &gitlab.MergeRequest{
			IID:          42,
			SHA:          "NEW",
			SourceBranch: "feature",
			TargetBranch: "main",
			Author:       gitlab.User{Username: "alice"},
			DiffRefs:     gitlab.DiffRefs{BaseSHA: "b", HeadSHA: "h", StartSHA: "s"},
		},
		changes: []gitlab.ChangeFile{
			{NewPath: "new.go", Diff: "@@ ... @@\n+new line"},
		},
		discussions: []gitlab.Discussion{priorSummary, priorFinding1},
	}
	llmResp := `{
		"findings": [{"file": "new.go", "line": 5, "severity": "warning", "category": "test", "body": "New."}],
		"prior_findings": [{"file": "old.go", "line": 10, "status": "resolved", "rationale": "fixed"}],
		"summary": "ok"
	}`
	o, err := New(Config{GitLab: fake, Runner: &dummyRunner{resp: llmResp}, Logger: silentLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := o.Run(context.Background(), "group/project", 42); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.resolveCalls) != 1 || fake.resolveCalls[0] != "d-prior-1" {
		t.Errorf("expected resolve of d-prior-1; got %v", fake.resolveCalls)
	}
	// The marker should NOT include d-prior-1 (resolved
	// is auto-collapsed) but SHOULD include the new
	// discussion ID.
	if len(fake.editSummaryCalls) != 1 {
		t.Fatalf("expected 1 EditSummary call; got %d", len(fake.editSummaryCalls))
	}
	marker := extractMarker(fake.editSummaryCalls[0].body)
	if strings.Contains(marker, "d-prior-1") {
		t.Errorf("marker should NOT include resolved prior d-prior-1; marker=%q", marker)
	}
	if !strings.Contains(marker, "new-disc-1") {
		t.Errorf("marker should include new-disc-1; marker=%q", marker)
	}
}

// extractMarker pulls the HTML-comment marker out of a
// summary body. The marker is always the first line of
// the body; we slice it out for assertions in the e2e
// tests. Returns "" if the body has no marker.
func extractMarker(body string) string {
	end := strings.Index(body, "\n")
	if end < 0 {
		return body
	}
	return body[:end]
}
