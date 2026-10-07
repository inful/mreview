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

	"github.com/inful/mreview/internal/ci/artifact"
	"github.com/inful/mreview/internal/config"
	"github.com/inful/mreview/internal/event"
	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/policy"
	"github.com/inful/mreview/internal/reviewer"
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

	// WorkDir is the path to the reviewed repo's working
	// tree. tokensave's MCP server reads from this path; the
	// local tool registry is otherwise empty (see clients.go
	// for why). REQUIRED — mreview does not auto-detect the
	// reviewed repo, and an empty value used to silently
	// produce a runtime panic, then a three-minute
	// failed-tool-call loop that surfaced as "model reached
	// its output limit". Set via --workdir or
	// MREVIEW_WORKDIR=<abs-path>. CI typically sets
	// "$CI_PROJECT_DIR".
	//
	// Note: NO `type:"path"` annotation. The annotation made
	// kong silently default the field to os.Getwd() when the
	// flag was omitted — exactly the lie the help text used
	// to tell ("defaults to the repo root"). Without it, an
	// empty value flows through to runReview, where the
	// fast-fail check produces a clear ExitConfig pointing
	// at this flag and MREVIEW_WORKDIR.
	//
	// Examples:
	//   --workdir ~/repos/sfb/57_pipeline_canary
	//   MREVIEW_WORKDIR=$CI_PROJECT_DIR mreview review …
	WorkDir string `name:"workdir" env:"MREVIEW_WORKDIR" help:"REQUIRED: path to the reviewed repo's working tree. Set via --workdir or MREVIEW_WORKDIR=<path>. mreview refuses to run without it."`

	// BotUsername is the username the GitLab token posts as;
	// dedupe ignores comments authored by other humans. When
	// empty, dedupe uses every existing comment as the
	// baseline.
	BotUsername string `name:"bot-username" env:"GITLAB_BOT_USERNAME" help:"Username of the bot that posts comments (for dedupe)."`

	// Dry-run.
	DryRun bool `name:"dry-run" help:"Log the intended GitLab posts without performing them."`

	// DebugLLM prints the raw response from the LLM (before
	// the orchestrator parses it into findings) to stderr.
	// Useful when a review fails parse and you want to see
	// exactly what the model emitted vs. what the parser
	// expected — distinct from --dry-run (which suppresses
	// GitLab side-effects) and from --verbose (which raises
	// the slog level; this writes the full raw text to
	// stderr regardless of log level). Off by default.
	DebugLLM bool `name:"debug-llm" help:"Print the raw LLM response to stderr for debugging. Default false."`

	// Retry for GitLab API calls.
	Retries      int           `default:"3" name:"retries" env:"MREVIEW_RETRIES" help:"GitLab API retry attempts on transient errors."`
	RetryBackoff time.Duration `default:"500ms" name:"retry-backoff" env:"MREVIEW_RETRY_BACKOFF" help:"Initial retry backoff; exponential with jitter."`

	// MaxOutputTokens bounds each LLM generation request. The
	// harness's default (when 0) is 8192 tokens; some local /
	// routing models produce verbose chain-of-thought
	// preambles that exhaust that budget before the model emits
	// any tool call, surfacing as "model reached its output
	// limit (8192 tokens) before finishing" and ending the run.
	// 16384 leaves headroom for verbose models while staying
	// well under flagship-provider output caps. Operators on a
	// known-tight-budget model can lower this via the flag or
	// MREVIEW_MAX_OUTPUT_TOKENS env var.
	MaxOutputTokens int `default:"16384" name:"max-output-tokens" env:"MREVIEW_MAX_OUTPUT_TOKENS" help:"Per-generation max_tokens sent to the LLM provider. Default 16384; raise for verbose chain-of-thought models, lower to save cost on tight-budget models."`

	// MaxTurns caps the agent's tool-use loop. The harness
	// default (when 0) is 25; mreview's previous hard-coded
	// value (10) was generous but verbosity across turns
	// can compound quickly with a chatty model. 6 gives the
	// agent enough rounds to (1) explore the tokensave
	// index, (2) read 2-3 of the MR's changed files, and
	// (3) emit the final JSON — past that, the runs we
	// observed tended to thrash on the same queries. Operators
	// on a model that needs more room can raise it; lower
	// for tight-budget models that emit a lot per turn.
	MaxTurns int `default:"6" name:"max-turns" env:"MREVIEW_MAX_TURNS" help:"Cap on the agent's tool-use loop. Default 6; raise for complex MRs, lower for tight-budget models that emit more per turn."`

	// NoDedup disables the same-SHA-skip path. The default
	// behaviour: if a prior mreview summary exists on the
	// MR with a commit SHA matching the current MR HEAD,
	// skip the LLM run entirely (the prior review still
	// represents this commit). Set this to true to force a
	// fresh review regardless — useful when the operator
	// wants to rerun the model after a prompt change, or
	// when the prior review was wrong and a re-roll is
	// desired. CI that doesn't want any review to be silently
	// skipped (compliance reasons) should set this to true.
	NoDedup bool `name:"no-dedup" env:"MREVIEW_NO_DEDUP" help:"Disable same-commit-skip dedup; force a fresh review every run. Default false."`

	// Per-event branching (issue #41 / migration step 1 of #42).
	//
	// OnDrafts default is "run" (changed from "skip" in v0.9.3).
	// The previous default silently skipped review for draft
	// MRs, which produced an operational gap: a developer
	// could move an MR from draft to ready without GitLab
	// firing a new pipeline (the "Mark as ready" event is
	// not a default pipeline trigger), so the per-event
	// guard never got a chance to flip Proceed=true. With
	// the new default, every MR pipeline — including those
	// that fire while the MR is still a draft — runs the
	// review. Operators who want the old skip-on-draft
	// behaviour can set MREVIEW_ON_DRAFTS=skip or
	// --on-drafts=skip explicitly.
	OnDrafts string `default:"run" name:"on-drafts" enum:"run,skip" env:"MREVIEW_ON_DRAFTS" help:"Action on draft MRs (CI_MERGE_REQUEST_DRAFT=true): run the review or skip with exit 0. Default run."`
	OnPush   string `default:"skip" name:"on-push" enum:"run,skip" env:"MREVIEW_ON_PUSH" help:"Action on direct branch pushes (CI_PIPELINE_SOURCE=push): run the review or skip with exit 0. Default skip."`

	// PolicyFile points at a YAML file with the policy schema
	// documented in internal/policy. Empty = no policy. The
	// file is loaded and validated at startup; a malformed
	// policy returns ExitConfig before any GitLab / harness
	// work happens. The orchestrator calls the enforcer on
	// the agent's findings; any error-verdict finding makes
	// the review exit with ExitPolicy = 8.
	PolicyFile string `name:"policy-file" env:"MREVIEW_POLICY_FILE" type:"path" help:"Path to a YAML policy file. Empty = no policy. See internal/policy for the schema."`

	// Tokensave MCP integration (issue #42 step 4). The
	// harness runtime spawns the tokensave subprocess and
	// registers its tools under the mcp__tokensave__*
	// namespace. Disable for repos where tokensave is
	// unavailable (offline / air-gapped) or when the operator
	// wants to fall back to read_file-only mode.
	TokensaveEnabled bool   `default:"true" name:"tokensave-enabled" env:"MREVIEW_TOKENSAVE_ENABLED" help:"Enable the tokensave MCP server (the agent's primary code-graph tool). Default true."`
	TokensaveBin     string `name:"tokensave-bin" env:"MREVIEW_TOKENSAVE_BIN" help:"Path to the tokensave binary (default: PATH-resolved 'tokensave'). Used when --tokensave-enabled is true."`

	// ArtifactsDir points at the directory the central CI
	// pipeline populated before mreview ran. The orchestrator
	// reads build.log, test_results.json, lint.json, and
	// vulns.json from this directory and injects them into the
	// harness prompt as pre-loaded context. Per-artifact
	// failures (missing / empty / malformed) are degraded
	// information, not errors — the agent sees explicit
	// "NOT AVAILABLE" markers. Empty = no artifacts (the
	// prompt still renders an all-NOT-AVAILABLE block).
	//
	// The directory itself being unreachable IS an error
	// (caller exits with ExitConfig).
	ArtifactsDir string `default:".mreview-artifacts" name:"artifacts-dir" env:"MREVIEW_ARTIFACTS_DIR" type:"path" help:"Directory containing CI artifacts (build.log, test_results.json, lint.json, vulns.json). Default .mreview-artifacts."`

	// Skills (issue #44) wires the optional skills MCP server
	// into the harness runtime. The server exposes
	// mcp__skills__list_skills and mcp__skills__read_skill so
	// the agent can pull team-authored review guidance on
	// demand. Set --skills-repo to a GitLab project that hosts
	// a skills/ directory of .md files; the server reads them
	// at startup and caches for the run.
	//
	// All four flags are optional; when --skills-repo is empty
	// the skills MCP server is NOT started and existing
	// deployments see no behavior change. The token env defaults
	// to the same env var --gitlab-token reads from.
	SkillsRepo     string `name:"skills-repo"      env:"MREVIEW_SKILLS_REPO"      help:"GitLab project path (group/project) that hosts the skills .md files. Empty disables the skills MCP server."`
	SkillsDir      string `name:"skills-dir"       env:"MREVIEW_SKILLS_DIR"       help:"Directory within --skills-repo containing the .md files. Default 'skills'."`
	SkillsRef      string `name:"skills-ref"       env:"MREVIEW_SKILLS_REF"       help:"Branch / tag / SHA to read the skills from. Default 'main'."`
	SkillsTokenEnv string `name:"skills-token-env" env:"MREVIEW_SKILLS_TOKEN_ENV" help:"NAME of the env var carrying the PAT for the skills repo. Default reuses --gitlab-token-env (GITLAB_TOKEN)."`

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
//
// The function is intentionally thin: it wires four named
// helpers together (prepare / load / execute / summarize)
// and translates the per-helper signals into the runReview
// return contract:
//
//   - prepareReview returns (nil, errSkipReview) for the
//     per-event guard's skip case, which the caller
//     translates to a nil return (clean exit, no review).
//   - prepareReview returns (nil, *ExitError) for config
//     failures (empty workdir, bad policy file).
//   - executeReview returns (nil, *ExitError) for any
//     orchestrator / GitLab error, with the kind already
//     mapped to a typed exit code.
//   - summarizeReview returns *ExitError only for the
//     policy-violation case (result.PolicyError == true).
//
// The 4-helper split (vs. the original monolithic 170-line
// function) is the point of sub-phase 4 of the codebase
// review plan: each helper is small enough to test in
// isolation, and the public surface (runReview) shrinks
// to a wiring diagram. See PHASE-fix-review-findings.md
// for the rationale.
func runReview(parentCtx context.Context, stdout io.Writer, c *ReviewCmd, cfg *config.File, logger *slog.Logger) error {
	prep, err := prepareReview(c, logger)
	if errors.Is(err, errSkipReview) {
		return nil
	}
	if err != nil {
		return err
	}

	rev, err := loadReviewer(parentCtx, c, prep, logger)
	if err != nil {
		return err
	}

	result, err := executeReview(parentCtx, rev, c, logger)
	if err != nil {
		return err
	}

	return summarizeReview(result, stdout, logger)
}

