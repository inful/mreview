package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inful/mreview/internal/logging"
	"github.com/inful/mreview/internal/reviewer"
)

// runReviewDirect drives runReview() with a minimal ReviewCmd
// and captures stdout/stderr. It is the test-only entry point
// for the function — the production path is run() in main.go,
// which the existing TestRun_* tests in main_test.go and
// event_test.go already cover end-to-end.
//
// Using this directly (not via run()) lets us:
//   - Lock the runReview contract at a smaller surface — a
//     regression here surfaces without the kong parser in
//     the way.
//   - Skip the per-event guard's effects on the env-based
//     test fixtures by setting t.Setenv directly.
//   - Use a JSON logger to assert on log structure rather
//     than parsing the text format.
//
// The cfg parameter is passed through as nil (no policy
// file) — when a test needs policy it builds a *ReviewCmd
// with PolicyFile set and we exercise the load path.
func runReviewDirect(t *testing.T, env map[string]string, mutate func(c *ReviewCmd)) (code int, stdout, stderr string) {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
	stdoutBuf := &bytes.Buffer{}
	stderrBuf := &bytes.Buffer{}
	c := &ReviewCmd{
		Repo:        "foo/bar",
		MR:          42,
		GitLabURL:   "https://gitlab.example.com",
		GitLabToken: "test-token",
		Provider:    "local",
		Model:       "test-model",
		WorkDir:     t.TempDir(),
		BotUsername: "review-bot",
		// LogFormat + Verbose live on the parent CLI struct,
		// not on ReviewCmd. The test always uses JSON + Debug
		// so the per-event-guard debug line is visible.
	}
	if mutate != nil {
		mutate(c)
	}
	logger, err := logging.New(stderrBuf, "json", true)
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	runErr := runReview(context.Background(), stdoutBuf, c, nil, logger)
	stdout = stdoutBuf.String()
	stderr = stderrBuf.String()
	if runErr == nil {
		return 0, stdout, stderr
	}
	var exitErr *ExitError
	if errors.As(runErr, &exitErr) {
		return exitErr.Code, stdout, stderr
	}
	// runReview may also return a non-ExitError when, e.g.,
	// a downstream dependency errors. Map those to ExitInternal
	// so the test sees a deterministic code.
	return ExitInternal, stdout, stderr
}

// ---------------------------------------------------------------------------
// Task 1.1: per-event guard
// ---------------------------------------------------------------------------
//
// The event_test.go suite already covers these scenarios via
// run() (the kong-level entry point). These tests drive
// runReview() directly to lock the function's contract at a
// smaller surface; the redundancy is the point — a regression
// in runReview's guard logic surfaces here without the kong
// layer in the way.

