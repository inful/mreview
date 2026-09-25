package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/inful/mreview/internal/config"
)

// Exit codes documented in README.md. Stable; CI scripts may depend on them.
const (
	ExitOK        = 0 // success
	ExitConfig    = 2 // configuration error (missing flag, bad flag, missing config)
	ExitAuth      = 3 // authentication error (401/403 against GitLab or LLM)
	ExitNotFound  = 4 // not found (404 on MR, project, model)
	ExitConflict  = 5 // conflict (line anchor out of range, stale MR head)
	ExitTransient = 6 // transient failure exhausted (5xx/429 retries exhausted)
	ExitInternal  = 7 // unexpected internal error
	// ExitPolicy is set when `policy.yaml` produces at least one
	// error-verdict finding (or any forbid/require rule fires).
	// Distinct from ExitInternal so CI scripts can tell "policy
	// said no" apart from "review crashed." Introduced by
	// migration step 2 of #42 (architecture reset).
	ExitPolicy = 8
)

// DefaultConfigPath is the conventional location for the user's
// mreview configuration. ~ / .config follows XDG Base Directory.
const DefaultConfigPath = ".config/mreview/config.yaml"

// CLI holds the parsed command line.
//
// Global flags live directly on this struct (kong hoists them so they're
// accepted anywhere on the line). Subcommands are bound as struct fields
// tagged with cmd:""; main.go dispatches on ctx.Command() to the matching
// run* function.
type CLI struct {
	// Global flags.
	LogFormat string `default:"text" enum:"text,json" name:"log-format" help:"Log output format."`
	Verbose   bool   `help:"Enable debug logging."`
	Config    string `name:"config" env:"MREVIEW_CONFIG" type:"path" help:"Path to YAML config file (default: ~/.config/mreview/config.yaml). When set, values populate env vars before flag parsing; explicit flags always win."`

	// Subcommands. Each is a flag-bag struct dispatched from main.go.
	Review    *ReviewCmd    `cmd:"review" help:"Review a merge request once and exit."`
	Doctor    *DoctorCmd    `cmd:"doctor" help:"Validate config + provider + GitLab connectivity."`
	SkillsMCP *SkillsMCPCmd `cmd:"skills-mcp" hidden:"" help:"Run the skills MCP server over stdio. Internal — invoked by the harness library as a subprocess when --skills-repo is set."`
}

// resolveConfigPath returns the config file path the user wants to
// load. Honors the --config flag, then MREVIEW_CONFIG env, then the
// default location. Empty string means "no config file".
func resolveConfigPath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if env := os.Getenv("MREVIEW_CONFIG"); env != "" {
		return env
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, DefaultConfigPath)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// preScanConfigPath extracts --config / -config / MREVIEW_CONFIG
// from raw args WITHOUT going through kong. Used by run() to
// load the config file before the first kong.Parse so config
// values can populate env vars.
//
// Supports both `--config=PATH`, `--config PATH`, and
// `-config=PATH`. Stops at the first non-flag arg (the
// subcommand name) since config is a global flag.
//
// Returns empty string when --config isn't present and no
// default config exists. Caller treats empty as "skip config".
func preScanConfigPath(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		// Stop scanning at the subcommand — the only flag before
		// the subcommand name is the global --config.
		if !strings.HasPrefix(arg, "-") {
			break
		}
		switch {
		case arg == "--config" || arg == "-config":
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		case strings.HasPrefix(arg, "--config="):
			return strings.TrimPrefix(arg, "--config=")
		case strings.HasPrefix(arg, "-config="):
			return strings.TrimPrefix(arg, "-config=")
		}
	}
	return resolveConfigPath("")
}

// loadOptionalFile reads path and returns its content. An empty
// path returns ("", nil) — the "no customization" case. label is
// used in the wrapped error to identify which file failed.
//
// Used for the optional --system-prompt-file / --user-prompt-file
// inputs that layer team-specific guidance on top of the
// system-owned prompt sections.
func loadOptionalFile(path, label string) (string, error) {
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s file %q: %w", label, path, err)
	}
	return string(data), nil
}

