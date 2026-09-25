// Command mreview is an automated LLM merge-request reviewer for GitLab.
//
// It fetches the diff for a merge request, sends it to a local LLM
// (Ollama or any OpenAI-compatible endpoint), and posts the result back
// to the MR as inline comments plus a summary thread.
//
// See README.md for usage, flags, env vars, and exit codes.
//
// Subcommands:
//
//	mreview review  — review one merge request and exit
//	mreview serve   — run an HTTP server that consumes GitLab webhooks
//	mreview doctor  — validate config + LLM + GitLab connectivity
//
// This file wires them together: parses flags, builds the slog logger,
// dispatches to the matched subcommand, and maps the result to a process
// exit code.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/alecthomas/kong"

	"github.com/inful/mreview/internal/config"
	"github.com/inful/mreview/internal/logging"
)

// exitSignal is the panic value raised by kong's Exit function so we can
// distinguish "kong wants to exit" (--help) from a real panic. Recovered
// in run() to convert into the matching exit code.
type exitSignal struct{ code int }

// run is the testable entry point. args, stdout, and stderr are injected
// so tests can drive the parser and capture output without touching
// os.Args, os.Stdout, or os.Stderr.
//
// parentCtx is the long-lived context for the subcommand. Production
// wires this to signal.NotifyContext(SIGINT, SIGTERM); tests can pass
// a cancellable context to drive shutdown deterministically.
//
// Kong flow:
//
//   - --help: kong panics via Exit(0); defer recover catches it.
//   - parse error (missing required, bad type, unknown flag): Parse
//     returns *kong.ParseError; we print it and exit 2.
//   - parse success + config error (e.g. --log-format=yaml): the
//     subcommand's setup fails and we exit 2.
//   - subcommand success: exit 0.
//   - subcommand failure: ExitError carries the typed code.
func run(parentCtx context.Context, args []string, stdout, stderr io.Writer) (exitCode int) {
	var cli CLI
	// Pre-scan args for --config (and -config) so we can load
	// the config file BEFORE kong parses. This lets config values
	// populate env vars that kong's env:"" tags resolve against.
	cfgPath := preScanConfigPath(args)
	var cfg *config.File
	if cfgPath != "" {
		var cfgErr error
		cfg, cfgErr = config.Load(cfgPath)
		if cfgErr != nil {
			_, _ = fmt.Fprintln(stderr, cfgErr.Error())
			return ExitConfig
		}
		// Cleanup restores env vars that applyConfigToEnv mutated.
		// In production this is a no-op (process is about to exit)
		// but it keeps tests isolated.
		defer applyConfigToEnv(cfg)()
	}

	parser, err := kong.New(&cli,
		// Panic with a sentinel so we own the exit code.
		kong.Exit(func(code int) { panic(&exitSignal{code: code}) }),
		// Route kong's own writes (help text, usage on error) through
		// our buffers so tests can capture them.
		kong.Writers(stdout, stderr),
		kong.Name("mreview"),
		kong.Description("Automated LLM merge-request reviews for GitLab."),
	)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return ExitInternal
	}

	defer func() {
		if r := recover(); r != nil {
			if sig, ok := r.(*exitSignal); ok {
				// Kong wanted to exit (--help raises Exit(0)).
				exitCode = sig.code
				return
			}
			panic(r) // real panic; surface it.
		}
	}()

	// Single parse pass: env vars are already populated by
	// applyConfigToEnv above. CLI flags override env vars (kong's
	// documented precedence), so explicit flags always win.
	ctx, parseErr := parser.Parse(args)
	if parseErr != nil {
		_, _ = fmt.Fprintln(stderr, parseErr.Error())
		return ExitConfig
	}

	logger, err := logging.New(stderr, cli.LogFormat, cli.Verbose)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return ExitConfig
	}

	slog.SetDefault(logger)
	defer slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	switch ctx.Command() {
	case "review":
		return exitCodeFromError(runReview(parentCtx, stdout, cli.Review, cfg, logger))
	case "doctor":
		return exitCodeFromError(runDoctor(parentCtx, stdout, cli.Doctor, logger))
	case "skills-mcp":
		return exitCodeFromError(runSkillsMCP(parentCtx, stdout, cli.SkillsMCP, logger))
	default:
		logger.Error("no subcommand matched", "command", ctx.Command())
		return ExitConfig
	}
}