// TestRunReview_PerEventGuard_SkipsOnDrafts drives a CI env
// where the MR is a draft and the operator's --on-drafts=skip
// (the default). The guard must short-circuit before any
// workdir / policy / harness work; we assert that by
// checking for the "skipping review" debug line and the
// absence of the "starting review" line.
func TestRunReview_PerEventGuard_SkipsOnDrafts(t *testing.T) {
	code, _, stderr := runReviewDirect(t, map[string]string{
		"CI_PIPELINE_SOURCE":     "merge_request_event",
		"CI_MERGE_REQUEST_IID":   "42",
		"CI_MERGE_REQUEST_DRAFT": "true",
	}, func(c *ReviewCmd) {
		// OnDrafts is the CLI default ("skip"). Set
		// explicitly so the test stays stable if the
		// default ever changes.
		c.OnDrafts = "skip"
	})
	if code != 0 {
		t.Errorf("draft + on-drafts=skip should exit 0, got %d\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "skipping review per per-event guard") {
		t.Errorf("expected skip debug line, got: %s", stderr)
	}
	if strings.Contains(stderr, "starting review") {
		t.Errorf("guard should fire before 'starting review'; got: %s", stderr)
	}
	if !strings.Contains(stderr, "draft") {
		t.Errorf("expected skip reason to mention 'draft', got: %s", stderr)
	}
}

// TestRunReview_PerEventGuard_ProceedsOnDraftsWithOverride
// covers --on-drafts=run: the guard must let the review
// through and emit the "proceeding with review per
// override" log line. We don't assert on the downstream
// outcome; we only assert the guard did the right thing.
//
// installFakeReviewer is required here because the test
// fixture has no CI env (the guard lets it through) and
// the real orchestrator would try to make a GitLab API
// call. The fake returns a successful empty result so the
// test exits cleanly.
func TestRunReview_PerEventGuard_ProceedsOnDraftsWithOverride(t *testing.T) {
	installFakeReviewer(t, &fakeReviewer{result: &reviewer.Result{}})
	_, _, stderr := runReviewDirect(t, map[string]string{
		"CI_PIPELINE_SOURCE":     "merge_request_event",
		"CI_MERGE_REQUEST_IID":   "42",
		"CI_MERGE_REQUEST_DRAFT": "true",
	}, func(c *ReviewCmd) {
		c.OnDrafts = "run"
	})
	if !strings.Contains(stderr, "proceeding with review per override") {
		t.Errorf("expected override log line, got: %s", stderr)
	}
	if !strings.Contains(stderr, "starting review") {
		t.Errorf("override should let review proceed past guard; got: %s", stderr)
	}
}

// TestRunReview_PerEventGuard_SkipsOnPush covers the push
// source with the default --on-push=skip.
func TestRunReview_PerEventGuard_SkipsOnPush(t *testing.T) {
	code, _, stderr := runReviewDirect(t, map[string]string{
		"CI_PIPELINE_SOURCE":   "push",
		"CI_MERGE_REQUEST_IID": "",
	}, func(c *ReviewCmd) {
		c.OnPush = "skip"
	})
	if code != 0 {
		t.Errorf("push + on-push=skip should exit 0, got %d\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "skipping review per per-event guard") {
		t.Errorf("expected skip debug line, got: %s", stderr)
	}
	if !strings.Contains(stderr, "push") {
		t.Errorf("expected skip reason to mention 'push', got: %s", stderr)
	}
}

// TestRunReview_PerEventGuard_LocalInvocationProceeds pins
// the local-dev contract: with no CI env, the guard always
// lets the review through regardless of --on-drafts /
// --on-push. Asserts the review starts; the downstream auth
// failure is irrelevant to the guard's behaviour.
//
// installFakeReviewer is required here for the same reason
// as ProceedsOnDraftsWithOverride: the test reaches the
// orchestrator, which would otherwise make a real network
// call.
func TestRunReview_PerEventGuard_LocalInvocationProceeds(t *testing.T) {
	installFakeReviewer(t, &fakeReviewer{result: &reviewer.Result{}})
	_, _, stderr := runReviewDirect(t, map[string]string{
		"CI_PIPELINE_SOURCE": "",
	}, nil)
	if !strings.Contains(stderr, "starting review") {
		t.Errorf("local invocation should proceed past guard; got: %s", stderr)
	}
	if strings.Contains(stderr, "skipping review per per-event guard") {
		t.Errorf("local invocation should NOT skip; got: %s", stderr)
	}
}

// ---------------------------------------------------------------------------
// Task 1.2: empty --workdir fail-fast
// ---------------------------------------------------------------------------

// TestRunReview_EmptyWorkdir_FailsFast covers the regression
// path from the tokensave-only refactor: an empty --workdir
// must abort with ExitConfig before the orchestrator is
// built. The check sits between the per-event guard and
// the policy load, so a scheduled run that's skipping
// (draft / push) doesn't surface a confusing config error
// instead of the expected skip log.
func TestRunReview_EmptyWorkdir_FailsFast(t *testing.T) {
	t.Setenv("MREVIEW_WORKDIR", "") // make sure the env fallback isn't masking the test
	stdoutBuf := &bytes.Buffer{}
	stderrBuf := &bytes.Buffer{}
	c := &ReviewCmd{
		Repo:        "foo/bar",
		MR:          42,
		GitLabToken: "test-token",
		Provider:    "local",
		Model:       "test-model",
		WorkDir:     "", // the bug condition
	}
	logger, err := logging.New(stderrBuf, "json", true)
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	runErr := runReview(context.Background(), stdoutBuf, c, nil, logger)
	if runErr == nil {
		t.Fatalf("empty --workdir should error, got nil\nstderr: %s", stderrBuf.String())
	}
	var exitErr *ExitError
	if !errors.As(runErr, &exitErr) {
		t.Fatalf("expected *ExitError, got %T: %v", runErr, runErr)
	}
	if exitErr.Code != ExitConfig {
		t.Errorf("expected ExitConfig (%d), got %d", ExitConfig, exitErr.Code)
	}
	stderr := stderrBuf.String()
	if !strings.Contains(stderr, "workdir is required") {
		t.Errorf("expected 'workdir is required' in stderr, got: %s", stderr)
	}
	if !strings.Contains(stderr, "MREVIEW_WORKDIR") {
		t.Errorf("error should mention MREVIEW_WORKDIR env var, got: %s", stderr)
	}
}

// ---------------------------------------------------------------------------
// Task 1.3: artifact load graceful degradation
// ---------------------------------------------------------------------------

// TestRunReview_ArtifactsDir_Missing_Proceeds covers the
// degraded path: --artifacts-dir points at a directory that
// doesn't exist. mreview logs a debug line and proceeds
// without artifacts. The review path is reachable; the
// test asserts the warning AND that "starting review" still
// fires.
//
// installFakeReviewer is required: without it, runReview
// would try to build a real orchestrator and make a GitLab
// network call after the artifact-load path completes.
func TestRunReview_ArtifactsDir_Missing_Proceeds(t *testing.T) {
	installFakeReviewer(t, &fakeReviewer{result: &reviewer.Result{}})
	_, _, stderr := runReviewDirect(t, nil, func(c *ReviewCmd) {
		c.ArtifactsDir = "/nonexistent/path/to/artifacts"
	})
	if !strings.Contains(stderr, "artifacts dir not available") {
		t.Errorf("expected 'artifacts dir not available' log line, got: %s", stderr)
	}
	if !strings.Contains(stderr, "starting review") {
		t.Errorf("missing artifacts dir should not block the review; got: %s", stderr)
	}
}

// TestRunReview_ArtifactsDir_Present_LogsLoaded covers the
// happy path: --artifacts-dir points at a directory with
// stub artifact files. The "artifacts loaded" log line fires
// with a status label per artifact.
//
// installFakeReviewer is required (same reason as the
// missing-dir test).
func TestRunReview_ArtifactsDir_Present_LogsLoaded(t *testing.T) {
	installFakeReviewer(t, &fakeReviewer{result: &reviewer.Result{}})
	dir := t.TempDir()
	// Minimal stub artifacts. The loader treats absent /
	// malformed files as LoadResult with no error at the
	// directory level — we just need the files to exist so
	// the loader considers the dir reachable.
	if err := os.WriteFile(filepath.Join(dir, "build.log"), []byte("ok\n"), 0o644); err != nil {
		t.Fatalf("write build.log: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "test_results.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write test_results.json: %v", err)
	}

	_, _, stderr := runReviewDirect(t, nil, func(c *ReviewCmd) {
		c.ArtifactsDir = dir
	})
	if !strings.Contains(stderr, "artifacts loaded") {
		t.Errorf("expected 'artifacts loaded' log line, got: %s", stderr)
	}
	if !strings.Contains(stderr, `"build"`) {
		t.Errorf("expected build status in log, got: %s", stderr)
	}
	if !strings.Contains(stderr, `"tests"`) {
		t.Errorf("expected tests status in log, got: %s", stderr)
	}
}
