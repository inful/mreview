package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/inful/mreview/internal/config"
	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/llm"
	"github.com/inful/mreview/internal/reviewer"
)

// ReviewCmd holds the flags for `mreview review`.
//
// All connection details default to env vars (--gitlab-token via
// GITLAB_TOKEN) so the binary can be used from CI / cron without
// exposing secrets on the command line.
type ReviewCmd struct {
	Repo string `required:"" help:"Repository path (group/project)."`
	MR   int    `required:"" name:"mr" help:"Merge request IID."`

	// GitLab connection.
	GitLabURL   string `default:"https://gitlab.com" name:"gitlab-url" env:"GITLAB_URL" help:"GitLab base URL."`
	GitLabToken string `required:"" env:"GITLAB_TOKEN" name:"gitlab-token" help:"GitLab Personal Access Token (api scope)."`

	// LLM connection.
	LLMURL      string  `default:"http://localhost:11434/v1" name:"llm-url" env:"LLM_URL" help:"LLM OpenAI-compatible base URL."`
	LLMAPIKey   string  `env:"LLM_API_KEY" name:"llm-api-key" help:"LLM API key (optional for local servers)."`
	Model       string  `default:"qwen2.5-coder:7b" env:"LLM_MODEL" help:"LLM model name."`
	Temperature float64 `default:"0.2" name:"temperature" env:"MREVIEW_TEMPERATURE" help:"LLM sampling temperature (0.0-2.0)."`
	MaxTokens   int     `default:"2048" name:"max-tokens" env:"MREVIEW_MAX_TOKENS" help:"LLM max output tokens per call."`

	// ReasoningEffort controls how much reasoning-capable models
	// (OpenAI o-series, Azure AI Foundry, Groq, Together, etc.)
	// think before answering. Empty = use the server's default.
	// Ignored by providers/models that don't support the field.
	ReasoningEffort string `name:"reasoning-effort" env:"MREVIEW_REASONING_EFFORT" help:"Reasoning budget for o-series-style models: 'low' / 'medium' / 'high'. Empty = server default. No-op for models that don't support the field."`

	// Diff / chunking.
	MaxDiffBytes int `default:"200000" name:"max-diff-bytes" env:"MREVIEW_MAX_DIFF_BYTES" help:"Per-chunk byte budget; larger files return an error."`

	// MaxBatchBytes is the byte budget for packing multiple
	// chunks into a single LLM call. 0 = one chunk per call
	// (default). Operators with large-context models should
	// raise this; a preset layer can also compute it from the
	// model's declared context window.
	MaxBatchBytes int `name:"max-batch-bytes" env:"MREVIEW_MAX_BATCH_BYTES" help:"Byte budget for packing multiple chunks into one LLM call. 0 (default) = one chunk per call. Raise for large-context models."`

	// Per-call timeout.
	PerChunkTimeout time.Duration `default:"120s" name:"per-chunk-timeout" env:"MREVIEW_PER_CHUNK_TIMEOUT" help:"Per-LLM-call timeout."`

	// ChunkRetries is the chunk-level retry budget for transient
	// errors (currently: per-call timeout). Default 1 (one retry,
	// two total attempts). Setting this to 0 disables the
	// chunk-level retry — the openai-go client's own MaxRetries=2
	// still catches 5xx / 429 at the transport level.
	ChunkRetries int `default:"1" name:"chunk-retries" env:"MREVIEW_CHUNK_RETRIES" help:"Chunk-level retry budget for transient errors (timeouts). Default 1."`

	// AllowPartial restores the legacy "log + substitute empty
	// response" behaviour when a chunk fails. The default after
	// issue #31 is atomic failure: any chunk failure aborts the
	// review with a *ChunkFailureError before posting anything
	// to GitLab. Operators running on large MRs who prefer the
	// half-completed-review behaviour can set this true.
	AllowPartial bool `name:"allow-partial" env:"MREVIEW_ALLOW_PARTIAL" help:"On chunk failure, log a warn and continue with empty findings instead of aborting the whole review."`

	// Dry-run.
	DryRun bool `name:"dry-run" help:"Log the intended LLM and GitLab calls without performing them."`

	// Comment mode (both / inline-only / summary-only).
	CommentMode string `default:"both" name:"comment-mode" enum:"both,inline-only,summary-only" env:"MREVIEW_COMMENT_MODE" help:"Which kinds of comments to post: both (default), inline-only, or summary-only."`

	// Ignore paths (comma-separated glob patterns).
	IgnorePaths []string `name:"ignore-paths" sep:" " env:"MREVIEW_IGNORE_PATHS" help:"Glob patterns for files to skip (e.g. '**/*.pb.go' 'vendor/**'). Repeatable; comma-separated via env."`

	// Prompt customization. Both files are appended AFTER the
	// system-owned schema/rules — they cannot replace or override
	// the JSON schema, severity semantics, or output format.
	SystemPromptFile string `name:"system-prompt-file" env:"MREVIEW_SYSTEM_PROMPT_FILE" help:"Path to a file with team-specific text appended to the system prompt. Use for documentation standards, language-specific dependency preferences, project conventions. The system-owned schema stays first."`
	UserPromptFile   string `name:"user-prompt-file"   env:"MREVIEW_USER_PROMPT_FILE"   help:"Path to a file with team-specific context appended to the user prompt (after the diff chunks). Use for per-MR context the LLM should weigh heavily."`

	// BotUsername is the username the GitLab token posts as;
	// dedupe ignores comments authored by other humans. When
	// empty, dedupe uses every existing comment as the baseline
	// (more conservative; useful when the operator wants to
	// avoid clobbering anyone's prior review notes).
	BotUsername string `name:"bot-username" env:"GITLAB_BOT_USERNAME" help:"Username of the bot that posts comments (for dedupe)."`

	// Retry for GitLab API calls.
	Retries      int           `default:"3" name:"retries" env:"MREVIEW_RETRIES" help:"GitLab API retry attempts on transient errors."`
	RetryBackoff time.Duration `default:"500ms" name:"retry-backoff" env:"MREVIEW_RETRY_BACKOFF" help:"Initial retry backoff; exponential with jitter."`

	// Verbose is intentionally NOT declared here — it lives on
	// the parent CLI struct so it's accepted globally. Declaring
	// it again on ReviewCmd would shadow and produce a "duplicate
	// flag --verbose" error from kong.
}

