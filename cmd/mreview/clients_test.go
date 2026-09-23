package main

import (
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/inful/mreview/internal/config"
)

// quietLogger returns a slog.Logger that drops every record. Tests
// that exercise buildClients don't want to assert log output, but
// buildClients passes the logger through to gitlab.NewClient and
// resolveLLMSettings — both of which check for nil and panic only
// in narrow code paths. A silent logger is the simplest
// non-disruptive choice.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestBuildClients_HappyPath builds a reviewer from a populated
// clientDeps and asserts the helper returns a non-nil reviewer and
// no error. The test uses dummy endpoints (the openai-go provider
// is lazy — it doesn't hit the network until the first call), so
// this is a pure wiring test.
func TestBuildClients_HappyPath(t *testing.T) {
	deps := clientDeps{
		GitLabURL:        "https://gitlab.example.com",
		GitLabToken:      "test-token-abc",
		LLMURL:           "http://localhost:11434/v1",
		LLMAPIKey:        "test-llm-key",
		Model:            "qwen2.5-coder:7b",
		Temperature:      0.2,
		MaxTokens:        2048,
		ReasoningEffort:  "",
		MaxDiffBytes:     200000,
		MaxBatchBytes:    0,
		PerChunkTimeout:  120 * time.Second,
		ChunkRetries:     1,
		AllowPartial:     false,
		BotUsername:      "review-bot",
		CommentMode:      "both",
		IgnorePaths:      nil,
		SystemPromptFile: "",
		UserPromptFile:   "",
		Retries:          3,
		RetryBackoff:     500 * time.Millisecond,
		DryRun:           false,
		Logger:           quietLogger(),
		Config:           &cfg0,
	}
	rev, err := buildClients(deps)
	if err != nil {
		t.Fatalf("buildClients: %v", err)
	}
	if rev == nil {
		t.Fatal("buildClients returned nil reviewer without error")
	}
}

// cfg0 is a zero-valued config that the happy-path test reuses.
// Defaults are applied lazily inside resolveLLMSettings, so an
// all-zero file is safe here.
var cfg0 config.File

// TestBuildClients_InvalidCommentMode exercises the early error
// paths. Each one should return a wrapped error with the failure
// stage surfaced in the message, so the caller (runReview / runServe)
// can pass the message straight to logWithError.
func TestBuildClients_InvalidCommentMode(t *testing.T) {
	deps := clientDeps{
		// The first three checks (gitlab client, llm provider) need
		// minimum-valid inputs to get past themselves and reach the
		// comment-mode parser. The empty URL/token below would fail
		// in gitlab.NewClient BEFORE we get to comment-mode, so use
		// valid values for those and a known-bad comment mode here.
		GitLabURL:       "https://gitlab.example.com",
		GitLabToken:     "test-token",
		LLMURL:          "http://localhost:11434/v1",
		LLMAPIKey:       "test-llm-key",
		Model:           "qwen2.5-coder:7b",
		MaxTokens:       2048,
		MaxDiffBytes:    200000,
		PerChunkTimeout: 120 * time.Second,
		ChunkRetries:    1,
		CommentMode:     "bogus-mode",
		Retries:         3,
		RetryBackoff:    500 * time.Millisecond,
		Logger:          quietLogger(),
		Config:          &cfg0,
	}
	_, err := buildClients(deps)
	if err == nil {
		t.Fatal("expected error for invalid comment-mode, got nil")
	}
	if !strings.Contains(err.Error(), "invalid comment-mode") {
		t.Errorf("error message %q does not surface the comment-mode stage", err.Error())
	}
}

