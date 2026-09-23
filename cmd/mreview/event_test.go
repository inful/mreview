package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestRun_Review_UnsupportedSource_ExitsZero drives `mreview
// review` end-to-end with CI_PIPELINE_SOURCE set to a value the
// guard skips. Asserts:
//   - exit code is 0 (skip is a clean exit, not a failure)
//   - no GitLab API call is made (no "starting review" log line)
//   - the debug log line carries the skip reason (visible only
//     when --verbose is set; the test enables it)
//
// This is the integration test that pins the guard's behaviour
// at the CLI boundary — the unit tests in internal/event cover
// the decision logic; this one proves the wiring reaches the
// CLI layer.
func TestRun_Review_UnsupportedSource_ExitsZero(t *testing.T) {
	cases := []struct {
		name      string
		envSource string
	}{
		{"trigger", "trigger"},
		{"pipeline", "pipeline"},
		{"parent_pipeline", "parent_pipeline"},
		{"webide", "webide"},
		{"chat", "chat"},
		{"external_pull_request_event", "external_pull_request_event"},
		{"unknown future value", "future-source"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CI_PIPELINE_SOURCE", c.envSource)
			t.Setenv("CI_MERGE_REQUEST_IID", "")
			t.Setenv("CI_MERGE_REQUEST_DRAFT", "")

			// Need a repo + mr so the parser is happy. We expect
			// the guard to fire BEFORE buildClients, so the
			// token never gets used.
			stdout := &bytes.Buffer{}
			stderr := &bytes.Buffer{}
			code := run(context.Background(),
				[]string{
					"review",
					"--repo=foo/bar",
					"--mr=42",
					"--gitlab-token=test",
					"--verbose",
					"--log-format=json",
				},
				stdout, stderr,
			)
			if code != 0 {
				t.Errorf("skip should exit 0; got %d\nstderr: %s",
					code, stderr.String())
			}

			// Guard debug line is emitted before "starting review";
			// the absence of the latter proves we never reached
			// buildClients / GitLab. --verbose enables Debug-level
			// output so the skip line is visible.
			if strings.Contains(stderr.String(), "starting review") {
				t.Errorf("guard should fire before review starts; got %s",
					stderr.String())
			}
			if !strings.Contains(stderr.String(), "skipping review per per-event guard") {
				t.Errorf("expected skip debug line, got: %s", stderr.String())
			}
		})
	}
}

