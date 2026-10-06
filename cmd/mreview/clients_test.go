package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// minimalClientDeps returns a clientDeps with sensible
// defaults for smoke tests: a non-empty WorkDir (required by
// buildHarnessRuntime), a local provider (so provider.Build
// doesn't need a real API key), and a JSON logger writing
// to /dev/null (so test output stays clean).
//
// Tests override the fields they care about via the returned
// struct before calling buildReviewer / buildHarnessRuntime.
func minimalClientDeps() clientDeps {
	return clientDeps{
		GitLabURL:    "https://gitlab.example.com",
		GitLabToken:  "test-token",
		ProviderName: "local",
		Model:        "test-model",
		WorkDir:      "/tmp",
		MaxTurns:     1,
		Logger:       slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
	}
}

// TestBuildReviewer_BogusProvider_Errors covers the
// provider.Build error path. The harness library's provider
// constructor rejects unknown names; buildReviewer wraps the
// error with "build provider" so the operator can identify
// the source. This is the cheapest end-to-end smoke test
// for buildReviewer — no network, no subprocesses, just the
// early validation gate.
func TestBuildReviewer_BogusProvider_Errors(t *testing.T) {
	deps := minimalClientDeps()
	deps.ProviderName = "totally-not-a-real-provider"
	deps.ProviderBaseURL = "http://invalid" // so even a name-match doesn't accidentally succeed

	_, err := buildReviewer(context.Background(), deps)
	if err == nil {
		t.Fatal("buildReviewer with bogus provider should error, got nil")
	}
	if !strings.Contains(err.Error(), "build provider") {
		t.Errorf("error %q should mention 'build provider'", err.Error())
	}
}

// TestBuildHarnessRuntime_EmptyWorkdir_Errors covers the
// WorkDir guard inside buildHarnessRuntime. The function
// refuses to build a runtime when WorkDir is empty because
// tokensave's --path would point at whatever the mreview
// process happened to start in (typically a developer's
// local clone of mreview itself), and the agent would
// silently index the wrong project.
//
// buildReviewer wraps this error with "build harness
// runtime:"; we test buildHarnessRuntime directly to pin
// the original error message and the wrapping context
// separately.
func TestBuildHarnessRuntime_EmptyWorkdir_Errors(t *testing.T) {
	deps := minimalClientDeps()
	deps.WorkDir = "" // the bug condition

	// buildHarnessRuntime takes an llm.LLMProvider, not a
	// clientDeps alone. We pass nil for the provider here
	// because the WorkDir check fires BEFORE the runtime
	// tries to use the provider — the function returns
	// before any code touches llmProvider.
	_, err := buildHarnessRuntime(context.Background(), nil, deps)
	if err == nil {
		t.Fatal("buildHarnessRuntime with empty WorkDir should error, got nil")
	}
	if !strings.Contains(err.Error(), "workdir is required") {
		t.Errorf("error %q should mention 'workdir is required'", err.Error())
	}
	if !strings.Contains(err.Error(), "MREVIEW_WORKDIR") {
		t.Errorf("error %q should mention the MREVIEW_WORKDIR env var", err.Error())
	}
}

// TestBuildReviewer_EmptyWorkdir_Errors is the
// end-to-end version of the previous test: the same error
// is raised through buildReviewer, wrapped with "build
// harness runtime:". Confirms the wrapping is consistent
// so log-grep operators find both the function name and
// the underlying cause.
func TestBuildReviewer_EmptyWorkdir_Errors(t *testing.T) {
	deps := minimalClientDeps()
	deps.WorkDir = ""

	_, err := buildReviewer(context.Background(), deps)
	if err == nil {
		t.Fatal("buildReviewer with empty WorkDir should error, got nil")
	}
	if !strings.Contains(err.Error(), "build harness runtime") {
		t.Errorf("error %q should mention 'build harness runtime'", err.Error())
	}
	if !strings.Contains(err.Error(), "workdir is required") {
		t.Errorf("error %q should mention 'workdir is required' (the underlying cause)", err.Error())
	}
}
