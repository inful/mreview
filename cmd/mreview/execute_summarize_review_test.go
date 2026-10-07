package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/logging"
	"github.com/inful/mreview/internal/reviewer"
)

// helper-level tests for executeReview and summarizeReview.
// These are the two helpers extracted from runReview that
// are small enough to test as pure functions — executeReview
// just maps the orchestrator's error to a typed *ExitError,
// summarizeReview just formats the result.

// newTestLoggerForHelper is a tiny indirection so each test
// in this file can pass a logger that writes to a buffer
// the test controls. JSON mode keeps the assertions stable
// across slog version changes.
func newTestLoggerForHelper(buf *bytes.Buffer) *slog.Logger {
	logger, err := logging.New(buf, "json", true)
	if err != nil {
		panic("logging.New: " + err.Error())
	}
	return logger
}

// ---------------------------------------------------------------------------
// executeReview
// ---------------------------------------------------------------------------

// TestExecuteReview_GitLabError_Mapping is the helper-level
// twin of TestRunReview_GitLabError_Mapping. Same 6-case
// table; the assertion is the same (kind → exit code). The
// difference is the surface: this test calls executeReview
// directly, not via run() / runReview, so a regression in
// the mapping logic is caught at the helper boundary.
func TestExecuteReview_GitLabError_Mapping(t *testing.T) {
	cases := []struct {
		name     string
		kind     gitlab.Kind
		wantCode int
	}{
		{"other", gitlab.KindOther, ExitInternal},
		{"bad_request", gitlab.KindBadRequest, ExitConfig},
		{"auth", gitlab.KindAuth, ExitAuth},
		{"not_found", gitlab.KindNotFound, ExitNotFound},
		{"conflict", gitlab.KindConflict, ExitConflict},
		{"transient", gitlab.KindTransient, ExitTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			logger := newTestLoggerForHelper(buf)

			gitlabErr := &gitlab.Error{
				Kind:       tc.kind,
				StatusCode: 500,
				Method:     "GET",
				URL:        "https://gitlab.example.com/test",
				Body:       "test error",
			}
			fake := &fakeReviewer{err: gitlabErr}

			result, err := executeReview(context.Background(), fake, &ReviewCmd{
				Repo: "foo/bar",
				MR:   42,
			}, "foo/bar", logger)

			if result != nil {
				t.Errorf("expected nil result on error, got %+v", result)
			}
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			var exitErr *ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("expected *ExitError, got %T: %v", err, err)
			}
			if exitErr.Code != tc.wantCode {
				t.Errorf("kind=%s: code=%d, want %d", tc.kind, exitErr.Code, tc.wantCode)
			}
			if fake.callCount != 1 {
				t.Errorf("fake.Run called %d times, want 1", fake.callCount)
			}
			if fake.lastProject != "foo/bar" {
				t.Errorf("fake.lastProject = %q, want %q", fake.lastProject, "foo/bar")
			}
			if fake.lastIID != 42 {
				t.Errorf("fake.lastIID = %d, want 42", fake.lastIID)
			}
		})
	}
}

// TestExecuteReview_NonGitLabError_MapsToInternal covers
// the fallback path: a non-typed error from the orchestrator
// (e.g. context cancellation, internal bug) maps to
// ExitInternal via the default case. This is what catches
// programming errors that don't have a gitlab.Error wrapper.
func TestExecuteReview_NonGitLabError_MapsToInternal(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := newTestLoggerForHelper(buf)
	fake := &fakeReviewer{err: errTest} // stringError, not *gitlab.Error

	_, err := executeReview(context.Background(), fake, &ReviewCmd{
		Repo: "foo/bar",
		MR:   42,
	}, "foo/bar", logger)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected *ExitError, got %T", err)
	}
	if exitErr.Code != ExitInternal {
		t.Errorf("non-gitlab error should map to ExitInternal, got %d", exitErr.Code)
	}
}

