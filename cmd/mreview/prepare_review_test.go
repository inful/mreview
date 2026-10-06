package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inful/mreview/internal/logging"
)

// helper-level tests for prepareReview. The end-to-end
// behaviour is already covered by TestRunReview_* in
// review_test.go; these tests pin the helper's contract at
// a smaller surface so a refactor of prepareReview doesn't
// have to drive the full runReview pipeline.

// runPrepareReview is the test-side equivalent of runReviewDirect
// for prepareReview only. It sets a sensible default *ReviewCmd
// and lets the test override fields via the callback. The
// returned (code, stdout, stderr) mirror the runReviewDirect
// signature so the test pattern is uniform across helpers.
func runPrepareReview(t *testing.T, env map[string]string, mutate func(c *ReviewCmd)) (skip bool, exitCode int, stdout, stderr string, prep *preparedReview) {
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
	}
	if mutate != nil {
		mutate(c)
	}
	logger, err := logging.New(stderrBuf, "json", true)
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	prep, prepErr := prepareReview(c, logger)
	stdout = stdoutBuf.String()
	stderr = stderrBuf.String()
	if errors.Is(prepErr, errSkipReview) {
		return true, 0, stdout, stderr, nil
	}
	if prepErr != nil {
		var exitErr *ExitError
		if errors.As(prepErr, &exitErr) {
			return false, exitErr.Code, stdout, stderr, nil
		}
		t.Fatalf("prepareReview returned non-ExitError: %v", prepErr)
	}
	return false, 0, stdout, stderr, prep
}

// TestPrepareReview_SkipsOnDrafts covers the per-event
// guard's skip path. The guard fires BEFORE any other
// check, so workdir / policy / artifact load are not
// touched. The sentinel error errSkipReview is the
// specific signal runReview translates to a clean nil
// return.
func TestPrepareReview_SkipsOnDrafts(t *testing.T) {
	skip, code, _, stderr, prep := runPrepareReview(t, map[string]string{
		"CI_PIPELINE_SOURCE":     "merge_request_event",
		"CI_MERGE_REQUEST_IID":   "42",
		"CI_MERGE_REQUEST_DRAFT": "true",
	}, func(c *ReviewCmd) {
		c.OnDrafts = "skip"
	})
	if !skip {
		t.Errorf("expected skip=true, got false (code=%d, prep=%+v)", code, prep)
	}
	if prep != nil {
		t.Errorf("expected nil prep on skip, got %+v", prep)
	}
	if !strings.Contains(stderr, "skipping review per per-event guard") {
		t.Errorf("expected skip debug line, got: %s", stderr)
	}
	if strings.Contains(stderr, "starting review") {
		t.Errorf("skip path should not log 'starting review'; got: %s", stderr)
	}
}

// TestPrepareReview_ProceedsOnDraftsWithOverride covers the
// guard's override path. The skip is NOT taken, so the
// function falls through to workdir + policy + artifact
// load. With WorkDir set and no policy/artifact dirs, the
// result is a populated *preparedReview with nil
// policy/artifact.
func TestPrepareReview_ProceedsOnDraftsWithOverride(t *testing.T) {
	skip, _, _, stderr, prep := runPrepareReview(t, map[string]string{
		"CI_PIPELINE_SOURCE":     "merge_request_event",
		"CI_MERGE_REQUEST_IID":   "42",
		"CI_MERGE_REQUEST_DRAFT": "true",
	}, func(c *ReviewCmd) {
		c.OnDrafts = "run"
	})
	if skip {
		t.Errorf("expected skip=false, got true")
	}
	if prep == nil {
		t.Fatal("expected non-nil prep, got nil")
	}
	if prep.policy != nil {
		t.Errorf("expected nil policy (no --policy-file), got %+v", prep.policy)
	}
	if prep.artifactSet != nil {
		t.Errorf("expected nil artifactSet (no --artifacts-dir), got %+v", prep.artifactSet)
	}
	if !strings.Contains(stderr, "proceeding with review per override") {
		t.Errorf("expected override log line, got: %s", stderr)
	}
	if !strings.Contains(stderr, "starting review") {
		t.Errorf("expected 'starting review' log line, got: %s", stderr)
	}
}

// TestPrepareReview_EmptyWorkdir_FailsFast covers the
// workdir guard. The check fires AFTER the per-event guard
// (so a scheduled run that should skip isn't bothered by a
// config error) but BEFORE the policy/artifact load. The
// error is wrapped in *ExitError{Code: ExitConfig} so the
// run() dispatcher maps it to exit code 2.
func TestPrepareReview_EmptyWorkdir_FailsFast(t *testing.T) {
	t.Setenv("MREVIEW_WORKDIR", "") // ensure the env fallback isn't masking the test
	skip, code, _, stderr, prep := runPrepareReview(t, map[string]string{
		"CI_PIPELINE_SOURCE": "",
	}, func(c *ReviewCmd) {
		c.WorkDir = "" // the bug condition
	})
	if skip {
		t.Errorf("expected skip=false, got true")
	}
	if code != ExitConfig {
		t.Errorf("expected ExitConfig (%d), got %d", ExitConfig, code)
	}
	if prep != nil {
		t.Errorf("expected nil prep on fail-fast, got %+v", prep)
	}
	if !strings.Contains(stderr, "workdir is required") {
		t.Errorf("expected 'workdir is required' in stderr, got: %s", stderr)
	}
	if !strings.Contains(stderr, "MREVIEW_WORKDIR") {
		t.Errorf("error should mention MREVIEW_WORKDIR env var, got: %s", stderr)
	}
}