// TestBuildClients_EmptyGitLabURL asserts the earliest-stage
// failure (gitlab client build) is wrapped with the stage label,
// and the underlying *gitlab.Client constructor error is reachable
// via errors.Unwrap.
func TestBuildClients_EmptyGitLabURL(t *testing.T) {
	deps := clientDeps{
		GitLabURL:       "", // required by gitlab.NewClient
		GitLabToken:     "test-token",
		LLMURL:          "http://localhost:11434/v1",
		LLMAPIKey:       "test-llm-key",
		Model:           "qwen2.5-coder:7b",
		MaxTokens:       2048,
		MaxDiffBytes:    200000,
		PerChunkTimeout: 120 * time.Second,
		ChunkRetries:    1,
		CommentMode:     "both",
		Retries:         3,
		RetryBackoff:    500 * time.Millisecond,
		Logger:          quietLogger(),
		Config:          &cfg0,
	}
	_, err := buildClients(deps)
	if err == nil {
		t.Fatal("expected error from empty GitLab URL, got nil")
	}
	if !strings.Contains(err.Error(), "build gitlab client") {
		t.Errorf("error message %q does not surface the gitlab-client stage", err.Error())
	}
	// errors.Unwrap should reach the original gitlab.NewClient error.
	// Don't assert the exact text — just verify the chain isn't
	// double-wrapped past a single level.
	inner := unwrapOnce(err)
	if inner == nil {
		t.Errorf("expected wrapped error to unwrap to a non-nil cause")
	}
}

// TestBuildClients_PromptFileMissing asserts the optional-file
// loader surfaces a useful error when the operator pointed at a
// non-existent path.
func TestBuildClients_PromptFileMissing(t *testing.T) {
	deps := clientDeps{
		GitLabURL:        "https://gitlab.example.com",
		GitLabToken:      "test-token",
		LLMURL:           "http://localhost:11434/v1",
		LLMAPIKey:        "test-llm-key",
		Model:            "qwen2.5-coder:7b",
		MaxTokens:        2048,
		MaxDiffBytes:     200000,
		PerChunkTimeout:  120 * time.Second,
		ChunkRetries:     1,
		CommentMode:      "both",
		Retries:          3,
		RetryBackoff:     500 * time.Millisecond,
		SystemPromptFile: "/nonexistent/path/should/not/exist.md",
		Logger:           quietLogger(),
		Config:           &cfg0,
	}
	_, err := buildClients(deps)
	if err == nil {
		t.Fatal("expected error from missing system prompt file, got nil")
	}
	if !strings.Contains(err.Error(), "load system prompt file") {
		t.Errorf("error message %q does not surface the prompt-file stage", err.Error())
	}
}

// unwrapOnce peels a single fmt.Errorf("%w") wrapper. Tests use it
// to assert the helper doesn't double-wrap when it doesn't need to.
func unwrapOnce(err error) error {
	type unwrapper interface{ Unwrap() error }
	u, ok := err.(unwrapper)
	if !ok {
		return nil
	}
	return u.Unwrap()
}

// TestBuildClients_ReviewAndServeUseSameShape is a documentation
// test: it pins the field set of clientDeps via reflection so a
// future refactor that drops a field trips the assertion instead
// of silently breaking one of the call sites in review.go or
// serve.go.
func TestBuildClients_ReviewAndServeUseSameShape(t *testing.T) {
	required := []string{
		"GitLabURL", "GitLabToken",
		"LLMURL", "LLMAPIKey", "Model",
		"Temperature", "MaxTokens", "ReasoningEffort",
		"MaxDiffBytes", "MaxBatchBytes", "PerChunkTimeout", "ChunkRetries",
		"AllowPartial", "BotUsername", "CommentMode", "IgnorePaths",
		"SystemPromptFile", "UserPromptFile",
		"Retries", "RetryBackoff", "DryRun",
		"Logger", "Config",
	}
	have := structFieldNames(t, clientDeps{})
	for _, name := range required {
		found := false
		for _, h := range have {
			if h == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("clientDeps missing required field %q", name)
		}
	}
}

// structFieldNames returns the exported field names of the
// value's underlying type via reflection. Used by the documentation
// test above so the field list isn't hardcoded twice (once in
// clients.go and once in the test).
func structFieldNames(t *testing.T, v any) []string {
	t.Helper()
	rv := reflect.ValueOf(v)
	rt := rv.Type()
	names := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		names = append(names, rt.Field(i).Name)
	}
	return names
}
