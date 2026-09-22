package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	Review *ReviewCmd `cmd:"review" help:"Review a merge request once and exit."`
	Serve  *ServeCmd  `cmd:"serve"  help:"Run an HTTP server that consumes GitLab webhooks."`
	Doctor *DoctorCmd `cmd:"doctor" help:"Validate config + LLM + GitLab connectivity."`
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