// TestPrepareReview_InvalidPolicyFile_FailsFast covers
// the policy load path. A malformed YAML file returns a
// policy.Load error, which prepareReview wraps in
// *ExitError{Code: ExitConfig}. The error message starts
// with the "policy file" reason so log-grep finds it
// without the wrapping context.
func TestPrepareReview_InvalidPolicyFile_FailsFast(t *testing.T) {
	dir := t.TempDir()
	policyPath := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(policyPath, []byte("severity_overrides: [bogus"), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	skip, code, _, stderr, prep := runPrepareReview(t, map[string]string{
		"CI_PIPELINE_SOURCE": "",
	}, func(c *ReviewCmd) {
		c.PolicyFile = policyPath
	})
	if skip {
		t.Errorf("expected skip=false, got true")
	}
	if code != ExitConfig {
		t.Errorf("expected ExitConfig (%d), got %d", ExitConfig, code)
	}
	if prep != nil {
		t.Errorf("expected nil prep on policy fail, got %+v", prep)
	}
	if !strings.Contains(stderr, "policy file") {
		t.Errorf("expected 'policy file' in stderr, got: %s", stderr)
	}
}

// TestPrepareReview_ValidInputs_ReturnsPrepared covers the
// happy path: WorkDir set, valid empty policy file, valid
// artifact dir. The returned *preparedReview must carry
// the loaded policy + artifact set so the orchestrator can
// thread them into the harness prompt.
func TestPrepareReview_ValidInputs_ReturnsPrepared(t *testing.T) {
	dir := t.TempDir()
	policyPath := filepath.Join(dir, "policy.yaml")
	// Minimal valid policy: empty file parses as zero-rule.
	if err := os.WriteFile(policyPath, []byte(""), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	artifactsDir := filepath.Join(dir, "artifacts")
	if err := os.MkdirAll(artifactsDir, 0o755); err != nil {
		t.Fatalf("mkdir artifacts: %v", err)
	}

	skip, code, _, stderr, prep := runPrepareReview(t, map[string]string{
		"CI_PIPELINE_SOURCE": "",
	}, func(c *ReviewCmd) {
		c.PolicyFile = policyPath
		c.ArtifactsDir = artifactsDir
	})
	if skip {
		t.Errorf("expected skip=false, got true")
	}
	if code != 0 {
		t.Errorf("expected code=0, got %d", code)
	}
	if prep == nil {
		t.Fatal("expected non-nil prep, got nil")
	}
	if prep.policy == nil {
		t.Errorf("expected non-nil policy (file is valid empty), got nil")
	}
	// Empty artifacts dir still has BuildPath/TestsPath/etc. missing;
	// the loader doesn't fail at the directory level, so artifactSet
	// should be populated (just with all "missing" LoadResults).
	if prep.artifactSet == nil {
		t.Errorf("expected non-nil artifactSet, got nil")
	}
	if !strings.Contains(stderr, "policy loaded") {
		t.Errorf("expected 'policy loaded' log line, got: %s", stderr)
	}
	if !strings.Contains(stderr, "artifacts loaded") {
		t.Errorf("expected 'artifacts loaded' log line, got: %s", stderr)
	}
}

// Compile-time guard: preparedReview carries a policy
// pointer and an artifact set pointer. The fields are
// unexported, but a future refactor that changes the shape
// (e.g. drops artifactSet in favour of loading inline)
// would surface here. Kept as a separate test to document
// the contract.
func TestPreparedReview_FieldsExist(t *testing.T) {
	prep := preparedReview{}
	// Direct field access; this test won't compile if the
	// fields are renamed or removed, which is the point.
	_ = prep.policy
	_ = prep.artifactSet
}

// TestErrSkipReview_IsExportedSentinel covers the sentinel
// error's identity. errors.Is comparisons rely on the
// sentinel's identity, not its message. A test that
// compares err.Error() to a literal string is fragile (the
// message could change); errors.Is is the contract.
func TestErrSkipReview_IsExportedSentinel(t *testing.T) {
	if errSkipReview == nil {
		t.Fatal("errSkipReview must not be nil")
	}
	// errors.Is must match by identity.
	if !errors.Is(errSkipReview, errSkipReview) {
		t.Errorf("errSkipReview must satisfy errors.Is(errSkipReview, errSkipReview)")
	}
	// A wrapped error with errors.Is must resolve.
	wrapped := errors.Join(errors.New("context"), errSkipReview)
	if !errors.Is(wrapped, errSkipReview) {
		t.Errorf("wrapped errSkipReview should be detectable via errors.Is")
	}
	// A different error must NOT match.
	if errors.Is(errors.New("other"), errSkipReview) {
		t.Errorf("a different error must not match errSkipReview")
	}
}

// Compile-time guards for the slog logger type so this
// file's helper doesn't accidentally use a different
// logger type from the rest of the package. The actual
// test (TestPreparedReview_FieldsExist + the others above)
// covers the behaviour; this is just a documentation
// point.
var _ context.Context = context.Background()
var _ *slog.Logger = (*slog.Logger)(nil)
