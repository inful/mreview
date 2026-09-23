package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/signal"
	"syscall"
	"time"

	"github.com/inful/mreview/internal/config"
	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/llm"
	"github.com/inful/mreview/internal/reviewer"
	"github.com/inful/mreview/internal/server"
)

// ServeCmd holds the flags for `mreview serve`.
//
// ServeCmd runs a long-lived HTTP server that consumes GitLab
// merge_request webhooks and reviews each event in the background.
type ServeCmd struct {
	Addr            string        `default:":8080" name:"addr" env:"MREVIEW_ADDR" help:"HTTP listen address."`
	WebhookSecret   string        `env:"GITLAB_WEBHOOK_SECRET" name:"webhook-secret" help:"GitLab webhook shared secret."`
	QueueSize       int           `default:"32" name:"queue-size" env:"MREVIEW_QUEUE_SIZE" help:"Maximum number of concurrent review jobs."`
	ShutdownTimeout time.Duration `default:"30s" name:"shutdown-timeout" env:"MREVIEW_SHUTDOWN_TIMEOUT" help:"Time to wait for in-flight reviews on shutdown."`
	RequiredLabel   string        `name:"required-label" env:"MREVIEW_REQUIRED_LABEL" help:"Only review MRs carrying this GitLab label (e.g. 'mreview'). Empty reviews every MR."`

	// GitLab connection (same flags as review).
	GitLabURL   string `default:"https://gitlab.com" name:"gitlab-url" env:"GITLAB_URL" help:"GitLab base URL."`
	GitLabToken string `env:"GITLAB_TOKEN" name:"gitlab-token" help:"GitLab Personal Access Token (api scope)."`

	// LLM connection (same flags as review).
	LLMURL          string        `default:"http://localhost:11434/v1" name:"llm-url" env:"LLM_URL" help:"LLM OpenAI-compatible base URL."`
	LLMAPIKey       string        `env:"LLM_API_KEY" name:"llm-api-key" help:"LLM API key (optional for local servers)."`
	Model           string        `default:"qwen2.5-coder:7b" env:"LLM_MODEL" help:"LLM model name."`
	Temperature     float64       `default:"0.2" name:"temperature" env:"MREVIEW_TEMPERATURE" help:"LLM sampling temperature."`
	MaxTokens       int           `default:"2048" name:"max-tokens" env:"MREVIEW_MAX_TOKENS" help:"LLM max output tokens per call."`
	ReasoningEffort string        `name:"reasoning-effort" env:"MREVIEW_REASONING_EFFORT" help:"Reasoning budget for o-series-style models: 'low' / 'medium' / 'high'. Empty = server default. No-op for models that don't support the field."`
	MaxDiffBytes    int           `default:"200000" name:"max-diff-bytes" env:"MREVIEW_MAX_DIFF_BYTES" help:"Per-chunk byte budget."`
	MaxBatchBytes   int           `name:"max-batch-bytes" env:"MREVIEW_MAX_BATCH_BYTES" help:"Byte budget for packing multiple chunks into one LLM call. 0 (default) = one chunk per call. Raise for large-context models."`
	PerChunkTimeout time.Duration `default:"120s" name:"per-chunk-timeout" env:"MREVIEW_PER_CHUNK_TIMEOUT" help:"Per-LLM-call timeout."`
	ChunkRetries    int           `default:"1" name:"chunk-retries" env:"MREVIEW_CHUNK_RETRIES" help:"Chunk-level retry budget for transient errors (timeouts). Default 1."`
	AllowPartial    bool          `name:"allow-partial" env:"MREVIEW_ALLOW_PARTIAL" help:"On chunk failure, log a warn and continue with empty findings instead of aborting the whole review."`
	BotUsername     string        `name:"bot-username" env:"GITLAB_BOT_USERNAME" help:"Bot username (for dedupe)."`

	// Retry for GitLab API calls.
	Retries      int           `default:"3" name:"retries" env:"MREVIEW_RETRIES" help:"GitLab API retry attempts."`
	RetryBackoff time.Duration `default:"500ms" name:"retry-backoff" env:"MREVIEW_RETRY_BACKOFF" help:"Initial retry backoff."`

	// Comment mode (both / inline-only / summary-only).
	CommentMode string `default:"both" name:"comment-mode" enum:"both,inline-only,summary-only" env:"MREVIEW_COMMENT_MODE" help:"Which kinds of comments to post."`

	// Ignore paths (comma-separated glob patterns).
	IgnorePaths []string `name:"ignore-paths" sep:" " env:"MREVIEW_IGNORE_PATHS" help:"Glob patterns for files to skip."`

	// Prompt customization (same as review).
	SystemPromptFile string `name:"system-prompt-file" env:"MREVIEW_SYSTEM_PROMPT_FILE" help:"Path to a file with team-specific text appended to the system prompt."`
	UserPromptFile   string `name:"user-prompt-file"   env:"MREVIEW_USER_PROMPT_FILE"   help:"Path to a file with team-specific context appended to the user prompt."`
}

