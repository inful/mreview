package main

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/inful/mreview/internal/config"
	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/llm"
	"github.com/inful/mreview/internal/reviewer"
)

// clientDeps bundles the inputs buildClients needs to wire up the
// GitLab client, LLM provider, prompt-file loaders, and *Reviewer.
// Both runReview and runServe populate one of these from their own
// flag structs so the bootstrap logic lives in exactly one place.
//
// Field names mirror the CLI flag names of *ReviewCmd and *ServeCmd.
// Renaming requires updating the call sites in review.go and
// serve.go too.
type clientDeps struct {
	// GitLab connection.
	GitLabURL   string
	GitLabToken string

	// LLM connection.
	LLMURL    string
	LLMAPIKey string
	Model     string

	// Reviewer fields.
	Temperature     float64
	MaxTokens       int
	ReasoningEffort string
	MaxDiffBytes    int
	MaxBatchBytes   int
	PerChunkTimeout time.Duration
	ChunkRetries    int
	AllowPartial    bool
	BotUsername     string
	CommentMode     string
	IgnorePaths     []string

	// Prompt customisation (both files are optional).
	SystemPromptFile string
	UserPromptFile   string

	// GitLab retry policy.
	Retries      int
	RetryBackoff time.Duration

	// Subcommand-specific options.
	DryRun bool // `mreview review --dry-run`; ignored by serve

	// Surrounding state.
	Logger *slog.Logger
	Config *config.File
}

// buildClients wires up everything the ReviewMR call needs:
// a *gitlab.Client, an llm.Provider, the optional prompt-file
// suffixes, and a configured *reviewer.Reviewer.
//
// Both `mreview review` and `mreview serve` use identical wiring;
// the only subcommand-specific knob is DryRun, which the review
// subcommand forwards and serve always leaves false.
//
// Errors are wrapped with the failure stage ("build gitlab
// client", "invalid comment-mode", etc.) so the caller can pass
// them to logWithError without losing context. The *slog.Logger
// is intentionally not used directly inside buildClients — the
// caller decides the exit-code mapping (ExitConfig vs ExitAuth
// etc.) based on the error string.
func buildClients(deps clientDeps) (*reviewer.Reviewer, error) {
	glClient, err := gitlab.NewClient(deps.GitLabURL, deps.GitLabToken, gitlab.RetryConfig{
		MaxAttempts:    deps.Retries + 1,
		InitialBackoff: deps.RetryBackoff,
		MaxBackoff:     gitlab.DefaultMaxBackoff,
		Logger:         deps.Logger,
	}, deps.Logger)
	if err != nil {
		return nil, fmt.Errorf("build gitlab client: %w", err)
	}

	llmProvider, err := llm.NewOpenAIProvider(llm.OpenAIConfig{
		BaseURL:    deps.LLMURL,
		APIKey:     deps.LLMAPIKey,
		Model:      deps.Model,
		MaxRetries: 2,
	})
	if err != nil {
		return nil, fmt.Errorf("build llm provider: %w", err)
	}

	mode, err := reviewer.ParseCommentMode(deps.CommentMode)
	if err != nil {
		return nil, fmt.Errorf("invalid comment-mode: %w", err)
	}

	systemSuffix, err := loadOptionalFile(deps.SystemPromptFile, "system prompt")
	if err != nil {
		return nil, fmt.Errorf("load system prompt file: %w", err)
	}
	userSuffix, err := loadOptionalFile(deps.UserPromptFile, "user prompt")
	if err != nil {
		return nil, fmt.Errorf("load user prompt file: %w", err)
	}

	// Apply LLM preset. CLI flags take precedence; the function
	// returns the resolved trio. We don't mutate any caller-side
	// state — buildClients owns the resolution.
	maxBatchBytes, perChunkTimeout, reasoningEffort := resolveLLMSettings(
		deps.Config,
		deps.Model,
		deps.MaxBatchBytes,
		deps.PerChunkTimeout,
		deps.ReasoningEffort,
		deps.MaxTokens,
		deps.Logger,
	)

	rev, err := reviewer.NewReviewer(reviewer.Config{
		GitLab:             glClient,
		LLM:                llmProvider,
		Model:              deps.Model,
		MaxDiffBytes:       deps.MaxDiffBytes,
		MaxBatchBytes:      maxBatchBytes,
		Temperature:        deps.Temperature,
		MaxTokens:          deps.MaxTokens,
		ReasoningEffort:    llm.ReasoningEffort(reasoningEffort),
		PerChunkTimeout:    perChunkTimeout,
		ChunkRetries:       deps.ChunkRetries,
		AllowPartial:       deps.AllowPartial,
		Logger:             deps.Logger,
		DryRun:             deps.DryRun,
		BotUsername:        deps.BotUsername,
		CommentMode:        mode,
		IgnorePaths:        deps.IgnorePaths,
		SystemPromptSuffix: systemSuffix,
		UserPromptSuffix:   userSuffix,
	})
	if err != nil {
		return nil, fmt.Errorf("build reviewer: %w", err)
	}

	// glClient and llmProvider are unreachable after this point;
	// their lifetime is bound to rev (rev.Config holds them). Future
	// callers that need them post-buildClients can return them from
	// here — for now, returning *Reviewer alone keeps the signature
	// tight.
	_ = glClient
	_ = llmProvider
	return rev, nil
}
