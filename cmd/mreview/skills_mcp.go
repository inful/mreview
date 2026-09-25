package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/skills"
	skillsmcp "github.com/inful/mreview/internal/skills/mcp"
)

// SkillsMCPCmd holds the env-var-driven inputs for the skills MCP
// server subcommand.
//
// Why env vars (not flags): this command is spawned as a
// subprocess by the harness library. The harness passes the
// already-resolved values via the process environment; flags
// would require a wrapper script that quotes each arg
// correctly. Env vars avoid the quoting mess and keep the
// spawn line simple (`mreview skills-mcp` with five env vars).
//
// Why hidden: end-users should never invoke this directly. The
// subcommand exists for the harness subprocess pattern; if it
// were public, users could misconfigure it. Hide from `--help`
// while keeping it callable.
//
// Issue #44.
type SkillsMCPCmd struct {
	// RepoPath is the GitLab project (e.g. "inful/mreview-skills").
	// Required — the loader has nothing to read otherwise.
	RepoPath string `env:"MREVIEW_SKILLS_REPO" required:"" help:"GitLab project path that hosts the skills .md files."`

	// Directory within RepoPath (default "skills").
	Directory string `env:"MREVIEW_SKILLS_DIR" default:"skills" help:"Directory within --skills-repo that contains the .md files."`

	// Ref is the branch / tag / SHA to read (default "main").
	Ref string `env:"MREVIEW_SKILLS_REF" default:"main" help:"Branch / tag / SHA the loader reads from."`

	// GitLabURL is the API base the subprocess uses to fetch
	// skills. Required — passed through to *gitlab.Client.
	GitLabURL string `env:"GITLAB_URL" required:"" help:"GitLab API base URL (e.g. https://gitlab.com)."`

	// Token is the PAT the subprocess authenticates with.
	// Carried in env (not flag) for the same quoting reason
	// as RepoPath.
	Token string `env:"GITLAB_TOKEN" required:"" help:"GitLab Personal Access Token."`

	// Retry policy for the GitLab API calls. Same defaults
	// as the review subcommand.
	Retries         int           `env:"MREVIEW_RETRIES" default:"3"`
	RetryBackoff    time.Duration `env:"MREVIEW_RETRY_BACKOFF" default:"500ms"`
	RetryMaxBackoff time.Duration `env:"MREVIEW_RETRY_MAX_BACKOFF" default:"30s"`
}

// runSkillsMCP wires a skills.Loader from env vars and serves
// the MCP server over stdio. Blocks until ctx is cancelled or
// the client (the harness library) disconnects.
//
// Logging goes to stderr — stdout is the MCP transport. A
// graceful shutdown returns nil; context cancellation surfaces
// as a nil too (the harness stops calling us). Transport errors
// return as ExitInternal so the parent process logs them.
func runSkillsMCP(parentCtx context.Context, _ io.Writer, c *SkillsMCPCmd, logger *slog.Logger) error {
	logger.Info("starting skills MCP server",
		"repo", c.RepoPath,
		"directory", c.Directory,
		"ref", c.Ref,
		"gitlab_url", c.GitLabURL,
	)

	glClient, err := gitlab.NewClient(c.GitLabURL, c.Token, gitlab.RetryConfig{
		MaxAttempts:    c.Retries + 1,
		InitialBackoff: c.RetryBackoff,
		MaxBackoff:     c.RetryMaxBackoff,
		Logger:         logger,
	}, logger)
	if err != nil {
		return &ExitError{Code: ExitConfig, Reason: "skills-mcp: build gitlab client", Wrapped: err}
	}

	loader := skills.New(glClient, c.RepoPath, c.Directory, c.Ref, logger)

	if err := skillsmcp.Serve(parentCtx, loader); err != nil && !errors.Is(err, context.Canceled) {
		return &ExitError{Code: ExitInternal, Reason: "skills-mcp: serve", Wrapped: err}
	}
	logger.Info("skills MCP server stopped", "repo", c.RepoPath)
	return nil
}

// compile-time check that the error helper still imports fmt.
var _ = fmt.Errorf
