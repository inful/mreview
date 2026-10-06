package main

import (
	"testing"

	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/reviewer"
)

// TestRunReview_Success_ReturnsNil_PrintsURL covers the
// happy path: the orchestrator returns a non-nil Result
// with no PolicyError, the summary was posted, and the MR
// has a WebURL. runReview must:
//   - return nil (the no-error case)
//   - print the WebURL to stdout (one line)
//   - emit the "review complete" log line
//
// This is the most important integration test for the
// public CLI surface: it's the only one that asserts the
// "everything worked" end-state. The per-event guard,
// workdir check, and policy/artifact load paths are
// already covered by their own tests; this one stitches
// them together.
func TestRunReview_Success_ReturnsNil_PrintsURL(t *testing.T) {
	const wantURL = "https://gitlab.example.com/foo/bar/-/merge_requests/42"

	fake := &fakeReviewer{
		result: &reviewer.Result{
			MR: &gitlab.MergeRequest{
				IID:    42,
				WebURL: wantURL,
			},
			Findings: []reviewer.PostedFinding{
				{Finding: reviewer.Finding{File: "x.go", Line: 1, Body: "test"}},
			},
			Summary:     &gitlab.Note{ID: 1, Body: "summary"},
			PolicyError: false,
		},
	}
	installFakeReviewer(t, fake)

	code, stdout, stderr := runReviewDirect(t, map[string]string{
		"CI_PIPELINE_SOURCE": "",
	}, nil)

	if code != 0 {
		t.Errorf("success should exit 0, got %d\nstderr: %s", code, stderr)
	}
	if !contains(stdout, wantURL) {
		t.Errorf("expected WebURL in stdout, got: %s", stdout)
	}
	if !contains(stderr, "review complete") {
		t.Errorf("expected 'review complete' log line, got: %s", stderr)
	}
	if !contains(stderr, `"findings":1`) {
		t.Errorf("expected 1 finding in 'review complete' log, got: %s", stderr)
	}
	if !contains(stderr, `"summary_posted":true`) {
		t.Errorf("expected summary_posted=true in log, got: %s", stderr)
	}
	if !contains(stderr, `"policy_error":false`) {
		t.Errorf("expected policy_error=false in log, got: %s", stderr)
	}
}

// TestRunReview_Success_NoWebURL_NoPrint covers the
// edge case: the orchestrator returns a Result with no MR
// (or an MR with no WebURL). runReview must NOT panic and
// must NOT print anything to stdout. This is the path the
// orchestrator takes on, e.g., a same-commit dedup skip
// where the MR object is populated but the WebURL might
// be empty after JSON round-tripping.
func TestRunReview_Success_NoWebURL_NoPrint(t *testing.T) {
	fake := &fakeReviewer{
		result: &reviewer.Result{
			MR:          nil, // simulates SkippedReason-only path
			PolicyError: false,
		},
	}
	installFakeReviewer(t, fake)

	code, stdout, stderr := runReviewDirect(t, map[string]string{
		"CI_PIPELINE_SOURCE": "",
	}, nil)
	if code != 0 {
		t.Errorf("success with nil MR should exit 0, got %d\nstderr: %s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("expected empty stdout when MR is nil, got: %q", stdout)
	}
	if !contains(stderr, "review complete") {
		t.Errorf("expected 'review complete' log line, got: %s", stderr)
	}
}
