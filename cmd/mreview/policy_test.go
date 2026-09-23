package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

// TestRun_Review_PolicyFile_InvalidYAML_ExitsConfig covers the
// fail-fast path: a malformed policy file is rejected at
// startup with ExitConfig before any GitLab / harness work
// happens.
func TestRun_Review_PolicyFile_InvalidYAML_ExitsConfig(t *testing.T) {
	dir := t.TempDir()
	policyPath := dir + "/bad.yaml"
	if err := os.WriteFile(policyPath, []byte("severity_overrides: [bogus"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := run(context.Background(),
		[]string{
			"review",
			"--repo=foo/bar",
			"--mr=42",
			"--gitlab-token=test",
			"--policy-file=" + policyPath,
			"--log-format=json",
		},
		stdout, stderr,
	)
	if code != ExitConfig {
		t.Errorf("malformed policy should exit %d, got %d", ExitConfig, code)
	}
	// The guard fires before policy loading, but we set no CI
	// env vars here so the local path proceeds through policy
	// load. Assert the policy error reached the user.
	if !strings.Contains(stderr.String(), "policy") {
		t.Errorf("expected 'policy' in stderr, got: %s", stderr.String())
	}
}

// TestRun_Review_PolicyFile_UnknownField_ExitsConfig confirms
// the strict-validator rejects unknown top-level keys.
func TestRun_Review_PolicyFile_UnknownField_ExitsConfig(t *testing.T) {
	dir := t.TempDir()
	policyPath := dir + "/unknown.yaml"
	if err := os.WriteFile(policyPath, []byte("bogus: 42\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := run(context.Background(),
		[]string{
			"review",
			"--repo=foo/bar",
			"--mr=42",
			"--gitlab-token=test",
			"--policy-file=" + policyPath,
			"--log-format=json",
		},
		stdout, stderr,
	)
	if code != ExitConfig {
		t.Errorf("unknown policy field should exit %d, got %d", ExitConfig, code)
	}
	if !strings.Contains(stderr.String(), "unknown top-level field") {
		t.Errorf("expected 'unknown top-level field' in stderr, got: %s", stderr.String())
	}
}

// TestRun_Review_PolicyFile_MissingFile_ExitsConfig covers the
// "policy file doesn't exist" path.
func TestRun_Review_PolicyFile_MissingFile_ExitsConfig(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := run(context.Background(),
		[]string{
			"review",
			"--repo=foo/bar",
			"--mr=42",
			"--gitlab-token=test",
			"--policy-file=/nonexistent/policy.yaml",
			"--log-format=json",
		},
		stdout, stderr,
	)
	if code != ExitConfig {
		t.Errorf("missing policy file should exit %d, got %d", ExitConfig, code)
	}
	if !strings.Contains(stderr.String(), "read") {
		t.Errorf("expected 'read' in stderr (file-open error), got: %s", stderr.String())
	}
}

// TestRun_Review_PolicyFile_Valid_ProceedsPastLoad covers the
// happy path: a well-formed policy file loads cleanly and the
// "policy loaded" log line fires. We don't reach the GitLab
// call (the test token is rejected by gitlab.com), so we only
// assert the load succeeded.
//
// This is the integration smoke test for PR #2's "load only"
// wiring — the actual enforcement call lives in PR #3 (the
// orchestrator).
func TestRun_Review_PolicyFile_Valid_ProceedsPastLoad(t *testing.T) {
	dir := t.TempDir()
	policyPath := dir + "/policy.yaml"
	yaml := `
severity_overrides:
  - pattern: "**/*.go"
    severity: error
forbid:
  - id: no-todo
    pattern: "TODO"
    message: "TODO comments are not allowed"
require:
  - id: has-tests
    pattern: "internal/**/*_test.go"
    message: "internal changes require a test"
labels:
  security-review: error
`
	if err := os.WriteFile(policyPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	run(context.Background(),
		[]string{
			"review",
			"--repo=foo/bar",
			"--mr=42",
			"--gitlab-token=test",
			"--policy-file=" + policyPath,
			"--log-format=json",
		},
		stdout, stderr,
	)
	if !strings.Contains(stderr.String(), "policy loaded") {
		t.Errorf("expected 'policy loaded' log line, got: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"severity_overrides":1`) {
		t.Errorf("expected 1 severity_overrides in log, got: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"forbid_rules":1`) {
		t.Errorf("expected 1 forbid_rules in log, got: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"require_rules":1`) {
		t.Errorf("expected 1 require_rules in log, got: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"label_rules":1`) {
		t.Errorf("expected 1 label_rules in log, got: %s", stderr.String())
	}
}

// TestRun_Review_PolicyFile_EmptyFile_OK covers the "empty
// policy file" path: an empty file is a valid (zero-rule)
// policy and loads cleanly.
func TestRun_Review_PolicyFile_EmptyFile_OK(t *testing.T) {
	dir := t.TempDir()
	policyPath := dir + "/empty.yaml"
	if err := os.WriteFile(policyPath, []byte(""), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	run(context.Background(),
		[]string{
			"review",
			"--repo=foo/bar",
			"--mr=42",
			"--gitlab-token=test",
			"--policy-file=" + policyPath,
			"--log-format=json",
		},
		stdout, stderr,
	)
	if !strings.Contains(stderr.String(), "policy loaded") {
		t.Errorf("expected 'policy loaded' log line, got: %s", stderr.String())
	}
	// Zero rules logged.
	if !strings.Contains(stderr.String(), `"severity_overrides":0`) {
		t.Errorf("expected 0 severity_overrides in log, got: %s", stderr.String())
	}
}

// TestRun_Review_NoPolicyFile_Proceeds confirms that omitting
// --policy-file entirely is a no-op (the review runs without
// any policy at all).
func TestRun_Review_NoPolicyFile_Proceeds(t *testing.T) {
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
	if strings.Contains(stderr.String(), "policy loaded") {
		t.Errorf("without --policy-file, should not log 'policy loaded'; got: %s",
			stderr.String())
	}
	// The local-proceeds path should still run.
	if !strings.Contains(stderr.String(), "starting review") {
		t.Errorf("expected 'starting review' log line, got: %s", stderr.String())
	}
}

// TestRun_Review_HelpFlagShowsPolicyFlag pins the
// discoverability contract: the new flag shows up in
// `mreview review --help`.
func TestRun_Review_HelpFlagShowsPolicyFlag(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := run(context.Background(),
		[]string{"review", "--help"},
		stdout, stderr,
	)
	if code != ExitOK {
		t.Errorf("--help returned %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "--policy-file") {
		t.Errorf("--help output missing --policy-file\n%s", stdout.String())
	}
}