// runServe is invoked by run() after CLI parsing matches the
// "serve" subcommand. It builds the same GitLab + LLM clients as
// review, wires a worker pool that runs the reviewer per webhook,
// and blocks until parentCtx is canceled.
func runServe(parentCtx context.Context, stdout io.Writer, c *ServeCmd, cfg *config.File, logger *slog.Logger) error {
	logger.Info("serve: starting",
		"addr", c.Addr,
		"model", c.Model,
		"queue_size", c.QueueSize,
	)

	glClient, err := gitlab.NewClient(c.GitLabURL, c.GitLabToken, gitlab.RetryConfig{
		MaxAttempts:    c.Retries + 1,
		InitialBackoff: c.RetryBackoff,
		MaxBackoff:     30 * time.Second,
		Logger:         logger,
	}, logger)
	if err != nil {
		return logWithError(logger, ExitConfig, "build gitlab client", err)
	}

	llmProvider, err := llm.NewOpenAIProvider(llm.OpenAIConfig{
		BaseURL:    c.LLMURL,
		APIKey:     c.LLMAPIKey,
		Model:      c.Model,
		MaxRetries: 2,
	})
	if err != nil {
		return logWithError(logger, ExitConfig, "build llm provider", err)
	}

	mode, err := reviewer.ParseCommentMode(c.CommentMode)
	if err != nil {
		return logWithError(logger, ExitConfig, "invalid comment-mode", err)
	}

	systemSuffix, err := loadOptionalFile(c.SystemPromptFile, "system prompt")
	if err != nil {
		return logWithError(logger, ExitConfig, "load system prompt file", err)
	}
	userSuffix, err := loadOptionalFile(c.UserPromptFile, "user prompt")
	if err != nil {
		return logWithError(logger, ExitConfig, "load user prompt file", err)
	}

	// Apply LLM preset (CLI flags take precedence).
	c.MaxBatchBytes, c.PerChunkTimeout, c.ReasoningEffort = resolveLLMSettings(
		cfg, c.Model, c.MaxBatchBytes, c.PerChunkTimeout, c.ReasoningEffort, c.MaxTokens, logger,
	)

	rev, err := reviewer.NewReviewer(reviewer.Config{
		GitLab:             glClient,
		LLM:                llmProvider,
		Model:              c.Model,
		MaxDiffBytes:       c.MaxDiffBytes,
		MaxBatchBytes:      c.MaxBatchBytes,
		Temperature:        c.Temperature,
		MaxTokens:          c.MaxTokens,
		ReasoningEffort:    llm.ReasoningEffort(c.ReasoningEffort),
		PerChunkTimeout:    c.PerChunkTimeout,
		ChunkRetries:       c.ChunkRetries,
		AllowPartial:       c.AllowPartial,
		Logger:             logger,
		DryRun:             false,
		BotUsername:        c.BotUsername,
		CommentMode:        mode,
		IgnorePaths:        c.IgnorePaths,
		SystemPromptSuffix: systemSuffix,
		UserPromptSuffix:   userSuffix,
	})
	if err != nil {
		return logWithError(logger, ExitConfig, "build reviewer", err)
	}

	// Job handler: one ReviewMR per webhook. Errors are logged
	// (worker pool reports them); they don't fail the server.
	handler := func(ctx context.Context, job server.Job) {
		jobLog := logger.With(
			"project", job.Project,
			"iid", job.IID,
			"action", job.Action,
			"event", job.EventType,
		)
		result, err := rev.ReviewMR(ctx, job.Project, job.IID, job.Action)
		if err != nil {
			// gitlab.Error already has the right Kind; the
			// worker only sees generic errors, so we log the
			// full message and continue.
			jobLog.Error("review failed", "err", err.Error())
			return
		}
		jobLog.Info("review complete",
			"findings", len(result.Findings),
			"summary_posted", result.Summary != nil,
			"dedupe_size", result.DedupeSize,
		)
	}

	srv, err := server.New(server.Config{
		Addr:            c.Addr,
		WebhookSecret:   c.WebhookSecret,
		Handler:         handler,
		Logger:          logger,
		ShutdownTimeout: c.ShutdownTimeout,
		QueueSize:       c.QueueSize,
		RequiredLabel:   c.RequiredLabel,
	})
	if err != nil {
		return logWithError(logger, ExitConfig, "build server", err)
	}

	ctx, stop := signal.NotifyContext(parentCtx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("serve: ready", "addr", c.Addr)
	if err := srv.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return logWithError(logger, ExitInternal, "serve failed", err)
	}
	_, _ = fmt.Fprintln(stdout, "serve: shutdown complete")
	return nil
}