// applyConfigToEnv injects the config file's values into the
// process environment so kong's env:"" tags pick them up at
// parse time. CLI flags still override env vars, so the user
// always wins.
//
// Returns a cleanup function that restores the env vars we set.
// In production this is a no-op (process is about to exit) but
// it keeps tests isolated — without it, env pollution from one
// test would leak into the next.
//
// The config file references env-var NAMES (not secret values)
// for sensitive fields like the GitLab token. We resolve those
// references here: if cfg.GitLab.TokenEnv is "GITLAB_TOKEN_FOO"
// and the operator has GITLAB_TOKEN_FOO set in their secret
// store, we copy its value to GITLAB_TOKEN (the env-var the
// CLI flags expect).
//
// The bindings are listed once in a table below; each entry's
// get function returns the value (or "" to skip). Adding a new
// config → env mapping is a one-line table addition. The
// exhaustive-coverage test in main_test.go pins every binding.
func applyConfigToEnv(cfg *config.File) func() {
	type op struct {
		key string
		// prior is the value the env var held before we set it;
		// empty means "unset". restore picks the right os call.
		prior string
		was   bool
	}
	var mutations []op

	set := func(key, value string) {
		if value == "" {
			return
		}
		if prior, ok := os.LookupEnv(key); ok {
			mutations = append(mutations, op{key: key, prior: prior, was: true})
		} else {
			mutations = append(mutations, op{key: key, was: false})
		}
		_ = os.Setenv(key, value)
	}

	bindings := []struct {
		key string
		get func(*config.File) string
	}{
		// GitLab connection.
		{"GITLAB_URL", func(c *config.File) string { return c.GitLab.URL }},
		{"GITLAB_TOKEN", func(c *config.File) string {
			if c.GitLab.TokenEnv == "" {
				return ""
			}
			return os.Getenv(c.GitLab.TokenEnv)
		}},

		// Provider connection (after #42, the harness library
		// owns the provider matrix; mreview only forwards the
		// base URL + model name).
		{"MREVIEW_PROVIDER_BASE_URL", func(c *config.File) string { return c.Provider.BaseURL }},
		{"MREVIEW_MODEL", func(c *config.File) string { return c.Provider.Model }},

		// Review tunables.
		{"GITLAB_BOT_USERNAME", func(c *config.File) string { return c.Review.BotUsernameEnv }},

		// Retry.
		{"MREVIEW_RETRIES", func(c *config.File) string {
			// --retries = MaxAttempts - 1.
			if c.Retry.MaxAttempts == 0 {
				return ""
			}
			return strconv.Itoa(c.Retry.MaxAttempts - 1)
		}},
		{"MREVIEW_RETRY_BACKOFF", func(c *config.File) string { return c.Retry.InitialBackoff }},
		{"MREVIEW_RETRY_MAX_BACKOFF", func(c *config.File) string { return c.Retry.MaxBackoff }},
	}

	for _, b := range bindings {
		set(b.key, b.get(cfg))
	}

	return func() {
		for _, m := range mutations {
			if m.was {
				_ = os.Setenv(m.key, m.prior)
			} else {
				_ = os.Unsetenv(m.key)
			}
		}
	}
}

// exitCodeFromError maps an error to a process exit code, as documented
// in README.md.
//
//   - nil: ExitOK (0)
//   - *ExitError: returns its Code
//   - any other error: ExitInternal (7)
func exitCodeFromError(err error) int {
	if err == nil {
		return ExitOK
	}
	var exitErr *ExitError
	if errors.As(err, &exitErr) {
		return exitErr.Code
	}
	return ExitInternal
}

// main parses os.Args and calls os.Exit with the code returned by run.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	os.Exit(code)
}