// preparedReview is the bundle of loaded data that the
// per-event guard + workdir check + policy + artifact load
// produces. The struct is unexported because it's only
// meaningful between prepareReview and loadReviewer; nothing
// outside the package needs to see it.
type preparedReview struct {
	policy      *policy.Policy
	artifactSet *artifact.Set
}

// errSkipReview is a sentinel returned by prepareReview
// when the per-event guard decides to skip. The caller
// (runReview) translates it to a nil return so the skip
// path looks like a clean exit to the run() dispatcher.
//
// Defined as a package-level var (not a typed error) so
// errors.Is(err, errSkipReview) is a direct comparison and
// no type assertion is needed.
var errSkipReview = errors.New("review skipped per per-event guard")

// prepareReview runs the gates that fire BEFORE the
// orchestrator is built: per-event guard, workdir check,
// policy load, artifact load. Returns (nil, errSkipReview)
// when the guard says to skip; (nil, *ExitError) when a
// config check fails; (loaded, nil) when the path is
// clear. The "starting review" log line fires here so it
// appears at a consistent point in the JSON log stream
// regardless of which subsequent step fails.
func prepareReview(c *ReviewCmd, logger *slog.Logger) (*preparedReview, error) {
	// Per-event guard (issue #41 / #42 migration step 1).
	ev := event.Detect()
	decision := event.Decide(ev, event.Prefer(c.OnDrafts), event.Prefer(c.OnPush))
	if !decision.Proceed {
		// Info (not Debug) so the operator sees the skip in
		// default log output. A silent skip in CI is a real
		// UX problem: the pipeline reports "success" but the
		// review never ran, and the operator has no breadcrumb
		// to find out why. Verbose mode still emits this line
		// (and everything else); suppressing it under the
		// default INFO level was over-eager.
		logger.Info("skipping review per per-event guard",
			"reason", decision.Reason,
			"source", ev.Source,
			"mr_iid", ev.MRIID,
			"mr_draft", ev.MRDraft,
			"on_drafts", c.OnDrafts,
			"on_push", c.OnPush,
		)
		return nil, errSkipReview
	}
	if decision.Reason != "" {
		logger.Info("proceeding with review per override",
			"reason", decision.Reason,
			"source", ev.Source,
		)
	}

	// Fast-fail on the most common config mistake: empty
	// --workdir. Without this check, mreview would build a
	// harness runtime that points tokensave at the operator's
	// CWD, index the wrong project, and either return
	// unhelpful "no such symbol" responses or, on the old
	// read_file path, ENOENT-loop until the model's output
	// budget exhausted. Placed AFTER the event guard so
	// scheduled invocations that are skipping (draft / push)
	// don't surface a confusing config error instead of the
	// expected skip log.
	if c.WorkDir == "" {
		const msg = "workdir is required: pass --workdir <path-to-reviewed-repo> or set MREVIEW_WORKDIR=<path>"
		logger.Error(msg,
			"flag", "--workdir",
			"env", "MREVIEW_WORKDIR",
		)
		return nil, &ExitError{Code: ExitConfig, Reason: msg}
	}

	// Policy load (issue #42 migration step 2).
	var pol *policy.Policy
	if c.PolicyFile != "" {
		var err error
		pol, err = policy.Load(c.PolicyFile)
		if err != nil {
			return nil, logWithError(logger, ExitConfig, "policy file", err)
		}
		logger.Info("policy loaded",
			"path", c.PolicyFile,
			"severity_overrides", len(pol.SeverityOverrides),
			"forbid_rules", len(pol.Forbid),
			"require_rules", len(pol.Require),
			"label_rules", len(pol.Labels),
		)
	}

	// CI artifact load (issue #43 / #42 step 5). The
	// orchestrator threads the set into the harness prompt.
	// Missing / empty / unreadable artifacts are degraded
	// information, not errors — the prompt surfaces an
	// explicit "NOT AVAILABLE" marker. The directory itself
	// being unreachable is treated leniently: a debug log +
	// no artifacts (the prompt still renders the
	// all-NOT-AVAILABLE block). CI operators will see the
	// debug line if they typo the path.
	var artifactSet *artifact.Set
	if c.ArtifactsDir != "" {
		set, err := artifact.LoadAll(c.ArtifactsDir, artifact.Source{
			BuildPath: "build.log",
			TestsPath: "test_results.json",
			LintPath:  "lint.json",
			VulnsPath: "vulns.json",
		})
		if err != nil {
			// Info (not Debug) for the same reason as the
			// per-event guard: a missing artifacts dir is
			// degraded information, not a diagnostic detail.
			// The operator should see this in the default
			// log output so they can correct the CI path
			// (typo, missing job) without enabling verbose.
			logger.Info("artifacts dir not available; proceeding without",
				"dir", c.ArtifactsDir,
				"err", err.Error(),
			)
		} else {
			artifactSet = &set
			logger.Info("artifacts loaded",
				"dir", c.ArtifactsDir,
				"build", statusLabel(set.Build),
				"tests", statusLabel(set.Tests),
				"lint", statusLabel(set.Lint),
				"vulns", statusLabel(set.Vulns),
			)
		}
	}

	logger.Info("starting review",
		"repo", c.Repo,
		"mr", c.MR,
		"provider", c.Provider,
		"model", c.Model,
		"dry_run", c.DryRun,
		"source", ev.Source,
	)

	return &preparedReview{policy: pol, artifactSet: artifactSet}, nil
}

