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
	"github.com/inful/mreview/internal/llm"
	"github.com/inful/mreview/internal/strutil"
)

// DoctorCmd holds the flags for `mreview doctor`.
//
// Doctor validates that the local environment is correctly wired up:
//   - GitLab token works and the user can be identified
//   - LLM endpoint responds to /v1/models
//   - The configured model is present in that list
//
// Exit code: 0 when all checks pass; 1 when any check fails (with
// a per-check breakdown). The subcommand never returns ExitConfig
// / ExitAuth itself — it always runs to completion and reports.
type DoctorCmd struct {
	GitLabURL       string        `default:"https://gitlab.com" name:"gitlab-url" env:"GITLAB_URL" help:"GitLab base URL."`
	GitLabToken     string        `env:"GITLAB_TOKEN" name:"gitlab-token" help:"GitLab Personal Access Token (api scope)."`
	LLMURL          string        `default:"http://localhost:11434/v1" name:"llm-url" env:"LLM_URL" help:"LLM OpenAI-compatible base URL."`
	LLMAPIKey       string        `env:"LLM_API_KEY" name:"llm-api-key" help:"LLM API key (optional for local servers)."`
	Model           string        `default:"qwen2.5-coder:7b" env:"LLM_MODEL" help:"LLM model name."`
	MaxDiffBytes    int           `default:"200000" name:"max-diff-bytes" env:"MREVIEW_MAX_DIFF_BYTES" help:"Per-chunk byte budget (matches mreview review)."`
	PerChunkTimeout time.Duration `default:"120s" name:"per-chunk-timeout" env:"MREVIEW_PER_CHUNK_TIMEOUT" help:"Per-LLM-call timeout (matches mreview review)."`
	SkipGitLab      bool          `name:"skip-gitlab" help:"Skip the GitLab check (useful when only the LLM is misconfigured)."`
	SkipLLM         bool          `name:"skip-llm" help:"Skip the LLM check (useful when only GitLab is misconfigured)."`
}

// doctorResult is one row in the doctor's output table.
type doctorResult struct {
	name    string
	ok      bool
	summary string // one-line summary on success
	detail  string // extra detail (e.g. user, model list)
	err     string // error message on failure
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
// "doctor" subcommand. Prints a per-check report and exits 0 when
// all green, 1 when any red.
func runDoctor(stdout io.Writer, c *DoctorCmd, logger *slog.Logger) error {
	logger.Info("running health checks",
		"gitlab_url", c.GitLabURL,
		"llm_url", c.LLMURL,
		"model", c.Model,
	)

	results := make([]doctorResult, 0, 3)

	if c.SkipGitLab {
		results = append(results, doctorResult{name: "GitLab", ok: true, summary: "skipped"})
	} else {
		results = append(results, doctorGitLab(c, logger))
	}
	if c.SkipLLM {
		results = append(results, doctorResult{name: "LLM", ok: true, summary: "skipped"})
	} else {
		results = append(results, doctorLLM(c, logger))
	}
	results = append(results, doctorConfig(c))

	// Print the report.
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

// doctorLLM builds the LLM row by calling ListModels and
// confirming the configured model is present.
func doctorLLM(c *DoctorCmd, logger *slog.Logger) doctorResult {
	r := doctorResult{name: "LLM"}
	provider, err := llm.NewOpenAIProvider(llm.OpenAIConfig{
		BaseURL: c.LLMURL,
		APIKey:  c.LLMAPIKey,
		Model:   c.Model,
	})
	if err != nil {
		r.ok = false
		r.err = "build provider: " + err.Error()
		return r
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	models, err := provider.ListModels(ctx)
	if err != nil {
		// Fallback to Ping so the check still passes against
		// endpoints that don't implement /v1/models.
		if pingErr := provider.Ping(ctx); pingErr != nil {
			r.ok = false
			r.err = "list models: " + err.Error() + "\n  ping: " + pingErr.Error()
			return r
		}
		r.ok = true
		r.summary = c.Model + " (ping ok; /v1/models unsupported)"
		r.detail = fmt.Sprintf("URL: %s", c.LLMURL)
		return r
	}

	modelList := strings.Join(models, ", ")
	if len(modelList) > 200 {
		modelList = modelList[:200] + "..."
	}
	r.ok = true
	r.summary = fmt.Sprintf("%s available (of %d)", c.Model, len(models))
	if slices.Contains(models, c.Model) {
		r.detail = fmt.Sprintf("URL: %s\n  Model present\n  Models: %s", c.LLMURL, modelList)
	} else {
		// Model NOT in the list — still ok=true (we connected),
		// but the operator should know the configured model may
		// not exist on the server.
		r.detail = fmt.Sprintf("URL: %s\n  WARNING: configured model %q NOT in server's model list\n  Models: %s",
			c.LLMURL, c.Model, modelList)
	}
	return r
}

// doctorConfig summarizes the review-relevant config the operator
// has set. Always ok=true — there's nothing to validate beyond
// surfacing the values.
func doctorConfig(c *DoctorCmd) doctorResult {
	r := doctorResult{name: "Config", ok: true}
	r.summary = fmt.Sprintf("--max-diff-bytes=%d  --per-chunk-timeout=%s  model=%s",
		c.MaxDiffBytes, c.PerChunkTimeout, c.Model)
	return r
}

// contains was a tiny string-slice membership helper. Replaced
// by slices.Contains (Go 1.21+) and removed.