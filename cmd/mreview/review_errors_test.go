package main

import (
	"testing"

	"github.com/inful/mreview/internal/gitlab"
)

// TestRunReview_GitLabError_Mapping covers the error →
// ExitError mapping at the end of runReview. Each
// gitlab.Error.Kind maps to a specific ExitError code; the
// test pins every mapping in one place so a future refactor
// that drops or rearranges a case is caught immediately.
//
// The table is intentionally exhaustive: 6 kinds (Other /
// BadRequest / Auth / NotFound / Conflict / Transient) × 1
// expected exit code each. The default case (KindOther) is
// the one the operator is most likely to see when something
// goes unexpectedly wrong, so the test asserts it
// explicitly rather than letting it fall into the
// ExitInternal catch-all silently.
func TestRunReview_GitLabError_Mapping(t *testing.T) {
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
			gitlabErr := &gitlab.Error{
				Kind:       tc.kind,
				StatusCode: 500, // arbitrary; the kind is what matters
				Method:     "GET",
				URL:        "https://gitlab.example.com/test",
				Body:       "test error",
			}
			fake := &fakeReviewer{err: gitlabErr}
			installFakeReviewer(t, fake)

			code, _, stderr := runReviewDirect(t, map[string]string{
				"CI_PIPELINE_SOURCE": "",
			}, nil)
			if code != tc.wantCode {
				t.Errorf("kind=%s: got exit %d, want %d\nstderr: %s",
					tc.kind, code, tc.wantCode, stderr)
			}

			// The fake must have been called exactly once
			// (the guard didn't short-circuit because
			// there's no CI env, and runReview's error
			// mapping fired).
			if fake.callCount != 1 {
				t.Errorf("fake.Run called %d times, want 1", fake.callCount)
			}

			// runReview should have forwarded the project
			// and IID through to the orchestrator.
			if fake.lastProject != "foo/bar" {
				t.Errorf("fake.lastProject = %q, want %q", fake.lastProject, "foo/bar")
			}
			if fake.lastIID != 42 {
				t.Errorf("fake.lastIID = %d, want 42", fake.lastIID)
			}
		})
	}
}

// TestRunReview_NonGitLabError_MapsToInternal covers the
// fallback path: a non-typed error (not a *gitlab.Error)
// from the orchestrator maps to ExitInternal. This is what
// happens for, e.g., a context cancellation that bubbles up
// as a non-typed error, or a programming error in the
// orchestrator itself.
func TestRunReview_NonGitLabError_MapsToInternal(t *testing.T) {
	fake := &fakeReviewer{err: errTest}
	installFakeReviewer(t, fake)

	code, _, _ := runReviewDirect(t, map[string]string{
		"CI_PIPELINE_SOURCE": "",
	}, nil)
	if code != ExitInternal {
		t.Errorf("non-gitlab error should map to ExitInternal, got %d", code)
	}
}

// errTest is a non-typed error used by tests that need a
// generic error value distinct from *gitlab.Error.
var errTest = stringError("test-only non-gitlab error")

// stringError is a minimal error implementation used in
// tests. We avoid errors.New so the literal doesn't appear in
// a stack-trace-producing position.
type stringError string

func (e stringError) Error() string { return string(e) }