// TestRun_Review_DraftMR_ExitsZero sets up an MR-event run with
// CI_MERGE_REQUEST_DRAFT=true and verifies the guard skips with
// exit 0. The default --on-drafts is "skip"; this test pins that
// default.
func TestRun_Review_DraftMR_ExitsZero(t *testing.T) {
	t.Setenv("CI_PIPELINE_SOURCE", "merge_request_event")
	t.Setenv("CI_MERGE_REQUEST_IID", "42")
	t.Setenv("CI_MERGE_REQUEST_DRAFT", "true")

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := run(context.Background(),
		[]string{
			"review",
			"--repo=foo/bar",
			"--mr=42",
			"--gitlab-token=test",
			"--verbose",
			"--log-format=json",
		},
		stdout, stderr,
	)
	if code != 0 {
		t.Errorf("draft MR with default --on-drafts=skip should exit 0; got %d\nstderr: %s",
			code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "skipping review per per-event guard") {
		t.Errorf("expected skip debug line, got: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "draft MR") {
		t.Errorf("expected skip reason to mention draft MR, got: %s", stderr.String())
	}
}

// TestRun_Review_DraftMR_WithRunOverride_Proceeds exercises the
// override path. The guard let the invocation through; the
// downstream GitLab call fails auth (test token) — that's
// expected and proves the review path is reachable.
//
// The test asserts the override log line and the "starting
// review" line appear, but accepts any non-panic exit code from
// the auth-failing downstream.
func TestRun_Review_DraftMR_WithRunOverride_Proceeds(t *testing.T) {
	t.Setenv("CI_PIPELINE_SOURCE", "merge_request_event")
	t.Setenv("CI_MERGE_REQUEST_IID", "42")
	t.Setenv("CI_MERGE_REQUEST_DRAFT", "true")

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	run(context.Background(),
		[]string{
			"review",
			"--repo=foo/bar",
			"--mr=42",
			"--gitlab-token=test",
			"--on-drafts=run",
			"--log-format=json",
		},
		stdout, stderr,
	)
	if !strings.Contains(stderr.String(), "starting review") {
		t.Errorf("--on-drafts=run should let the review proceed; got:\n%s",
			stderr.String())
	}
	if !strings.Contains(stderr.String(), "proceeding with review per override") {
		t.Errorf("expected override log line, got: %s", stderr.String())
	}
}

// TestRun_Review_LocalInvocation_NoEnv_Proceeds pins the local-dev
// contract: with CI_PIPELINE_SOURCE unset, the guard always lets
// the review through. The downstream auth failure is expected;
// the test only verifies the guard let the call through.
func TestRun_Review_LocalInvocation_NoEnv_Proceeds(t *testing.T) {
	t.Setenv("CI_PIPELINE_SOURCE", "")
	t.Setenv("CI_MERGE_REQUEST_IID", "")
	t.Setenv("CI_MERGE_REQUEST_DRAFT", "")

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	run(context.Background(),
		[]string{
			"review",
			"--repo=foo/bar",
			"--mr=42",
			"--gitlab-token=test",
			"--log-format=json",
		},
		stdout, stderr,
	)
	if !strings.Contains(stderr.String(), "starting review") {
		t.Errorf("local invocation should proceed; got: %s", stderr.String())
	}
}

// TestRun_Review_HelpFlagShowsNewFlags verifies the new flags
// show up in `mreview review --help` — the operator-facing
// discoverability contract.
func TestRun_Review_HelpFlagShowsNewFlags(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := run(context.Background(),
		[]string{"review", "--help"},
		stdout, stderr,
	)
	if code != ExitOK {
		t.Errorf("--help returned %d, want 0", code)
	}
	for _, want := range []string{
		// Per-event guard (PR #1).
		"--on-drafts", "--on-push", "skip", "run",
		// Tokensave MCP integration (PR #4).
		"--tokensave-enabled", "--tokensave-bin",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("--help output missing %q\n%s", want, stdout.String())
		}
	}
}

// TestRun_Review_PushSource_DefaultSkips sets CI_PIPELINE_SOURCE
// to push and verifies the default (--on-push=skip) exits 0
// without a review.
func TestRun_Review_PushSource_DefaultSkips(t *testing.T) {
	t.Setenv("CI_PIPELINE_SOURCE", "push")
	t.Setenv("CI_MERGE_REQUEST_IID", "")
	t.Setenv("CI_MERGE_REQUEST_DRAFT", "")

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := run(context.Background(),
		[]string{
			"review",
			"--repo=foo/bar",
			"--mr=42",
			"--gitlab-token=test",
			"--verbose",
			"--log-format=json",
		},
		stdout, stderr,
	)
	if code != 0 {
		t.Errorf("push with default --on-push=skip should exit 0; got %d\nstderr: %s",
			code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "push source") {
		t.Errorf("expected reason to mention push source, got: %s", stderr.String())
	}
}

// TestRun_Review_PushSource_WithRunOverride_Proceeds exercises
// the --on-push=run override. The guard let the call through;
// the downstream auth failure is expected and proves the review
// path is reachable.
func TestRun_Review_PushSource_WithRunOverride_Proceeds(t *testing.T) {
	t.Setenv("CI_PIPELINE_SOURCE", "push")
	t.Setenv("CI_MERGE_REQUEST_IID", "")
	t.Setenv("CI_MERGE_REQUEST_DRAFT", "")

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	run(context.Background(),
		[]string{
			"review",
			"--repo=foo/bar",
			"--mr=42",
			"--gitlab-token=test",
			"--on-push=run",
			"--log-format=json",
		},
		stdout, stderr,
	)
	if !strings.Contains(stderr.String(), "starting review") {
		t.Errorf("--on-push=run should let the review proceed; got:\n%s",
			stderr.String())
	}
}
