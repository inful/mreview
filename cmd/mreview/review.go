package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/inful/mreview/internal/config"
	"github.com/inful/mreview/internal/event"
	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/policy"
)

// ReviewCmd holds the flags for `mreview review` after the
// architecture reset (#42). The LLM/connection flags are
// gone — the harness library owns the provider matrix; mreview
// only picks the provider name + supplies a few tunables.
type ReviewCmd struct {
	Repo string `required:"" help:"Repository path (group/project)."`
	MR   int    `required:"" name:"mr" help:"Merge request IID."`

	// GitLab connection.
	GitLabURL   string `default:"https://gitlab.com" name:"gitlab-url" env:"GITLAB_URL" help:"GitLab base URL."`
	GitLabToken string `required:"" env:"GITLAB_TOKEN" name:"gitlab-token" help:"GitLab Personal Access Token (api scope)."`

	// Provider selection (issue #42, migration step 3).
	//
	// Provider is one of: anthropic / openai / gemini /
	// litellm / openrouter / local. Each provider has its own
	// env var for the API key (ANTHROPIC_API_KEY /
	// OPENAI_API_KEY / etc.); harness's provider
	// constructors read those natively. BaseURL is required
	// only for self-hosted proxies (litellm / local).
	Provider string `default:"local" name:"provider" enum:"anthropic,openai,gemini,litellm,openrouter,local" env:"MREVIEW_PROVIDER" help:"LLM provider: anthropic / openai / gemini / litellm / openrouter / local. Default local."`
	BaseURL  string `name:"provider-base-url" env:"MREVIEW_PROVIDER_BASE_URL" help:"Provider base URL (required for litellm / local; ignored for hosted providers)."`
	Model    string `default:"qwen2.5-coder:7b" name:"model" env:"MREVIEW_MODEL" help:"Model name (provider-specific; passed to the harness provider)."`

	// WorkDir is the harness agent's working directory — the
	// repo root. The harness's read_file tool restricts reads
	// to this path (no escape upward). CI sets it to
	// "$CI_PROJECT_DIR".
	WorkDir string `name:"workdir" env:"MREVIEW_WORKDIR" type:"path" help:"Working directory the harness runs against (defaults to the repo root)."`

	// BotUsername is the username the GitLab token posts as;
	// dedupe ignores comments authored by other humans. When
	// empty, dedupe uses every existing comment as the
	// baseline.
	BotUsername string `name:"bot-username" env:"GITLAB_BOT_USERNAME" help:"Username of the bot that posts comments (for dedupe)."`

	// Dry-run.
	DryRun bool `name:"dry-run" help:"Log the intended GitLab posts without performing them."`

	// Retry for GitLab API calls.
	Retries      int           `default:"3" name:"retries" env:"MREVIEW_RETRIES" help:"GitLab API retry attempts on transient errors."`
	RetryBackoff time.Duration `default:"500ms" name:"retry-backoff" env:"MREVIEW_RETRY_BACKOFF" help:"Initial retry backoff; exponential with jitter."`

	// Per-event branching (issue #41 / migration step 1 of #42).
	OnDrafts string `default:"skip" name:"on-drafts" enum:"run,skip" env:"MREVIEW_ON_DRAFTS" help:"Action on draft MRs (CI_MERGE_REQUEST_DRAFT=true): run the review or skip with exit 0. Default skip."`
	OnPush   string `default:"skip" name:"on-push" enum:"run,skip" env:"MREVIEW_ON_PUSH" help:"Action on direct branch pushes (CI_PIPELINE_SOURCE=push): run the review or skip with exit 0. Default skip."`

	// PolicyFile points at a YAML file with the policy schema
	// documented in internal/policy. Empty = no policy. The
	// file is loaded and validated at startup; a malformed
	// policy returns ExitConfig before any GitLab / harness
	// work happens. The orchestrator calls the enforcer on
	// the agent's findings; any error-verdict finding makes
	// the review exit with ExitPolicy = 8.
	PolicyFile string `name:"policy-file" env:"MREVIEW_POLICY_FILE" type:"path" help:"Path to a YAML policy file. Empty = no policy. See internal/policy for the schema."`

	// Verbose is intentionally NOT declared here — it lives on
	// the parent CLI struct so it's accepted globally.
}