// loadReviewer builds the orchestrator. Thin wrapper over
// the package-level reviewRunner var (which tests swap for a
// fake). Returns *ExitError on any build failure; the
// wrapped error is already informative enough that the
// caller doesn't need to add context.
func loadReviewer(parentCtx context.Context, c *ReviewCmd, prep *preparedReview, logger *slog.Logger) (reviewerInterface, error) {
	rev, err := reviewRunner(parentCtx, clientDeps{
		GitLabURL:        c.GitLabURL,
		GitLabToken:      c.GitLabToken,
		ProviderName:     c.Provider,
		ProviderAPIKey:   providerAPIKey(c.Provider),
		ProviderBaseURL:  c.BaseURL,
		Model:            c.Model,
		WorkDir:          c.WorkDir,
		Policy:           prep.policy,
		BotUsername:      c.BotUsername,
		DryRun:           c.DryRun,
		DebugLLM:         c.DebugLLM,
		Retries:          c.Retries,
		RetryBackoff:     c.RetryBackoff,
		MaxOutputTokens:  c.MaxOutputTokens,
		MaxTurns:         c.MaxTurns,
		NoDedup:          c.NoDedup,
		TokensaveEnabled: c.TokensaveEnabled,
		TokensaveBin:     c.TokensaveBin,
		SkillsRepo:       c.SkillsRepo,
		SkillsDir:        c.SkillsDir,
		SkillsRef:        c.SkillsRef,
		SkillsTokenEnv:   c.SkillsTokenEnv,
		Artifacts:        prep.artifactSet,
		Logger:           logger,
	})
	if err != nil {
		return nil, logWithError(logger, ExitConfig, err.Error(), err)
	}
	return rev, nil
}

