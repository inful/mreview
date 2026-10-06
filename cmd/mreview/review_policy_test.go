package main

import (
	"testing"

	"github.com/inful/mreview/internal/reviewer"
)

// TestRunReview_PolicyViolation_ExitsPolicy covers the path
// where the orchestrator's policy enforcement raised at
// least one error-verdict finding. The orchestrator
// signals this by setting result.PolicyError = true; runReview
// must convert that to *ExitError{Code: ExitPolicy, Reason:
// "policy violation"} so the process exit code tells the CI
// pipeline that a policy gate failed.
//
// This is the only branch in runReview that depends on
// fields INSIDE the result (as opposed to the error
// returned from Run), so the fake has to construct a
// non-nil *reviewer.Result with PolicyError = true.
func TestRunReview_PolicyViolation_ExitsPolicy(t *testing.T) {
	fake := &fakeReviewer{
		result: &reviewer.Result{
			MR:          nil, // not needed for this assertion
			Findings:    nil,
			Summary:     nil,
			PolicyError: true, // the key signal
		},
	}
	installFakeReviewer(t, fake)

	code, _, stderr := runReviewDirect(t, map[string]string{
		"CI_PIPELINE_SOURCE": "",
	}, nil)

	if code != ExitPolicy {
		t.Errorf("policy violation should exit %d, got %d\nstderr: %s",
			ExitPolicy, code, stderr)
	}
	// The 'review complete' log line carries policy_error=true;
	// the "policy violation" string itself is only in the
	// *ExitError.Reason field (not logged). The exit code is
	// the operator-facing signal; the log line is the audit
	// trail.
	if !contains(stderr, "review complete") {
		t.Errorf("expected 'review complete' log line, got: %s", stderr)
	}
	if !contains(stderr, `"policy_error":true`) {
		t.Errorf("expected policy_error=true in log, got: %s", stderr)
	}
}

// contains is a tiny indirection over strings.Contains so
// the test file's assertions are uniform — the policy
// tests use it like the rest of the test suite.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