// TestExecuteReview_Success_ReturnsResult covers the
// happy path: the orchestrator returns a result with no
// error. executeReview must return (result, nil) — the
// PolicyError check lives in summarizeReview, not here.
func TestExecuteReview_Success_ReturnsResult(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := newTestLoggerForHelper(buf)
	want := &reviewer.Result{
		MR: &gitlab.MergeRequest{IID: 42, WebURL: "https://example.com/42"},
		Findings: []reviewer.PostedFinding{
			{Finding: reviewer.Finding{File: "x.go", Line: 1, Body: "test"}},
		},
		PolicyError: false,
	}
	fake := &fakeReviewer{result: want}

	got, err := executeReview(context.Background(), fake, &ReviewCmd{
		Repo: "foo/bar",
		MR:   42,
	}, "foo/bar", logger)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if got != want {
		t.Errorf("result = %+v, want %+v", got, want)
	}
}

// ---------------------------------------------------------------------------
// summarizeReview
// ---------------------------------------------------------------------------

// TestSummarizeReview_NoMR_NoPrint covers the edge case
// where the result has no MR (the same-commit dedup skip
// path). summarizeReview must NOT panic on a nil MR and
// must NOT print anything to stdout.
func TestSummarizeReview_NoMR_NoPrint(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	logger := newTestLoggerForHelper(stderr)
	result := &reviewer.Result{MR: nil}

	err := summarizeReview(result, stdout, logger)
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("expected no stdout, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "review complete") {
		t.Errorf("expected 'review complete' log line, got: %s", stderr.String())
	}
}

// TestSummarizeReview_WithMR_PrintsURL covers the happy
// path: the result has a non-nil MR with WebURL. The
// summarizeReview helper must print the URL to stdout
// (matching the repo-mr-file convention) and emit the
// "review complete" log line. Returns nil because
// PolicyError is false.
func TestSummarizeReview_WithMR_PrintsURL(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	logger := newTestLoggerForHelper(stderr)
	result := &reviewer.Result{
		MR: &gitlab.MergeRequest{
			IID:    42,
			WebURL: "https://gitlab.example.com/foo/bar/-/merge_requests/42",
		},
		PolicyError: false,
	}

	err := summarizeReview(result, stdout, logger)
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
	if !strings.Contains(stdout.String(), "https://gitlab.example.com/foo/bar/-/merge_requests/42") {
		t.Errorf("expected WebURL in stdout, got: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "review complete") {
		t.Errorf("expected 'review complete' log line, got: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"policy_error":false`) {
		t.Errorf("expected policy_error=false in log, got: %s", stderr.String())
	}
}

// TestSummarizeReview_PolicyError_ReturnsExitPolicy covers
// the only branch in summarizeReview that returns a
// non-nil error: result.PolicyError == true. The exit
// code is ExitPolicy (8), and the reason is "policy
// violation" (matching the documentation).
func TestSummarizeReview_PolicyError_ReturnsExitPolicy(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	logger := newTestLoggerForHelper(stderr)
	result := &reviewer.Result{
		MR:          nil,
		PolicyError: true,
	}

	err := summarizeReview(result, stdout, logger)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected *ExitError, got %T", err)
	}
	if exitErr.Code != ExitPolicy {
		t.Errorf("expected ExitPolicy (%d), got %d", ExitPolicy, exitErr.Code)
	}
	if exitErr.Reason != "policy violation" {
		t.Errorf("Reason = %q, want %q", exitErr.Reason, "policy violation")
	}
	// The 'review complete' log line still fires (audit trail).
	if !strings.Contains(stderr.String(), "review complete") {
		t.Errorf("expected 'review complete' log line even on policy error, got: %s",
			stderr.String())
	}
	if !strings.Contains(stderr.String(), `"policy_error":true`) {
		t.Errorf("expected policy_error=true in log, got: %s", stderr.String())
	}
}

// TestSummarizeReview_NoPolicyError_ReturnsNil covers the
// default branch: PolicyError is false, the helper returns
// nil so run() can return 0.
func TestSummarizeReview_NoPolicyError_ReturnsNil(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	logger := newTestLoggerForHelper(stderr)
	result := &reviewer.Result{
		MR:          &gitlab.MergeRequest{IID: 1, WebURL: "https://example.com/1"},
		PolicyError: false,
	}

	err := summarizeReview(result, stdout, logger)
	if err != nil {
		t.Errorf("expected nil error for clean review, got %v", err)
	}
}