// ExitError lets a subcommand signal a typed exit code without coupling
// to os.Exit. Subcommands return *ExitError; main() maps it back to an
// integer via exitCodeFromError.
type ExitError struct {
	Code    int
	Reason  string // short, human-readable; logged at error level
	Wrapped error  // optional cause; logged at debug level
}

// Error implements the error interface so ExitError can flow through any
// error-returning API.
func (e *ExitError) Error() string {
	if e.Wrapped != nil {
		return fmt.Sprintf("%s: %v", e.Reason, e.Wrapped)
	}
	return e.Reason
}

// Unwrap allows errors.Is / errors.As to reach the wrapped cause.
func (e *ExitError) Unwrap() error { return e.Wrapped }

// resolveLLMSettings applies the LLM preset for model. CLI values
// take precedence; preset values fill in when CLI values are unset.
//
// Returns the effective (maxBatchBytes, perChunkTimeout,
// reasoningEffort). When no preset matches the model, the CLI
// values pass through unchanged. Logs at Info when a preset fires
// (or when a CLI flag overrides the preset's derived value) and
// at Warn when the preset is dangling or the derivation is infeasible.
func resolveLLMSettings(
	cfg *config.File,
	model string,
	cliMaxBatchBytes int,
	cliPerChunkTimeout time.Duration,
	cliReasoningEffort string,
	maxTokens int,
	logger *slog.Logger,
) (int, time.Duration, string) {
	maxBatchBytes := cliMaxBatchBytes
	perChunkTimeout := cliPerChunkTimeout
	reasoningEffort := cliReasoningEffort

	presetName, preset, ok := cfg.ApplyPreset(model)
	if !ok {
		// presetName is non-empty when the model mapped to a name
		// that has no matching preset definition — log loudly so
		// the operator notices the misconfiguration.
		if presetName != "" {
			logger.Warn("LLM preset references unknown name; ignoring",
				"model", model,
				"preset", presetName,
			)
		}
		return maxBatchBytes, perChunkTimeout, reasoningEffort
	}

	logger.Info("LLM preset applied",
		"preset", presetName,
		"model", model,
		"context_window", preset.ContextWindow,
	)

	if maxBatchBytes == 0 {
		switch {
		case preset.MaxBatchBytes > 0:
			// Preset-supplied override wins over the derivation.
			// Operators tune this for models whose effective context
			// is smaller than their advertised window, or for
			// models that can't process a packed batch within
			// PerChunkTimeout even when the prompt technically fits.
			maxBatchBytes = preset.MaxBatchBytes
			logger.Info("max_batch_bytes from preset",
				"preset", presetName,
				"max_batch_bytes", maxBatchBytes,
				"context_window", preset.ContextWindow,
				"max_tokens", maxTokens,
				"derived_value", config.DeriveMaxBatchBytes(preset.ContextWindow, maxTokens),
			)
		default:
			derived := config.DeriveMaxBatchBytes(preset.ContextWindow, maxTokens)
			if derived > 0 {
				maxBatchBytes = derived
				logger.Info("max_batch_bytes derived from preset",
					"preset", presetName,
					"context_window", preset.ContextWindow,
					"max_tokens", maxTokens,
					"max_batch_bytes", derived,
				)
			} else {
				logger.Warn("max_batch_bytes derivation infeasible; packing disabled",
					"preset", presetName,
					"context_window", preset.ContextWindow,
					"max_tokens", maxTokens,
				)
			}
		}
	} else {
		logger.Info("max_batch_bytes override (CLI flag wins over preset)",
			"preset", presetName,
			"cli_value", maxBatchBytes,
			"derived_value", config.DeriveMaxBatchBytes(preset.ContextWindow, maxTokens),
		)
	}

	if perChunkTimeout == 0 {
		if d, valid := config.ParsePresetTimeout(preset.PerChunkTimeout); valid {
			perChunkTimeout = d
			logger.Info("per_chunk_timeout from preset",
				"preset", presetName,
				"per_chunk_timeout", d,
			)
		}
	}

	if reasoningEffort == "" && preset.ReasoningEffort != "" {
		reasoningEffort = preset.ReasoningEffort
		logger.Info("reasoning_effort from preset",
			"preset", presetName,
			"reasoning_effort", reasoningEffort,
		)
	}

	return maxBatchBytes, perChunkTimeout, reasoningEffort
}
