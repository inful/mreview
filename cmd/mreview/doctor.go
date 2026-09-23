package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/provider"
	"github.com/inful/mreview/internal/strutil"
)

// DoctorCmd holds the flags for `mreview doctor` after the
// architecture reset (#42). The LLM-specific flags are gone —
// the doctor now reports which provider is configured and
// checks connectivity through the harness matrix.
type DoctorCmd struct {
	GitLabURL   string `default:"https://gitlab.com" name:"gitlab-url" env:"GITLAB_URL" help:"GitLab base URL."`
	GitLabToken string `env:"GITLAB_TOKEN" name:"gitlab-token" help:"GitLab Personal Access Token (api scope)."`

	Provider string `default:"local" name:"provider" enum:"anthropic,openai,gemini,litellm,openrouter,local" env:"MREVIEW_PROVIDER" help:"Provider to check."`
	BaseURL  string `name:"provider-base-url" env:"MREVIEW_PROVIDER_BASE_URL" help:"Provider base URL."`
	Model    string `default:"qwen2.5-coder:7b" name:"model" env:"MREVIEW_MODEL" help:"Model name to check."`

	SkipGitLab   bool `name:"skip-gitlab" help:"Skip the GitLab check."`
	SkipProvider bool `name:"skip-provider" help:"Skip the provider check."`
}

// doctorResult is one row in the doctor's output table.
type doctorResult struct {
	name    string
	ok      bool
	summary string
	detail  string
	err     string
}

func (r doctorResult) String() string {
	status := "ok"
	if !r.ok {
		status = "FAIL"
	}
	out := fmt.Sprintf("%s: %s", r.name, status)
	if r.summary != "" {
		out += "  " + r.summary
	}
	if r.detail != "" {
		out += "\n  " + r.detail
	}
	if r.err != "" {
		out += "\n  " + r.err
	}
	return out
}

// runDoctor is invoked by run() after CLI parsing matches the
// "doctor" subcommand.
func runDoctor(parentCtx context.Context, stdout io.Writer, c *DoctorCmd, logger *slog.Logger) error {
	logger.Info("running health checks",
		"gitlab_url", c.GitLabURL,
		"provider", c.Provider,
		"model", c.Model,
	)

	results := make([]doctorResult, 0, 3)

	if c.SkipGitLab {
		results = append(results, doctorResult{name: "GitLab", ok: true, summary: "skipped"})
	} else {
		results = append(results, doctorGitLab(c, logger))
	}
	if c.SkipProvider {
		results = append(results, doctorResult{name: "Provider", ok: true, summary: "skipped"})
	} else {
		results = append(results, doctorProvider(parentCtx, c, logger))
	}
	results = append(results, doctorConfig(c))

	_, _ = fmt.Fprintln(stdout, "mreview doctor")
	_, _ = fmt.Fprintln(stdout, "─────────────")
	allOK := true
	for _, r := range results {
		_, _ = fmt.Fprintln(stdout, r.String())
		if !r.ok {
			allOK = false
		}
	}
	_, _ = fmt.Fprintln(stdout)
	if allOK {
		_, _ = fmt.Fprintln(stdout, "All checks passed.")
		return nil
	}
	_, _ = fmt.Fprintln(stdout, "One or more checks failed. See above for details.")
	return &ExitError{Code: ExitInternal, Reason: "doctor reported failures"}
}

// doctorGitLab builds the GitLab row by calling CurrentUser.
func doctorGitLab(c *DoctorCmd, logger *slog.Logger) doctorResult {
	r := doctorResult{name: "GitLab"}
	if strings.TrimSpace(c.GitLabToken) == "" {
		r.ok = false
		r.err = "GITLAB_TOKEN / --gitlab-token not set"
		return r
	}

	client, err := gitlab.NewClient(c.GitLabURL, c.GitLabToken, gitlab.RetryConfig{
		MaxAttempts:    2,
		InitialBackoff: 100 * time.Millisecond,
		MaxBackoff:     1 * time.Second,
		Logger:         logger,
	}, logger)
	if err != nil {
		r.ok = false
		r.err = "build client: " + err.Error()
		return r
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	user, err := client.CurrentUser(ctx)
	if err != nil {
		r.ok = false
		var ge *gitlab.Error
		if errors.As(err, &ge) {
			r.err = fmt.Sprintf("%s (status=%d): %s", ge.Kind, ge.StatusCode, strutil.Truncate(ge.Body, 120))
		} else {
			r.err = err.Error()
		}
		return r
	}

	r.ok = true
	r.summary = user.Username
	r.detail = fmt.Sprintf("URL: %s", c.GitLabURL)
	if user.Name != "" {
		r.detail += "\n  Name: " + user.Name
	}
	return r
}

// doctorProvider builds the provider row by checking the
// harness library's provider matrix. The harness library
// doesn't expose a connectivity check at this layer, so we
// just confirm the constructor doesn't error — connectivity
// is verified at review time. Operators wanting real
// connectivity should run `mreview review --dry-run`.
func doctorProvider(parentCtx context.Context, c *DoctorCmd, logger *slog.Logger) doctorResult {
	r := doctorResult{name: "Provider"}
	ctx, cancel := context.WithTimeout(parentCtx, 10*time.Second)
	defer cancel()

	p, err := provider.Build(ctx, provider.Config{
		Name:    provider.Name(c.Provider),
		APIKey:  providerAPIKey(c.Provider),
		BaseURL: c.BaseURL,
	})
	if err != nil {
		r.ok = false
		r.err = "build provider: " + err.Error()
		return r
	}
	infos := p.Models()
	modelNames := make([]string, len(infos))
	for i, m := range infos {
		modelNames[i] = m.ID
	}
	r.ok = true
	r.summary = fmt.Sprintf("%s ready (model=%s, %d known models)", c.Provider, c.Model, len(infos))
	if len(modelNames) > 0 {
		list := strings.Join(modelNames, ", ")
		if len(list) > 200 {
			list = list[:200] + "..."
		}
		r.detail = fmt.Sprintf("Models: %s", list)
	}
	if slices.Contains(modelNames, c.Model) {
		if r.detail != "" {
			r.detail += "\n  "
		}
		r.detail += "Configured model present"
	} else if len(modelNames) > 0 {
		if r.detail != "" {
			r.detail += "\n  "
		}
		r.detail += fmt.Sprintf("WARNING: configured model %q NOT in server's model list", c.Model)
	}
	return r
}

// doctorConfig summarizes the review-relevant config.
func doctorConfig(c *DoctorCmd) doctorResult {
	r := doctorResult{name: "Config", ok: true}
	r.summary = fmt.Sprintf("provider=%s model=%s", c.Provider, c.Model)
	return r
}
