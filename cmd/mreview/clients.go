package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sausheong/harness/llm"
	"github.com/sausheong/harness/runtime"
	"github.com/sausheong/harness/tool"
	"github.com/sausheong/harness/tools/file"

	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/policy"
	"github.com/inful/mreview/internal/provider"
	"github.com/inful/mreview/internal/reviewer"
)

// clientDeps bundles the inputs buildReviewer needs to wire
// up the GitLab client, the harness runtime, the provider,
// and the orchestrator. Mirrors the flag names of ReviewCmd
// so the wiring lives in exactly one place.
type clientDeps struct {
	// GitLab connection.
	GitLabURL   string
	GitLabToken string

	// Provider connection.
	ProviderName    string
	ProviderAPIKey  string
	ProviderBaseURL string

	// Provider / model.
	Model string

	// WorkDir is the directory the harness runs against —
	// the repo root. Used by read_file's WorkDir
	// restriction.
	WorkDir string

	// Policy + review tunables.
	Policy       *policy.Policy
	CommentMode  string
	BotUsername  string
	DryRun       bool
	Retries      int
	RetryBackoff time.Duration
	Logger       *slog.Logger
}

// buildReviewer wires up everything the ReviewMR call needs:
// a *gitlab.Client, a harness runtime (built with read-only
// tools), the provider from the harness matrix, the optional
// policy, and a configured *reviewer.Orchestrator.
//
// The harness runtime is built here, not in the orchestrator,
// because building a runtime is expensive (tool registry +
// provider + session) and the orchestrator should be cheap to
// construct for tests.
func buildReviewer(ctx context.Context, deps clientDeps) (*reviewer.Orchestrator, error) {
	glClient, err := gitlab.NewClient(deps.GitLabURL, deps.GitLabToken, gitlab.RetryConfig{
		MaxAttempts:    deps.Retries + 1,
		InitialBackoff: deps.RetryBackoff,
		MaxBackoff:     gitlab.DefaultMaxBackoff,
		Logger:         deps.Logger,
	}, deps.Logger)
	if err != nil {
		return nil, fmt.Errorf("build gitlab client: %w", err)
	}

	llmProvider, err := provider.Build(ctx, provider.Config{
		Name:    provider.Name(deps.ProviderName),
		APIKey:  deps.ProviderAPIKey,
		BaseURL: deps.ProviderBaseURL,
	})
	if err != nil {
		return nil, fmt.Errorf("build provider: %w", err)
	}

	// Build the harness runtime with the read-only tool
	// surface. PR #4 will wire in tokensave MCP; PR #5
	// will wire in CI artifact loading. PR #3 ships the
	// skeleton with read_file only.
	rt, err := buildHarnessRuntime(ctx, llmProvider, deps)
	if err != nil {
		return nil, fmt.Errorf("build harness runtime: %w", err)
	}

	rev, err := reviewer.New(reviewer.Config{
		GitLab:      glClient,
		Runner:      reviewer.NewHarnessRunnerFromRuntime(rt),
		WorkDir:     deps.WorkDir,
		Policy:      deps.Policy,
		BotUsername: deps.BotUsername,
		DryRun:      deps.DryRun,
		Logger:      deps.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("build orchestrator: %w", err)
	}
	return rev, nil
}

// buildHarnessRuntime constructs a harness runtime with the
// read-only tool registry. The harness library owns the LLM
// loop; mreview owns only the tool surface.
//
// Read-only contract (enforced by tests in orchestrator_test.go):
//   - read_file is the ONLY file tool registered.
//   - harness's bash, edit_file, write_file tools are NOT
//     registered (mreview never references them).
//   - tokensave MCP server is registered when --tokensave-
//     enabled=true (PR #4); the MCP server is a one-shot
//     subprocess that the harness library owns.
func buildHarnessRuntime(ctx context.Context, llmProvider llm.LLMProvider, deps clientDeps) (*runtime.Runtime, error) {
	reg := tool.NewRegistry()
	reg.Register(&file.ReadFileTool{WorkDir: deps.WorkDir})

	return runtime.BuildRuntime(
		runtime.RuntimeDeps{},
		runtime.RuntimeInputs{
			Provider: llmProvider,
			Tools:    reg,
		},
		runtime.AgentSpec{
			ID:           "mreview",
			Name:         "mreview",
			Model:        deps.Model,
			Workspace:    deps.WorkDir,
			SystemPrompt: reviewSystemPrompt,
			MaxTurns:     10,
		},
	)
}

// reviewSystemPrompt is a small wrapper around
// prompts.ReviewSystemPrompt so the tool registry above can
// reference it without an import cycle. Kept as a thin
// wrapper; PR #6 will move the prompt out of this file.
var reviewSystemPrompt = stringPrompt()

// stringPrompt returns the system prompt as a string for
// runtime.AgentSpec.SystemPrompt. The harness library
// expects a string here; the prompts package owns the full
// content (which is large — see internal/prompts/review.go).
func stringPrompt() string {
	return reviewSystemPromptText
}

// reviewSystemPromptText is set from internal/prompts at
// startup via SetReviewSystemPrompt. Avoids an import cycle
// (cmd/mreview imports internal/reviewer; internal/prompts
// doesn't import cmd/mreview).
var reviewSystemPromptText string

// SetReviewSystemPrompt wires the prompts-package text into
// the runtime.AgentSpec at startup. Called from main.go
// before buildHarnessRuntime is invoked.
func SetReviewSystemPrompt(text string) {
	reviewSystemPromptText = text
}

// ParseCommentMode kept for the ReviewCmd enum binding; the
// review subcommand no longer uses comment-mode (PR #3 wires
// summary post policy into the orchestrator directly). Kept
// as a stub so the existing CLI flag stays parseable.
func ParseCommentMode(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "both", "inline-only", "summary-only", "inline", "summary":
		return strings.ToLower(strings.TrimSpace(s)), nil
	default:
		return "", fmt.Errorf("review: unknown comment-mode %q", s)
	}
}