// executeReview runs the orchestrator and maps its error
// to a typed *ExitError. The gitlab.Error.Kind -> exit
// code mapping is the only switch in the package; the test
// in review_errors_test.go pins every case in one place.
func executeReview(parentCtx context.Context, rev reviewerInterface, c *ReviewCmd, logger *slog.Logger) (*reviewer.Result, error) {
	result, err := rev.Run(parentCtx, c.Repo, c.MR)
	if err != nil {
		// Map gitlab.Error.Kind → typed ExitError.
		var ge *gitlab.Error
		if errors.As(err, &ge) {
			switch ge.Kind {
			case gitlab.KindAuth:
				return nil, logWithError(logger, ExitAuth, "auth failure", err)
			case gitlab.KindNotFound:
				return nil, logWithError(logger, ExitNotFound, "MR not found", err)
			case gitlab.KindConflict:
				return nil, logWithError(logger, ExitConflict, "conflict", err)
			case gitlab.KindTransient:
				return nil, logWithError(logger, ExitTransient, "transient exhausted", err)
			case gitlab.KindBadRequest:
				return nil, logWithError(logger, ExitConfig, "bad request", err)
			default:
				return nil, logWithError(logger, ExitInternal, "review failed", err)
			}
		}
		return nil, logWithError(logger, ExitInternal, "review failed", err)
	}
	return result, nil
}

// summarizeReview logs the "review complete" line, prints
// the MR URL (when present), and converts a PolicyError
// into the ExitPolicy exit code. Returns nil on a clean
// review so run() can return 0.
func summarizeReview(result *reviewer.Result, stdout io.Writer, logger *slog.Logger) error {
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

// statusLabel turns a LoadResult into a one-line log label:
// "present", "malformed", or "missing". Keeps the artifact
// log line readable.
func statusLabel[T any](r artifact.LoadResult[T]) string {
	switch {
	case r.Value != nil:
		return "present"
	case r.ParseError != nil:
		return "malformed"
	default:
		return "missing"
	}
}