// providerAPIKey returns the API key for the configured
// provider by reading the provider-specific env var. The
// harness library already knows the convention
// (Anthropic reads ANTHROPIC_API_KEY, OpenAI reads
// OPENAI_API_KEY, etc.); mreview just forwards the env-var
// lookup the harness can't do without help.
func providerAPIKey(name string) string {
	switch name {
	case "anthropic":
		return os.Getenv("ANTHROPIC_API_KEY")
	case "openai":
		return os.Getenv("OPENAI_API_KEY")
	case "gemini":
		return os.Getenv("GOOGLE_API_KEY")
	case "litellm":
		return os.Getenv("LITELLM_API_KEY")
	case "openrouter":
		return os.Getenv("OPENROUTER_API_KEY")
	case "local":
		return "" // local servers don't need keys
	default:
		return ""
	}
}

// runReview is invoked by run() after CLI parsing matches the
// "review" subcommand.
func runReview(parentCtx context.Context, stdout io.Writer, c *ReviewCmd, cfg *config.File, logger *slog.Logger) error {
	// Per-event guard (issue #41 / #42 migration step 1).
	ev := event.Detect()
	decision := event.Decide(ev, event.Prefer(c.OnDrafts), event.Prefer(c.OnPush))
	if !decision.Proceed {
		logger.Debug("skipping review per per-event guard",
			"reason", decision.Reason,
			"source", ev.Source,
			"mr_iid", ev.MRIID,
			"mr_draft", ev.MRDraft,
			"on_drafts", c.OnDrafts,
			"on_push", c.OnPush,
		)
		return nil
	}
	if decision.Reason != "" {
		logger.Info("proceeding with review per override",
			"reason", decision.Reason,
			"source", ev.Source,
		)
	}

	// Policy load (issue #42 migration step 2).
	var pol *policy.Policy
	if c.PolicyFile != "" {
		var err error
		pol, err = policy.Load(c.PolicyFile)
		if err != nil {
			return logWithError(logger, ExitConfig, "policy file", err)
		}
		logger.Info("policy loaded",
			"path", c.PolicyFile,
			"severity_overrides", len(pol.SeverityOverrides),
			"forbid_rules", len(pol.Forbid),
			"require_rules", len(pol.Require),
			"label_rules", len(pol.Labels),
		)
	}

	logger.Info("starting review",
		"repo", c.Repo,
		"mr", c.MR,
		"provider", c.Provider,
		"model", c.Model,
		"dry_run", c.DryRun,
		"source", ev.Source,
	)

	rev, err := buildReviewer(parentCtx, clientDeps{
		GitLabURL:       c.GitLabURL,
		GitLabToken:     c.GitLabToken,
		ProviderName:    c.Provider,
		ProviderAPIKey:  providerAPIKey(c.Provider),
		ProviderBaseURL: c.BaseURL,
		Model:           c.Model,
		WorkDir:         c.WorkDir,
		Policy:          pol,
		BotUsername:     c.BotUsername,
		DryRun:          c.DryRun,
		Retries:         c.Retries,
		RetryBackoff:    c.RetryBackoff,
		Logger:          logger,
	})
	if err != nil {
		return logWithError(logger, ExitConfig, err.Error(), err)
	}

	result, err := rev.Run(parentCtx, c.Repo, c.MR)
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
		return logWithError(logger, ExitInternal, "review failed", err)
	}

	logger.Info("review complete",
		"findings", len(result.Findings),
		"summary_posted", result.Summary != nil,
		"policy_error", result.PolicyError,
	)

	// Print the MR URL on success (matches repo-mr-file convention).
	if result.MR != nil && result.MR.WebURL != "" {
		_, _ = fmt.Fprintln(stdout, result.MR.WebURL)
	}

	// ExitPolicy = 8 when any error-verdict finding fired.
	if result.PolicyError {
		return &ExitError{Code: ExitPolicy, Reason: "policy violation"}
	}
	return nil
}

// logWithError logs at error level and returns a typed
// *ExitError so main() can map it to the right exit code.
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