// runReview is invoked by run() after CLI parsing matches the
// "review" subcommand. It wires up the GitLab + LLM clients and
// delegates to internal/reviewer.
func runReview(stdout io.Writer, c *ReviewCmd, cfg *config.File, logger *slog.Logger) error {
	logger.Info("starting review",
		"repo", c.Repo,
		"mr", c.MR,
		"model", c.Model,
		"dry_run", c.DryRun,
	)

	rev, err := buildClients(clientDeps{
		GitLabURL:        c.GitLabURL,
		GitLabToken:      c.GitLabToken,
		LLMURL:           c.LLMURL,
		LLMAPIKey:        c.LLMAPIKey,
		Model:            c.Model,
		Temperature:      c.Temperature,
		MaxTokens:        c.MaxTokens,
		ReasoningEffort:  c.ReasoningEffort,
		MaxDiffBytes:     c.MaxDiffBytes,
		MaxBatchBytes:    c.MaxBatchBytes,
		PerChunkTimeout:  c.PerChunkTimeout,
		ChunkRetries:     c.ChunkRetries,
		AllowPartial:     c.AllowPartial,
		BotUsername:      c.BotUsername,
		CommentMode:      c.CommentMode,
		IgnorePaths:      c.IgnorePaths,
		SystemPromptFile: c.SystemPromptFile,
		UserPromptFile:   c.UserPromptFile,
		Retries:          c.Retries,
		RetryBackoff:     c.RetryBackoff,
		DryRun:           c.DryRun,
		Logger:           logger,
		Config:           cfg,
	})
	if err != nil {
		return logWithError(logger, ExitConfig, err.Error(), err)
	}

	result, err := rev.ReviewMR(context.Background(), c.Repo, c.MR)
	if err != nil {
		// Map gitlab.Error.Kind → typed ExitError.
		var ge *gitlab.Error
		if errors.As(err, &ge) {
			switch ge.Kind {
			case gitlab.KindAuth:
				return logWithError(logger, ExitAuth, "auth failure", err)
			case gitlab.KindNotFound:
				return logWithError(logger, ExitNotFound, "MR not found", err)
			case gitlab.KindConflict:
				return logWithError(logger, ExitConflict, "conflict", err)
			case gitlab.KindTransient:
				return logWithError(logger, ExitTransient, "transient exhausted", err)
			case gitlab.KindBadRequest:
				return logWithError(logger, ExitConfig, "bad request", err)
			default:
				return logWithError(logger, ExitInternal, "review failed", err)
			}
		}
		var oe *llm.OversizedError
		if errors.As(err, &oe) {
			// Treat "single file too big" as a config error: the
			// operator should either raise --max-diff-bytes or
			// split the MR.
			return logWithError(logger, ExitConfig, "diff too large to chunk", err)
		}
		var cfe *reviewer.ChunkFailureError
		if errors.As(err, &cfe) {
			// Atomic-failure from issue #31: a chunk failed
			// after retries and AllowPartial is false. No
			// summary or inline findings have been posted; the
			// operator should re-run. Use ExitInternal (7) —
			// this is a new failure mode that the operator
			// must intervene on.
			return logWithError(logger, ExitInternal, "review incomplete", err)
		}
		return logWithError(logger, ExitInternal, "review failed", err)
	}

	logger.Info("review complete",
		"findings", len(result.Findings),
		"summary_posted", result.Summary != nil,
	)

	// Print the MR URL on success (matches repo-mr-file convention).
	if result.MR != nil && result.MR.WebURL != "" {
		_, _ = fmt.Fprintln(stdout, result.MR.WebURL)
	}
	return nil
}

// logWithError logs at error level and returns a typed *ExitError
// so main() can map it to the right exit code.
//
// Kept here (rather than in cli.go) because it's only used by the
// subcommand handlers; cli.go stays flag-only.
func logWithError(logger *slog.Logger, code int, reason string, err error) error {
	logger.Error(reason, "err", errStr(err))
	return &ExitError{Code: code, Reason: reason, Wrapped: err}
}

// errStr returns err.Error() or "" when err is nil.
func errStr(err error) string {
	if err == nil {
		return ""
	}
	return strings.TrimSpace(err.Error())
}
